// Copyright 2017-2021 Lei Ni (nilei81@gmail.com) and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
//
// This file contains code derived from CockroachDB. The async send message
// pattern used in ASyncSend/connectAndProcess/connectAndProcess is similar
// to the one used in CockroachDB.
//
// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

/*
Package transport implements the transport component used for exchanging
Raft messages between NodeHosts.

This package is internally used by Dragonboat, applications are not expected
to import this package.
*/
package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armadakv/armada/vfs"

	"github.com/cockroachdb/errors"
	"github.com/lni/goutils/logutil"
	"github.com/lni/goutils/netutil"
	circuit "github.com/lni/goutils/netutil/rubyist/circuitbreaker"
	"github.com/lni/goutils/syncutil"

	"github.com/armadakv/armada/raft/config"
	"github.com/armadakv/armada/raft/internal/registry"
	"github.com/armadakv/armada/raft/internal/server"
	"github.com/armadakv/armada/raft/internal/settings"
	"github.com/armadakv/armada/raft/logger"
	"github.com/armadakv/armada/raft/raftio"
	pb "github.com/armadakv/armada/raft/raftpb"
)

const (
	maxMsgBatchSize = settings.MaxMessageBatchSize
)

var lazyFreeCycle = settings.LazyFreeCycle

var (
	plog                = logger.GetLogger("transport")
	sendQueueLen        = settings.SendQueueLength
	dialTimeoutSecond   = settings.GetConnectedTimeoutSecond
	idleTimeout         = time.Minute
	errChunkSendSkipped = errors.New("chunk skipped")
	errBatchSendSkipped = errors.New("batch skipped")
	dn                  = logutil.DescribeNode
)

// IMessageHandler is the interface required to handle incoming raft requests.
type IMessageHandler interface {
	HandleMessageBatch(batch pb.MessageBatch) (uint64, uint64)
	HandleUnreachable(shardID uint64, replicaID uint64)
	HandleSnapshotStatus(shardID uint64, replicaID uint64, rejected bool)
	HandleSnapshot(shardID uint64, replicaID uint64, from uint64)
}

// ITransport is the interface of the transport layer used for exchanging
// Raft messages.
type ITransport interface {
	Name() string
	Send(pb.Message) bool
	SendSnapshot(pb.Message) bool
	GetStreamSink(shardID uint64, replicaID uint64) *Sink
	Close() error
}

//
// funcs used mainly in testing
//

// StreamChunkSendFunc is a func type that is used to determine whether a
// snapshot chunk should indeed be sent. This func is used in test only.
type StreamChunkSendFunc func(pb.Chunk) (pb.Chunk, bool)

// SendMessageBatchFunc is a func type that is used to determine whether the
// specified message batch should be sent. This func is used in test only.
type SendMessageBatchFunc func(pb.MessageBatch) (pb.MessageBatch, bool)

type sendQueue struct {
	ch chan pb.Message
	rl *server.RateLimiter
}

func (sq *sendQueue) rateLimited() bool {
	return sq.rl.RateLimited()
}

func (sq *sendQueue) increase(msg pb.Message) {
	if msg.Type != pb.Replicate {
		return
	}
	sq.rl.Increase(pb.GetEntrySliceInMemSize(msg.Entries))
}

func (sq *sendQueue) decrease(msg pb.Message) {
	if msg.Type != pb.Replicate {
		return
	}
	sq.rl.Decrease(pb.GetEntrySliceInMemSize(msg.Entries))
}

// ITransportEvent is the interface for notifying connection status changes.
type ITransportEvent interface {
	ConnectionEstablished(string, bool)
	ConnectionFailed(string, bool)
}

type failedSend uint64

type nodeMap map[raftio.NodeInfo]struct{}

const (
	success failedSend = iota
	circuitBreakerNotReady
	unknownTarget
	rateLimited
	chanIsFull
)

// DefaultTransportFactory is the default transport module used.
type DefaultTransportFactory struct{}

// Create creates a default transport instance.
func (dtm *DefaultTransportFactory) Create(nhConfig config.NodeHostConfig,
	handler raftio.MessageHandler,
	chunkHandler raftio.ChunkHandler,
) raftio.ITransport {
	return NewTCPTransport(nhConfig, handler, chunkHandler)
}

// Validate returns a boolean value indicating whether the specified address is
// valid.
func (dtm *DefaultTransportFactory) Validate(addr string) bool {
	panic("not suppose to be called")
}

// Transport is the transport layer for delivering raft messages and snapshots.
type Transport struct {
	mu struct {
		sync.Mutex
		queues   map[string]sendQueue
		breakers map[string]*circuit.Breaker
	}
	sysEvents    ITransportEvent
	ctx          context.Context
	preSendBatch atomic.Value
	preSend      atomic.Value
	postSend     atomic.Value
	msgHandler   IMessageHandler
	resolver     registry.IResolver
	trans        raftio.ITransport
	fs           vfs.FS
	stopper      *syncutil.Stopper
	dir          server.SnapshotDirFunc
	env          *server.Env
	metrics      *transportMetrics
	chunks       *Chunk
	cancel       context.CancelFunc
	sourceID     string
	nhConfig     config.NodeHostConfig
	jobs         uint64
}

var _ ITransport = (*Transport)(nil)

// NewTransport creates a new Transport object.
func NewTransport(nhConfig config.NodeHostConfig,
	handler IMessageHandler, env *server.Env, resolver registry.IResolver,
	dir server.SnapshotDirFunc, sysEvents ITransportEvent,
	fs vfs.FS,
) (*Transport, error) {
	sourceID := nhConfig.RaftAddress
	if nhConfig.NodeRegistryEnabled() {
		sourceID = env.NodeHostID()
	}
	t := &Transport{
		nhConfig:   nhConfig,
		env:        env,
		sourceID:   sourceID,
		resolver:   resolver,
		stopper:    syncutil.NewStopper(),
		dir:        dir,
		sysEvents:  sysEvents,
		fs:         fs,
		msgHandler: handler,
	}
	chunks := NewChunk(t.handleRequest,
		t.snapshotReceived, t.dir, t.nhConfig.GetDeploymentID(), fs)
	t.trans = create(nhConfig, t.handleRequest, chunks.Add)
	t.chunks = chunks
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.mu.queues = make(map[string]sendQueue)
	t.mu.breakers = make(map[string]*circuit.Breaker)
	msgConn := func() float64 {
		t.mu.Lock()
		defer t.mu.Unlock()
		return float64(len(t.mu.queues))
	}
	ssCount := func() float64 {
		return float64(atomic.LoadUint64(&t.jobs))
	}
	t.metrics = newTransportMetrics(true, msgConn, ssCount)

	plog.Infof("transport type: %s", t.trans.Name())
	if err := t.trans.Start(); err != nil {
		plog.Errorf("transport failed to start %v", err)
		if cerr := t.trans.Close(); cerr != nil {
			plog.Errorf("failed to close the transport module %v", cerr)
		}
		return nil, err
	}
	t.stopper.RunWorker(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				chunks.Tick()
			case <-t.stopper.ShouldStop():
				return
			}
		}
	})
	return t, nil
}

// Name returns the type name of the transport module
func (t *Transport) Name() string {
	return t.trans.Name()
}

// GetTrans returns the transport instance.
func (t *Transport) GetTrans() raftio.ITransport {
	return t.trans
}

// SetPreSendBatchHook set the SendMessageBatch hook.
// This function is only expected to be used in monkey testing.
func (t *Transport) SetPreSendBatchHook(h SendMessageBatchFunc) {
	t.preSendBatch.Store(h)
}

// SetPreStreamChunkSendHook sets the StreamChunkSend hook function that will
// be called before each snapshot chunk is sent.
func (t *Transport) SetPreStreamChunkSendHook(h StreamChunkSendFunc) {
	t.preSend.Store(h)
}

// Close closes the Transport object.
func (t *Transport) Close() error {
	t.cancel()
	t.stopper.Stop()
	t.chunks.Close()
	return t.trans.Close()
}

// GetCircuitBreaker returns the circuit breaker used for the specified
// target node.
func (t *Transport) GetCircuitBreaker(key string) *circuit.Breaker {
	t.mu.Lock()
	breaker, ok := t.mu.breakers[key]
	if !ok {
		breaker = netutil.NewBreaker()
		t.mu.breakers[key] = breaker
	}
	t.mu.Unlock()

	return breaker
}

func (t *Transport) handleRequest(req pb.MessageBatch) {
	did := t.nhConfig.GetDeploymentID()
	if req.DeploymentId != did {
		plog.Warningf("deployment id does not match %d vs %d, message dropped",
			req.DeploymentId, did)
		return
	}
	if req.BinVer != raftio.TransportBinVersion {
		plog.Warningf("binary compatibility version not match %d vs %d",
			req.BinVer, raftio.TransportBinVersion)
		return
	}
	addr := req.SourceAddress
	if len(addr) > 0 {
		for _, r := range req.Requests {
			if r.From != 0 {
				t.resolver.Add(r.ShardID, r.From, addr)
			}
		}
	}
	ssCount, msgCount := t.msgHandler.HandleMessageBatch(req)
	dropedMsgCount := uint64(len(req.Requests)) - ssCount - msgCount
	t.metrics.receivedMessages(ssCount, msgCount, dropedMsgCount)
}

func (t *Transport) snapshotReceived(shardID uint64,
	replicaID uint64, from uint64,
) {
	t.msgHandler.HandleSnapshot(shardID, replicaID, from)
}

func (t *Transport) notifyUnreachable(addr string, affected nodeMap) {
	plog.Warningf("%s became unreachable, affected %d nodes", addr, len(affected))
	for n := range affected {
		t.msgHandler.HandleUnreachable(n.ShardID, n.ReplicaID)
	}
}

// Send asynchronously sends raft messages to their target nodes.
//
// The generic async send Go pattern used in Send() is found in CockroachDB's
// codebase.
func (t *Transport) Send(req pb.Message) bool {
	v, _ := t.send(req)
	if !v {
		t.metrics.messageSendFailure(1)
	}
	return v
}

func (t *Transport) send(req pb.Message) (bool, failedSend) {
	if req.Type == pb.InstallSnapshot {
		panic("snapshot message must be sent via its own channel.")
	}
	toReplicaID := req.To
	shardID := req.ShardID
	from := req.From
	addr, key, err := t.resolver.Resolve(shardID, toReplicaID)
	if err != nil {
		return false, unknownTarget
	}
	// fail fast
	if !t.GetCircuitBreaker(addr).Ready() {
		t.metrics.messageConnectionFailure()
		return false, circuitBreakerNotReady
	}
	// get the channel, create it in case it is not in the queue map
	t.mu.Lock()
	sq, ok := t.mu.queues[key]
	if !ok {
		sq = sendQueue{
			ch: make(chan pb.Message, sendQueueLen),
			rl: server.NewRateLimiter(t.nhConfig.MaxSendQueueSize),
		}
		t.mu.queues[key] = sq
	}
	t.mu.Unlock()
	if !ok {
		shutdownQueue := func() {
			t.mu.Lock()
			delete(t.mu.queues, key)
			t.mu.Unlock()
		}
		t.stopper.RunWorker(func() {
			affected := make(nodeMap)
			if !t.connectAndProcess(addr, sq, from, affected) {
				t.notifyUnreachable(addr, affected)
			}
			shutdownQueue()
		})
	}
	if sq.rateLimited() {
		return false, rateLimited
	}

	sq.increase(req)

	select {
	case sq.ch <- req:
		return true, success
	default:
		sq.decrease(req)
		return false, chanIsFull
	}
}

// connectAndProcess returns a boolean value indicating whether it is stopped
// gracefully when the system is being shutdown
func (t *Transport) connectAndProcess(remoteHost string,
	sq sendQueue, from uint64, affected nodeMap,
) bool {
	breaker := t.GetCircuitBreaker(remoteHost)
	successes := breaker.Successes()
	consecFailures := breaker.ConsecFailures()
	if err := func() error {
		plog.Debugf("%s is trying to connect to %s", t.sourceID, remoteHost)
		conn, err := t.trans.GetConnection(t.ctx, remoteHost)
		if err != nil {
			plog.Errorf("Nodehost %s failed to get a connection to %s, %v",
				t.sourceID, remoteHost, err)
			return err
		}
		defer conn.Close()
		breaker.Success()
		if successes == 0 || consecFailures > 0 {
			plog.Debugf("message streaming to %s established", remoteHost)
			t.sysEvents.ConnectionEstablished(remoteHost, false)
		}
		return t.processMessages(remoteHost, sq, conn, affected)
	}(); err != nil {
		plog.Warningf("breaker %s to %s failed, connect and process failed: %s",
			t.sourceID, remoteHost, err.Error())
		breaker.Fail()
		t.metrics.messageConnectionFailure()
		t.sysEvents.ConnectionFailed(remoteHost, false)
		return false
	}
	return true
}

func (t *Transport) processMessages(remoteHost string,
	sq sendQueue, conn raftio.IConnection, affected nodeMap,
) error {
	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()
	sz := uint64(0)
	batch := pb.MessageBatch{
		SourceAddress: t.sourceID,
		BinVer:        raftio.TransportBinVersion,
	}
	did := t.nhConfig.GetDeploymentID()
	requests := make([]pb.Message, 0)
	for {
		idleTimer.Reset(idleTimeout)
		select {
		case <-t.stopper.ShouldStop():
			return nil
		case <-idleTimer.C:
			return nil
		case req := <-sq.ch:
			n := raftio.NodeInfo{
				ShardID:   req.ShardID,
				ReplicaID: req.From,
			}
			affected[n] = struct{}{}
			sq.decrease(req)
			sz += uint64(req.SizeUpperLimit())
			requests = append(requests, req)
			for done := false; !done && sz < maxMsgBatchSize; {
				select {
				case req = <-sq.ch:
					sq.decrease(req)
					sz += uint64(req.SizeUpperLimit())
					requests = append(requests, req)
				case <-t.stopper.ShouldStop():
					return nil
				default:
					done = true
				}
			}
			batch.DeploymentId = did
			twoBatch := false
			if sz < maxMsgBatchSize || len(requests) == 1 {
				batch.Requests = requests
			} else {
				twoBatch = true
				batch.Requests = requests[:len(requests)-1]
			}
			if err := t.sendMessageBatch(conn, batch); err != nil {
				plog.Errorf("send batch failed, target %s (%v), %d",
					remoteHost, err, len(batch.Requests))
				return err
			}
			if twoBatch {
				batch.Requests = []pb.Message{requests[len(requests)-1]}
				if err := t.sendMessageBatch(conn, batch); err != nil {
					plog.Errorf("send batch failed, taret node %s (%v), %d",
						remoteHost, err, len(batch.Requests))
					return err
				}
			}
			sz = 0
			requests, batch = lazyFree(requests, batch)
			requests = requests[:0]
		}
	}
}

func lazyFree(reqs []pb.Message,
	mb pb.MessageBatch,
) ([]pb.Message, pb.MessageBatch) {
	if lazyFreeCycle > 0 {
		for i := 0; i < len(reqs); i++ {
			reqs[i].Entries = nil
		}
		mb.Requests = []pb.Message{}
	}
	return reqs, mb
}

func (t *Transport) sendMessageBatch(conn raftio.IConnection,
	batch pb.MessageBatch,
) error {
	if f := t.preSendBatch.Load(); f != nil {
		updated, shouldSend := f.(SendMessageBatchFunc)(batch)
		if !shouldSend {
			return errBatchSendSkipped
		}
		return conn.SendMessageBatch(updated)
	}
	if err := conn.SendMessageBatch(batch); err != nil {
		t.metrics.messageSendFailure(uint64(len(batch.Requests)))
		return err
	}
	t.metrics.messageSendSuccess(uint64(len(batch.Requests)))
	return nil
}

func create(nhConfig config.NodeHostConfig,
	requestHandler raftio.MessageHandler,
	chunkHandler raftio.ChunkHandler,
) raftio.ITransport {
	var tm config.TransportFactory
	if nhConfig.Expert.TransportFactory != nil {
		tm = nhConfig.Expert.TransportFactory
	} else {
		tm = &DefaultTransportFactory{}
	}
	return tm.Create(nhConfig, requestHandler, chunkHandler)
}

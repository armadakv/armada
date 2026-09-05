// Copyright JAMF Software, LLC

package storage

import (
	"sync"
	"sync/atomic"

	"github.com/armadakv/armada/raft/raftio"
)

const diagnosticEventQueueSize = 128

type events struct {
	refreshCh     chan struct{}
	compactionCh  chan struct{}
	diagnosticsCh chan any
	stopc         chan struct{}
	donec         chan struct{}
	engine        *Engine
	started       atomic.Bool
	stopOnce      sync.Once
	workers       sync.WaitGroup

	mu          sync.Mutex
	compactions map[uint64]uint64
}

func newEvents(engine *Engine) *events {
	return &events{
		refreshCh:     make(chan struct{}, 1),
		compactionCh:  make(chan struct{}, 1),
		diagnosticsCh: make(chan any, diagnosticEventQueueSize),
		stopc:         make(chan struct{}),
		donec:         make(chan struct{}),
		engine:        engine,
		compactions:   make(map[uint64]uint64),
	}
}

func (e *events) Start() {
	if !e.started.CompareAndSwap(false, true) {
		return
	}
	e.workers.Add(2)
	go func() {
		defer e.workers.Done()
		e.dispatchClusterRefreshes()
	}()
	go func() {
		defer e.workers.Done()
		e.dispatchCompactions()
	}()
	go e.dispatchDiagnostics()
	go func() {
		e.workers.Wait()
		close(e.donec)
	}()
}

// Stop releases callbacks waiting to enqueue work and waits for the
// correctness-relevant workers. Diagnostic logging is intentionally not waited
// on: logging must never delay Engine shutdown.
func (e *events) Stop() {
	e.stopOnce.Do(func() { close(e.stopc) })
	if e.started.Load() {
		<-e.donec
	}
}

func (e *events) dispatchClusterRefreshes() {
	for {
		select {
		case <-e.stopc:
			return
		case <-e.refreshCh:
			e.engine.Cluster.Notify()
		}
	}
}

func (e *events) dispatchCompactions() {
	for {
		select {
		case <-e.stopc:
			return
		case <-e.compactionCh:
			for shardID, index := range e.takeCompactions() {
				e.engine.NotifyLogCompacted(shardID, index)
			}
		}
	}
}

func (e *events) dispatchDiagnostics() {
	for {
		select {
		case <-e.stopc:
			return
		case event := <-e.diagnosticsCh:
			e.engine.log.Infof("raft: %T %+v", event, event)
		}
	}
}

func (e *events) signalRefresh() {
	select {
	case <-e.stopc:
		return
	default:
	}
	select {
	case <-e.stopc:
	case e.refreshCh <- struct{}{}:
	default:
	}
}

func (e *events) addCompaction(shardID uint64, index uint64) {
	select {
	case <-e.stopc:
		return
	default:
	}
	e.mu.Lock()
	if existing, ok := e.compactions[shardID]; !ok || index > existing {
		e.compactions[shardID] = index
	}
	e.mu.Unlock()
	select {
	case <-e.stopc:
		return
	default:
	}
	select {
	case <-e.stopc:
	case e.compactionCh <- struct{}{}:
	default:
	}
}

func (e *events) takeCompactions() map[uint64]uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	pending := e.compactions
	e.compactions = make(map[uint64]uint64)
	return pending
}

// diagnostic records best-effort observability data. All correctness-relevant
// Raft events use the dedicated refresh or compaction paths above.
func (e *events) diagnostic(event any) {
	select {
	case <-e.stopc:
		return
	default:
	}
	select {
	case <-e.stopc:
	case e.diagnosticsCh <- event:
	default:
		e.engine.log.Debugf("raft diagnostic event queue is full, dropped %T", event)
	}
}

type leaderUpdated struct {
	ShardID   uint64
	ReplicaID uint64
	Term      uint64
	LeaderID  uint64
}

func (e *events) LeaderUpdated(info raftio.LeaderInfo) {
	e.signalRefresh()
	e.diagnostic(leaderUpdated{ShardID: info.ShardID, ReplicaID: info.ReplicaID, Term: info.Term, LeaderID: info.LeaderID})
}

type nodeHostShuttingDown struct{}

func (e *events) NodeHostShuttingDown() {
	e.Stop()
}

type nodeUnloaded struct {
	ShardID   uint64
	ReplicaID uint64
}

func (e *events) NodeUnloaded(info raftio.NodeInfo) {
	e.signalRefresh()
	e.diagnostic(nodeUnloaded{ShardID: info.ShardID, ReplicaID: info.ReplicaID})
}

type nodeDeleted struct {
	ShardID   uint64
	ReplicaID uint64
}

func (e *events) NodeDeleted(info raftio.NodeInfo) {
	e.signalRefresh()
	e.diagnostic(nodeDeleted{ShardID: info.ShardID, ReplicaID: info.ReplicaID})
}

type nodeReady struct {
	ShardID   uint64
	ReplicaID uint64
}

func (e *events) NodeReady(info raftio.NodeInfo) {
	e.signalRefresh()
	e.diagnostic(nodeReady{ShardID: info.ShardID, ReplicaID: info.ReplicaID})
}

type membershipChanged struct {
	ShardID   uint64
	ReplicaID uint64
}

func (e *events) MembershipChanged(info raftio.NodeInfo) {
	e.signalRefresh()
	e.diagnostic(membershipChanged{ShardID: info.ShardID, ReplicaID: info.ReplicaID})
}

type connectionEstablished struct {
	Address            string
	SnapshotConnection bool
}

func (e *events) ConnectionEstablished(info raftio.ConnectionInfo) {
	e.diagnostic(connectionEstablished{Address: info.Address, SnapshotConnection: info.SnapshotConnection})
}

type connectionFailed struct {
	Address            string
	SnapshotConnection bool
}

func (e *events) ConnectionFailed(info raftio.ConnectionInfo) {
	e.diagnostic(connectionFailed{Address: info.Address, SnapshotConnection: info.SnapshotConnection})
}

type sendSnapshotStarted struct {
	ShardID   uint64
	ReplicaID uint64
	From      uint64
	Index     uint64
}

func (e *events) SendSnapshotStarted(info raftio.SnapshotInfo) {
	e.diagnostic(sendSnapshotStarted{ShardID: info.ShardID, ReplicaID: info.ReplicaID, From: info.From, Index: info.Index})
}

type sendSnapshotCompleted struct {
	ShardID   uint64
	ReplicaID uint64
	From      uint64
	Index     uint64
}

func (e *events) SendSnapshotCompleted(info raftio.SnapshotInfo) {
	e.diagnostic(sendSnapshotCompleted{ShardID: info.ShardID, ReplicaID: info.ReplicaID, From: info.From, Index: info.Index})
}

type sendSnapshotAborted struct {
	ShardID   uint64
	ReplicaID uint64
	From      uint64
	Index     uint64
}

func (e *events) SendSnapshotAborted(info raftio.SnapshotInfo) {
	e.diagnostic(sendSnapshotAborted{ShardID: info.ShardID, ReplicaID: info.ReplicaID, From: info.From, Index: info.Index})
}

type snapshotReceived struct {
	ShardID   uint64
	ReplicaID uint64
	From      uint64
	Index     uint64
}

func (e *events) SnapshotReceived(info raftio.SnapshotInfo) {
	e.diagnostic(snapshotReceived{ShardID: info.ShardID, ReplicaID: info.ReplicaID, From: info.From, Index: info.Index})
}

type snapshotRecovered struct {
	ShardID   uint64
	ReplicaID uint64
	From      uint64
	Index     uint64
}

func (e *events) SnapshotRecovered(info raftio.SnapshotInfo) {
	e.diagnostic(snapshotRecovered{ShardID: info.ShardID, ReplicaID: info.ReplicaID, From: info.From, Index: info.Index})
}

type snapshotCreated struct {
	ShardID   uint64
	ReplicaID uint64
	From      uint64
	Index     uint64
}

func (e *events) SnapshotCreated(info raftio.SnapshotInfo) {
	e.diagnostic(snapshotCreated{ShardID: info.ShardID, ReplicaID: info.ReplicaID, From: info.From, Index: info.Index})
}

type snapshotCompacted struct {
	ShardID   uint64
	ReplicaID uint64
	From      uint64
	Index     uint64
}

func (e *events) SnapshotCompacted(info raftio.SnapshotInfo) {
	e.diagnostic(snapshotCompacted{ShardID: info.ShardID, ReplicaID: info.ReplicaID, From: info.From, Index: info.Index})
}

type logCompacted struct {
	ShardID   uint64
	ReplicaID uint64
	Index     uint64
}

func (e *events) LogCompacted(info raftio.EntryInfo) {
	e.addCompaction(info.ShardID, info.Index)
	e.diagnostic(logCompacted{ShardID: info.ShardID, ReplicaID: info.ReplicaID, Index: info.Index})
}

type logDBCompacted struct {
	ShardID   uint64
	ReplicaID uint64
}

func (e *events) LogDBCompacted(info raftio.EntryInfo) {
	e.diagnostic(logDBCompacted{ShardID: info.ShardID, ReplicaID: info.ReplicaID})
}

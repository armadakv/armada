// Copyright JAMF Software, LLC

package replication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/raft/client"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/armada/storage"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/table"
	"github.com/benbjohnson/clock"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// TODO make configurable.
	desiredProposalSize           = 256 * 1024
	queueLenCheckInterval         = 50 * time.Millisecond
	metricsCheckInterval          = 5 * time.Second
	downloadCheckpointBytes int64 = 4 * 1024 * 1024
)

var downloadLeaseRefreshInterval = store.LeaseTTL / 3

type replicateResult int

const (
	resultUnknown replicateResult = iota
	resultLeaderBehind
	resultLeaderAhead
	resultFollowerLagging
	resultFollowerTailing
	resultTableNotExists
	resultBackoff
)

type workerFactory struct {
	reconcileInterval time.Duration
	pollInterval      time.Duration
	leaseInterval     time.Duration
	logTimeout        time.Duration
	snapshotTimeout   time.Duration
	recoverySemaphore *semaphore.Weighted
	log               *zap.SugaredLogger
	store             replicationManagerStore
	engine            *storage.Engine
	queue             *storage.IndexNotificationQueue
	logClient         armadapb.LogClient
	snapshotAccess    SnapshotAccess
	metrics           struct {
		replicationIndex  *prometheus.GaugeVec
		replicationLeased *prometheus.GaugeVec
	}
}

type tableQueue struct {
	table string
	queue *storage.IndexNotificationQueue
}

func (q tableQueue) Len() int {
	if q.queue == nil {
		return 0
	}
	return q.queue.Len(q.table)
}

type tableQueueLenStore struct {
	clock clock.Clock
	table string
	store replicationManagerStore
	id    uint64
}

func (tql tableQueueLenStore) Max() (uint64, error) {
	key := fmt.Sprintf("queue/%s/*", tql.table)
	p, err := tql.store.GetAllValues(key)
	if err != nil {
		return 0, err
	}
	var m uint64
	for _, s := range p {
		sep := strings.Split(s, "$")
		t, err := strconv.ParseInt(sep[0], 10, 64)
		if err != nil {
			return 0, err
		}
		pt := time.UnixMilli(t)
		if tql.clock.Since(pt) >= 30*time.Second {
			return 0, nil
		}
		v, err := strconv.ParseUint(sep[1], 10, 64)
		if err != nil {
			return 0, err
		}
		m = max(m, v)
	}
	return m, nil
}

func (tql tableQueueLenStore) Set(i uint64) error {
	key := fmt.Sprintf("queue/%s/%d", tql.table, tql.id)
	p, err := tql.store.Get(key)
	if err != nil && !errors.Is(err, kv.ErrNotExist) {
		return err
	}
	_, err = tql.store.Set(key, fmt.Sprintf("%d$%d", tql.clock.Now().UnixMilli(), i), p.Ver)
	return err
}

func (tql tableQueueLenStore) Get() (uint64, error) {
	key := fmt.Sprintf("queue/%s/%d", tql.table, tql.id)
	p, err := tql.store.Get(key)
	if err != nil {
		if errors.Is(err, kv.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	sep := strings.Split(p.Value, "$")
	ct, err := strconv.ParseInt(sep[0], 10, 64)
	if err != nil {
		return 0, err
	}
	pt := time.UnixMilli(ct)
	if tql.clock.Since(pt) >= 30*time.Second {
		return 0, nil
	}
	v, err := strconv.ParseUint(sep[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func (f *workerFactory) create(table string, source sourceTableState) *worker {
	workerCtx, workerCancel := context.WithCancel(context.Background())
	w := &worker{
		workerFactory: f,
		table:         table,
		sourceID:      source.ClusterID,
		closer:        make(chan struct{}),
		workerCtx:     workerCtx,
		workerCancel:  workerCancel,
		log:           f.log.Named(table),
		queue:         tableQueue{table: table, queue: f.queue},
		store:         tableQueueLenStore{table: table, store: f.store, id: f.engine.Config().NodeID, clock: clock.New()},
		throttle:      newThrottle(f.pollInterval),
		immediate:     make(chan time.Time, 1),
		metrics: struct {
			replicationLeaderIndex   prometheus.Gauge
			replicationFollowerIndex prometheus.Gauge
			replicationLeased        prometheus.Gauge
		}{
			replicationLeaderIndex:   f.metrics.replicationIndex.WithLabelValues("leader", table),
			replicationFollowerIndex: f.metrics.replicationIndex.WithLabelValues("follower", table),
			replicationLeased:        f.metrics.replicationLeased.WithLabelValues(table),
		},
	}
	w.forceRecovery.Store(source.NeedsFullRestore)
	return w
}

type replicationThrottle struct {
	intervals [5]time.Duration
	speed     int
}

func (t *replicationThrottle) current() time.Duration {
	return t.intervals[t.speed]
}

func (t *replicationThrottle) up() {
	t.speed = min(t.speed+1, len(t.intervals)-1)
}

func (t *replicationThrottle) down() {
	t.speed = max(t.speed-1, 0)
}

func newThrottle(pollInterval time.Duration) replicationThrottle {
	return replicationThrottle{intervals: [5]time.Duration{pollInterval, pollInterval / 2, pollInterval / 4, pollInterval / 16, pollInterval / 256}}
}

// worker connects to the log replication service and synchronizes the local state.
type worker struct {
	*workerFactory
	table           string
	sourceID        uint64
	forceRecovery   atomic.Bool
	closer          chan struct{}
	log             *zap.SugaredLogger
	queue           tableQueue
	store           tableQueueLenStore
	throttle        replicationThrottle
	leased          atomic.Bool
	leaseMu         sync.Mutex
	leaseCtx        context.Context
	leaseCancel     context.CancelFunc
	leaseTimer      *time.Timer
	leaseGeneration uint64
	workerCtx       context.Context
	workerCancel    context.CancelFunc
	metrics         struct {
		replicationLeaderIndex   prometheus.Gauge
		replicationFollowerIndex prometheus.Gauge
		replicationLeased        prometheus.Gauge
	}
	wg        sync.WaitGroup
	immediate chan time.Time
}

func (w *worker) acquireLease() {
	w.leaseMu.Lock()
	if w.workerCtx != nil && w.workerCtx.Err() != nil {
		w.leaseMu.Unlock()
		return
	}
	if w.leaseCtx == nil {
		parent := w.workerCtx
		if parent == nil {
			parent = context.Background()
		}
		w.leaseGeneration++
		w.leaseCtx, w.leaseCancel = context.WithCancel(parent)
	}
	if w.leaseTimer != nil {
		w.leaseTimer.Stop()
	}
	leaseDuration := time.Duration(0)
	if w.workerFactory != nil {
		leaseDuration = w.leaseInterval * 4
	}
	if leaseDuration > 0 {
		generation := w.leaseGeneration
		w.leaseTimer = time.AfterFunc(leaseDuration, func() {
			w.expireLease(generation)
		})
	}
	wasLeased := w.leased.Swap(true)
	w.leaseMu.Unlock()

	if !wasLeased && w.metrics.replicationLeased != nil {
		w.metrics.replicationLeased.Set(1)
	}
}

func (w *worker) expireLease(generation uint64) {
	w.leaseMu.Lock()
	if w.leaseCtx == nil || w.leaseGeneration != generation {
		w.leaseMu.Unlock()
		return
	}
	cancel := w.leaseCancel
	w.leaseCtx = nil
	w.leaseCancel = nil
	w.leaseTimer = nil
	wasLeased := w.leased.Swap(false)
	w.leaseMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if wasLeased && w.metrics.replicationLeased != nil {
		w.metrics.replicationLeased.Set(0)
	}
}

func (w *worker) loseLease() {
	w.leaseMu.Lock()
	if w.leaseTimer != nil {
		w.leaseTimer.Stop()
		w.leaseTimer = nil
	}
	cancel := w.leaseCancel
	w.leaseCtx = nil
	w.leaseCancel = nil
	wasLeased := w.leased.Swap(false)
	w.leaseMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if wasLeased && w.metrics.replicationLeased != nil {
		w.metrics.replicationLeased.Set(0)
	}
}

func (w *worker) leaseWork() (context.Context, bool) {
	w.leaseMu.Lock()
	defer w.leaseMu.Unlock()
	if !w.leased.Load() || w.leaseCtx == nil || w.leaseCtx.Err() != nil {
		return nil, false
	}
	return w.leaseCtx, true
}

// Start launches the replication goroutine. To stop it, call worker.Close.
func (w *worker) Start() {
	// Sleep up to reconcile interval to prevent the thundering herd
	// #nosec G404 -- Weak random number generator can be used because we do not care whether the result can be predicted.
	time.Sleep(time.Duration(rand.IntN(int(w.pollInterval.Milliseconds()))) * time.Millisecond)

	w.wg.Add(1)
	go func() {
		defer func() {
			w.log.Info("lease routine stopped")
			w.wg.Done()
		}()

		w.log.Info("lease routine started")
		t := time.NewTicker(w.leaseInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := w.engine.LeaseTable(w.table, w.leaseInterval*4); err != nil {
					w.loseLease()
				} else {
					w.acquireLease()
				}
			case <-w.closer:
				return
			}
		}
	}()

	w.wg.Add(1)
	go func() {
		defer func() {
			w.log.Info("stats routine stopped")
			w.wg.Done()
		}()

		w.log.Info("stats routine started")
		tq := time.NewTicker(queueLenCheckInterval)
		defer tq.Stop()
		tidx := time.NewTicker(metricsCheckInterval)
		defer tidx.Stop()
		for {
			select {
			case <-tidx.C:
				if idx, _, err := w.tableState(context.Background()); err == nil {
					w.metrics.replicationFollowerIndex.Set(float64(idx))
				}
			case <-tq.C:
				if v, _ := w.store.Get(); v != uint64(w.queue.Len()) {
					err := w.store.Set(uint64(w.queue.Len()))
					if err != nil {
						w.log.Errorf("unable to store current replication queue len: %v", err)
					}
				}
				if m, _ := w.store.Max(); m > 0 {
					select {
					case w.immediate <- time.Now():
					default:
					}
				}
			case <-w.closer:
				return
			}
		}
	}()

	w.wg.Add(1)
	go func() {
		defer func() {
			w.log.Info("replication routine stopped")
			w.wg.Done()
		}()

		w.log.Info("replication routine started")
		t := time.NewTicker(w.pollInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
			case <-w.immediate:
			case <-w.closer:
				return
			}

			leaseCtx, leased := w.leaseWork()
			if !leased {
				w.log.Debug("skipping replication - table not leased")
				continue
			}
			if w.forceRecovery.Load() {
				if err := w.recover(leaseCtx); err != nil {
					if errors.Is(err, table.ErrRecoveryPending) {
						// Say which record is holding things up. A recovery that
						// never advances wedges replication entirely now that
						// pending is no longer flattened to success, so the
						// phase and owner are the first things needed.
						if rec, ok, rerr := w.engine.RecoveryStatus(w.table); rerr == nil && ok {
							w.log.Debugf("recovery in flight (phase %s, coordinator %d, shard %d); holding off log replication",
								rec.Phase, rec.Coordinator, rec.RecoveryID)
						} else {
							w.log.Debugf("recovery still in flight; holding off log replication")
						}
					} else {
						w.log.Warnf("error in required full recovery: %v", err)
					}
					continue
				}
				if err := leaseCtx.Err(); err != nil {
					continue
				}
				if err := markSourceTableRestored(w.workerFactory.store, w.table, w.sourceID); err != nil {
					w.log.Warnf("could not persist completed full recovery: %v", err)
					continue
				}
				w.forceRecovery.Store(false)
				continue
			}
			t.Reset(w.throttle.current())
			if u, _ := w.store.Max(); u > 0 {
				w.throttle.up()
			} else {
				w.throttle.down()
			}
			idx, id, err := w.tableState(leaseCtx)
			if err != nil {
				if errors.Is(err, serrors.ErrTableNotFound) {
					w.log.Debugf("table not found: %v", err)
					continue
				}
				w.log.Errorf("cannot query leader index: %v", err)
				continue
			}
			result, err := w.do(leaseCtx, idx, w.engine.GetNoOPSession(id))
			switch result {
			case resultTableNotExists:
				w.log.Infof("the leader table disappeared ... backing off")
				// Give reconciler time to clean up the table.
				<-t.C
				t.Reset(2 * w.reconcileInterval)
			case resultLeaderBehind:
				w.log.Errorf("the leader log is behind ... backing off")
				// Give leader time to catch up.
				<-t.C
				t.Reset(10 * w.pollInterval)
			case resultBackoff:
				w.log.Infof("the leader asked for backoff ... backing off")
				// Give leader time to catch up.
				<-t.C
				t.Reset(10 * w.pollInterval)
			case resultLeaderAhead:
				if w.recoverySemaphore.TryAcquire(1) {
					func() {
						defer w.recoverySemaphore.Release(1)
						if err := w.recover(leaseCtx); err != nil {
							if errors.Is(err, table.ErrRecoveryPending) {
								w.log.Debugf("recovery still in flight")
							} else {
								w.log.Warnf("error in recovering table: %v", err)
							}
						}
					}()
				} else {
					w.log.Info("maximum number of recoveries already running")
					w.loseLease()
					if _, err := w.engine.ReturnTable(w.table); err != nil {
						w.log.Warnf("error returning table: %v", err)
					}
				}
			case resultFollowerLagging:
				// Burst when lagging behind the leader.
				w.throttle.up()
			case resultFollowerTailing:
			case resultUnknown:
				if err != nil {
					if errors.Is(err, context.Canceled) {
						continue
					}
					if errors.Is(err, context.DeadlineExceeded) {
						w.log.Warnf("unable to read leader log in time: %v", err)
					} else {
						w.log.Warnf("unknown worker error: %v", err)
					}
				}
			}
		}
	}()
}

// Close stops the replication.
func (w *worker) Close() {
	w.log.Info("worker stopped")
	if w.workerCancel != nil {
		w.workerCancel()
	}
	w.loseLease()
	close(w.closer)
	w.wg.Wait()

	ok, err := w.engine.ReturnTable(w.table)
	if err != nil {
		w.log.Errorf("returning table failed %v", err)
	}
	if ok {
		w.log.Info("table returned")
	}
}

func (w *worker) do(parent context.Context, leaderIndex uint64, session *client.Session) (replicateResult, error) {
	replicateRequest := &armadapb.ReplicateRequest{
		LeaderIndex: leaderIndex + 1,
		Table:       []byte(w.table),
		ClusterId:   w.sourceID,
	}
	ctx, cancel := context.WithTimeout(parent, w.logTimeout)
	defer cancel()
	stream, err := w.logClient.Replicate(ctx, replicateRequest, grpc.WaitForReady(true))
	if err != nil {
		if c, ok := status.FromError(err); ok && c.Code() == codes.Unavailable {
			return resultTableNotExists, fmt.Errorf("could not open log stream: %w", err)
		}
		return resultUnknown, fmt.Errorf("could not open log stream: %w", err)
	}
	var applied uint64
	nextLeaderIndex := leaderIndex + 1
	for {
		replicateRes, err := stream.Recv()
		if err == io.EOF {
			return resultUnknown, nil
		}
		if err != nil {
			if c, ok := status.FromError(err); ok && c.Code() == codes.NotFound {
				return resultTableNotExists, fmt.Errorf("error reading replication stream: %w", c.Err())
			}
			if c, ok := status.FromError(err); ok && c.Code() == codes.FailedPrecondition {
				return resultBackoff, fmt.Errorf("error reading replication stream: %w", c.Err())
			}
			return resultUnknown, fmt.Errorf("error reading replication stream: %w", err)
		}

		if replicateRes.LeaderIndex != 0 {
			w.metrics.replicationLeaderIndex.Set(float64(replicateRes.LeaderIndex))
		}

		switch res := replicateRes.Response.(type) {
		case *armadapb.ReplicateResponse_CommandsResponse:
			applied, err = w.proposeBatch(ctx, res.CommandsResponse.GetCommands(), nextLeaderIndex, session)
			if err != nil {
				return resultUnknown, fmt.Errorf("could not propose: %w", err)
			}
			nextLeaderIndex = applied + 1
		case *armadapb.ReplicateResponse_ErrorResponse:
			switch res.ErrorResponse.Error {
			case armadapb.ReplicateError_LEADER_BEHIND:
				return resultLeaderBehind, nil
			case armadapb.ReplicateError_USE_SNAPSHOT:
				return resultLeaderAhead, nil
			default:
				return resultUnknown, fmt.Errorf(
					"unknown replicate error response '%s' with id %d",
					res.ErrorResponse.Error.String(),
					res.ErrorResponse.Error,
				)
			}
		default:
			if applied != 0 && applied < replicateRes.LeaderIndex {
				return resultFollowerLagging, nil
			}
			return resultFollowerTailing, nil
		}
	}
}

func (w *worker) tableState(parent context.Context) (uint64, uint64, error) {
	t, err := w.engine.GetTable(w.table)
	if err != nil {
		return 0, 0, err
	}

	ctx, cancel := context.WithTimeout(parent, w.logTimeout)
	defer cancel()
	idxRes, err := t.LeaderIndex(ctx, true)
	if err != nil {
		return 0, 0, fmt.Errorf("could not get leader index key: %w", err)
	}
	return idxRes.Index, t.ClusterID, nil
}

func (w *worker) proposeBatch(ctx context.Context, commands []*armadapb.ReplicateCommand, expectedLeaderIndex uint64, session *client.Session) (uint64, error) {
	if len(commands) == 0 {
		return 0, fmt.Errorf("leader returned an empty replication command batch")
	}

	seq := armadapb.CommandFromVTPool()
	defer seq.ReturnToVTPool()
	var buff []byte
	propose := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		defer func() {
			seq.Sequence = seq.Sequence[:0]
			seq.LeaderIndex = nil
		}()
		size := seq.SizeVT()
		if cap(buff) < size {
			buff = make([]byte, 0, size)
		}
		buff = buff[:size]
		n, err := seq.MarshalToSizedBufferVT(buff)
		if err != nil {
			return fmt.Errorf("could not marshal command: %w", err)
		}
		if _, err := w.engine.SyncPropose(ctx, session, buff[:n]); err != nil {
			return fmt.Errorf("could not propose sequence: %w", err)
		}
		w.metrics.replicationFollowerIndex.Set(float64(*seq.LeaderIndex))
		return nil
	}

	var lastApplied uint64
	seq.Type = armadapb.Command_SEQUENCE
	for i, c := range commands {
		if err := ctx.Err(); err != nil {
			return lastApplied, err
		}
		if c == nil || c.Command == nil {
			return lastApplied, fmt.Errorf("leader returned an empty replication command")
		}
		if c.LeaderIndex == 0 || c.LeaderIndex != expectedLeaderIndex {
			return lastApplied, fmt.Errorf("leader returned source index %d, expected %d", c.LeaderIndex, expectedLeaderIndex)
		}

		if c.Command.LeaderIndex != nil && *c.Command.LeaderIndex != c.LeaderIndex {
			return lastApplied, fmt.Errorf("leader returned mismatched source indexes %d and %d", c.LeaderIndex, *c.Command.LeaderIndex)
		}

		leaderIndex := c.LeaderIndex
		c.Command.LeaderIndex = &leaderIndex
		seq.Sequence = append(seq.Sequence, c.Command)
		seq.LeaderIndex = &leaderIndex
		expectedLeaderIndex++
		if seq.SizeVT() >= desiredProposalSize || i == len(commands)-1 {
			if err := propose(); err != nil {
				return lastApplied, err
			}
			lastApplied = c.LeaderIndex
		}
	}

	return lastApplied, nil
}

// recover brings the local table back into range of the leader's log.
//
// Recovery state belongs to the table manager. The worker only resolves and
// downloads one pinned artifact; the next Log.Replicate call decides whether
// the recovered table can tail normally or needs another recovery.
func (w *worker) recover(parent context.Context) error {
	w.log.Info("recovering from snapshot")
	ctx, cancel := context.WithTimeout(parent, w.snapshotTimeout)
	defer cancel()

	result, err := w.resumeRecovery(ctx)
	if err != nil {
		return err
	}
	switch result {
	case table.RecoveryCompleted:
		w.log.Info("table recovery completed")
		return nil
	case table.RecoveryPending, table.RecoveryNeedsDownload:
		return table.ErrRecoveryPending
	case table.RecoveryNoRecovery:
		// There is no journalled work. Negotiate exactly one artifact and return
		// to ordinary log replication once it has been applied.
	default:
		return fmt.Errorf("unknown recovery result %d", result)
	}

	followerIndex, _, err := w.tableState(ctx)
	if err != nil {
		if !errors.Is(err, serrors.ErrTableNotFound) {
			return fmt.Errorf("failed to get table state: %w", err)
		}
		followerIndex = 0
	}
	art, ok, err := w.negotiate(ctx, followerIndex)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	w.log.Infof("recovering from %s snapshot object=%s (base=%d tip=%d)",
		art.Type, art.ObjectKey, art.BaseIndex, art.TipIndex)
	if err := w.engine.BeginRecovery(w.table, w.sourceID, art); err != nil {
		return err
	}

	result, err = w.resumeRecovery(ctx)
	if err != nil {
		return err
	}
	if result != table.RecoveryCompleted {
		return table.ErrRecoveryPending
	}
	w.log.Info("table recovered")
	return nil
}

// negotiate asks the leader which artifact a follower at followerIndex should
// use and turns the answer into a pinned recovery artifact. A NONE answer is a
// request for an on-demand full snapshot; that is the sole fallback when the
// shared store has no applicable artifact.
func (w *worker) negotiate(ctx context.Context, followerIndex uint64) (table.RecoveryArtifact, bool, error) {
	queryResp, err := w.querySnapshot(ctx, followerIndex)
	if err != nil {
		return table.RecoveryArtifact{}, false, err
	}

	if queryResp.Type == armadapb.SnapshotQueryResponse_NONE || queryResp.ObjectKey == LiveSnapshotObjectKey(w.table) {
		// Nothing committed applies (or the gRPC resolver has already mapped that
		// outcome to its live endpoint): fetch an on-demand snapshot. It has no
		// stable object identity, so it must never be resumed as an artifact.
		return table.RecoveryArtifact{Type: table.ArtifactLive}, true, nil
	}

	// The leader can legitimately offer a delta this follower cannot apply. That
	// must not be an error: recover() would retry against an unchanged answer
	// forever, which is the livelock that produced hundreds of re-recoveries,
	// each allocating a fresh shard. Degrade instead.
	//
	// Asking the leader outright for a full would avoid paying for an on-demand
	// snapshot, but that needs a "full only" flag in SnapshotQueryRequest.
	if queryResp.Type == armadapb.SnapshotQueryResponse_INCREMENTAL {
		var why string
		switch {
		case w.forceRecovery.Load():
			// The source incarnation changed, so everything held locally is
			// from a different table; a delta cannot erase it.
			why = "the source table incarnation changed, so only a full replacement is valid"
		case followerIndex == 0:
			why = "the local table has no data to apply a delta onto"
		case followerIndex < queryResp.BaseIndex:
			why = fmt.Sprintf("the delta is based at %d but the follower is at %d", queryResp.BaseIndex, followerIndex)
		}
		if why != "" {
			w.log.Infof("leader offered an incremental snapshot that does not apply here (%s); using a live snapshot", why)
			return table.RecoveryArtifact{Type: table.ArtifactLive}, true, nil
		}
	}

	art := table.RecoveryArtifact{
		ObjectKey: queryResp.ObjectKey,
		BaseIndex: queryResp.BaseIndex,
		TipIndex:  queryResp.TipIndex,
		SHA256:    hex.EncodeToString(queryResp.Sha256),
		SizeBytes: queryResp.SizeBytes,
	}
	switch queryResp.Type {
	case armadapb.SnapshotQueryResponse_FULL:
		art.Type = table.ArtifactFull
	case armadapb.SnapshotQueryResponse_INCREMENTAL:
		art.Type = table.ArtifactIncremental
		// The applicability checks the resolver should already have enforced are
		// done above, where a mismatch degrades rather than erroring.
	default:
		return table.RecoveryArtifact{}, false, fmt.Errorf("unknown snapshot query response type %s", queryResp.Type)
	}
	return art, true, nil
}

// resumeRecovery pushes the journalled recovery as far as the table manager
// reports it can progress. RecoveryCompleted is the only result that permits a
// required source restore to be marked complete.
func (w *worker) resumeRecovery(ctx context.Context) (table.RecoveryResult, error) {
	// Validate the pinned source before the manager is allowed to drive the
	// journal. A follower table can be deleted and recreated for a new leader
	// incarnation while an old recovery record remains in metadata.
	rec, ok, err := w.engine.RecoveryStatus(w.table)
	if err != nil {
		return table.RecoveryPending, err
	}
	if ok && rec.SourceID != 0 && rec.SourceID != w.sourceID {
		w.log.Warnf("discarding recovery from source shard %d; table now follows source shard %d", rec.SourceID, w.sourceID)
		if err := w.engine.AbandonRecovery(w.table); err != nil {
			return table.RecoveryPending, fmt.Errorf("abandon stale recovery: %w", err)
		}
		return table.RecoveryNoRecovery, nil
	}

	result, err := w.engine.DriveRecoveryResult(ctx, w.table)
	if err != nil || result != table.RecoveryNeedsDownload {
		return result, err
	}

	rec, ok, err = w.engine.RecoveryStatus(w.table)
	if err != nil {
		return table.RecoveryPending, err
	}
	if !ok || rec.Coordinator != w.engine.Config().NodeID {
		return table.RecoveryPending, nil
	}
	path, err := w.fetchArtifact(ctx, rec)
	if err != nil {
		return table.RecoveryPending, err
	}
	if err := w.engine.CompleteDownload(ctx, w.table, path); err != nil {
		if errors.Is(err, table.ErrRecoveryPending) {
			return table.RecoveryPending, nil
		}
		return table.RecoveryPending, err
	}

	// CompleteDownload advances the phase machine synchronously. Ask the manager
	// for its explicit outcome rather than inferring completion from a journal
	// lookup, a force flag, or the local source index.
	result, err = w.engine.DriveRecoveryResult(ctx, w.table)
	if err != nil {
		return result, err
	}
	if result == table.RecoveryNoRecovery {
		// CompleteDownload has already driven and retired this exact journal. Its
		// successful return is the completion contract for this hand-off; no
		// source-index or worker-local completion heuristic is involved.
		return table.RecoveryCompleted, nil
	}
	return result, nil
}

// fetchArtifact downloads the pinned artefact into the staging directory,
// resuming from whatever was already verified, and returns the staged path.
func (w *worker) fetchArtifact(ctx context.Context, rec table.RecoveryRecord) (string, error) {
	dir := filepath.Join(w.engine.Config().Table.DataDir, snapshot.StagedDirName)
	st, err := snapshot.NewStaged(dir, stagingName(rec))
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()

	transferCtx := ctx
	var stopLeaseRefresh func()
	if w.snapshotAccess.Leases != nil {
		if err := w.snapshotAccess.Leases.AcquireLease(ctx, w.table); err != nil {
			return "", fmt.Errorf("acquire download lease: %w", err)
		}
		defer func() {
			if err := w.snapshotAccess.Leases.ReleaseLease(context.WithoutCancel(ctx), w.table); err != nil {
				w.log.Warnf("could not release download lease: %v", err)
			}
		}()
		transferCtx, stopLeaseRefresh = w.refreshDownloadLease(ctx)
		defer stopLeaseRefresh()
	}

	offset, err := st.Offset()
	if err != nil {
		return "", err
	}
	if rec.Artifact.Type != table.ArtifactLive && rec.Artifact.SizeBytes > 0 {
		switch {
		case offset > rec.Artifact.SizeBytes:
			// This stage cannot belong to the pinned artifact. Start again rather
			// than asking the source for a nonsensical range.
			if err := st.Reset(); err != nil {
				return "", err
			}
			offset = 0
		case offset == rec.Artifact.SizeBytes:
			// The process may have crashed after committing the last bytes but
			// before CompleteDownload. Verify locally; a Range at EOF is normally
			// rejected with HTTP 416 and would make this durable stage unusable.
			if err := w.verifyArtifact(ctx, st, rec); err != nil {
				st.Discard()
				if aerr := w.engine.AbandonRecovery(w.table); aerr != nil {
					w.log.Warnf("could not abandon corrupt recovery: %v", aerr)
				}
				return "", err
			}
			return st.Path(), nil
		}
	}

	var (
		body   io.ReadCloser
		ranged bool
	)
	if rec.Artifact.Type == table.ArtifactLive {
		// An on-demand snapshot is regenerated per request and has no stable
		// identity, so there is nothing to resume onto.
		if err := st.Reset(); err != nil {
			return "", err
		}
		offset = 0
		if w.snapshotAccess.Live == nil {
			return "", fmt.Errorf("live snapshot source is not configured")
		}
		body, err = w.snapshotAccess.Live.GetLive(transferCtx, w.table)
	} else {
		if w.snapshotAccess.Objects == nil {
			return "", fmt.Errorf("snapshot getter is not configured")
		}
		body, ranged, err = w.snapshotAccess.Objects.GetFrom(transferCtx, rec.Artifact.ObjectKey, offset)
	}
	if err != nil {
		return "", fmt.Errorf("failed to download snapshot: %w", err)
	}
	defer func() { _ = body.Close() }()

	if offset > 0 && !ranged {
		w.log.Infof("snapshot source ignored the resume offset, restarting download of %s", rec.Artifact.ObjectKey)
		if err := st.Reset(); err != nil {
			return "", err
		}
		offset = 0
	}
	if offset > 0 {
		w.log.Infof("resuming download of %s at byte %d", rec.Artifact.ObjectKey, offset)
	}

	n, err := copyWithCheckpoints(transferCtx, st, body, offset)
	if err != nil {
		// Whatever arrived is still durable; commit it so the next attempt
		// resumes instead of starting over.
		if cerr := st.Commit(offset + n); cerr != nil {
			w.log.Warnf("could not record partial download offset: %v", cerr)
		}
		return "", fmt.Errorf("failed to download snapshot: %w", err)
	}
	if err := st.Commit(offset + n); err != nil {
		return "", err
	}

	if err := w.verifyArtifact(ctx, st, rec); err != nil {
		st.Discard()
		if aerr := w.engine.AbandonRecovery(w.table); aerr != nil {
			w.log.Warnf("could not abandon corrupt recovery: %v", aerr)
		}
		return "", err
	}
	return st.Path(), nil
}

// stagingName binds staged bytes to the exact recovery identity without
// exposing a leader-controlled table name as a filesystem path component.
func stagingName(rec table.RecoveryRecord) string {
	identity := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%d\x00%d\x00%d",
		rec.Table, rec.SourceID, rec.Artifact.Type, rec.Artifact.ObjectKey,
		rec.Artifact.BaseIndex, rec.Artifact.TipIndex, rec.Artifact.SizeBytes)
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

// refreshDownloadLease owns the lease renewal lifecycle for one transfer. A
// failed refresh cancels the transfer context so bytes are never copied after
// the GC protection lease can no longer be extended.
func (w *worker) refreshDownloadLease(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	interval := downloadLeaseRefreshInterval
	if interval <= 0 {
		interval = store.LeaseTTL / 3
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-ticker.C:
				if err := w.snapshotAccess.Leases.RefreshLease(ctx, w.table); err != nil {
					w.log.Warnf("could not refresh download lease: %v", err)
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

// copyWithCheckpoints copies an artifact and commits periodically so an
// interruption loses at most one checkpoint interval. The final caller commit
// remains responsible for recording completion.
func copyWithCheckpoints(ctx context.Context, st *snapshot.Staged, r io.Reader, offset int64) (int64, error) {
	buf := make([]byte, 128*1024)
	var copied, sinceCheckpoint int64
	for {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			written, err := st.Write(buf[:n])
			copied += int64(written)
			sinceCheckpoint += int64(written)
			if err != nil {
				return copied, err
			}
			if written != n {
				return copied, io.ErrShortWrite
			}
			if sinceCheckpoint >= downloadCheckpointBytes {
				if err := st.Commit(offset + copied); err != nil {
					return copied, err
				}
				sinceCheckpoint = 0
			}
		}
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return copied, nil
			}
			return copied, readErr
		}
	}
}

// verifyArtifact checks the staged bytes against the size and checksum the
// leader advertised. Both are computed and transported today but were never
// checked, which left a truncated or corrupted artefact to fail much later as
// an unparseable command stream.
func (w *worker) verifyArtifact(_ context.Context, st *snapshot.Staged, rec table.RecoveryRecord) error {
	if rec.Artifact.SizeBytes > 0 {
		fi, err := st.Stat()
		if err != nil {
			return err
		}
		if fi.Size() != rec.Artifact.SizeBytes {
			return fmt.Errorf("snapshot %s is %d bytes, expected %d", rec.Artifact.ObjectKey, fi.Size(), rec.Artifact.SizeBytes)
		}
	}
	if rec.Artifact.SHA256 == "" {
		// Live snapshots have no checksum to compare against.
		return nil
	}
	sum, err := st.SHA256()
	if err != nil {
		return err
	}
	if sum != rec.Artifact.SHA256 {
		return fmt.Errorf("snapshot %s checksum is %s, expected %s", rec.Artifact.ObjectKey, sum, rec.Artifact.SHA256)
	}
	return nil
}

func (w *worker) querySnapshot(ctx context.Context, followerIndex uint64) (*armadapb.SnapshotQueryResponse, error) {
	if w.snapshotAccess.Query == nil {
		return nil, fmt.Errorf("snapshot query resolver is not configured")
	}
	queryResp, err := w.snapshotAccess.Query.Query(ctx, w.table, w.sourceID, followerIndex)
	if err != nil {
		return nil, err
	}
	return queryResp, nil
}

// Copyright JAMF Software, LLC

package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/raft/client"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/storage"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/kv"
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
	desiredProposalSize   = 256 * 1024
	queueLenCheckInterval = 50 * time.Millisecond
	metricsCheckInterval  = 5 * time.Second
)

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
	snapshotQuery     SnapshotQueryResolver
	snapshotGetter    SnapshotObjectGetter
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
					w.log.Warnf("error in required full recovery: %v", err)
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
							w.log.Warnf("error in recovering table: %v", err)
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

func (w *worker) recover(parent context.Context) error {
	w.log.Info("recovering from snapshot")
	ctx, cancel := context.WithTimeout(parent, w.snapshotTimeout)
	defer cancel()

	followerIndex, _, err := w.tableState(ctx)
	if err != nil {
		if !errors.Is(err, serrors.ErrTableNotFound) {
			return fmt.Errorf("failed to get table state: %w", err)
		}
		followerIndex = 0
	}

	queryResp, err := w.querySnapshot(ctx, followerIndex)
	if err != nil {
		return err
	}

	if queryResp.Type == armadapb.SnapshotQueryResponse_NONE {
		queryResp = &armadapb.SnapshotQueryResponse{
			Type:      armadapb.SnapshotQueryResponse_FULL,
			BaseIndex: 0,
			ObjectKey: LiveSnapshotObjectKey(w.table),
		}
	}
	if w.forceRecovery.Load() && queryResp.Type != armadapb.SnapshotQueryResponse_FULL {
		return fmt.Errorf("source table incarnation changed; a full snapshot is required")
	}

	w.log.Infof("downloading %s snapshot object=%s (base=%d tip=%d)",
		queryResp.Type, queryResp.ObjectKey, queryResp.BaseIndex, queryResp.TipIndex)

	sf, err := snapshot.NewTemp()
	if err != nil {
		return err
	}
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	if err = w.downloadSnapshot(ctx, queryResp.ObjectKey, sf.File); err != nil {
		return fmt.Errorf("failed to download snapshot: %w", err)
	}

	if err = sf.Sync(); err != nil {
		return err
	}
	if _, err = sf.Seek(0, io.SeekStart); err != nil {
		return err
	}
	w.log.Info("snapshot downloaded, loading table")
	if err := ctx.Err(); err != nil {
		return err
	}
	if queryResp.Type == armadapb.SnapshotQueryResponse_INCREMENTAL {
		if err = w.engine.ApplySnapshot(ctx, w.table, sf); err != nil {
			return err
		}
	} else {
		if err = w.engine.Restore(ctx, w.table, sf); err != nil {
			return err
		}
	}
	w.log.Info("table recovered")
	return nil
}

func (w *worker) querySnapshot(ctx context.Context, followerIndex uint64) (*armadapb.SnapshotQueryResponse, error) {
	if w.snapshotQuery == nil {
		return nil, fmt.Errorf("snapshot query resolver is not configured")
	}
	queryResp, err := w.snapshotQuery.Query(ctx, w.table, w.sourceID, followerIndex)
	if err != nil {
		return nil, err
	}
	return queryResp, nil
}

func (w *worker) downloadSnapshot(ctx context.Context, objectKey string, dst io.Writer) error {
	if w.snapshotGetter == nil {
		return fmt.Errorf("snapshot getter is not configured")
	}
	reader, err := w.snapshotGetter.Get(ctx, objectKey)
	if err != nil {
		return err
	}
	defer reader.Close()

	if _, err := io.Copy(dst, reader); err != nil {
		return err
	}
	return nil
}

// Copyright JAMF Software, LLC

package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/storage"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/table"
	"github.com/benbjohnson/clock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestTableQueueLenStore(t *testing.T) {
	r := require.New(t)
	cl := clock.NewMock()
	qls := tableQueueLenStore{table: "foo", store: &kv.MapStore{}, id: 1, clock: cl}
	v, err := qls.Get()
	r.NoError(err)
	r.Equal(uint64(0), v)
	r.NoError(qls.Set(1000))
	v, err = qls.Get()
	r.NoError(err)
	r.Equal(uint64(1000), v)
	v, err = qls.Max()
	r.NoError(err)
	r.Equal(uint64(1000), v)
	cl.Add(40 * time.Second)
	v, err = qls.Get()
	r.NoError(err)
	r.Equal(uint64(0), v)
	v, err = qls.Max()
	r.NoError(err)
	r.Equal(uint64(0), v)
	r.NoError(qls.Set(2000))
	v, err = qls.Max()
	r.NoError(err)
	r.Equal(uint64(2000), v)
}

func TestWorkerLeaseLossCancelsWork(t *testing.T) {
	w := &worker{workerCtx: context.Background()}
	w.acquireLease()

	leaseCtx, leased := w.leaseWork()
	require.True(t, leased)
	require.NoError(t, leaseCtx.Err())

	w.loseLease()
	require.ErrorIs(t, leaseCtx.Err(), context.Canceled)
	_, leased = w.leaseWork()
	require.False(t, leased)

	w.acquireLease()
	nextLeaseCtx, leased := w.leaseWork()
	require.True(t, leased)
	require.NotSame(t, leaseCtx, nextLeaseCtx)
	require.NoError(t, nextLeaseCtx.Err())
}

func TestWorkerLeaseExpiryCancelsWork(t *testing.T) {
	w := &worker{
		workerFactory: &workerFactory{leaseInterval: time.Millisecond},
		workerCtx:     context.Background(),
	}
	w.acquireLease()

	leaseCtx, leased := w.leaseWork()
	require.True(t, leased)
	select {
	case <-leaseCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("lease work was not canceled when the lease expired")
	}
	_, leased = w.leaseWork()
	require.False(t, leased)
}

type blockingLogClient struct{}

func (blockingLogClient) Replicate(ctx context.Context, _ *armadapb.ReplicateRequest, _ ...grpc.CallOption) (armadapb.Log_ReplicateClient, error) {
	return &blockingReplicateStream{ctx: ctx}, nil
}

type blockingReplicateStream struct {
	grpc.ClientStream
	ctx context.Context
}

func (s *blockingReplicateStream) Recv() (*armadapb.ReplicateResponse, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func TestWorkerLeaseLossCancelsLogReplay(t *testing.T) {
	w := &worker{
		table:    "table",
		sourceID: 1,
		workerFactory: &workerFactory{
			logTimeout: time.Minute,
			logClient:  blockingLogClient{},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := w.do(ctx, 0, nil)
		result <- err
	}()

	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("log replay did not stop when its lease context was canceled")
	}
}

func TestWorker_do(t *testing.T) {
	r := require.New(t)
	leaderEngine, followerEngine := prepareLeaderAndFollowerEngine(t)
	srv := startReplicationServer(leaderEngine)
	defer srv.Shutdown()

	t.Log("create tables")
	_, err := leaderEngine.CreateTable("test")
	r.NoError(err)
	_, err = followerEngine.CreateTable("test")
	r.NoError(err)

	var at table.ActiveTable
	t.Log("load some data")
	r.Eventually(func() bool {
		at, err = leaderEngine.GetTable("test")
		return err == nil
	}, 5*time.Second, 500*time.Millisecond, "table not created in time")

	keyCount := 1000
	r.NoError(fillData(keyCount, at))

	t.Log("create worker")
	conn, err := grpc.NewClient(srv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	r.NoError(err)
	logger, _ := observer.New(zap.DebugLevel)
	queue := storage.NewNotificationQueue()
	defer queue.Close()
	go queue.Run()
	f := &workerFactory{
		logTimeout:    time.Minute,
		engine:        followerEngine,
		queue:         queue,
		store:         &kv.MapStore{},
		logClient:     armadapb.NewLogClient(conn),
		log:           zap.New(logger).Sugar(),
		pollInterval:  500 * time.Millisecond,
		leaseInterval: 500 * time.Millisecond,
		metrics: struct {
			replicationIndex  *prometheus.GaugeVec
			replicationLeased *prometheus.GaugeVec
		}{
			replicationIndex: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Name: "regatta_replication_index",
					Help: "Regatta replication index",
				}, []string{"role", "table"},
			),
			replicationLeased: prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Name: "regatta_replication_leased",
					Help: "Regatta replication has the worker table leased",
				}, []string{"table"},
			),
		},
	}
	w := f.create("test", sourceTableState{ClusterID: at.ClusterID})
	idx, id, err := w.tableState(context.Background())
	r.NoError(err)
	_, err = w.do(context.Background(), idx, f.engine.GetNoOPSession(id))
	r.NoError(err)
	table, err := followerEngine.GetTable("test")
	r.NoError(err)

	func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		response, err := table.Range(ctx, &armadapb.RangeRequest{
			Table:        []byte("test"),
			Key:          []byte{0},
			RangeEnd:     []byte{0},
			Linearizable: true,
			CountOnly:    true,
		})
		r.NoError(err)
		r.Equal(int64(keyCount), response.Count)
	}()

	idxBefore, _, err := w.tableState(context.Background())
	r.NoError(err)

	t.Log("reset table")
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.NoError(table.Reset(ctx))
	}()
	idx, id, err = w.tableState(context.Background())
	r.NoError(err)
	r.Equal(uint64(0), idx)

	t.Log("do after reset")
	_, err = w.do(context.Background(), idx, f.engine.GetNoOPSession(id))
	r.NoError(err)

	idxAfter, _, err := w.tableState(context.Background())
	r.NoError(err)
	r.Equal(idxBefore, idxAfter)

	err = leaderEngine.DeleteTable("test")
	r.NoError(err)
	t.Log("recreate table with a new source identity")
	_, err = leaderEngine.CreateTable("test")
	r.NoError(err)

	idx, id, err = w.tableState(context.Background())
	r.NoError(err)
	result, err := w.do(context.Background(), idx, f.engine.GetNoOPSession(id))
	r.Equal(resultUnknown, result)
	r.ErrorContains(err, "incarnation changed")
}

type mockSnapshotQueryResolver struct {
	queryResp *armadapb.SnapshotQueryResponse
	queryErr  error
}

func (m *mockSnapshotQueryResolver) Query(_ context.Context, _ string, _, _ uint64) (*armadapb.SnapshotQueryResponse, error) {
	return m.queryResp, m.queryErr
}

type mockSnapshotGetter struct {
	err error
}

func (m mockSnapshotGetter) GetFrom(_ context.Context, _ string, _ int64) (io.ReadCloser, bool, error) {
	if m.err != nil {
		return nil, false, m.err
	}
	return io.NopCloser(&emptyReader{}), false, nil
}

func (m mockSnapshotGetter) GetLive(_ context.Context, _ string) (io.ReadCloser, error) {
	if m.err != nil {
		return nil, m.err
	}
	return io.NopCloser(&emptyReader{}), nil
}

type emptyReader struct{}

func (e *emptyReader) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

// TestWorker_recover_negotiation tests query/selection outcomes without a full Raft engine.
func TestWorker_recover_negotiation(t *testing.T) {
	t.Run("unimplemented propagates without stream fallback", func(t *testing.T) {
		mock := &mockSnapshotQueryResolver{
			queryErr: status.Error(codes.Unimplemented, "no shared store"),
		}
		_, fe := prepareLeaderAndFollowerEngine(t)
		require.NoError(t, fe.WaitUntilReady(t.Context()))
		w := &worker{
			table: "test",
			workerFactory: &workerFactory{
				snapshotTimeout: 5 * time.Second,
				engine:          fe,
				queue:           storage.NewNotificationQueue(),
				snapshotAccess:  SnapshotAccess{Query: mock, Objects: mockSnapshotGetter{}, Live: mockSnapshotGetter{}},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover(context.Background())
		require.ErrorContains(t, err, "no shared store")
	})

	t.Run("NONE response returns error with fallback to full snapshot", func(t *testing.T) {
		mock := &mockSnapshotQueryResolver{
			queryResp: &armadapb.SnapshotQueryResponse{Type: armadapb.SnapshotQueryResponse_NONE},
		}
		_, fe := prepareLeaderAndFollowerEngine(t)
		require.NoError(t, fe.WaitUntilReady(t.Context()))
		w := &worker{
			table: "test",
			workerFactory: &workerFactory{
				snapshotTimeout: 5 * time.Second,
				engine:          fe,
				queue:           storage.NewNotificationQueue(),
				snapshotAccess:  SnapshotAccess{Query: mock, Objects: mockSnapshotGetter{err: fmt.Errorf("no shared store")}, Live: mockSnapshotGetter{err: fmt.Errorf("no shared store")}},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover(context.Background())
		require.ErrorContains(t, err, "no shared store")
	})

	t.Run("required recovery falls back to a live full when offered an incremental", func(t *testing.T) {
		// The source incarnation changed, so a delta cannot be applied — but
		// erroring here would retry against an unchanged answer forever and,
		// once the journalled recovery finished, start a fresh full recovery on
		// every tick, allocating a new shard each time. The offer must degrade
		// to a live full snapshot instead.
		mock := &mockSnapshotQueryResolver{
			queryResp: &armadapb.SnapshotQueryResponse{
				Type:      armadapb.SnapshotQueryResponse_INCREMENTAL,
				BaseIndex: 10,
				TipIndex:  20,
			},
		}
		_, fe := prepareLeaderAndFollowerEngine(t)
		require.NoError(t, fe.WaitUntilReady(t.Context()))
		w := &worker{
			table: "test",
			workerFactory: &workerFactory{
				snapshotTimeout: 5 * time.Second,
				engine:          fe,
				queue:           storage.NewNotificationQueue(),
				snapshotAccess:  SnapshotAccess{Query: mock, Objects: mockSnapshotGetter{}, Live: mockSnapshotGetter{}},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		w.forceRecovery.Store(true)

		art, ok, err := w.negotiate(context.Background(), 15)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, table.ArtifactLive, art.Type, "an incremental must never be applied over a changed incarnation")
	})

	t.Run("a delta based ahead of the follower degrades instead of erroring", func(t *testing.T) {
		mock := &mockSnapshotQueryResolver{
			queryResp: &armadapb.SnapshotQueryResponse{
				Type:      armadapb.SnapshotQueryResponse_INCREMENTAL,
				BaseIndex: 500,
				TipIndex:  600,
			},
		}
		_, fe := prepareLeaderAndFollowerEngine(t)
		require.NoError(t, fe.WaitUntilReady(t.Context()))
		w := &worker{
			table: "test",
			workerFactory: &workerFactory{
				snapshotTimeout: 5 * time.Second,
				engine:          fe,
				queue:           storage.NewNotificationQueue(),
				snapshotAccess:  SnapshotAccess{Query: mock, Objects: mockSnapshotGetter{}, Live: mockSnapshotGetter{}},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}

		art, ok, err := w.negotiate(context.Background(), 100)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, table.ArtifactLive, art.Type)
	})

	t.Run("a live object key is never recorded as a stable full artifact", func(t *testing.T) {
		mock := &mockSnapshotQueryResolver{queryResp: &armadapb.SnapshotQueryResponse{
			Type:      armadapb.SnapshotQueryResponse_FULL,
			ObjectKey: LiveSnapshotObjectKey("test"),
		}}
		w := &worker{table: "test", workerFactory: &workerFactory{snapshotAccess: SnapshotAccess{Query: mock}}}

		art, ok, err := w.negotiate(context.Background(), 0)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, table.ArtifactLive, art.Type)
	})

	t.Run("transient query error propagates without legacy fallback", func(t *testing.T) {
		mock := &mockSnapshotQueryResolver{
			queryErr: status.Error(codes.Internal, "internal server error"),
		}
		_, fe := prepareLeaderAndFollowerEngine(t)
		require.NoError(t, fe.WaitUntilReady(t.Context()))
		w := &worker{
			table: "test",
			workerFactory: &workerFactory{
				snapshotTimeout: 5 * time.Second,
				engine:          fe,
				queue:           storage.NewNotificationQueue(),
				snapshotAccess:  SnapshotAccess{Query: mock, Objects: mockSnapshotGetter{}, Live: mockSnapshotGetter{}},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover(context.Background())
		require.ErrorContains(t, err, "internal server error")
	})
}

func fillData(keyCount int, at table.ActiveTable) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := range keyCount {
		if _, err := at.Put(ctx, &armadapb.PutRequest{
			Key:   fmt.Appendf(nil, "foo-%d", i),
			Value: []byte("bar"),
		}); err != nil {
			return err
		}
	}
	return nil
}

func TestWorker_recover(t *testing.T) {
	r := require.New(t)
	leaderEngine, followerEngine := prepareLeaderAndFollowerEngine(t)

	t.Log("create tables")
	r.Eventually(func() bool {
		_, err := leaderEngine.CreateTable("test")
		return err == nil
	}, 5*time.Second, 100*time.Millisecond, "table test not created in time")
	r.Eventually(func() bool {
		_, err := leaderEngine.CreateTable("test2")
		return err == nil
	}, 5*time.Second, 100*time.Millisecond, "table test2 not created in time")

	var at table.ActiveTable
	t.Log("load some data")
	r.Eventually(func() bool {
		var err error
		at, err = leaderEngine.GetTable("test")
		return err == nil
	}, 5*time.Second, 500*time.Millisecond, "table not created in time")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := at.Put(ctx, &armadapb.PutRequest{
		Key:   []byte("foo"),
		Value: []byte("bar"),
	})
	r.NoError(err)

	t.Log("prepare snapshot objects")
	snapshots := make(map[string]string)
	snapshotTips := make(map[string]uint64)
	for _, tableName := range []string{"test", "test2"} {
		tab, err := leaderEngine.GetTable(tableName)
		r.NoError(err)
		sf, err := snapshot.NewTemp()
		r.NoError(err)
		t.Cleanup(func() {
			_ = sf.Close()
			_ = os.Remove(sf.Path())
		})
		sctx, scancel := context.WithTimeout(context.Background(), time.Second)
		resp, err := tab.Snapshot(sctx, sf)
		scancel()
		r.NoError(err)
		final, err := (&armadapb.Command{
			Table:       []byte(tableName),
			Type:        armadapb.Command_DUMMY,
			LeaderIndex: &resp.Index,
		}).MarshalVT()
		r.NoError(err)
		_, err = sf.Write(final)
		r.NoError(err)
		r.NoError(sf.Sync())
		snapshots[fmt.Sprintf("snapshots/%s/full/%d.snap", tableName, resp.Index)] = sf.Path()
		snapshotTips[tableName] = resp.Index
	}

	t.Log("create worker")
	mockQuery := &mockSnapshotQueryResolver{}
	w := &worker{
		table: "test",
		workerFactory: &workerFactory{
			snapshotTimeout: time.Minute,
			engine:          followerEngine,
			queue:           storage.NewNotificationQueue(),
			snapshotAccess:  SnapshotAccess{Query: mockQuery, Objects: &snapshotFileGetter{byKey: snapshots}, Live: &snapshotFileGetter{byKey: snapshots}},
		},
		log: zaptest.NewLogger(t).Sugar(),
	}

	t.Log("recover table from leader")
	mockQuery.queryResp = &armadapb.SnapshotQueryResponse{
		Type:      armadapb.SnapshotQueryResponse_FULL,
		BaseIndex: 0,
		TipIndex:  snapshotTips["test"],
		ObjectKey: fmt.Sprintf("snapshots/test/full/%d.snap", snapshotTips["test"]),
	}
	r.NoError(w.recover(context.Background()))
	tab, err := followerEngine.GetTable("test")
	r.NoError(err)
	r.Equal("test", tab.Name)
	ir, err := tab.LeaderIndex(ctx, false)
	r.NoError(err)
	r.Greater(ir.Index, uint64(1))

	w = &worker{
		table: "test2",
		workerFactory: &workerFactory{
			snapshotTimeout: time.Minute,
			engine:          followerEngine,
			queue:           storage.NewNotificationQueue(),
			snapshotAccess:  SnapshotAccess{Query: mockQuery, Objects: &snapshotFileGetter{byKey: snapshots}, Live: &snapshotFileGetter{byKey: snapshots}},
		},
		log: zaptest.NewLogger(t).Sugar(),
	}
	mockQuery.queryResp = &armadapb.SnapshotQueryResponse{
		Type:      armadapb.SnapshotQueryResponse_FULL,
		BaseIndex: 0,
		TipIndex:  snapshotTips["test2"],
		ObjectKey: fmt.Sprintf("snapshots/test2/full/%d.snap", snapshotTips["test2"]),
	}
	t.Log("recover second table from leader")
	r.NoError(w.recover(context.Background()))
	tab, err = followerEngine.GetTable("test2")
	r.NoError(err)
	r.Equal("test2", tab.Name)
}

type snapshotFileGetter struct {
	byKey map[string]string
}

func (g *snapshotFileGetter) GetLive(_ context.Context, table string) (io.ReadCloser, error) {
	p, ok := g.byKey[LiveSnapshotObjectKey(table)]
	if !ok {
		return nil, fmt.Errorf("no live snapshot for table %q", table)
	}
	return os.Open(p)
}

func (g *snapshotFileGetter) GetFrom(_ context.Context, objectKey string, offset int64) (io.ReadCloser, bool, error) {
	p, ok := g.byKey[objectKey]
	if !ok {
		return nil, false, fmt.Errorf("unknown object key %q", objectKey)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, false, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, false, err
		}
		return f, true, nil
	}
	return f, false, nil
}

// rangeAwareGetter serves a fixed payload and records the offsets it was asked
// for, so tests can assert that a resumed download really resumed.
type rangeAwareGetter struct {
	payload []byte
	honour  bool
	offsets []int64
	delay   time.Duration
	chunk   int
}

type slowReader struct {
	reader io.Reader
	delay  time.Duration
	chunk  int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.chunk > 0 && len(p) > r.chunk {
		p = p[:r.chunk]
	}
	time.Sleep(r.delay)
	return r.reader.Read(p)
}

type failingRangeGetter struct {
	payload   []byte
	failAfter int
	failed    bool
	offsets   []int64
}

func (g *failingRangeGetter) GetFrom(_ context.Context, _ string, offset int64) (io.ReadCloser, bool, error) {
	g.offsets = append(g.offsets, offset)
	reader := bytes.NewReader(g.payload[offset:])
	if !g.failed {
		g.failed = true
		return io.NopCloser(&failAfterReader{Reader: reader, remaining: g.failAfter}), offset > 0, nil
	}
	return io.NopCloser(reader), offset > 0, nil
}

type failAfterReader struct {
	*bytes.Reader
	remaining int
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, fmt.Errorf("injected transfer failure")
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.Reader.Read(p)
	r.remaining -= n
	return n, err
}

type recordingLeaseKeeper struct {
	acquires  int
	refreshes int
	releases  int
}

func (k *recordingLeaseKeeper) AcquireLease(context.Context, string) error {
	k.acquires++
	return nil
}

func (k *recordingLeaseKeeper) RefreshLease(context.Context, string) error {
	k.refreshes++
	return nil
}

func (k *recordingLeaseKeeper) ReleaseLease(context.Context, string) error {
	k.releases++
	return nil
}

func (g *rangeAwareGetter) GetFrom(_ context.Context, _ string, offset int64) (io.ReadCloser, bool, error) {
	g.offsets = append(g.offsets, offset)
	payload := g.payload
	ranged := offset > 0 && g.honour
	if ranged {
		payload = payload[offset:]
	}
	var reader io.Reader = bytes.NewReader(payload)
	if g.delay > 0 {
		reader = &slowReader{reader: reader, delay: g.delay, chunk: g.chunk}
	}
	return io.NopCloser(reader), ranged, nil
}

func (g *rangeAwareGetter) GetLive(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(g.payload)), nil
}

func newDownloadWorker(t *testing.T, getter *rangeAwareGetter) *worker {
	t.Helper()
	fe := prepareEngine(t, "follower")
	require.NoError(t, fe.WaitUntilReady(t.Context()))
	return &worker{
		table: "test",
		workerFactory: &workerFactory{
			snapshotTimeout: 30 * time.Second,
			engine:          fe,
			queue:           storage.NewNotificationQueue(),
			snapshotAccess:  SnapshotAccess{Objects: getter, Live: getter},
		},
		log: zaptest.NewLogger(t).Sugar(),
	}
}

func stagedArtifact(t *testing.T, w *worker, rec table.RecoveryRecord) (dir, part string) {
	t.Helper()
	dir = filepath.Join(w.engine.Config().Table.DataDir, snapshot.StagedDirName)
	return dir, filepath.Join(dir, stagingName(rec)+".part")
}

func TestWorker_fetchArtifact(t *testing.T) {
	payload := bytes.Repeat([]byte("armada"), 64)
	sum := sha256.Sum256(payload)

	newRecord := func(w *worker) table.RecoveryRecord {
		return table.RecoveryRecord{
			Table:       w.table,
			Phase:       table.PhaseDownloading,
			Coordinator: w.engine.Config().NodeID,
			Artifact: table.RecoveryArtifact{
				Type:      table.ArtifactFull,
				ObjectKey: "snapshots/test/full/1.snap",
				SHA256:    hex.EncodeToString(sum[:]),
				SizeBytes: int64(len(payload)),
			},
		}
	}

	t.Run("a stage name never embeds the table path", func(t *testing.T) {
		rec := newRecord(newDownloadWorker(t, &rangeAwareGetter{}))
		rec.Table = "../../outside"
		name := stagingName(rec)
		require.NotContains(t, name, "/")
		require.NotContains(t, name, "\\")
		require.NotContains(t, name, rec.Table)
	})

	t.Run("a fresh download is staged and verified", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: payload, honour: true}
		w := newDownloadWorker(t, getter)

		path, err := w.fetchArtifact(context.Background(), newRecord(w))
		require.NoError(t, err)

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, payload, got)
		require.Equal(t, []int64{0}, getter.offsets)
	})

	t.Run("an interrupted download resumes from the recorded offset", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: payload, honour: true}
		w := newDownloadWorker(t, getter)

		rec := newRecord(w)
		// Simulate a crash that left the first 100 verified bytes on disk.
		dir, part := stagedArtifact(t, w, rec)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(part, payload[:100], 0o600))
		require.NoError(t, os.WriteFile(part+snapshot.OffsetSuffix, []byte("100"), 0o600))

		path, err := w.fetchArtifact(context.Background(), rec)
		require.NoError(t, err)
		require.Equal(t, []int64{100}, getter.offsets, "the download must resume, not restart")

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("a failed copy checkpoints bytes and resumes at that offset", func(t *testing.T) {
		getter := &failingRangeGetter{payload: payload, failAfter: 100}
		w := newDownloadWorker(t, &rangeAwareGetter{payload: payload, honour: true})
		w.snapshotAccess.Objects = getter

		rec := newRecord(w)
		_, err := w.fetchArtifact(context.Background(), rec)
		require.ErrorContains(t, err, "injected transfer failure")
		require.Equal(t, []int64{0}, getter.offsets)

		_, part := stagedArtifact(t, w, rec)
		checkpoint, err := os.ReadFile(part + snapshot.OffsetSuffix)
		require.NoError(t, err)
		require.Equal(t, "100", string(checkpoint))

		path, err := w.fetchArtifact(context.Background(), rec)
		require.NoError(t, err)
		require.Equal(t, []int64{0, 100}, getter.offsets)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("a complete checkpoint is verified without another range request", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: payload, honour: true}
		w := newDownloadWorker(t, getter)
		rec := newRecord(w)
		dir, part := stagedArtifact(t, w, rec)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(part, payload, 0o600))
		require.NoError(t, os.WriteFile(part+snapshot.OffsetSuffix, []byte(strconv.Itoa(len(payload))), 0o600))

		path, err := w.fetchArtifact(context.Background(), rec)
		require.NoError(t, err)
		require.Equal(t, part, path)
		require.Empty(t, getter.offsets, "a complete staged artifact must not request Range: bytes=size-")
	})

	t.Run("different artifacts never share staged bytes", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: payload, honour: true}
		w := newDownloadWorker(t, getter)
		old := newRecord(w)
		old.Artifact.ObjectKey = "snapshots/test/full/old.snap"
		dir, oldPart := stagedArtifact(t, w, old)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(oldPart, payload[:100], 0o600))
		require.NoError(t, os.WriteFile(oldPart+snapshot.OffsetSuffix, []byte("100"), 0o600))

		current := newRecord(w)
		current.Artifact.ObjectKey = "snapshots/test/full/current.snap"
		path, err := w.fetchArtifact(context.Background(), current)
		require.NoError(t, err)
		require.Equal(t, []int64{0}, getter.offsets, "a new artifact must not resume old bytes")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("a long download refreshes its lease", func(t *testing.T) {
		getter := &rangeAwareGetter{
			payload: bytes.Repeat([]byte("a"), 512*1024), honour: true,
			delay: 20 * time.Millisecond, chunk: 64 * 1024,
		}
		w := newDownloadWorker(t, getter)
		lease := &recordingLeaseKeeper{}
		w.snapshotAccess.Leases = lease
		oldInterval := downloadLeaseRefreshInterval
		downloadLeaseRefreshInterval = 5 * time.Millisecond
		t.Cleanup(func() { downloadLeaseRefreshInterval = oldInterval })

		rec := newRecord(w)
		sum := sha256.Sum256(getter.payload)
		rec.Artifact.SizeBytes = int64(len(getter.payload))
		rec.Artifact.SHA256 = hex.EncodeToString(sum[:])
		_, err := w.fetchArtifact(context.Background(), rec)
		require.NoError(t, err)
		require.Equal(t, 1, lease.acquires)
		require.Positive(t, lease.refreshes, "the lease must refresh before its TTL")
		require.Equal(t, 1, lease.releases)
	})

	t.Run("a redirected proxy download keeps renewing its HTTP lease", func(t *testing.T) {
		payload := bytes.Repeat([]byte("a"), 512*1024)
		var originRequests atomic.Int32
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			originRequests.Add(1)
			flusher, _ := w.(http.Flusher)
			for offset := 0; offset < len(payload); offset += 64 * 1024 {
				_, _ = w.Write(payload[offset : offset+64*1024])
				flusher.Flush()
				time.Sleep(10 * time.Millisecond)
			}
		}))
		defer origin.Close()

		var leasePuts, leaseDeletes atomic.Int32
		leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/snapshots/test/full/1.snap":
				http.Redirect(w, r, origin.URL, http.StatusTemporaryRedirect)
			case "/snapshot-leases/test":
				switch r.Method {
				case http.MethodPut:
					leasePuts.Add(1)
				case http.MethodDelete:
					leaseDeletes.Add(1)
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		}))
		defer leader.Close()

		getter := NewHTTPSnapshotObjectGetter(leader.Client(), leader.URL)
		w := newDownloadWorker(t, &rangeAwareGetter{payload: payload, honour: true})
		w.snapshotAccess = SnapshotAccess{
			Objects: getter,
			Live:    getter,
			Leases:  getter.LeaseKeeper("follower.example:5012"),
		}
		oldInterval := downloadLeaseRefreshInterval
		downloadLeaseRefreshInterval = 5 * time.Millisecond
		t.Cleanup(func() { downloadLeaseRefreshInterval = oldInterval })

		rec := newRecord(w)
		sum := sha256.Sum256(payload)
		rec.Artifact.SizeBytes = int64(len(payload))
		rec.Artifact.SHA256 = hex.EncodeToString(sum[:])
		_, err := w.fetchArtifact(context.Background(), rec)
		require.NoError(t, err)
		require.Equal(t, int32(1), originRequests.Load(), "the snapshot GET must follow the presigned redirect")
		require.Greater(t, leasePuts.Load(), int32(1), "the leader lease must refresh while redirected bytes stream")
		require.Equal(t, int32(1), leaseDeletes.Load())
	})

	t.Run("a mismatched Content-Range never appends to the staged prefix", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: payload, honour: true}
		w := newDownloadWorker(t, getter)
		rec := newRecord(w)
		dir, part := stagedArtifact(t, w, rec)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(part, payload[:100], 0o600))
		require.NoError(t, os.WriteFile(part+snapshot.OffsetSuffix, []byte("100"), 0o600))

		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Content-Range", "bytes 0-10/384")
			rw.WriteHeader(http.StatusPartialContent)
			_, _ = rw.Write(payload[:11])
		}))
		defer srv.Close()
		w.snapshotAccess.Objects = NewHTTPSnapshotObjectGetter(srv.Client(), srv.URL)

		_, err := w.fetchArtifact(context.Background(), rec)
		require.ErrorContains(t, err, "invalid Content-Range")
		got, err := os.ReadFile(part)
		require.NoError(t, err)
		require.Equal(t, payload[:100], got)
	})

	t.Run("a source that ignores the offset restarts from zero", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: payload, honour: false}
		w := newDownloadWorker(t, getter)
		rec := newRecord(w)

		dir, part := stagedArtifact(t, w, rec)
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(part, payload[:100], 0o600))
		require.NoError(t, os.WriteFile(part+snapshot.OffsetSuffix, []byte("100"), 0o600))

		path, err := w.fetchArtifact(context.Background(), rec)
		require.NoError(t, err)

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, payload, got, "the staged prefix must be discarded, not appended to")
	})

	t.Run("a checksum mismatch abandons the recovery", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: []byte("corrupted"), honour: true}
		w := newDownloadWorker(t, getter)

		rec := newRecord(w)
		require.NoError(t, w.engine.BeginRecovery(w.table, 1, rec.Artifact))

		_, err := w.fetchArtifact(context.Background(), rec)
		require.ErrorContains(t, err, "expected")

		_, ok, err := w.engine.RecoveryStatus(w.table)
		require.NoError(t, err)
		require.False(t, ok, "a corrupt artifact must clear the journal so the next pass renegotiates")

		_, part := stagedArtifact(t, w, rec)
		_, statErr := os.Stat(part)
		require.True(t, os.IsNotExist(statErr), "the corrupt staged file must be discarded")
	})

	t.Run("a size mismatch is rejected", func(t *testing.T) {
		getter := &rangeAwareGetter{payload: payload[:10], honour: true}
		w := newDownloadWorker(t, getter)

		rec := newRecord(w)
		require.NoError(t, w.engine.BeginRecovery(w.table, 1, rec.Artifact))
		_, err := w.fetchArtifact(context.Background(), rec)
		require.ErrorContains(t, err, "bytes, expected")
	})
}

func TestWorker_resumeRecoveryAbandonsStaleSource(t *testing.T) {
	getter := &rangeAwareGetter{payload: []byte("snapshot"), honour: true}
	w := newDownloadWorker(t, getter)
	createTableAndWait(t, w.engine, w.table)

	art := table.RecoveryArtifact{Type: table.ArtifactFull, ObjectKey: "snapshots/test/full/1.snap"}
	require.NoError(t, w.engine.BeginRecovery(w.table, 100, art))
	w.sourceID = 200

	result, err := w.resumeRecovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, table.RecoveryNoRecovery, result)
	_, ok, err := w.engine.RecoveryStatus(w.table)
	require.NoError(t, err)
	require.False(t, ok, "a journal from a previous source incarnation must not be driven")
}

// TestWorker_recoverIncremental drives the incremental recovery path end to
// end: the follower already holds a shard, the leader offers a delta, and the
// worker must replay it into the live shard rather than building a recovery
// shard.
func TestWorker_recoverIncremental(t *testing.T) {
	const tableName = "incr"
	r := require.New(t)
	leaderEngine, followerEngine := prepareLeaderAndFollowerEngine(t)

	leaderTable := createTableAndWait(t, leaderEngine, tableName)
	createTableAndWait(t, followerEngine, tableName)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for i := range 3 {
		_, err := leaderTable.Put(ctx, &armadapb.PutRequest{
			Table: []byte(tableName),
			Key:   []byte{byte('a' + i)},
			Value: []byte{byte('A' + i)},
		})
		r.NoError(err)
	}

	// Seed the follower with a full snapshot so it has a live shard to apply a
	// delta onto.
	fullPath, fullTip := exportSnapshot(t, leaderTable, tableName, 0)
	objects := map[string]string{
		fmt.Sprintf("snapshots/%s/full/%d.snap", tableName, fullTip): fullPath,
	}

	mockQuery := &mockSnapshotQueryResolver{}
	getter := &snapshotFileGetter{byKey: objects}
	w := &worker{
		table: tableName,
		workerFactory: &workerFactory{
			snapshotTimeout: time.Minute,
			logTimeout:      30 * time.Second,
			engine:          followerEngine,
			queue:           storage.NewNotificationQueue(),
			snapshotAccess:  SnapshotAccess{Query: mockQuery, Objects: getter, Live: getter},
		},
		log: zaptest.NewLogger(t).Sugar(),
	}
	mockQuery.queryResp = &armadapb.SnapshotQueryResponse{
		Type:      armadapb.SnapshotQueryResponse_FULL,
		TipIndex:  fullTip,
		ObjectKey: fmt.Sprintf("snapshots/%s/full/%d.snap", tableName, fullTip),
	}
	r.NoError(w.recover(context.Background()))

	seeded, err := followerEngine.GetTable(tableName)
	r.NoError(err)
	seededShard := seeded.ClusterID

	// New writes on the leader, captured as a delta from the full snapshot's tip.
	for i := range 3 {
		_, err := leaderTable.Put(ctx, &armadapb.PutRequest{
			Table: []byte(tableName),
			Key:   []byte{byte('x' + i)},
			Value: []byte{byte('X' + i)},
		})
		r.NoError(err)
	}
	incrPath, incrTip := exportSnapshot(t, leaderTable, tableName, fullTip)
	incrKey := fmt.Sprintf("snapshots/%s/incr/%d_%d.snap", tableName, fullTip, incrTip)
	objects[incrKey] = incrPath

	mockQuery.queryResp = &armadapb.SnapshotQueryResponse{
		Type:      armadapb.SnapshotQueryResponse_INCREMENTAL,
		BaseIndex: fullTip,
		TipIndex:  incrTip,
		ObjectKey: incrKey,
	}
	r.NoError(w.recover(context.Background()))

	after, err := followerEngine.GetTable(tableName)
	r.NoError(err)
	r.Equal(seededShard, after.ClusterID, "an incremental must replay into the live shard, not build a new one")

	resp, err := after.Range(ctx, &armadapb.RangeRequest{
		Key:          []byte{0},
		RangeEnd:     []byte{0},
		Linearizable: true,
	})
	r.NoError(err)
	r.Len(resp.Kvs, 6, "the delta should have added the leader's newer keys")

	_, ok, err := followerEngine.RecoveryStatus(tableName)
	r.NoError(err)
	r.False(ok)
}

// TestWorker_recoverRejectsUnsafeIncremental checks the follower-side re-check
// of an untrusted resolver's answer: a delta may only be applied to a live
// shard that is already at or past the delta's base index. When it is not, the
// offer degrades to a live full snapshot rather than erroring — an error would
// be retried against an unchanged answer forever.
func TestWorker_recoverRejectsUnsafeIncremental(t *testing.T) {
	r := require.New(t)
	fe := prepareEngine(t, "follower")
	r.NoError(fe.WaitUntilReady(t.Context()))

	getter := &snapshotFileGetter{byKey: map[string]string{}}
	w := &worker{
		table: "missing",
		workerFactory: &workerFactory{
			snapshotTimeout: 10 * time.Second,
			logTimeout:      10 * time.Second,
			engine:          fe,
			queue:           storage.NewNotificationQueue(),
			snapshotAccess: SnapshotAccess{
				Query: &mockSnapshotQueryResolver{queryResp: &armadapb.SnapshotQueryResponse{
					Type:      armadapb.SnapshotQueryResponse_INCREMENTAL,
					BaseIndex: 500,
					TipIndex:  600,
				}},
				Objects: getter,
				Live:    getter,
			},
		},
		log: zaptest.NewLogger(t).Sugar(),
	}

	// The table does not exist locally, so there is nothing to apply a delta to
	// and the follower must ask for a live full snapshot instead.
	art, ok, err := w.negotiate(context.Background(), 0)
	r.NoError(err)
	r.True(ok)
	r.Equal(table.ArtifactLive, art.Type)
}

// exportSnapshot writes a full (sinceIndex == 0) or incremental snapshot of tab
// in the shared-store wire format and returns its path and tip index.
// createTableAndWait creates a table and waits until a linearizable read
// against its shard succeeds.
//
// CreateTable only publishes the table metadata; the Raft shard behind it comes
// up asynchronously, so a linearizable read issued straight afterwards can be
// rejected with "request dropped as the shard is not ready". Probing with the
// very read the callers go on to make (LeaderIndex, the first thing
// worker.tableState does) is a stricter gate than waiting for a leader to be
// elected, which can be true while the replica still has entries to apply.
func createTableAndWait(t *testing.T, e *storage.Engine, name string) table.ActiveTable {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := e.CreateTable(name)
		return err == nil
	}, 10*time.Second, 100*time.Millisecond, "table %q was not created", name)

	var tab table.ActiveTable
	require.Eventually(t, func() bool {
		var err error
		tab, err = e.GetTable(name)
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = tab.LeaderIndex(ctx, true)
		return err == nil
	}, 30*time.Second, 50*time.Millisecond, "shard for table %q did not become readable", name)
	return tab
}

func exportSnapshot(t *testing.T, tab table.ActiveTable, tableName string, sinceIndex uint64) (string, uint64) {
	t.Helper()
	sf, err := snapshot.NewTemp()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var tip uint64
	if sinceIndex == 0 {
		resp, err := tab.Snapshot(ctx, sf)
		require.NoError(t, err)
		tip = resp.Index
	} else {
		resp, err := tab.IncrementalSnapshot(ctx, sf, sinceIndex)
		require.NoError(t, err)
		tip = resp.Index
	}

	final, err := (&armadapb.Command{
		Table:       []byte(tableName),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &tip,
	}).MarshalVT()
	require.NoError(t, err)
	_, err = sf.Write(final)
	require.NoError(t, err)
	require.NoError(t, sf.Sync())
	return sf.Path(), tip
}

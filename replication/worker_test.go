// Copyright JAMF Software, LLC

package replication

import (
	"context"
	"fmt"
	"io"
	"os"
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
	logger, obs := observer.New(zap.DebugLevel)
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
	w := f.create("test")
	idx, id, err := w.tableState()
	r.NoError(err)
	_, err = w.do(idx, f.engine.GetNoOPSession(id))
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

	idxBefore, _, err := w.tableState()
	r.NoError(err)

	t.Log("reset table")
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		r.NoError(table.Reset(ctx))
	}()
	idx, id, err = w.tableState()
	r.NoError(err)
	r.Equal(uint64(0), idx)

	t.Log("do after reset")
	_, err = w.do(idx, f.engine.GetNoOPSession(id))
	r.NoError(err)

	idxAfter, _, err := w.tableState()
	r.NoError(err)
	r.Equal(idxBefore, idxAfter)

	err = leaderEngine.DeleteTable("test")
	r.NoError(err)
	t.Log("create empty table test")
	_, err = leaderEngine.CreateTable("test")
	r.NoError(err)

	t.Log("load some data")
	r.Eventually(func() bool {
		at, err = leaderEngine.GetTable("test")
		return err == nil
	}, 5*time.Second, 500*time.Millisecond, "table not created in time")
	r.NoError(err)

	keyCount = 90
	r.NoError(fillData(keyCount, at))
	w.Start()
	idx, id, err = w.tableState()
	r.NoError(err)
	r.Eventually(func() bool {
		return obs.FilterMessage("the leader log is behind ... backing off").Len() > 0
	}, 5*time.Second, 100*time.Millisecond)

	keyCount = 1000
	r.NoError(fillData(keyCount, at))
	r.Eventually(func() bool {
		idx, id, err = w.tableState()
		r.NoError(err)
		return idx > uint64(200)
	}, 5*time.Second, 100*time.Millisecond)
}

type mockSnapshotQueryResolver struct {
	queryResp *armadapb.SnapshotQueryResponse
	queryErr  error
}

func (m *mockSnapshotQueryResolver) Query(_ context.Context, _ string, _ uint64) (*armadapb.SnapshotQueryResponse, error) {
	return m.queryResp, m.queryErr
}

type mockSnapshotGetter struct {
	err error
}

func (m mockSnapshotGetter) Get(_ context.Context, _ string) (io.ReadCloser, error) {
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
				snapshotQuery:   mock,
				snapshotGetter:  mockSnapshotGetter{},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover()
		require.ErrorContains(t, err, "no shared store")
	})

	t.Run("NONE response returns error without legacy fallback", func(t *testing.T) {
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
				snapshotQuery:   mock,
				snapshotGetter:  mockSnapshotGetter{},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover()
		require.ErrorContains(t, err, "leader has no available snapshots")
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
				snapshotQuery:   mock,
				snapshotGetter:  mockSnapshotGetter{},
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover()
		require.ErrorContains(t, err, "internal server error")
	})
}

func fillData(keyCount int, at table.ActiveTable) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := 0; i < keyCount; i++ {
		if _, err := at.Put(ctx, &armadapb.PutRequest{
			Key:   []byte(fmt.Sprintf("foo-%d", i)),
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
	getter := &snapshotFileGetter{byKey: snapshots}

	t.Log("create worker")
	mockQuery := &mockSnapshotQueryResolver{}
	w := &worker{
		table: "test",
		workerFactory: &workerFactory{
			snapshotTimeout: time.Minute,
			engine:          followerEngine,
			queue:           storage.NewNotificationQueue(),
			snapshotQuery:   mockQuery,
			snapshotGetter:  getter,
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
	r.NoError(w.recover())
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
			snapshotQuery:   mockQuery,
			snapshotGetter:  getter,
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
	r.NoError(w.recover())
	tab, err = followerEngine.GetTable("test2")
	r.NoError(err)
	r.Equal("test2", tab.Name)
}

type snapshotFileGetter struct {
	byKey map[string]string
}

func (g *snapshotFileGetter) Get(_ context.Context, objectKey string) (io.ReadCloser, error) {
	p, ok := g.byKey[objectKey]
	if !ok {
		return nil, fmt.Errorf("unknown object key %q", objectKey)
	}
	return os.Open(p)
}

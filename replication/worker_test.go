// Copyright JAMF Software, LLC

package replication

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/table"
	"github.com/benbjohnson/clock"
	"github.com/gogo/status"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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

// mockSnapshotClient is a minimal stub of armadapb.SnapshotClient for negotiation tests.
type mockSnapshotClient struct {
	armadapb.UnimplementedSnapshotServer
	queryResp *armadapb.SnapshotQueryResponse
	queryErr  error
}

func (m *mockSnapshotClient) Query(_ context.Context, _ *armadapb.SnapshotQueryRequest, _ ...grpc.CallOption) (*armadapb.SnapshotQueryResponse, error) {
	return m.queryResp, m.queryErr
}

func (m *mockSnapshotClient) Stream(_ context.Context, _ *armadapb.SnapshotRequest, _ ...grpc.CallOption) (armadapb.Snapshot_StreamClient, error) {
	return nil, fmt.Errorf("legacy stream called unexpectedly")
}

// mockLegacySnapshotClient records that the legacy stream was invoked.
type mockLegacySnapshotClient struct {
	armadapb.UnimplementedSnapshotServer
	queryErr     error
	streamCalled bool
}

func (m *mockLegacySnapshotClient) Query(_ context.Context, _ *armadapb.SnapshotQueryRequest, _ ...grpc.CallOption) (*armadapb.SnapshotQueryResponse, error) {
	return nil, m.queryErr
}

func (m *mockLegacySnapshotClient) Stream(_ context.Context, _ *armadapb.SnapshotRequest, _ ...grpc.CallOption) (armadapb.Snapshot_StreamClient, error) {
	m.streamCalled = true
	// Return a sentinel error so recover() surfaces it rather than panicking.
	return nil, fmt.Errorf("legacy stream: sentinel")
}

// TestWorker_recover_negotiation tests the three negotiation outcomes without a
// full Raft engine: Unimplemented → legacy fallback, NONE → error, real error → propagated.
func TestWorker_recover_negotiation(t *testing.T) {
	t.Run("unimplemented falls back to legacy stream", func(t *testing.T) {
		legacy := &mockLegacySnapshotClient{
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
				snapshotClient:  legacy,
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover()
		// Legacy stream returns a sentinel error; that error must propagate (not the query error).
		require.ErrorContains(t, err, "legacy stream: sentinel")
		require.True(t, legacy.streamCalled, "legacy stream should have been called")
	})

	t.Run("NONE response returns error without legacy fallback", func(t *testing.T) {
		mock := &mockSnapshotClient{
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
				snapshotClient:  mock,
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover()
		require.ErrorContains(t, err, "leader has no available snapshots")
	})

	t.Run("transient query error propagates without legacy fallback", func(t *testing.T) {
		mock := &mockSnapshotClient{
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
				snapshotClient:  mock,
			},
			log: zaptest.NewLogger(t).Sugar(),
		}
		err := w.recover()
		require.ErrorContains(t, err, "snapshot query failed")
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
	srv := startReplicationServer(leaderEngine)
	defer srv.Shutdown()

	t.Log("create tables")
	_, err := leaderEngine.CreateTable("test")
	r.NoError(err)
	_, err = leaderEngine.CreateTable("test2")
	r.NoError(err)

	var at table.ActiveTable
	t.Log("load some data")
	r.Eventually(func() bool {
		var err error
		at, err = leaderEngine.GetTable("test")
		return err == nil
	}, 5*time.Second, 500*time.Millisecond, "table not created in time")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = at.Put(ctx, &armadapb.PutRequest{
		Key:   []byte("foo"),
		Value: []byte("bar"),
	})
	r.NoError(err)

	t.Log("create worker")
	conn, err := grpc.NewClient(srv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	r.NoError(err)
	w := &worker{table: "test", workerFactory: &workerFactory{snapshotTimeout: time.Minute, engine: followerEngine, queue: storage.NewNotificationQueue(), snapshotClient: armadapb.NewSnapshotClient(conn)}, log: zaptest.NewLogger(t).Sugar()}

	t.Log("recover table from leader")
	r.NoError(w.recover())
	tab, err := followerEngine.GetTable("test")
	r.NoError(err)
	r.Equal("test", tab.Name)
	ir, err := tab.LeaderIndex(ctx, false)
	r.NoError(err)
	r.Greater(ir.Index, uint64(1))

	w = &worker{table: "test2", workerFactory: &workerFactory{snapshotTimeout: time.Minute, engine: followerEngine, queue: storage.NewNotificationQueue(), snapshotClient: armadapb.NewSnapshotClient(conn)}, log: zaptest.NewLogger(t).Sugar()}
	t.Log("recover second table from leader")
	r.NoError(w.recover())
	tab, err = followerEngine.GetTable("test2")
	r.NoError(err)
	r.Equal("test2", tab.Name)
}

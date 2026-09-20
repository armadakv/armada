// Copyright JAMF Software, LLC

package replication

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/armadaserver"
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/raftpb"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/storage"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/armada/vfs"
	pvfs "github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type testReplicationServer struct {
	armadapb.UnimplementedLogServer
	armadapb.UnimplementedMetadataServer
	armadapb.UnimplementedSnapshotServer
	metaResp     *armadapb.MetadataResponse
	metaErr      error
	repResp      []*armadapb.ReplicateResponse
	repErr       error
	snapshotFile string
	queryResp    *armadapb.SnapshotQueryResponse
	queryErr     error
}

func (t testReplicationServer) Get(context.Context, *armadapb.MetadataRequest) (*armadapb.MetadataResponse, error) {
	return t.metaResp, t.metaErr
}

func (t testReplicationServer) Replicate(_ *armadapb.ReplicateRequest, s armadapb.Log_ReplicateServer) error {
	for _, r := range t.repResp {
		s.Send(r)
	}
	return t.repErr
}

func (t testReplicationServer) Stream(_ *armadapb.SnapshotRequest, s armadapb.Snapshot_StreamServer) error {
	f, err := os.Open(t.snapshotFile)
	if err != nil {
		return err
	}
	_, err = io.Copy(&snapshot.Writer{Sender: s}, f)
	return err
}

func (t testReplicationServer) Query(context.Context, *armadapb.SnapshotQueryRequest) (*armadapb.SnapshotQueryResponse, error) {
	return t.queryResp, t.queryErr
}

type testSnapshotGetter struct {
	path string
}

func (t testSnapshotGetter) GetFrom(_ context.Context, _ string, _ int64) (io.ReadCloser, bool, error) {
	f, err := os.Open(t.path)
	return f, false, err
}

func (t testSnapshotGetter) GetLive(_ context.Context, _ string) (io.ReadCloser, error) {
	return os.Open(t.path)
}

func testServer(t *testing.T, regf func(server *grpc.Server)) *grpc.ClientConn {
	lis := bufconn.Listen(10 * 1024 * 1024)
	srv := grpc.NewServer()
	regf(srv)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(":0",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	return conn
}

func TestManager_reconcile(t *testing.T) {
	r := require.New(t)
	t.Log("start follower Raft")
	_, followerEngine := prepareLeaderAndFollowerEngine(t)

	conn := testServer(t, func(server *grpc.Server) {
		s := testReplicationServer{metaResp: &armadapb.MetadataResponse{Tables: []*armadapb.Table{
			{
				Name:      "test",
				ClusterId: 1,
			},
			{
				Name:      "test2",
				ClusterId: 2,
			},
		}}}
		armadapb.RegisterMetadataServer(server, s)
		armadapb.RegisterLogServer(server, s)
	})

	queue := storage.NewNotificationQueue()
	go queue.Run()
	m := NewManager(followerEngine, queue, conn, SnapshotAccess{}, Config{
		ReconcileInterval: 250 * time.Millisecond,
		Workers: WorkerConfig{
			PollInterval:        10 * time.Millisecond,
			LeaseInterval:       100 * time.Millisecond,
			LogRPCTimeout:       100 * time.Millisecond,
			SnapshotRPCTimeout:  100 * time.Millisecond,
			MaxRecoveryInFlight: 1,
		},
	})
	m.Start()
	r.Eventually(func() bool {
		return m.hasWorker("test")
	}, 10*time.Second, 250*time.Millisecond, "replication worker not found in registry")
	r.Eventually(func() bool {
		return m.hasWorker("test2")
	}, 10*time.Second, 250*time.Millisecond, "replication worker not found in registry")
	m.Close()
	r.Empty(m.workerSnapshot())
}

func TestManager_reconcileTables(t *testing.T) {
	r := require.New(t)
	leaderEngine, followerEngine := prepareLeaderAndFollowerEngine(t)
	srv := startReplicationServer(leaderEngine)
	defer srv.Shutdown()

	t.Log("create replicator")
	conn, err := grpc.NewClient(srv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	r.NoError(err)

	m := NewManager(followerEngine, nil, conn, SnapshotAccess{}, Config{})
	m.factory.store = &kv.MapStore{}

	t.Log("create table")
	_, err = leaderEngine.CreateTable("test")
	r.NoError(err)
	r.NoError(m.reconcileTables())
	r.Eventually(func() bool {
		_, err := followerEngine.GetTable("test")
		return err == nil
	}, 10*time.Second, 200*time.Millisecond, "table not created in time")

	t.Log("create another table")
	_, err = leaderEngine.CreateTable("test2")
	r.NoError(err)
	r.NoError(m.reconcileTables())
	r.Eventually(func() bool {
		_, err := followerEngine.GetTable("test2")
		return err == nil
	}, 10*time.Second, 200*time.Millisecond, "table not created in time")

	t.Log("skip network errors")
	r.NoError(conn.Close())

	tabs, err := followerEngine.GetTables()
	r.NoError(err)
	r.Len(tabs, 2)
}

func TestManager_RecreatesFollowerTableWhenLeaderIncarnationChanges(t *testing.T) {
	const tableName = "test"
	r := require.New(t)
	leaderEngine, followerEngine := prepareLeaderAndFollowerEngine(t)
	srv := startReplicationServer(leaderEngine)
	defer srv.Shutdown()

	conn, err := grpc.NewClient(srv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	r.NoError(err)
	defer conn.Close()
	m := NewManager(followerEngine, nil, conn, SnapshotAccess{}, Config{})
	m.factory.store = &kv.MapStore{}

	firstLeaderTable, err := leaderEngine.CreateTable(tableName)
	r.NoError(err)
	r.NoError(m.reconcileTables())
	var firstFollowerTable table.ActiveTable
	r.Eventually(func() bool {
		var err error
		firstFollowerTable, err = followerEngine.GetTable(tableName)
		if err != nil {
			return false
		}
		_, _, ok, _ := followerEngine.GetLeaderID(firstFollowerTable.ClusterID)
		return ok
	}, 5*time.Second, 50*time.Millisecond)

	_, err = followerEngine.Put(context.Background(), &armadapb.PutRequest{
		Table: []byte(tableName),
		Key:   []byte("old-only"),
		Value: []byte("must disappear"),
	})
	r.NoError(err)

	r.NoError(leaderEngine.DeleteTable(tableName))
	secondLeaderTable, err := leaderEngine.CreateTable(tableName)
	r.NoError(err)
	r.NotEqual(firstLeaderTable.ClusterID, secondLeaderTable.ClusterID)

	r.NoError(m.reconcileTables())
	r.Eventually(func() bool {
		table, err := followerEngine.GetTable(tableName)
		if err != nil || table.ClusterID == firstFollowerTable.ClusterID {
			return false
		}
		_, _, ok, _ := followerEngine.GetLeaderID(table.ClusterID)
		return ok
	}, 5*time.Second, 50*time.Millisecond)

	followerTable, err := followerEngine.GetTable(tableName)
	r.NoError(err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := followerTable.Range(ctx, &armadapb.RangeRequest{
		Key:          []byte{0},
		RangeEnd:     []byte{0},
		Linearizable: true,
	})
	r.NoError(err)
	r.Empty(response.Kvs, "data from the old leader table incarnation must not survive")

	state, found, err := m.loadSourceTableState(tableName)
	r.NoError(err)
	r.True(found)
	r.Equal(secondLeaderTable.ClusterID, state.ClusterID)
	r.True(state.NeedsFullRestore)
}

func TestManager_recover(t *testing.T) {
	r := require.New(t)
	t.Log("start follower Raft")
	_, followerEngine := prepareLeaderAndFollowerEngine(t)

	conn := testServer(t, func(server *grpc.Server) {
		s := testReplicationServer{
			metaResp: &armadapb.MetadataResponse{Tables: []*armadapb.Table{
				{
					Name:      "test",
					ClusterId: 1,
				},
			}},
			repResp: []*armadapb.ReplicateResponse{
				{
					LeaderIndex: 100,
					Response:    &armadapb.ReplicateResponse_ErrorResponse{ErrorResponse: &armadapb.ReplicateErrResponse{Error: armadapb.ReplicateError_USE_SNAPSHOT}},
				},
			},
			snapshotFile: "snapshot/testdata/snapshot.bin",
			queryResp: &armadapb.SnapshotQueryResponse{
				Type:      armadapb.SnapshotQueryResponse_FULL,
				BaseIndex: 0,
				TipIndex:  100,
				ObjectKey: "snapshots/test/full/100.snap",
			},
		}
		armadapb.RegisterMetadataServer(server, s)
		armadapb.RegisterLogServer(server, s)
		armadapb.RegisterSnapshotServer(server, s)
	})

	queue := storage.NewNotificationQueue()
	go queue.Run()
	m := NewManager(followerEngine, queue, conn, SnapshotAccess{
		Objects: testSnapshotGetter{path: "snapshot/testdata/snapshot.bin"},
		Live:    testSnapshotGetter{path: "snapshot/testdata/snapshot.bin"},
	}, Config{
		ReconcileInterval: 250 * time.Millisecond,
		Workers: WorkerConfig{
			PollInterval:        10 * time.Millisecond,
			LeaseInterval:       100 * time.Millisecond,
			LogRPCTimeout:       100 * time.Millisecond,
			SnapshotRPCTimeout:  10 * time.Second,
			MaxRecoveryInFlight: 1,
		},
	})
	r.NoError(m.Start())
	defer m.Close()
	r.Eventually(func() bool {
		return m.factory.engine.HasNodeInfo(10002, 1)
	}, 10*time.Second, 1*time.Second)
}

func getTestPort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// getTestUDPPort returns a probably-free UDP port. The engine's transport is
// QUIC, so probing a TCP port says nothing about whether the engine can bind:
// two engines can be handed the same free TCP port and then collide on UDP.
func getTestUDPPort() int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return getTestPort()
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func prepareLeaderAndFollowerEngine(t *testing.T) (leaderTM *storage.Engine, followerTM *storage.Engine) {
	t.Helper()
	t.Log("start leader Raft")
	leaderTM = prepareEngine(t, "leader")
	t.Log("start follower Raft")
	followerTM = prepareEngine(t, "follower")
	return
}

// prepareEngine starts one single-node engine. Tests that only need a follower
// should call this directly: every extra engine claims another ephemeral port,
// and getTestPort hands out a port it has already released.
func prepareEngine(t *testing.T, clusterName string) *storage.Engine {
	t.Helper()
	r := require.New(t)

	// Picking a port and binding it are separate steps, so another engine can
	// take it in between. Retry rather than failing the test on a collision.
	var (
		e   *storage.Engine
		err error
	)
	for attempt := range 5 {
		address := fmt.Sprintf("127.0.0.1:%d", getTestUDPPort())
		e, err = storage.New(storage.Config{
			FS:                vfs.NewMem(),
			Log:               zaptest.NewLogger(t).Sugar(),
			InitialMembers:    map[uint64]string{1: address},
			QUICUDPBufferSize: 4 * 1024 * 1024, // 4 MiB — fits within most CI kernel limits
			Gossip: storage.GossipConfig{
				ClusterName: clusterName,
			},
			NodeID:         1,
			RTTMillisecond: 5,
			RaftAddress:    address,
			Table:          storage.TableConfig{HeartbeatRTT: 1, ElectionRTT: 5, FS: pvfs.NewMem(), MaxInMemLogSize: 1024 * 1024, BlockCacheSize: 1024, TableCacheSize: 1024, DataDir: t.TempDir()},
			Meta:           storage.MetaConfig{HeartbeatRTT: 1, ElectionRTT: 5},
		})
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "address already in use") {
			break
		}
		t.Logf("port collision on attempt %d, retrying: %v", attempt+1, err)
	}
	r.NoError(err)
	r.NoError(e.Start())
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func startReplicationServer(engine *storage.Engine) *armadaserver.Server {
	testNodeAddress := fmt.Sprintf("127.0.0.1:%d", getTestPort())
	l, _ := net.Listen("tcp", testNodeAddress)
	server := armadaserver.NewServer(l, zap.NewNop().Sugar())
	armadapb.RegisterMetadataServer(server, &armadaserver.MetadataServer{Tables: engine})
	armadapb.RegisterSnapshotServer(server, &armadaserver.SnapshotServer{Tables: engine})
	armadapb.RegisterLogServer(
		server,
		armadaserver.NewLogServer(
			engine,
			&testLogReader{nh: engine.NodeHost},
			zap.NewNop(),
			1024,
		),
	)
	go func() {
		err := server.Serve()
		if err != nil {
			panic(err)
		}
	}()
	// Let the server start.
	time.Sleep(100 * time.Millisecond)
	return server
}

type testLogReader struct {
	nh *raft.NodeHost
}

func (t *testLogReader) QueryRaftLog(ctx context.Context, clusterID uint64, logRange raft.LogRange, maxSize uint64) ([]raftpb.Entry, error) {
	// Empty log range should return immediately.
	if logRange.FirstIndex == logRange.LastIndex {
		return nil, nil
	}
	rs, err := t.nh.QueryRaftLog(clusterID, logRange.FirstIndex, logRange.LastIndex, maxSize)
	if err != nil {
		return nil, err
	}
	defer rs.Release()
	result := <-rs.ResultC()
	ent, _ := result.RaftLogs()
	return ent, nil
}

// Copyright JAMF Software, LLC

package table

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
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/config"
	"github.com/armadakv/armada/replication/snapshot"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/vfs"
	pvfs "github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
)

var minimalTestConfig = func() Config {
	return Config{
		NodeID: 1,
		Table:  TableConfig{HeartbeatRTT: 1, ElectionRTT: 5, FS: pvfs.NewMem(), BlockCacheSize: 1024, TableCacheSize: 1024},
		Meta:   MetaConfig{HeartbeatRTT: 1, ElectionRTT: 5},
	}
}

func TestManager_CreateTable(t *testing.T) {
	const testTableName = "test"
	r := require.New(t)
	node, m := startRaftNode(t)
	defer node.Close()

	tm := NewManager(node, m, &kv.MapStore{}, minimalTestConfig())
	tm.Start()
	defer tm.Close()

	t.Log("create table")
	_, err := tm.CreateTable(testTableName)
	r.NoError(err)

	t.Log("get table")
	tab, err := tm.GetTable(testTableName)
	r.NoError(err)
	r.Equal(testTableName, tab.Name)
	r.Greater(tab.ClusterID, tableIDsRangeStart)

	t.Log("create existing table")
	_, err = tm.CreateTable(testTableName)
	r.ErrorIs(err, serrors.ErrTableExists)

	ts, err := tm.GetTables()
	r.NoError(err)
	r.Len(ts, 1)
}

func TestManager_DeleteTable(t *testing.T) {
	const testTableName = "test"
	r := require.New(t)
	node, m := startRaftNode(t)
	defer node.Close()

	tm := NewManager(node, m, &kv.MapStore{}, minimalTestConfig())
	tm.cleanupGracePeriod = 0
	tm.Start()
	defer tm.Close()

	t.Log("create table")
	_, err := tm.CreateTable(testTableName)
	r.NoError(err)

	t.Log("get table")
	tab, err := tm.GetTable(testTableName)
	r.NoError(err)
	r.NoError(tm.reconcile())

	time.Sleep(1 * time.Second)
	t.Log("delete table")
	r.NoError(tm.DeleteTable(testTableName))
	r.NoError(tm.reconcile())

	t.Log("check table")
	_, err = tm.GetTable(testTableName)
	r.ErrorIs(err, serrors.ErrTableNotFound)

	t.Log("delete non-existent table")
	_, err = tm.GetTable("foo")
	r.ErrorIs(err, serrors.ErrTableNotFound)
	r.NoError(tm.cleanup())

	// LogDB cleaned
	_, err = tm.nh.GetLogReader(tab.ClusterID)
	r.ErrorIs(err, raft.ErrLogDBNotCreatedOrClosed)

	// FS cleaned
	files, err := tm.cfg.Table.FS.List("")
	r.NoError(err)
	r.Len(files, 1, "FS should contain only a root directory (named after hostname)")
}

func TestManager_LeaseTable(t *testing.T) {
	const existingTable = "existingTable"
	type args struct {
		name     string
		duration time.Duration
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "Lease existing table",
			args: args{name: existingTable},
		},
		{
			name: "Lease unknown table",
			args: args{name: "unknown"},
		},
	}

	node, m := startRaftNode(t)
	defer node.Close()
	tm := NewManager(node, m, &kv.MapStore{}, minimalTestConfig())
	tm.Start()
	defer tm.Close()
	_, err := tm.CreateTable(existingTable)
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tm.LeaseTable(tt.args.name, tt.args.duration)
			require.NoError(t, err)
		})
	}
}

func TestManager_ReturnTable(t *testing.T) {
	const (
		existingTable = "existingTable"
		leasedTable   = "leasedTable"
	)
	type args struct {
		name string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "Return existing table",
			args: args{name: existingTable},
		},
		{
			name: "Return leased table",
			args: args{name: leasedTable},
			want: true,
		},
		{
			name: "Return unknown table",
			args: args{name: "unknown"},
		},
	}

	node, m := startRaftNode(t)
	defer node.Close()
	tm := NewManager(node, m, &kv.MapStore{}, minimalTestConfig())
	tm.Start()
	defer tm.Close()
	_, err := tm.CreateTable(existingTable)
	require.NoError(t, err)

	_, err = tm.CreateTable(leasedTable)
	require.NoError(t, err)
	require.NoError(t, tm.LeaseTable(leasedTable, 60*time.Second))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tm.ReturnTable(tt.args.name)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestManager_GetTable(t *testing.T) {
	const existingTable = "existingTable"
	type args struct {
		name string
	}
	tests := []struct {
		name    string
		args    args
		want    ActiveTable
		wantErr error
	}{
		{
			name: "Get existing table",
			args: args{name: existingTable},
			want: ActiveTable{
				Table: Table{Name: existingTable},
			},
		},
		{
			name:    "Get unknown table",
			args:    args{name: "unknown"},
			wantErr: serrors.ErrTableNotFound,
		},
	}

	node, m := startRaftNode(t)
	defer node.Close()
	tm := NewManager(node, m, &kv.MapStore{}, minimalTestConfig())
	tm.Start()
	defer tm.Close()
	_, err := tm.CreateTable(existingTable)
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			got, err := tm.GetTable(tt.args.name)
			if tt.wantErr != nil {
				r.ErrorIs(err, tt.wantErr)
				return
			}
			r.NoError(err)
			r.Equal(tt.want.Name, got.Name)
		})
	}
}

func TestManager_Restore(t *testing.T) {
	const existingTable = "existingTable"
	node, m := startRaftNode(t)
	defer node.Close()
	cfg := minimalTestConfig()
	cfg.Table.MaxInMemLogSize = 1024
	tm := NewManager(node, m, &kv.MapStore{}, cfg)
	tm.Start()
	defer tm.Close()
	_, err := tm.CreateTable(existingTable)
	require.NoError(t, err)

	tab, err := tm.GetTable(existingTable)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := range 4 {
		_, err := tab.Put(ctx, &armadapb.PutRequest{
			Table: []byte(existingTable),
			Key:   snapshotTestKey(i),
			Value: []byte(strings.Repeat(string(rune('a'+i)), 300)),
		})
		require.NoError(t, err)
	}

	sf, err := snapshot.NewTemp()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, sf.Close())
		require.NoError(t, os.Remove(sf.Path()))
	}()
	resp, err := tab.Snapshot(ctx, sf)
	require.NoError(t, err)
	final, err := (&armadapb.Command{
		Table:       []byte(existingTable),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &resp.Index,
	}).MarshalVT()
	require.NoError(t, err)
	_, err = sf.Write(final)
	require.NoError(t, err)
	require.NoError(t, sf.Sync())
	_, err = sf.Seek(0, io.SeekStart)
	require.NoError(t, err)

	require.NoError(t, tm.Restore(existingTable, sf))

	tab2, err := tm.GetTable(existingTable)
	require.NoError(t, err)
	require.Greater(t, tab2.ClusterID, tab.ClusterID, "restored table should have higher ID assigned")

	rangeResp, err := tab2.Range(ctx, &armadapb.RangeRequest{
		Key:          []byte{0},
		RangeEnd:     []byte{0},
		Linearizable: true,
	})
	require.NoError(t, err)
	require.Len(t, rangeResp.Kvs, 4)
	for i, kv := range rangeResp.Kvs {
		require.Equal(t, snapshotTestKey(i), kv.Key)
		require.Equal(t, []byte(strings.Repeat(string(rune('a'+i)), 300)), kv.Value)
	}
	leaderIndex, err := tab2.LeaderIndex(ctx, true)
	require.NoError(t, err)
	require.Equal(t, resp.Index, leaderIndex.Index)
}

func snapshotTestKey(i int) []byte {
	return fmt.Appendf(nil, "key-%d", i)
}

type snapshotCommandReader struct {
	records [][]byte
	next    int
}

func (r *snapshotCommandReader) Read(p []byte) (int, error) {
	if r.next == len(r.records) {
		return 0, io.EOF
	}
	record := r.records[r.next]
	r.next++
	if len(record) > len(p) {
		return 0, io.ErrShortBuffer
	}
	return copy(p, record), nil
}

func marshalSnapshotCommands(t *testing.T, commands ...*armadapb.Command) [][]byte {
	t.Helper()
	records := make([][]byte, len(commands))
	for i, command := range commands {
		var err error
		records[i], err = command.MarshalVT()
		require.NoError(t, err)
	}
	return records
}

func TestReadSnapshot(t *testing.T) {
	const tableName = "test"
	first := uint64(10)
	second := uint64(20)
	third := uint64(30)

	t.Run("retains every threshold-crossing record and commits the terminal marker", func(t *testing.T) {
		commands := marshalSnapshotCommands(t,
			&armadapb.Command{Table: []byte(tableName), Type: armadapb.Command_PUT, LeaderIndex: &second, Kv: &armadapb.KeyValue{Key: []byte("b"), Value: []byte("second")}},
			&armadapb.Command{Table: []byte(tableName), Type: armadapb.Command_PUT, LeaderIndex: &first, Kv: &armadapb.KeyValue{Key: []byte("a"), Value: []byte("first")}},
			&armadapb.Command{Table: []byte(tableName), Type: armadapb.Command_DELETE, LeaderIndex: &third, Kv: &armadapb.KeyValue{Key: []byte("c")}},
			&armadapb.Command{Table: []byte(tableName), Type: armadapb.Command_DUMMY, LeaderIndex: &third},
		)

		var proposed []struct {
			type_ armadapb.Command_CommandType
			keys  []string
			index uint64
		}
		err := readSnapshot(&snapshotCommandReader{records: commands}, tableName, 1, func(command *armadapb.Command) error {
			proposal := struct {
				type_ armadapb.Command_CommandType
				keys  []string
				index uint64
			}{type_: command.Type}
			if command.LeaderIndex != nil {
				proposal.index = *command.LeaderIndex
			}
			for _, child := range command.Sequence {
				proposal.keys = append(proposal.keys, string(child.Kv.Key))
			}
			proposed = append(proposed, proposal)
			return nil
		})
		require.NoError(t, err)
		require.Len(t, proposed, 4)
		require.Equal(t, []string{"b", "a", "c"}, []string{proposed[0].keys[0], proposed[1].keys[0], proposed[2].keys[0]})
		require.Equal(t, armadapb.Command_DUMMY, proposed[3].type_)
		require.Equal(t, third, proposed[3].index)
	})

	t.Run("accepts an empty snapshot without proposing user data", func(t *testing.T) {
		commands := marshalSnapshotCommands(t,
			&armadapb.Command{Table: []byte(tableName), Type: armadapb.Command_DUMMY, LeaderIndex: &third},
		)
		var proposed []*armadapb.Command
		err := readSnapshot(&snapshotCommandReader{records: commands}, tableName, 1024, func(command *armadapb.Command) error {
			proposed = append(proposed, command)
			return nil
		})
		require.NoError(t, err)
		require.Len(t, proposed, 1)
		require.Equal(t, armadapb.Command_DUMMY, proposed[0].Type)
		require.Empty(t, proposed[0].Sequence)
		require.Nil(t, proposed[0].Kv)
	})

	for _, tt := range []struct {
		name     string
		commands []*armadapb.Command
		wantErr  string
	}{
		{
			name:     "missing terminal marker",
			commands: []*armadapb.Command{{Table: []byte(tableName), Type: armadapb.Command_PUT, LeaderIndex: &first, Kv: &armadapb.KeyValue{Key: []byte("key")}}},
			wantErr:  "missing its terminal marker",
		},
		{
			name: "duplicate terminal marker",
			commands: []*armadapb.Command{
				{Table: []byte(tableName), Type: armadapb.Command_DUMMY, LeaderIndex: &first},
				{Table: []byte(tableName), Type: armadapb.Command_DUMMY, LeaderIndex: &first},
			},
			wantErr: "after its terminal marker",
		},
		{
			name:     "terminal marker without source index",
			commands: []*armadapb.Command{{Table: []byte(tableName), Type: armadapb.Command_DUMMY}},
			wantErr:  "terminal marker has no leader index",
		},
		{
			name:     "command for another table",
			commands: []*armadapb.Command{{Table: []byte("other"), Type: armadapb.Command_DUMMY, LeaderIndex: &first}},
			wantErr:  "expected \"test\"",
		},
		{
			name: "record after terminal marker",
			commands: []*armadapb.Command{
				{Table: []byte(tableName), Type: armadapb.Command_DUMMY, LeaderIndex: &first},
				{Table: []byte(tableName), Type: armadapb.Command_PUT, LeaderIndex: &first, Kv: &armadapb.KeyValue{Key: []byte("key")}},
			},
			wantErr: "after its terminal marker",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := readSnapshot(&snapshotCommandReader{records: marshalSnapshotCommands(t, tt.commands...)}, tableName, 1024, func(*armadapb.Command) error {
				return nil
			})
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestManagerWaitUntilReadyStartsExistingTables(t *testing.T) {
	const testTableName = "existing"
	node, members := startRaftNode(t)
	defer node.Close()

	tm := NewManager(node, members, &kv.MapStore{}, minimalTestConfig())
	tab, err := tm.createTable(testTableName)
	require.NoError(t, err)

	tm.Start()
	defer tm.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tm.WaitUntilReady(ctx))

	active, err := tm.GetTable(testTableName)
	require.NoError(t, err)
	require.Equal(t, tab.ClusterID, active.ClusterID)
	_, err = active.LocalIndex(ctx, false)
	require.NoError(t, err)
}

func TestManagerWaitUntilReadyHonorsContext(t *testing.T) {
	tm := &Manager{closed: make(chan struct{}), readyChan: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, tm.WaitUntilReady(ctx), context.Canceled)
	tm.Close()
}

func TestManager_reconcile(t *testing.T) {
	const testTableName = "test"
	r := require.New(t)
	node, m := startRaftNode(t)
	defer node.Close()

	const reconcileInterval = 1 * time.Second
	tm := NewManager(node, m, &kv.MapStore{}, minimalTestConfig())
	tm.reconcileInterval = reconcileInterval

	tm.Start()
	defer tm.Close()
	_, err := tm.createTable(testTableName)
	r.NoError(err)
	time.Sleep(reconcileInterval * 3)

	r.NoError(tm.DeleteTable(testTableName))
	time.Sleep(reconcileInterval * 3)
	_, err = tm.GetTable(testTableName)
	r.ErrorIs(err, serrors.ErrTableNotFound)
}

func Test_diffTables(t *testing.T) {
	type args struct {
		tables   map[string]Table
		raftInfo []raft.ShardInfo
	}
	tests := []struct {
		name        string
		args        args
		wantToStart map[uint64]Table
		wantToStop  []uint64
	}{
		{
			name: "Start a single table",
			args: args{
				tables: map[string]Table{
					"foo": {
						Name:      "foo",
						ClusterID: 10001,
					},
				},
				raftInfo: []raft.ShardInfo{},
			},
			wantToStart: map[uint64]Table{
				10001: {
					Name:      "foo",
					ClusterID: 10001,
				},
			},
			wantToStop: nil,
		},
		{
			name: "Start a single table with invalid ClusterID",
			args: args{
				tables: map[string]Table{
					"foo": {
						Name:      "foo",
						ClusterID: 10,
					},
				},
				raftInfo: []raft.ShardInfo{},
			},
			wantToStart: nil,
			wantToStop:  nil,
		},
		{
			name: "Stop a single table",
			args: args{
				tables: map[string]Table{
					"foo": {
						Name:      "foo",
						ClusterID: 10001,
					},
					"bar": {
						Name:      "foo",
						ClusterID: 10002,
					},
				},
				raftInfo: []raft.ShardInfo{
					{
						ShardID: 10001,
					},
					{
						ShardID: 10002,
					},
					{
						ShardID: 10003,
					},
				},
			},
			wantToStart: nil,
			wantToStop:  []uint64{10003},
		},
		{
			name: "Stop a single table with invalid ClusterID",
			args: args{
				tables: map[string]Table{
					"foo": {
						Name:      "foo",
						ClusterID: 10001,
					},
					"bar": {
						Name:      "foo",
						ClusterID: 10002,
					},
				},
				raftInfo: []raft.ShardInfo{
					{
						ShardID: 10001,
					},
					{
						ShardID: 10002,
					},
					{
						ShardID: 10,
					},
				},
			},
			wantToStart: nil,
			wantToStop:  nil,
		},
		{
			name: "Start a recovery table",
			args: args{
				tables: map[string]Table{
					"foo": {
						Name:      "foo",
						RecoverID: 10001,
					},
				},
				raftInfo: []raft.ShardInfo{},
			},
			wantToStart: map[uint64]Table{
				10001: {
					Name:      "foo",
					RecoverID: 10001,
				},
			},
			wantToStop: nil,
		},
		{
			name: "Recover existing table table",
			args: args{
				tables: map[string]Table{
					"foo": {
						Name:      "foo",
						ClusterID: 10001,
						RecoverID: 10002,
					},
				},
				raftInfo: []raft.ShardInfo{},
			},
			wantToStart: map[uint64]Table{
				10001: {
					Name:      "foo",
					ClusterID: 10001,
					RecoverID: 10002,
				},
				10002: {
					Name:      "foo",
					ClusterID: 10001,
					RecoverID: 10002,
				},
			},
			wantToStop: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			gotToStart, gotToStop := diffTables(tt.args.tables, tt.args.raftInfo)
			r.Equal(tt.wantToStart, gotToStart)
			r.Equal(tt.wantToStop, gotToStop)
		})
	}
}

func startRaftNode(t *testing.T) (*raft.NodeHost, map[uint64]string) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, l.Close())
	nhc := config.NodeHostConfig{
		WALDir:         "wal",
		NodeHostDir:    "dragonboat",
		RTTMillisecond: 1,
		RaftAddress:    l.Addr().String(),
		EnableMetrics:  true,
	}
	require.NoError(t, nhc.Prepare())
	nhc.Expert.FS = vfs.NewMem()
	nhc.Expert.Engine.ExecShards = 1
	nhc.Expert.LogDB.Shards = 1
	nh, err := raft.NewNodeHost(nhc)
	require.NoError(t, err)
	return nh, map[uint64]string{1: l.Addr().String()}
}

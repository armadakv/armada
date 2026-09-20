// Copyright JAMF Software, LLC

package armadaserver

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/raftpb"
	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/armada/storage"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/objfs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMetadataServer_Get(t *testing.T) {
	type fields struct {
		Tables []string
	}
	tests := []struct {
		name    string
		fields  fields
		want    *armadapb.MetadataResponse
		wantErr error
	}{
		{
			name: "Get metadata - no tables",
			want: &armadapb.MetadataResponse{Tables: nil},
		},
		{
			name: "Get metadata - single table",
			fields: fields{
				Tables: []string{"foo"},
			},
			want: &armadapb.MetadataResponse{Tables: []*armadapb.Table{
				{
					Name: "foo",
					Type: armadapb.Table_REPLICATED,
				},
			}},
		},
		{
			name: "Get metadata - multiple tables",
			fields: fields{
				Tables: []string{"foo", "bar"},
			},
			want: &armadapb.MetadataResponse{Tables: []*armadapb.Table{
				{
					Name: "bar",
					Type: armadapb.Table_REPLICATED,
				},
				{
					Name: "foo",
					Type: armadapb.Table_REPLICATED,
				},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			m := &MetadataServer{
				Tables: newInMemTestEngine(t, tt.fields.Tables...),
			}
			got, err := m.Get(context.TODO(), &armadapb.MetadataRequest{})
			if tt.wantErr != nil {
				r.ErrorIs(err, tt.wantErr)
				return
			}
			for _, table := range got.Tables {
				r.NotZero(table.ClusterId)
				table.ClusterId = 0
			}
			r.Equal(tt.want, got)
		})
	}
}

func TestEntryToCommand(t *testing.T) {
	zero := uint64(0)
	tests := []struct {
		name    string
		entry   raftpb.Entry
		wantCmd *armadapb.Command
		wantErr error
	}{
		{
			name:    "ConfigChange Entry Type",
			entry:   raftpb.Entry{Type: raftpb.ConfigChangeEntry, Index: 0},
			wantCmd: &armadapb.Command{Type: armadapb.Command_DUMMY, LeaderIndex: &zero},
			wantErr: nil,
		},
		{
			name: "Valid Entry",
			entry: raftpb.Entry{
				Type: raftpb.EncodedEntry,
				Cmd:  []byte{0, 10, 12, 114, 101, 103, 97, 116, 116, 97, 45, 116, 101, 115, 116, 26, 23, 10, 12, 49, 54, 50, 56, 48, 48, 50, 54, 52, 57, 95, 48, 34, 7, 118, 97, 108, 117, 101, 95, 48},
			},
			wantCmd: &armadapb.Command{
				Kv: &armadapb.KeyValue{
					Key:   []byte("1628002649_0"),
					Value: []byte("value_0"),
				},
				Table:       []byte("regatta-test"),
				LeaderIndex: &zero,
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)

			gotCmd, gotErr := entryToCommand(tt.entry)
			if tt.wantErr == nil {
				r.NoError(gotErr)
			} else {
				r.Error(gotErr)
			}

			r.Equal(tt.wantCmd.LeaderIndex, gotCmd.LeaderIndex)
			r.Equal(tt.wantCmd.Table, gotCmd.Table)
			r.Equal(tt.wantCmd.Type, gotCmd.Type)
			if tt.wantCmd.Kv != nil {
				r.Equal(tt.wantCmd.Kv.Value, gotCmd.Kv.Value)
				r.Equal(tt.wantCmd.Kv.Key, gotCmd.Kv.Key)
			}
		})
	}
}

type captureSnapshotStream struct {
	grpc.ServerStream
	chunks []*armadapb.SnapshotChunk
}

func (c *captureSnapshotStream) Context() context.Context {
	return context.TODO()
}

func (c *captureSnapshotStream) Send(chunk *armadapb.SnapshotChunk) error {
	c.chunks = append(c.chunks, chunk)
	return nil
}

func TestSnapshotServer_Stream(t *testing.T) {
	type fields struct {
		Tables []string
	}
	type args struct {
		req *armadapb.SnapshotRequest
	}
	tests := []struct {
		name            string
		fields          fields
		args            args
		wantErr         require.ErrorAssertionFunc
		wantChunksCount int
	}{
		{
			name:    "table not exist",
			args:    args{req: &armadapb.SnapshotRequest{Table: table1Name}},
			wantErr: require.Error,
		},
		{
			name:            "snapshot of empty table",
			fields:          fields{Tables: []string{string(table1Name)}},
			args:            args{req: &armadapb.SnapshotRequest{Table: table1Name}},
			wantErr:         require.NoError,
			wantChunksCount: 1, // empty table: commandSnapshot writes no KV commands, only the trailing DUMMY is present
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &SnapshotServer{
				Tables: newInMemTestEngine(t, tt.fields.Tables...),
			}
			if len(tt.fields.Tables) > 0 {
				table, err := s.Tables.GetTable(string(tt.args.req.Table))
				require.NoError(t, err)
				tt.args.req.ClusterId = table.ClusterID
			}
			capture := &captureSnapshotStream{}
			tt.wantErr(t, s.Stream(tt.args.req, capture), fmt.Sprintf("Stream(%v)", tt.args.req))
			require.Len(t, capture.chunks, tt.wantChunksCount)
		})
	}

	t.Run("snapshot of table with data produces KV commands", func(t *testing.T) {
		engine := newInMemTestEngine(t, string(table1Name))
		// Write some data so commandSnapshot has user keys to emit.
		_, err := engine.Put(context.Background(), &armadapb.PutRequest{
			Table: table1Name,
			Key:   []byte("key1"),
			Value: []byte("value1"),
		})
		require.NoError(t, err)
		_, err = engine.Put(context.Background(), &armadapb.PutRequest{
			Table: table1Name,
			Key:   []byte("key2"),
			Value: []byte("value2"),
		})
		require.NoError(t, err)

		s := &SnapshotServer{Tables: engine}
		table, err := engine.GetTable(string(table1Name))
		require.NoError(t, err)
		capture := &captureSnapshotStream{}
		require.NoError(t, s.Stream(&armadapb.SnapshotRequest{Table: table1Name, ClusterId: table.ClusterID}, capture))
		// At least 1 chunk must be present regardless of size.
		require.NotEmpty(t, capture.chunks)

		// Decode all chunks into a single byte slice and verify it contains
		// at least one PUT command for each key we wrote.
		var raw []byte
		for _, chunk := range capture.chunks {
			raw = append(raw, chunk.Data...)
		}
		require.NotEmpty(t, raw, "snapshot data must not be empty for a table with data")
	})
}

type tableServiceStub struct {
	activeTable table.ActiveTable
	err         error
}

func (s tableServiceStub) GetTables() ([]table.Table, error) {
	return nil, s.err
}

func (s tableServiceStub) GetTable(string) (table.ActiveTable, error) {
	return s.activeTable, s.err
}

func (s tableServiceStub) Restore(context.Context, string, io.Reader) error {
	return s.err
}

func (s tableServiceStub) RestoreLegacy(context.Context, string, io.Reader) error {
	return s.err
}

func (s tableServiceStub) CreateTable(string) (table.Table, error) {
	return table.Table{}, s.err
}

func (s tableServiceStub) DeleteTable(string) error {
	return s.err
}

func TestReplicationHandlersRequireClusterID(t *testing.T) {
	tables := tableServiceStub{
		activeTable: table.ActiveTable{Table: table.Table{ClusterID: 1}},
	}

	snapshotServer := &SnapshotServer{Tables: tables}
	err := snapshotServer.Stream(&armadapb.SnapshotRequest{Table: []byte("orders")}, &captureSnapshotStream{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = snapshotServer.Query(context.Background(), &armadapb.SnapshotQueryRequest{Table: "orders"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	logServer := &LogServer{Tables: tables}
	err = logServer.Replicate(&armadapb.ReplicateRequest{Table: []byte("orders"), LeaderIndex: 1}, &captureLogStream{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSnapshotServer_QueryGetTableErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code codes.Code
	}{
		{
			name: "table not found",
			err:  serrors.ErrTableNotFound,
			code: codes.NotFound,
		},
		{
			name: "retryable error",
			err:  raft.ErrTimeout,
			code: codes.Unavailable,
		},
		{
			name: "other error",
			err:  stderrors.New("unknown"),
			code: codes.FailedPrecondition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &SnapshotServer{Tables: tableServiceStub{err: tt.err}}
			_, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
				Table:     "orders",
				ClusterId: 1,
			})
			require.Equal(t, tt.code, status.Code(err))
		})
	}
}

func TestSnapshotServer_Query(t *testing.T) {
	t.Run("incremental is selected for a follower inside the delta range", func(t *testing.T) {
		bucket := bucketWithMetas(t, store.Meta{
			Table:     "orders",
			Type:      store.SnapshotTypeIncremental,
			BaseIndex: 100,
			TipIndex:  150,
			SizeBytes: 1024,
			SHA256:    "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		})

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_INCREMENTAL, resp.Type)
		require.Equal(t, uint64(100), resp.BaseIndex)
		require.Equal(t, uint64(150), resp.TipIndex)
		require.Equal(t, store.IncrSnapKey("orders", 100, 150), resp.ObjectKey)
	})

	t.Run("a follower at or above the GC horizon is told it needs nothing", func(t *testing.T) {
		// The chain-then-full regression. Once a chain has lifted the follower
		// above the horizon it can tail the log, and offering it the best
		// remaining artefact hands it a full — no delta is based that high —
		// throwing away everything the chain achieved.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 400},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		advanceGCHorizon(t, engine, table.ClusterID, 150)

		for _, followerIndex := range []uint64{150, 151, 399} {
			resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
				Table:         "orders",
				FollowerIndex: followerIndex,
				ClusterId:     table.ClusterID,
			})
			require.NoError(t, err)
			require.Equalf(t, armadapb.SnapshotQueryResponse_NONE, resp.Type,
				"follower at %d is at or above the horizon and can tail", followerIndex)
		}
	})

	t.Run("an uncompacted leader still offers a snapshot", func(t *testing.T) {
		// A zero horizon means the leader has never compacted, so it says
		// nothing about what the follower needs. Answering NONE there would
		// make a fresh follower replay the whole log through raft, which is
		// precisely what snapshot recovery exists to avoid.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 400},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 0,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, uint64(400), resp.TipIndex)
	})

	t.Run("one artefact always lands the follower at or above the horizon", func(t *testing.T) {
		// The shared selector drops artefacts whose tip is below the horizon, so
		// any selection lands the follower where normal log replication can
		// resume. A chain can only be needed if the horizon advances during
		// replay.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 400},
			store.Meta{Table: "orders", Type: store.SnapshotTypeIncremental, BaseIndex: 110, TipIndex: 130, GCHorizon: 100},
			store.Meta{Table: "orders", Type: store.SnapshotTypeIncremental, BaseIndex: 130, TipIndex: 200, GCHorizon: 120},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		// Horizon at 160 puts the 110-130 delta below it, so it is dropped as
		// stale and the 130-200 delta does not reach back to this follower.
		advanceGCHorizon(t, engine, table.ClusterID, 160)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, uint64(400), resp.TipIndex)
		require.GreaterOrEqual(t, resp.TipIndex, uint64(160),
			"whatever is offered must leave the follower at or above the horizon")
	})

	t.Run("a delta whose tip clears the horizon is offered, not a full", func(t *testing.T) {
		// The counterpart: the delta reaches this follower and its tip clears
		// the horizon, so one link finishes the job and the full is not needed.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 400},
			store.Meta{Table: "orders", Type: store.SnapshotTypeIncremental, BaseIndex: 110, TipIndex: 200, GCHorizon: 100},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		advanceGCHorizon(t, engine, table.ClusterID, 160)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_INCREMENTAL, resp.Type)
		require.Equal(t, uint64(110), resp.BaseIndex)
		require.Equal(t, uint64(200), resp.TipIndex)
	})

	t.Run("incremental only returns NONE when no full snapshot can replace it", func(t *testing.T) {
		bucket := bucketWithMetas(t, store.Meta{
			Table:     "orders",
			Type:      store.SnapshotTypeIncremental,
			BaseIndex: 100,
			TipIndex:  150,
		})

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		// Advancing the GC horizon past the follower makes the delta unsafe:
		// the MVCC versions it would need have been compacted away.
		advanceGCHorizon(t, engine, table.ClusterID, 130)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_NONE, resp.Type)
	})

	t.Run("a follower below the GC horizon gets a full snapshot", func(t *testing.T) {
		// The incremental is the better fit on index alone, so this only passes
		// if the GC-horizon guard rejects it in favour of the full.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 160},
			store.Meta{Table: "orders", Type: store.SnapshotTypeIncremental, BaseIndex: 110, TipIndex: 150},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		advanceGCHorizon(t, engine, table.ClusterID, 130)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, uint64(160), resp.TipIndex)
	})

	t.Run("a delta that recorded its export horizon is served below the live horizon", func(t *testing.T) {
		// The regression this guards: a follower only ever negotiates a snapshot
		// because Replicate answered USE_SNAPSHOT, which happens exactly when it
		// is at or below the GC horizon. Rejecting deltas on that basis made the
		// incremental path unreachable in every configuration. An artefact whose
		// recorded export-time horizon is below its own base index is complete
		// for good, however far the live horizon has since moved.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 160},
			store.Meta{
				Table:     "orders",
				Type:      store.SnapshotTypeIncremental,
				BaseIndex: 110,
				TipIndex:  150,
				GCHorizon: 100,
			},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		// Live horizon well past the follower, which is the normal state for
		// anything that needs a snapshot at all.
		advanceGCHorizon(t, engine, table.ClusterID, 130)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_INCREMENTAL, resp.Type)
		require.Equal(t, uint64(110), resp.BaseIndex)
		require.Equal(t, uint64(150), resp.TipIndex)
	})

	t.Run("a delta exported at or below its own base is rejected", func(t *testing.T) {
		// Provenance recorded and bad: the exporter would have skipped compacted
		// versions, so the delta is silently short and must not be served.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 160},
			store.Meta{
				Table:     "orders",
				Type:      store.SnapshotTypeIncremental,
				BaseIndex: 110,
				TipIndex:  150,
				GCHorizon: 115,
			},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, uint64(160), resp.TipIndex)
	})

	t.Run("an artefact below the GC horizon is not offered", func(t *testing.T) {
		// Observed in the e2e harness as two recoveries back to back:
		//   serving shard is now 10009 at source index 2762 (artefact full tip 2762)
		//   serving shard is now 10010 at source index 3465 (artefact live tip 0)
		// The first landed exactly on its artefact's tip, but that tip was
		// already below the leader's GC horizon, so Replicate answered
		// USE_SNAPSHOT for tip+1 and the whole recovery ran again. Offering
		// nothing is better: the follower goes straight to a live snapshot,
		// which is current by construction.
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 2762},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		advanceGCHorizon(t, engine, table.ClusterID, 2800)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 2062,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_NONE, resp.Type,
			"an artefact the follower could not resume from must not be offered")
	})

	t.Run("an artefact at or above the GC horizon is still offered", func(t *testing.T) {
		bucket := bucketWithMetas(t,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 2900},
		)

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{Tables: engine, SnapshotStore: bucket}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		advanceGCHorizon(t, engine, table.ClusterID, 2800)

		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 2062,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, uint64(2900), resp.TipIndex)
	})

	t.Run("full snapshot is selected and returned", func(t *testing.T) {
		bucket, err := objfs.NewLocal(t.TempDir())
		require.NoError(t, err)
		meta := store.Meta{
			Table:     "orders",
			Type:      store.SnapshotTypeFull,
			BaseIndex: 0,
			TipIndex:  150,
			SizeBytes: 2048,
			SHA256:    "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		}
		raw, err := json.Marshal(meta)
		require.NoError(t, err)
		require.NoError(t, bucket.Upload(context.Background(), store.FullMetaKey("orders", 150), bytes.NewReader(raw)))

		engine := newInMemTestEngine(t, "orders")
		s := &SnapshotServer{
			Tables:        engine,
			SnapshotStore: bucket,
		}
		table, err := engine.GetTable("orders")
		require.NoError(t, err)
		resp, err := s.Query(context.Background(), &armadapb.SnapshotQueryRequest{
			Table:         "orders",
			FollowerIndex: 120,
			ClusterId:     table.ClusterID,
		})
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, uint64(0), resp.BaseIndex)
		require.Equal(t, uint64(150), resp.TipIndex)
		require.Equal(t, store.FullSnapKey("orders", 150), resp.ObjectKey)
		require.Len(t, resp.Sha256, 32)
	})
}

type captureLogStream struct {
	grpc.ServerStream
	ctx    context.Context
	stream []*armadapb.ReplicateResponse
}

func (c *captureLogStream) Context() context.Context {
	return c.ctx
}

func (c *captureLogStream) Send(r *armadapb.ReplicateResponse) error {
	c.stream = append(c.stream, r)
	return nil
}

func TestLogServer_Replicate(t *testing.T) {
	type fields struct {
		Tables         []string
		maxMessageSize uint64
	}
	type args struct {
		req *armadapb.ReplicateRequest
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr require.ErrorAssertionFunc
	}{
		{
			name:    "table not exist",
			args:    args{req: &armadapb.ReplicateRequest{Table: table1Name}},
			wantErr: require.Error,
		},
		{
			name:    "zero index",
			args:    args{req: &armadapb.ReplicateRequest{Table: table1Name}},
			wantErr: require.Error,
		},
		{
			name:    "stream",
			fields:  fields{Tables: []string{string(table1Name)}},
			args:    args{req: &armadapb.ReplicateRequest{Table: table1Name, LeaderIndex: 1}},
			wantErr: require.NoError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := newInMemTestEngine(t, tt.fields.Tables...)
			l := &LogServer{
				Tables:         te,
				LogReader:      te.LogReader,
				Log:            zaptest.NewLogger(t).Sugar(),
				maxMessageSize: tt.fields.maxMessageSize,
			}
			ctx, cancel := context.WithTimeout(context.TODO(), time.Second)
			defer cancel()
			if len(tt.fields.Tables) > 0 {
				table, err := te.GetTable(string(tt.args.req.Table))
				require.NoError(t, err)
				tt.args.req.ClusterId = table.ClusterID
			}
			stream := &captureLogStream{ctx: ctx}
			tt.wantErr(t, l.Replicate(tt.args.req, stream), fmt.Sprintf("Replicate(%v)", tt.args.req))
		})
	}
}

// bucketWithMetas builds a local bucket pre-populated with committed metadata.
func bucketWithMetas(t *testing.T, metas ...store.Meta) objfs.Bucket {
	t.Helper()
	bucket, err := objfs.NewLocal(t.TempDir())
	require.NoError(t, err)
	for _, m := range metas {
		raw, err := json.Marshal(m)
		require.NoError(t, err)
		key := store.FullMetaKey(m.Table, m.TipIndex)
		if m.Type == store.SnapshotTypeIncremental {
			key = store.IncrMetaKey(m.Table, m.BaseIndex, m.TipIndex)
		}
		require.NoError(t, bucket.Upload(context.Background(), key, bytes.NewReader(raw)))
	}
	return bucket
}

// advanceGCHorizon proposes a GC command so the orders table reports a horizon at index.
func advanceGCHorizon(t *testing.T, engine *storage.Engine, shardID, index uint64) {
	t.Helper()
	bts, err := (&armadapb.Command{
		Table:       []byte("orders"),
		Type:        armadapb.Command_GC,
		LeaderIndex: &index,
	}).MarshalVT()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = engine.SyncPropose(ctx, engine.GetNoOPSession(shardID), bts)
	require.NoError(t, err)
}

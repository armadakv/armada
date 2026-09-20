// Copyright Armada Contributors

package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/objfs"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mockGRPCSnapshotClient struct {
	resp *armadapb.SnapshotQueryResponse
	err  error
}

func (m *mockGRPCSnapshotClient) Query(_ context.Context, _ *armadapb.SnapshotQueryRequest, _ ...grpc.CallOption) (*armadapb.SnapshotQueryResponse, error) {
	return m.resp, m.err
}

func (m *mockGRPCSnapshotClient) Stream(_ context.Context, _ *armadapb.SnapshotRequest, _ ...grpc.CallOption) (armadapb.Snapshot_StreamClient, error) {
	return nil, fmt.Errorf("unexpected stream call")
}

func TestGRPCSnapshotQueryResolver_FallbackToLiveHTTP(t *testing.T) {
	t.Run("failed precondition falls back to live endpoint", func(t *testing.T) {
		r := NewGRPCSnapshotQueryResolver(&mockGRPCSnapshotClient{
			err: status.Error(codes.FailedPrecondition, "shared snapshot store is not configured"),
		})
		resp, err := r.Query(context.Background(), "orders", 42, 100)
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, "snapshots-live/orders", resp.ObjectKey)
	})

	t.Run("unimplemented falls back to live endpoint", func(t *testing.T) {
		r := NewGRPCSnapshotQueryResolver(&mockGRPCSnapshotClient{
			err: status.Error(codes.Unimplemented, "method not implemented"),
		})
		resp, err := r.Query(context.Background(), "orders", 42, 100)
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, "snapshots-live/orders", resp.ObjectKey)
	})

	t.Run("other errors are propagated", func(t *testing.T) {
		r := NewGRPCSnapshotQueryResolver(&mockGRPCSnapshotClient{
			err: status.Error(codes.Internal, "boom"),
		})
		_, err := r.Query(context.Background(), "orders", 42, 100)
		require.ErrorContains(t, err, "snapshot query failed")
		require.ErrorContains(t, err, "boom")
	})
}

// TestBucketSnapshotQueryResolver covers the follower-side resolver, which
// reads the shared store directly and so has no leader table to ask for a GC
// horizon — it relies on the horizon the exporter recorded in the artefact.
func TestBucketSnapshotQueryResolver(t *testing.T) {
	ctx := context.Background()

	upload := func(t *testing.T, bucket objfs.Bucket, metas ...store.Meta) {
		t.Helper()
		for _, m := range metas {
			raw, err := json.Marshal(m)
			require.NoError(t, err)
			key := store.FullMetaKey(m.Table, m.TipIndex)
			if m.Type == store.SnapshotTypeIncremental {
				key = store.IncrMetaKey(m.Table, m.BaseIndex, m.TipIndex)
			}
			require.NoError(t, bucket.Upload(ctx, key, bytes.NewReader(raw)))
		}
	}

	t.Run("an applicable incremental is returned", func(t *testing.T) {
		bucket, err := objfs.NewLocal(t.TempDir())
		require.NoError(t, err)
		upload(t, bucket, store.Meta{
			Table: "orders", Type: store.SnapshotTypeIncremental,
			BaseIndex: 100, TipIndex: 150, GCHorizon: 40,
		})

		resp, err := NewBucketSnapshotQueryResolver(bucket).Query(ctx, "orders", 1, 120)
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_INCREMENTAL, resp.Type)
		require.Equal(t, store.IncrSnapKey("orders", 100, 150), resp.ObjectKey)
	})

	t.Run("a follower at or below the recorded horizon gets the full snapshot", func(t *testing.T) {
		bucket, err := objfs.NewLocal(t.TempDir())
		require.NoError(t, err)
		upload(t, bucket,
			store.Meta{Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 160},
			store.Meta{
				Table: "orders", Type: store.SnapshotTypeIncremental,
				BaseIndex: 100, TipIndex: 150, GCHorizon: 130,
			},
		)

		resp, err := NewBucketSnapshotQueryResolver(bucket).Query(ctx, "orders", 1, 120)
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, uint64(160), resp.TipIndex)
	})

	t.Run("nothing applicable returns NONE", func(t *testing.T) {
		bucket, err := objfs.NewLocal(t.TempDir())
		require.NoError(t, err)
		upload(t, bucket, store.Meta{
			Table: "orders", Type: store.SnapshotTypeIncremental,
			BaseIndex: 100, TipIndex: 150, GCHorizon: 130,
		})

		resp, err := NewBucketSnapshotQueryResolver(bucket).Query(ctx, "orders", 1, 120)
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_NONE, resp.Type)
	})
}

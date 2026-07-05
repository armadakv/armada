package replication

import (
	"context"
	"fmt"
	"testing"

	"github.com/armadakv/armada/armadapb"
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
		resp, err := r.Query(context.Background(), "orders", 100)
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, "snapshots-live/orders", resp.ObjectKey)
	})

	t.Run("unimplemented falls back to live endpoint", func(t *testing.T) {
		r := NewGRPCSnapshotQueryResolver(&mockGRPCSnapshotClient{
			err: status.Error(codes.Unimplemented, "method not implemented"),
		})
		resp, err := r.Query(context.Background(), "orders", 100)
		require.NoError(t, err)
		require.Equal(t, armadapb.SnapshotQueryResponse_FULL, resp.Type)
		require.Equal(t, "snapshots-live/orders", resp.ObjectKey)
	})

	t.Run("other errors are propagated", func(t *testing.T) {
		r := NewGRPCSnapshotQueryResolver(&mockGRPCSnapshotClient{
			err: status.Error(codes.Internal, "boom"),
		})
		_, err := r.Query(context.Background(), "orders", 100)
		require.ErrorContains(t, err, "snapshot query failed")
		require.ErrorContains(t, err, "boom")
	})
}

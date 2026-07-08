// Copyright Armada Contributors

package replication

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/objfs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SnapshotQueryResolver resolves the best snapshot metadata for follower recovery.
type SnapshotQueryResolver interface {
	Query(ctx context.Context, table string, followerIndex uint64) (*armadapb.SnapshotQueryResponse, error)
}

type grpcSnapshotQueryResolver struct {
	client armadapb.SnapshotClient
}

func liveSnapshotObjectKey(table string) string {
	return fmt.Sprintf("snapshots-live/%s", table)
}

func NewGRPCSnapshotQueryResolver(client armadapb.SnapshotClient) SnapshotQueryResolver {
	return &grpcSnapshotQueryResolver{client: client}
}

func (r *grpcSnapshotQueryResolver) Query(ctx context.Context, table string, followerIndex uint64) (*armadapb.SnapshotQueryResponse, error) {
	resp, err := r.client.Query(ctx, &armadapb.SnapshotQueryRequest{
		Table:         table,
		FollowerIndex: followerIndex,
	})
	if err != nil {
		if st, ok := status.FromError(err); ok && (st.Code() == codes.FailedPrecondition || st.Code() == codes.Unimplemented) {
			return &armadapb.SnapshotQueryResponse{
				Type:      armadapb.SnapshotQueryResponse_FULL,
				BaseIndex: 0,
				ObjectKey: liveSnapshotObjectKey(table),
			}, nil
		}
		return nil, fmt.Errorf("snapshot query failed: %w", err)
	}
	return resp, nil
}

type bucketSnapshotQueryResolver struct {
	bucket objfs.Bucket
}

func NewBucketSnapshotQueryResolver(bucket objfs.Bucket) SnapshotQueryResolver {
	return &bucketSnapshotQueryResolver{bucket: bucket}
}

func (r *bucketSnapshotQueryResolver) Query(ctx context.Context, table string, followerIndex uint64) (*armadapb.SnapshotQueryResponse, error) {
	metas, err := store.ListMeta(ctx, r.bucket, table)
	if err != nil {
		return nil, fmt.Errorf("snapshot query failed: %w", err)
	}

	// TODO: once incremental restore is implemented in engine.Restore, remove
	// the filter below and pass all metas to SelectBestSnapshot directly.
	var fullMetas []store.Meta
	for _, m := range metas {
		if m.Type == store.SnapshotTypeFull {
			fullMetas = append(fullMetas, m)
		}
	}

	meta, ok := store.SelectBestSnapshot(fullMetas, followerIndex)
	if !ok {
		return &armadapb.SnapshotQueryResponse{
			Type: armadapb.SnapshotQueryResponse_NONE,
		}, nil
	}

	var snapshotType armadapb.SnapshotQueryResponse_SnapshotType
	var objectKey string
	switch meta.Type {
	case store.SnapshotTypeFull:
		snapshotType = armadapb.SnapshotQueryResponse_FULL
		objectKey = store.FullSnapKey(meta.Table, meta.TipIndex)
	case store.SnapshotTypeIncremental:
		// TODO: incremental restore is not yet implemented; this branch is
		// unreachable until the full-only filter below is removed.
		snapshotType = armadapb.SnapshotQueryResponse_INCREMENTAL
		objectKey = store.IncrSnapKey(meta.Table, meta.BaseIndex, meta.TipIndex)
	default:
		return nil, fmt.Errorf("snapshot query failed: unsupported snapshot type %q", meta.Type)
	}

	var sha []byte
	if meta.SHA256 != "" {
		sha, err = hex.DecodeString(meta.SHA256)
		if err != nil {
			return nil, fmt.Errorf("snapshot query failed: invalid checksum metadata: %w", err)
		}
	}

	return &armadapb.SnapshotQueryResponse{
		Type:      snapshotType,
		BaseIndex: meta.BaseIndex,
		TipIndex:  meta.TipIndex,
		ObjectKey: objectKey,
		Sha256:    sha,
		SizeBytes: meta.SizeBytes,
	}, nil
}

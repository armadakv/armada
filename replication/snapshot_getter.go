// Copyright Armada Contributors
package replication

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/armadakv/objfs"
)

// SnapshotObjectGetter fetches a snapshot object identified by objectKey.
type SnapshotObjectGetter interface {
	Get(ctx context.Context, objectKey string) (io.ReadCloser, error)
}

type httpSnapshotObjectGetter struct {
	client  *http.Client
	baseURL string
}

// NewHTTPSnapshotObjectGetter returns a snapshot getter that fetches objects
// from leader HTTP endpoint.
func NewHTTPSnapshotObjectGetter(client *http.Client, baseURL string) SnapshotObjectGetter {
	return &httpSnapshotObjectGetter{client: client, baseURL: baseURL}
}

func (g *httpSnapshotObjectGetter) Get(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/%s", g.baseURL, objectKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected status code: %d %s", resp.StatusCode, resp.Status)
	}
	return resp.Body, nil
}

type bucketSnapshotObjectGetter struct {
	bucket objfs.Bucket
}

// NewBucketSnapshotObjectGetter returns a snapshot getter that reads objects
// directly from a shared object store.
func NewBucketSnapshotObjectGetter(bucket objfs.Bucket) SnapshotObjectGetter {
	return &bucketSnapshotObjectGetter{bucket: bucket}
}

func (g *bucketSnapshotObjectGetter) Get(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	r, err := g.bucket.Get(ctx, objectKey)
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", objectKey, err)
	}
	return r, nil
}

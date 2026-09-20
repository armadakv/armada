// Copyright Armada Contributors
package replication

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/objfs"
)

// SnapshotObjectGetter fetches a snapshot artefact identified by objectKey.
type SnapshotObjectGetter interface {
	// GetFrom returns a reader over objectKey starting at offset. The returned
	// ranged flag reports whether offset was actually honoured; when it is
	// false the reader starts at byte zero and the caller must discard whatever
	// it had staged. Making that explicit is what keeps a resumed download from
	// silently concatenating a fresh copy onto a partial one.
	GetFrom(ctx context.Context, objectKey string, offset int64) (r io.ReadCloser, ranged bool, err error)
}

// LiveSnapshotGetter fetches an on-demand snapshot of a table from the leader.
// It is deliberately separate from SnapshotObjectGetter: a live snapshot is not
// an object in the shared store, it only exists as an endpoint on the leader,
// so routing it through an object getter is what made `--replication.snapshot-
// source=direct` unable to recover at all.
type LiveSnapshotGetter interface {
	GetLive(ctx context.Context, table string) (io.ReadCloser, error)
}

// SnapshotLeaseKeeper protects a table's artifacts from garbage collection for
// the full duration of a download. Implementations must make RefreshLease safe
// to call repeatedly for an already-acquired lease.
type SnapshotLeaseKeeper interface {
	AcquireLease(ctx context.Context, table string) error
	RefreshLease(ctx context.Context, table string) error
	ReleaseLease(ctx context.Context, table string) error
}

// SnapshotAccess bundles the ways a follower reaches leader snapshots.
type SnapshotAccess struct {
	// Objects fetches committed artefacts from the shared store, directly or
	// proxied through the leader.
	Objects SnapshotObjectGetter
	// Live fetches an on-demand leader snapshot, used when the shared store has
	// nothing applicable.
	Live LiveSnapshotGetter
	// Query resolves which artefact a follower at a given index should use.
	// When nil the leader is asked over gRPC.
	Query SnapshotQueryResolver
	// Leases is optional when the transport provides an equivalent remote lease
	// lifecycle. Direct shared-store downloads use it to acquire, refresh, and
	// release their lease for the entire transfer.
	Leases SnapshotLeaseKeeper
}

// HTTPSnapshotGetter fetches snapshots from the leader over HTTP. It satisfies
// both SnapshotObjectGetter and LiveSnapshotGetter.
type HTTPSnapshotGetter struct {
	client  *http.Client
	baseURL string
}

// NewHTTPSnapshotObjectGetter returns a snapshot getter that fetches objects
// from the leader HTTP endpoint. It serves both committed artefacts and live
// snapshots, and follows the 307 redirect a leader issues when its blob backend
// can hand out a pre-signed URL.
func NewHTTPSnapshotObjectGetter(client *http.Client, baseURL string) *HTTPSnapshotGetter {
	return &HTTPSnapshotGetter{client: client, baseURL: baseURL}
}

func (g *HTTPSnapshotGetter) GetFrom(ctx context.Context, objectKey string, offset int64) (io.ReadCloser, bool, error) {
	url := fmt.Sprintf("%s/%s", g.baseURL, objectKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("new request: %w", err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("do request: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if offset > 0 {
			if err := validateContentRange(resp.Header.Get("Content-Range"), offset); err != nil {
				_ = resp.Body.Close()
				return nil, false, fmt.Errorf("invalid Content-Range for resume at byte %d: %w", offset, err)
			}
			return resp.Body, true, nil
		}
		return resp.Body, false, nil
	case http.StatusOK:
		// The server ignored the range (or there was none): start over.
		return resp.Body, false, nil
	default:
		_ = resp.Body.Close()
		return nil, false, fmt.Errorf("unexpected status code: %d %s", resp.StatusCode, resp.Status)
	}
}

func (g *HTTPSnapshotGetter) GetLive(ctx context.Context, table string) (io.ReadCloser, error) {
	r, _, err := g.GetFrom(ctx, LiveSnapshotObjectKey(table), 0)
	return r, err
}

type bucketSnapshotObjectGetter struct {
	bucket objfs.Bucket
}

// NewBucketSnapshotObjectGetter returns a snapshot getter that reads objects
// directly from a shared object store.
func NewBucketSnapshotObjectGetter(bucket objfs.Bucket) SnapshotObjectGetter {
	return &bucketSnapshotObjectGetter{bucket: bucket}
}

func (g *bucketSnapshotObjectGetter) GetFrom(ctx context.Context, objectKey string, offset int64) (io.ReadCloser, bool, error) {
	if offset <= 0 {
		r, err := g.bucket.Get(ctx, objectKey)
		if err != nil {
			return nil, false, fmt.Errorf("get object %q: %w", objectKey, err)
		}
		return r, false, nil
	}
	r, err := g.bucket.GetRange(ctx, objectKey, offset, -1)
	if err != nil {
		return nil, false, fmt.Errorf("get object %q from offset %d: %w", objectKey, offset, err)
	}
	return r, true, nil
}

type httpLeaseKeeper struct {
	client  *http.Client
	baseURL string
	nodeID  string
}

// NewHTTPLeaseKeeper returns a lease keeper that asks the leader to manage the
// follower's shared-store lease. It is used when a follower proxies snapshot
// access through the leader and therefore has no bucket credentials itself.
// nodeID is the follower's stable raft address, rather than a transient HTTP
// connection address, so every renewal and release addresses the same lease.
func NewHTTPLeaseKeeper(client *http.Client, baseURL, nodeID string) SnapshotLeaseKeeper {
	return &httpLeaseKeeper{client: client, baseURL: strings.TrimRight(baseURL, "/"), nodeID: nodeID}
}

func (k *httpLeaseKeeper) AcquireLease(ctx context.Context, table string) error {
	return k.updateLease(ctx, http.MethodPut, table)
}

func (k *httpLeaseKeeper) RefreshLease(ctx context.Context, table string) error {
	return k.updateLease(ctx, http.MethodPut, table)
}

func (k *httpLeaseKeeper) ReleaseLease(ctx context.Context, table string) error {
	return k.updateLease(ctx, http.MethodDelete, table)
}

func (k *httpLeaseKeeper) updateLease(ctx context.Context, method, table string) error {
	leaseURL := fmt.Sprintf("%s/snapshot-leases/%s", k.baseURL, url.PathEscape(table))
	req, err := http.NewRequestWithContext(ctx, method, leaseURL, nil)
	if err != nil {
		return fmt.Errorf("new snapshot lease request: %w", err)
	}
	req.Header.Set(store.SnapshotLeaseHolderHeader, k.nodeID)

	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("do snapshot lease request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected snapshot lease status code: %d %s", resp.StatusCode, resp.Status)
	}
	return nil
}

// LeaseKeeper constructs an HTTP-backed lease keeper using the same leader
// client and base URL as this snapshot getter. Snapshot object GET requests may
// redirect to a blob backend; lease requests always target this stable leader
// endpoint and are therefore still renewed while a redirected download streams.
func (g *HTTPSnapshotGetter) LeaseKeeper(nodeID string) SnapshotLeaseKeeper {
	return NewHTTPLeaseKeeper(g.client, g.baseURL, nodeID)
}

type bucketLeaseKeeper struct {
	bucket objfs.Bucket
	nodeID string
}

// NewBucketLeaseKeeper returns a lease keeper backed by direct shared-store
// access. nodeID identifies this follower in the lease key.
func NewBucketLeaseKeeper(bucket objfs.Bucket, nodeID string) SnapshotLeaseKeeper {
	return &bucketLeaseKeeper{bucket: bucket, nodeID: nodeID}
}

func (k *bucketLeaseKeeper) AcquireLease(ctx context.Context, table string) error {
	return store.WriteLease(ctx, k.bucket, table, k.nodeID)
}

func (k *bucketLeaseKeeper) RefreshLease(ctx context.Context, table string) error {
	return store.WriteLease(ctx, k.bucket, table, k.nodeID)
}

func (k *bucketLeaseKeeper) ReleaseLease(ctx context.Context, table string) error {
	return store.ReleaseLease(ctx, k.bucket, table, k.nodeID)
}

// validateContentRange verifies that a 206 response starts exactly at offset.
// StatusPartialContent alone is not evidence that the server honored our
// request: appending a differently-ranged body corrupts the staged artifact.
func validateContentRange(header string, offset int64) error {
	parts := strings.Fields(header)
	if len(parts) != 2 || parts[0] != "bytes" {
		return fmt.Errorf("expected bytes range, got %q", header)
	}
	rangeAndSize := strings.SplitN(parts[1], "/", 2)
	if len(rangeAndSize) != 2 || rangeAndSize[1] == "*" {
		return fmt.Errorf("malformed byte range %q", header)
	}
	bounds := strings.SplitN(rangeAndSize[0], "-", 2)
	if len(bounds) != 2 {
		return fmt.Errorf("malformed byte range %q", header)
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil || start != offset {
		return fmt.Errorf("range starts at %d", start)
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil || end < start {
		return fmt.Errorf("invalid range end %q", bounds[1])
	}
	size, err := strconv.ParseInt(rangeAndSize[1], 10, 64)
	if err != nil || size <= end {
		return fmt.Errorf("invalid range size %q", rangeAndSize[1])
	}
	return nil
}

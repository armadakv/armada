// Copyright Armada Contributors

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/armadakv/objfs"
)

// LeaseTTL bounds how long a downloader lease protects a table's artefacts
// from garbage collection. A lease is refreshed for as long as the download is
// alive; the TTL is what stops a follower that died mid-download from pinning
// the table's history forever.
const LeaseTTL = 30 * time.Minute

// WriteLease claims (or refreshes) a downloader lease on tableName for nodeID.
// GC skips a table for as long as a fresh lease exists, which is what keeps an
// artefact from being deleted out from under a resumable download.
func WriteLease(ctx context.Context, bucket objfs.Bucket, tableName, nodeID string) error {
	if bucket == nil {
		return nil
	}
	body := strings.NewReader(time.Now().UTC().Format(time.RFC3339Nano))
	if err := bucket.Upload(ctx, LeaseKey(tableName, leaseNodeID(nodeID)), body); err != nil {
		return fmt.Errorf("write snapshot lease for table %s: %w", tableName, err)
	}
	return nil
}

// ReleaseLease drops the lease held by nodeID on tableName.
func ReleaseLease(ctx context.Context, bucket objfs.Bucket, tableName, nodeID string) error {
	if bucket == nil {
		return nil
	}
	if err := bucket.Delete(ctx, LeaseKey(tableName, leaseNodeID(nodeID))); err != nil {
		return fmt.Errorf("release snapshot lease for table %s: %w", tableName, err)
	}
	return nil
}

// leaseNodeID turns an arbitrary identifier (a raft address, say) into a token
// safe to use as the last path segment of a lease key.
func leaseNodeID(id string) string {
	r := strings.NewReplacer("/", "_", ":", "_", " ", "_")
	out := r.Replace(id)
	if out == "" {
		return "unknown"
	}
	return out
}

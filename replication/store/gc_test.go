// Copyright JAMF Software, LLC

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/armadakv/objfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uploadTestMeta is a helper that uploads a Meta object to the bucket.
func uploadTestMeta(t *testing.T, ctx context.Context, bucket objfs.Bucket, m Meta, key string) {
	t.Helper()
	data, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, bucket.Upload(ctx, key, bytes.NewReader(data)))
}

// uploadTestSnap uploads a placeholder snap artefact.
func uploadTestSnap(t *testing.T, ctx context.Context, bucket objfs.Bucket, key string) {
	t.Helper()
	require.NoError(t, bucket.Upload(ctx, key, bytes.NewReader([]byte("snap-data"))))
}

// setupFullAndIncrArtefacts inserts a full snapshot at tipFull and a chain of
// `incrCount` incremental snapshots into bucket. It returns the tip index of the
// last incremental (or tipFull when incrCount==0).
func setupFullAndIncrArtefacts(t *testing.T, ctx context.Context, bucket objfs.Bucket, table string, tipFull uint64, incrCount int, createdAt time.Time) {
	t.Helper()
	uploadTestSnap(t, ctx, bucket, FullSnapKey(table, tipFull))
	uploadTestMeta(t, ctx, bucket, Meta{
		Table:     table,
		Type:      SnapshotTypeFull,
		TipIndex:  tipFull,
		CreatedAt: createdAt,
		Format:    SnapshotFormat,
	}, FullMetaKey(table, tipFull))

	prev := tipFull
	for i := 0; i < incrCount; i++ {
		next := prev + 10
		uploadTestSnap(t, ctx, bucket, IncrSnapKey(table, prev, next))
		uploadTestMeta(t, ctx, bucket, Meta{
			Table:     table,
			Type:      SnapshotTypeIncremental,
			BaseIndex: prev,
			TipIndex:  next,
			CreatedAt: createdAt,
			Format:    SnapshotFormat,
		}, IncrMetaKey(table, prev, next))
		prev = next
	}
}

// TestGC_NoArtefacts verifies GC handles an empty bucket without errors.
func TestGC_NoArtefacts(t *testing.T) {
	bucket := NewLocalBucket(t)
	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: time.Hour,
		Interval:  time.Minute,
	}, newTestLogger())
	require.NoError(t, gc.RunOnce(context.Background()))
}

// TestGC_RetentionPreservesRecentArtefacts verifies that artefacts newer than
// the retention window are not deleted.
func TestGC_RetentionPreservesRecentArtefacts(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	now := time.Now().UTC()

	setupFullAndIncrArtefacts(t, ctx, bucket, "t", 100, 2, now)

	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: 24 * time.Hour, // objects are fresh
		Interval:  time.Minute,
	}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))

	// Nothing should be deleted.
	assert.Equal(t, 6, ObjectCount(t, bucket, ""), "all artefacts should survive (snap+meta x 3)")
}

// TestGC_DeletesOldIncrementalButKeepsLatestFull verifies that old incremental
// artefacts are removed while the latest full snapshot is always retained.
// To qualify for deletion an incremental's base_index must be below the latest
// full tip so it is not part of the active chain.
func TestGC_DeletesOldIncrementalButKeepsLatestFull(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	old := time.Now().UTC().Add(-48 * time.Hour)

	// Insert an old full at 100 + 2 old incrementals (100→110, 110→120).
	setupFullAndIncrArtefacts(t, ctx, bucket, "t", 100, 2, old)

	// Insert a newer full at 200 — this makes the incrementals (base < 200) stale.
	setupFullAndIncrArtefacts(t, ctx, bucket, "t", 200, 0, old)

	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: time.Hour, // retention = 1h, objects are 48h old
		Interval:  time.Minute,
	}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))

	// The newest full snapshot (at 200) must survive.
	ok, err := bucket.Exists(ctx, FullSnapKey("t", 200))
	require.NoError(t, err)
	assert.True(t, ok, "latest full snap should survive")

	ok, err = bucket.Exists(ctx, FullMetaKey("t", 200))
	require.NoError(t, err)
	assert.True(t, ok, "latest full meta should survive")

	// Old full at 100 should be gone.
	ok, err = bucket.Exists(ctx, FullSnapKey("t", 100))
	require.NoError(t, err)
	assert.False(t, ok, "old full at 100 should be deleted")

	// Old incrementals (base < 200) should be gone.
	ok, err = bucket.Exists(ctx, IncrSnapKey("t", 100, 110))
	require.NoError(t, err)
	assert.False(t, ok, "old incr snap should be deleted")

	ok, err = bucket.Exists(ctx, IncrMetaKey("t", 100, 110))
	require.NoError(t, err)
	assert.False(t, ok, "old incr meta should be deleted")
}

// TestGC_KeepsActiveIncrementalChain verifies that active incremental artefacts
// (those chaining from the latest full) are not deleted even if they are old.
func TestGC_KeepsActiveIncrementalChain(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	recent := time.Now().UTC()

	// Old full at 100.
	setupFullAndIncrArtefacts(t, ctx, bucket, "t", 100, 0, old)

	// New full at 200 (the "latest full").
	setupFullAndIncrArtefacts(t, ctx, bucket, "t", 200, 0, recent)

	// Active incremental chain from the new full (200→210).
	uploadTestSnap(t, ctx, bucket, IncrSnapKey("t", 200, 210))
	uploadTestMeta(t, ctx, bucket, Meta{
		Table: "t", Type: SnapshotTypeIncremental,
		BaseIndex: 200, TipIndex: 210, CreatedAt: old, // old but part of active chain
	}, IncrMetaKey("t", 200, 210))

	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: time.Hour, // old (< retention cutoff)
		Interval:  time.Minute,
	}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))

	// Old full at 100 should be gone (it's not the latest full).
	ok, err := bucket.Exists(ctx, FullSnapKey("t", 100))
	require.NoError(t, err)
	assert.False(t, ok, "old full at 100 should be deleted")

	// Latest full at 200 must survive.
	ok, err = bucket.Exists(ctx, FullSnapKey("t", 200))
	require.NoError(t, err)
	assert.True(t, ok, "latest full at 200 must survive")

	// Active incr chain (200→210) must survive even though it is old.
	ok, err = bucket.Exists(ctx, IncrSnapKey("t", 200, 210))
	require.NoError(t, err)
	assert.True(t, ok, "active incr 200→210 should survive")
}

// TestGC_MultipleTablesGCedIndependently verifies that GC operates correctly
// when the bucket contains artefacts for several tables.
func TestGC_MultipleTablesGCedIndependently(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	recent := time.Now().UTC()

	// t1: old full at 10, old incr 10→20, newer full at 100
	// → the incr (base=10 < 100) qualifies for deletion.
	setupFullAndIncrArtefacts(t, ctx, bucket, "t1", 10, 1, old)
	setupFullAndIncrArtefacts(t, ctx, bucket, "t1", 100, 0, old) // second full makes incr stale

	// t2: single recent full (should survive unchanged).
	setupFullAndIncrArtefacts(t, ctx, bucket, "t2", 20, 0, recent)

	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: time.Hour,
		Interval:  time.Minute,
	}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))

	// t1: latest full (100) survives.
	ok, _ := bucket.Exists(ctx, FullSnapKey("t1", 100))
	assert.True(t, ok, "t1 latest full should survive")

	// t1: old full (10) is deleted.
	ok, _ = bucket.Exists(ctx, FullSnapKey("t1", 10))
	assert.False(t, ok, "t1 old full should be deleted")

	// t1: old incr deleted.
	ok, _ = bucket.Exists(ctx, IncrSnapKey("t1", 10, 20))
	assert.False(t, ok, "t1 old incr should be deleted")

	// t2: all artefacts survive.
	ok, _ = bucket.Exists(ctx, FullSnapKey("t2", 20))
	assert.True(t, ok, "t2 full should survive")
}

// TestGC_WithLease verifies that a leased table is entirely skipped by GC.
func TestGC_WithLease(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	old := time.Now().UTC().Add(-48 * time.Hour)

	setupFullAndIncrArtefacts(t, ctx, bucket, "leased", 100, 2, old)

	// Upload a lease file for this table.
	require.NoError(t, bucket.Upload(ctx, LeaseKey("leased", "downloader-node"), bytes.NewReader(nil)))

	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: time.Hour,
		Interval:  time.Minute,
	}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))

	// Nothing should have been deleted because the table is leased.
	ok, _ := bucket.Exists(ctx, IncrSnapKey("leased", 100, 110))
	assert.True(t, ok, "leased table artefacts must not be deleted")
}

// TestGC_TombstonesWrittenBeforeDeletion verifies that GC writes tombstone log
// entries before deleting artefacts.
func TestGC_TombstonesWrittenBeforeDeletion(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	old := time.Now().UTC().Add(-48 * time.Hour)

	// Single full + one old incremental.
	setupFullAndIncrArtefacts(t, ctx, bucket, "t", 100, 1, old)
	// Add a second full so the first one qualifies for deletion.
	setupFullAndIncrArtefacts(t, ctx, bucket, "t", 200, 0, old)

	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: time.Hour,
		Interval:  time.Minute,
	}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))

	// At least one gc/ log entry should have been written.
	gcLogFound := false
	err := bucket.List(ctx, "gc/", func(a objfs.Attributes) error {
		gcLogFound = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, gcLogFound, "at least one GC tombstone log should be written")
}

// TestGC_RunLoopStopsOnContextCancel verifies that Run exits when the context
// is cancelled (smoke test).
func TestGC_RunLoopStopsOnContextCancel(t *testing.T) {
	bucket := NewLocalBucket(t)
	gc := NewGCWorker(GCConfig{
		Bucket:    bucket,
		Retention: time.Hour,
		Interval:  10 * time.Millisecond,
	}, newTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		gc.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("GCWorker.Run did not stop after context cancellation")
	}
}

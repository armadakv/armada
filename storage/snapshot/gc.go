// Copyright JAMF Software, LLC

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/thanos-io/objstore"
	"go.uber.org/zap"
)

// GCConfig holds the configuration for the GC worker.
type GCConfig struct {
	// Bucket is the blob store to garbage-collect. Must not be nil.
	Bucket objstore.Bucket
	// Retention is the maximum age of snapshot artefacts. Artefacts older than
	// this duration (subject to safety exceptions) are deleted.
	Retention time.Duration
	// Interval controls how often the GC cycle runs.
	Interval time.Duration
}

// GCWorker periodically deletes stale snapshot artefacts from shared object storage.
// It preserves at least one full snapshot per table and the active incremental chain.
type GCWorker struct {
	cfg GCConfig
	log *zap.SugaredLogger
}

// NewGCWorker creates a new GCWorker. The caller must call Run to start it.
func NewGCWorker(cfg GCConfig, log *zap.SugaredLogger) *GCWorker {
	return &GCWorker{
		cfg: cfg,
		log: log.Named("snapshot-gc"),
	}
}

// Run starts the GC loop and blocks until ctx is cancelled.
func (g *GCWorker) Run(ctx context.Context) {
	t := time.NewTicker(g.cfg.Interval)
	defer t.Stop()

	g.log.Info("snapshot GC worker started")
	defer g.log.Info("snapshot GC worker stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.RunOnce(ctx); err != nil {
				g.log.Errorf("snapshot GC error: %v", err)
			}
		}
	}
}

// RunOnce performs a single GC cycle. It is exported so tests can trigger it directly.
func (g *GCWorker) RunOnce(ctx context.Context) error {
	tableArtefacts, err := g.collectArtefacts(ctx)
	if err != nil {
		return fmt.Errorf("collect artefacts: %w", err)
	}

	leased, err := g.collectLeases(ctx)
	if err != nil {
		return fmt.Errorf("collect leases: %w", err)
	}

	cutoff := time.Now().UTC().Add(-g.cfg.Retention)
	var deleted int

	for _, artefacts := range tableArtefacts {
		d, err := g.gcTable(ctx, artefacts, leased, cutoff)
		if err != nil {
			g.log.Errorf("GC table: %v", err)
			continue
		}
		deleted += d
	}

	if deleted > 0 {
		g.log.Infof("GC cycle complete: deleted %d artefact(s)", deleted)
	}
	return nil
}

// tableArtefact groups a Meta with its associated object keys.
type tableArtefact struct {
	meta    Meta
	snapKey string
	metaKey string
}

// collectArtefacts walks all .meta files under snapshots/ and groups them by table name.
func (g *GCWorker) collectArtefacts(ctx context.Context) (map[string][]tableArtefact, error) {
	result := make(map[string][]tableArtefact)

	err := g.cfg.Bucket.Iter(ctx, "snapshots/", func(name string) error {
		if !strings.HasSuffix(name, ".meta") {
			return nil
		}
		// Skip lease files.
		if strings.Contains(name, "/.lease/") {
			return nil
		}

		r, err := g.cfg.Bucket.Get(ctx, name)
		if err != nil {
			if g.cfg.Bucket.IsObjNotFoundErr(err) {
				return nil
			}
			return err
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		var m Meta
		if err := unmarshalMeta(data, &m); err != nil {
			return nil // skip malformed meta files
		}

		snapKey := strings.TrimSuffix(name, ".meta")
		result[m.Table] = append(result[m.Table], tableArtefact{
			meta:    m,
			snapKey: snapKey,
			metaKey: name,
		})
		return nil
	}, objstore.WithRecursiveIter())

	return result, err
}

// collectLeases returns the set of object keys that have a downloader lease.
func (g *GCWorker) collectLeases(ctx context.Context) (map[string]struct{}, error) {
	leased := make(map[string]struct{})
	err := g.cfg.Bucket.Iter(ctx, "snapshots/", func(name string) error {
		if !strings.Contains(name, "/.lease/") {
			return nil
		}
		// The lease key is snapshots/{table}/.lease/{node_id}; it protects
		// the snap key without the lease suffix. We mark the table prefix as
		// leased so any artefact for that table is preserved.
		parts := strings.SplitN(name, "/.lease/", 2)
		if len(parts) == 2 {
			leased[parts[0]] = struct{}{}
		}
		return nil
	}, objstore.WithRecursiveIter())
	return leased, err
}

// gcTable applies GC rules to the artefacts of a single table and returns the
// number of deleted artefacts.
func (g *GCWorker) gcTable(ctx context.Context, artefacts []tableArtefact, leased map[string]struct{}, cutoff time.Time) (int, error) {
	if len(artefacts) == 0 {
		return 0, nil
	}

	tableName := artefacts[0].meta.Table
	tablePrefix := fmt.Sprintf("snapshots/%s", tableName)
	if _, ok := leased[tablePrefix]; ok {
		// A downloader holds a lease on this table; skip GC entirely this cycle.
		g.log.Debugf("GC: table %s has active lease, skipping", tableName)
		return 0, nil
	}

	// Separate fulls from incrementals.
	var fulls, incrs []tableArtefact
	for _, a := range artefacts {
		switch a.meta.Type {
		case SnapshotTypeFull:
			fulls = append(fulls, a)
		case SnapshotTypeIncremental:
			incrs = append(incrs, a)
		}
	}

	// Sort fulls descending by TipIndex so fulls[0] is the most recent.
	sort.Slice(fulls, func(i, j int) bool {
		return fulls[i].meta.TipIndex > fulls[j].meta.TipIndex
	})

	// Determine the active full tip (the most recent full snapshot is always retained).
	var latestFullTip uint64
	if len(fulls) > 0 {
		latestFullTip = fulls[0].meta.TipIndex
	}

	deleted := 0

	// GC old full snapshots (keep the most recent one regardless of age).
	for i, a := range fulls {
		if i == 0 {
			continue // always keep the latest full
		}
		if a.meta.CreatedAt.After(cutoff) {
			continue // not yet expired
		}
		if err := g.deleteArtefact(ctx, a); err != nil {
			g.log.Errorf("GC: delete full artefact %s: %v", a.snapKey, err)
			continue
		}
		deleted++
	}

	// GC incremental snapshots.
	for _, a := range incrs {
		// Keep any incremental that is part of the active chain
		// (i.e. its base is at or after the latest full tip).
		if a.meta.BaseIndex >= latestFullTip {
			continue
		}
		if a.meta.CreatedAt.After(cutoff) {
			continue
		}
		if err := g.deleteArtefact(ctx, a); err != nil {
			g.log.Errorf("GC: delete incr artefact %s: %v", a.snapKey, err)
			continue
		}
		deleted++
	}

	return deleted, nil
}

// deleteArtefact writes a tombstone log entry and then deletes the snap and meta
// objects from the bucket.
func (g *GCWorker) deleteArtefact(ctx context.Context, a tableArtefact) error {
	// Write a JSON tombstone for auditability before any deletion.
	tombstone := struct {
		Action    string       `json:"action"`
		Table     string       `json:"table"`
		Type      SnapshotType `json:"type"`
		BaseIndex uint64       `json:"base_index"`
		TipIndex  uint64       `json:"tip_index"`
		Key       string       `json:"key"`
		DeletedAt string       `json:"deleted_at"`
	}{
		Action:    "delete",
		Table:     a.meta.Table,
		Type:      a.meta.Type,
		BaseIndex: a.meta.BaseIndex,
		TipIndex:  a.meta.TipIndex,
		Key:       a.snapKey,
		DeletedAt: time.Now().UTC().Format(time.RFC3339),
	}
	tombstoneBytes, _ := json.Marshal(tombstone)
	logKey := GCLogKey(time.Now())
	if err := g.cfg.Bucket.Upload(ctx, logKey, bytes.NewReader(tombstoneBytes)); err != nil {
		g.log.Warnf("GC: failed to write tombstone for %s: %v", a.snapKey, err)
		// Non-fatal — proceed with deletion.
	}

	if err := g.cfg.Bucket.Delete(ctx, a.snapKey); err != nil {
		if !g.cfg.Bucket.IsObjNotFoundErr(err) {
			return fmt.Errorf("delete snap %s: %w", a.snapKey, err)
		}
	}
	if err := g.cfg.Bucket.Delete(ctx, a.metaKey); err != nil {
		if !g.cfg.Bucket.IsObjNotFoundErr(err) {
			return fmt.Errorf("delete meta %s: %w", a.metaKey, err)
		}
	}
	g.log.Infof("GC: deleted %s (table %s, %s, tip=%d)", a.snapKey, a.meta.Table, a.meta.Type, a.meta.TipIndex)
	return nil
}

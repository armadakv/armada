// Copyright JAMF Software, LLC

package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/armadakv/armada/armadapb"
	replicationSnapshot "github.com/armadakv/armada/replication/snapshot"
	"github.com/thanos-io/objstore"
	"go.uber.org/zap"
)

// TableSnapshotService provides table access for the snapshot exporter.
// The interface accepts the table name and writes the snapshot directly to the
// supplied io.Writer so that callers do not need to hold a concrete
// table.ActiveTable and the service can be mocked in tests.
//
// storage.EngineTableService (below) adapts storage.Engine to this interface.
type TableSnapshotService interface {
	// GetTableNames returns the names of all currently known tables.
	GetTableNames() ([]string, error)
	// Snapshot writes a full snapshot of the named table to w and returns the
	// applied leader index at which the snapshot was taken.
	Snapshot(ctx context.Context, tableName string, w io.Writer) (uint64, error)
	// IncrementalSnapshot writes a delta snapshot for tableName (changes since
	// sinceIndex) to w and returns the current leader index.
	IncrementalSnapshot(ctx context.Context, tableName string, w io.Writer, sinceIndex uint64) (uint64, error)
}

// ExporterConfig holds the operational parameters for a SnapshotExporter.
type ExporterConfig struct {
	// Bucket is the target blob store. Must not be nil.
	Bucket objstore.Bucket
	// NodeID uniquely identifies the node writing artefacts (written into Meta.NodeID).
	NodeID string
	// FullInterval is how often a full snapshot is exported for each table.
	FullInterval time.Duration
	// IncrInterval is how often an incremental snapshot is exported for each table.
	IncrInterval time.Duration
	// IncrMaxChain is the maximum number of consecutive incremental artefacts
	// before a new full snapshot must be taken. When the chain reaches this
	// length the incremental tick is a no-op.
	IncrMaxChain int
}

// SnapshotExporter periodically exports table snapshots to shared object storage.
// It runs two background loops: one for full exports and one for incremental exports.
type SnapshotExporter struct {
	cfg    ExporterConfig
	tables TableSnapshotService
	log    *zap.SugaredLogger
}

// NewSnapshotExporter creates a new SnapshotExporter. The caller must call Run to
// start the background loops.
func NewSnapshotExporter(tables TableSnapshotService, cfg ExporterConfig, log *zap.SugaredLogger) *SnapshotExporter {
	if cfg.IncrMaxChain <= 0 {
		cfg.IncrMaxChain = 8
	}
	return &SnapshotExporter{
		cfg:    cfg,
		tables: tables,
		log:    log.Named("snapshot-exporter"),
	}
}

// Run starts the exporter loops and blocks until ctx is cancelled. It is safe to
// call Run in its own goroutine.
func (e *SnapshotExporter) Run(ctx context.Context) {
	fullTicker := time.NewTicker(e.cfg.FullInterval)
	incrTicker := time.NewTicker(e.cfg.IncrInterval)
	defer fullTicker.Stop()
	defer incrTicker.Stop()

	e.log.Info("snapshot exporter started")
	defer e.log.Info("snapshot exporter stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case <-fullTicker.C:
			e.runFullExport(ctx)
		case <-incrTicker.C:
			e.runIncrementalExport(ctx)
		}
	}
}

// runFullExport exports a full snapshot for every known table.
func (e *SnapshotExporter) runFullExport(ctx context.Context) {
	tables, err := e.tables.GetTableNames()
	if err != nil {
		e.log.Errorf("full export: failed to list tables: %v", err)
		return
	}
	for _, t := range tables {
		if err := e.ExportFull(ctx, t); err != nil {
			e.log.Errorf("full export: table %s: %v", t, err)
		}
	}
}

// runIncrementalExport exports an incremental snapshot for every known table
// as long as the current incremental chain has not exceeded IncrMaxChain.
func (e *SnapshotExporter) runIncrementalExport(ctx context.Context) {
	tables, err := e.tables.GetTableNames()
	if err != nil {
		e.log.Errorf("incremental export: failed to list tables: %v", err)
		return
	}
	for _, t := range tables {
		if err := e.ExportIncremental(ctx, t); err != nil {
			e.log.Errorf("incremental export: table %s: %v", t, err)
		}
	}
}

// ExportFull takes a full snapshot of tableName and uploads it to the bucket.
// It is idempotent: if a committed meta file already exists for the current
// tip index the call is a no-op.
func (e *SnapshotExporter) ExportFull(ctx context.Context, tableName string) error {
	sf, err := replicationSnapshot.NewTemp()
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	// Write snapshot in armada-command-v1 format (snappy-compressed, length-prefixed).
	tipIndex, err := e.tables.Snapshot(ctx, tableName, sf)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	// Append DUMMY command carrying the leader index — required so followers
	// can advance sysLeaderIndex even when the delta was empty.
	final, err := (&armadapb.Command{
		Table:       []byte(tableName),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &tipIndex,
	}).MarshalVT()
	if err != nil {
		return fmt.Errorf("marshal dummy command: %w", err)
	}
	if _, err := sf.Write(final); err != nil {
		return fmt.Errorf("write dummy command: %w", err)
	}
	if err := sf.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	fi, err := sf.File.Stat()
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}
	size := fi.Size()

	metaKey := FullMetaKey(tableName, tipIndex)

	// Idempotency: skip if already committed.
	if exists, _ := e.cfg.Bucket.Exists(ctx, metaKey); exists {
		e.log.Debugf("full snapshot for table %s at index %d already committed, skipping", tableName, tipIndex)
		return nil
	}

	// Compute SHA-256 of the raw compressed file bytes.
	checksum, err := fileSHA256(sf.File)
	if err != nil {
		return fmt.Errorf("sha256: %w", err)
	}

	// Upload the snapshot data.
	snapKey := FullSnapKey(tableName, tipIndex)
	if _, err := sf.File.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := e.cfg.Bucket.Upload(ctx, snapKey, sf.File); err != nil {
		_ = e.cfg.Bucket.Delete(ctx, snapKey)
		return fmt.Errorf("upload snapshot: %w", err)
	}

	// Commit: write the meta file.
	meta := Meta{
		Table:     tableName,
		Type:      SnapshotTypeFull,
		BaseIndex: 0,
		TipIndex:  tipIndex,
		SizeBytes: size,
		SHA256:    checksum,
		CreatedAt: time.Now().UTC(),
		NodeID:    e.cfg.NodeID,
		Format:    SnapshotFormat,
	}
	if err := e.uploadMeta(ctx, metaKey, meta); err != nil {
		return err
	}
	e.log.Infof("exported full snapshot for table %s at index %d (%d bytes)", tableName, tipIndex, size)
	return nil
}

// ExportIncremental takes an incremental snapshot of tableName (delta since the
// latest committed artefact) and uploads it to the bucket. If no full snapshot
// exists yet, or the incremental chain length has reached IncrMaxChain, the
// call is a no-op and callers should wait for the next full export.
func (e *SnapshotExporter) ExportIncremental(ctx context.Context, tableName string) error {
	baseIndex, chainLen, err := e.findChainTip(ctx, tableName)
	if err != nil {
		return fmt.Errorf("find chain tip: %w", err)
	}
	if baseIndex == 0 {
		// No full snapshot yet — incremental is not possible.
		e.log.Debugf("incremental export: no full snapshot for table %s, skipping", tableName)
		return nil
	}
	if chainLen >= e.cfg.IncrMaxChain {
		e.log.Debugf("incremental export: chain length %d >= max %d for table %s, waiting for full", chainLen, e.cfg.IncrMaxChain, tableName)
		return nil
	}

	sf, err := replicationSnapshot.NewTemp()
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	tipIndex, err := e.tables.IncrementalSnapshot(ctx, tableName, sf, baseIndex)
	if err != nil {
		return fmt.Errorf("incremental snapshot: %w", err)
	}

	if tipIndex == baseIndex {
		// Nothing new to export.
		e.log.Debugf("incremental export: no new data for table %s since index %d", tableName, baseIndex)
		return nil
	}

	// Append DUMMY command.
	final, err := (&armadapb.Command{
		Table:       []byte(tableName),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &tipIndex,
	}).MarshalVT()
	if err != nil {
		return fmt.Errorf("marshal dummy command: %w", err)
	}
	if _, err := sf.Write(final); err != nil {
		return fmt.Errorf("write dummy command: %w", err)
	}
	if err := sf.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	fi, err := sf.File.Stat()
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}
	size := fi.Size()

	metaKey := IncrMetaKey(tableName, baseIndex, tipIndex)
	if exists, _ := e.cfg.Bucket.Exists(ctx, metaKey); exists {
		e.log.Debugf("incremental snapshot for table %s (%d→%d) already committed, skipping", tableName, baseIndex, tipIndex)
		return nil
	}

	checksum, err := fileSHA256(sf.File)
	if err != nil {
		return fmt.Errorf("sha256: %w", err)
	}

	snapKey := IncrSnapKey(tableName, baseIndex, tipIndex)
	if _, err := sf.File.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := e.cfg.Bucket.Upload(ctx, snapKey, sf.File); err != nil {
		_ = e.cfg.Bucket.Delete(ctx, snapKey)
		return fmt.Errorf("upload snapshot: %w", err)
	}

	meta := Meta{
		Table:     tableName,
		Type:      SnapshotTypeIncremental,
		BaseIndex: baseIndex,
		TipIndex:  tipIndex,
		SizeBytes: size,
		SHA256:    checksum,
		CreatedAt: time.Now().UTC(),
		NodeID:    e.cfg.NodeID,
		Format:    SnapshotFormat,
	}
	if err := e.uploadMeta(ctx, metaKey, meta); err != nil {
		return err
	}
	e.log.Infof("exported incremental snapshot for table %s (%d→%d, chain %d/%d, %d bytes)",
		tableName, baseIndex, tipIndex, chainLen+1, e.cfg.IncrMaxChain, size)
	return nil
}

// findChainTip discovers the latest committed artefact for tableName and
// returns the tip index and the current incremental chain length.
// baseIndex == 0 means no full snapshot exists yet.
func (e *SnapshotExporter) findChainTip(ctx context.Context, tableName string) (baseIndex uint64, chainLen int, err error) {
	prefix := fmt.Sprintf("snapshots/%s/", tableName)

	var fullMeta *Meta
	var incrMetas []Meta

	iterErr := e.cfg.Bucket.Iter(ctx, prefix, func(name string) error {
		if !strings.HasSuffix(name, ".meta") {
			return nil
		}
		r, err := e.cfg.Bucket.Get(ctx, name)
		if err != nil {
			if e.cfg.Bucket.IsObjNotFoundErr(err) {
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
			return nil // skip malformed meta
		}
		switch m.Type {
		case SnapshotTypeFull:
			if fullMeta == nil || m.TipIndex > fullMeta.TipIndex {
				cp := m
				fullMeta = &cp
			}
		case SnapshotTypeIncremental:
			incrMetas = append(incrMetas, m)
		}
		return nil
	}, objstore.WithRecursiveIter())
	if iterErr != nil {
		return 0, 0, iterErr
	}
	if fullMeta == nil {
		return 0, 0, nil
	}

	// Walk the incremental chain forward from the latest full tip.
	chainTip := fullMeta.TipIndex
	for {
		var next *Meta
		for i := range incrMetas {
			if incrMetas[i].BaseIndex == chainTip {
				next = &incrMetas[i]
				break
			}
		}
		if next == nil {
			break
		}
		chainTip = next.TipIndex
		chainLen++
	}

	return chainTip, chainLen, nil
}

// ListMeta lists all committed Meta artefacts for tableName, sorted by TipIndex ascending.
func (e *SnapshotExporter) ListMeta(ctx context.Context, tableName string) ([]Meta, error) {
	prefix := fmt.Sprintf("snapshots/%s/", tableName)
	var metas []Meta
	err := e.cfg.Bucket.Iter(ctx, prefix, func(name string) error {
		if !strings.HasSuffix(name, ".meta") {
			return nil
		}
		r, err := e.cfg.Bucket.Get(ctx, name)
		if err != nil {
			if e.cfg.Bucket.IsObjNotFoundErr(err) {
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
			return nil
		}
		metas = append(metas, m)
		return nil
	}, objstore.WithRecursiveIter())
	if err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].TipIndex < metas[j].TipIndex
	})
	return metas, nil
}

// uploadMeta marshals m and uploads it to key.
func (e *SnapshotExporter) uploadMeta(ctx context.Context, key string, m Meta) error {
	data, err := marshalMeta(m)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := e.cfg.Bucket.Upload(ctx, key, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("upload meta %s: %w", key, err)
	}
	return nil
}

// fileSHA256 computes the hex-encoded SHA-256 of the raw file content,
// seeking to the start before reading and restoring the position afterwards.
func fileSHA256(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

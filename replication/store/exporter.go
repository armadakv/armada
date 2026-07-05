// Copyright JAMF Software, LLC

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/armadakv/armada/armadapb"
	replicationSnapshot "github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/objfs"
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
	Bucket objfs.Bucket
	// NodeID uniquely identifies the node writing artefacts (written into Meta.NodeID).
	NodeID string
	// SnapshotTimeout is the maximum time allowed for a single incremental
	// snapshot triggered by log compaction. Defaults to 10 minutes when zero.
	SnapshotTimeout time.Duration
}

// SnapshotExporter exports table snapshots to shared object storage.
//
// Full snapshots are taken on explicit demand via ExportFull — for example
// when a follower requests an initial bootstrap or an operator triggers one.
//
// Incremental snapshots are triggered by Raft log compaction events delivered
// via NotifyLogCompacted. Each incremental captures the delta since the
// previous tip (full or incremental). Because log compaction only fires on
// the Raft leader, only the leader writes incremental artefacts.
//
// Full and incremental artefacts are independent: GC ages them out by
// retention time without any relationship between the two types.
type SnapshotExporter struct {
	cfg    ExporterConfig
	tables TableSnapshotService
	log    *zap.SugaredLogger

	// incrCh carries table names whose log was just compacted. Buffered so
	// that a burst of compaction notifications does not block the caller.
	incrCh chan string
}

// NewSnapshotExporter creates a new SnapshotExporter. The caller must call Run
// in a goroutine to start the background export loop.
func NewSnapshotExporter(tables TableSnapshotService, cfg ExporterConfig, log *zap.SugaredLogger) *SnapshotExporter {
	if cfg.SnapshotTimeout <= 0 {
		cfg.SnapshotTimeout = 10 * time.Minute
	}
	return &SnapshotExporter{
		cfg:    cfg,
		tables: tables,
		log:    log.Named("snapshot-exporter"),
		incrCh: make(chan string, 64),
	}
}

// NotifyLogCompacted should be called by the Raft event handler whenever the
// log for a table shard has been compacted. The call must only be made when
// the local node is the Raft leader for that shard — the caller is responsible
// for the leadership check (see storage.Engine.NotifyLogCompacted).
//
// The call is non-blocking: if the internal queue is full the notification is
// silently dropped (the next compaction will retrigger the export).
func (e *SnapshotExporter) NotifyLogCompacted(tableName string) {
	select {
	case e.incrCh <- tableName:
	default:
		e.log.Debugf("incremental export queue full, dropping notification for table %s", tableName)
	}
}

// Run processes log-compaction notifications and blocks until ctx is
// cancelled. It should be started in its own goroutine.
func (e *SnapshotExporter) Run(ctx context.Context) {
	e.log.Info("snapshot exporter started")
	defer e.log.Info("snapshot exporter stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case tableName := <-e.incrCh:
			sctx, cancel := context.WithTimeout(ctx, e.cfg.SnapshotTimeout)
			err := e.ExportIncremental(sctx, tableName)
			cancel()
			if err != nil {
				e.log.Errorf("incremental export: table %s: %v", tableName, err)
			}
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

	checksum, err := fileSHA256(sf.File)
	if err != nil {
		return fmt.Errorf("sha256: %w", err)
	}

	snapKey := FullSnapKey(tableName, tipIndex)
	if _, err := sf.File.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := e.cfg.Bucket.Upload(ctx, snapKey, sf.File); err != nil {
		if cleanErr := e.cfg.Bucket.Delete(ctx, snapKey); cleanErr != nil && !errors.Is(cleanErr, objfs.ErrNotExist) {
			e.log.Warnf("upload failed and cleanup of partial artefact %s also failed: %v", snapKey, cleanErr)
		}
		return fmt.Errorf("upload snapshot: %w", err)
	}

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

// ExportIncremental takes an incremental snapshot of tableName capturing
// changes since the latest committed tip (full or incremental) and uploads it
// to the bucket. If no prior artefact exists the incremental is taken from
// the beginning (base index 0).
func (e *SnapshotExporter) ExportIncremental(ctx context.Context, tableName string) error {
	baseIndex, err := e.latestTip(ctx, tableName)
	if err != nil {
		return fmt.Errorf("find latest tip: %w", err)
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
		if cleanErr := e.cfg.Bucket.Delete(ctx, snapKey); cleanErr != nil && !errors.Is(cleanErr, objfs.ErrNotExist) {
			e.log.Warnf("upload failed and cleanup of partial artefact %s also failed: %v", snapKey, cleanErr)
		}
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
	e.log.Infof("exported incremental snapshot for table %s (%d→%d, %d bytes)", tableName, baseIndex, tipIndex, size)
	return nil
}

// latestTip returns the highest committed tip index across all artefacts
// (full and incremental) for tableName. Returns 0 if no artefacts exist yet.
func (e *SnapshotExporter) latestTip(ctx context.Context, tableName string) (uint64, error) {
	metas, err := e.ListMeta(ctx, tableName)
	if err != nil {
		return 0, err
	}
	if len(metas) == 0 {
		return 0, nil
	}
	last := metas[len(metas)-1]
	return last.TipIndex, nil
}

// ListMeta lists all committed Meta artefacts for tableName, sorted by TipIndex ascending.
func (e *SnapshotExporter) ListMeta(ctx context.Context, tableName string) ([]Meta, error) {
	return ListMeta(ctx, e.cfg.Bucket, tableName)
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

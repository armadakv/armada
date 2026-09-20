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
	"github.com/armadakv/armada/storage/table/fsm"
	"github.com/armadakv/objfs"
	"go.uber.org/zap"
)

// TableSnapshotService provides table access for the snapshot exporter.
// The interface accepts the table name and writes the snapshot directly to the
// supplied io.Writer so that callers do not need to hold a concrete
// table.ActiveTable and the service can be mocked in tests.
//
// storage.EngineTableService adapts storage.Engine to this interface.

type TableSnapshotService interface {
	// GetTableNames returns the names of all currently known tables.
	GetTableNames() ([]string, error)
	// Snapshot writes a full snapshot of the named table to w and returns the
	// applied leader index at which the snapshot was taken.
	Snapshot(ctx context.Context, tableName string, w io.Writer) (uint64, error)
	// IncrementalSnapshot writes a delta snapshot for tableName (changes since
	// sinceIndex) to w and returns its base, horizon, and tip from one atomic
	// storage view.
	IncrementalSnapshot(ctx context.Context, tableName string, w io.Writer, sinceIndex uint64) (*fsm.SnapshotResponse, error)
	// GCHorizon returns the table's MVCC garbage-collection horizon. A delta
	// taken from at or below this index is silently incomplete.
	GCHorizon(ctx context.Context, tableName string) (uint64, error)
	// IsLeader reports whether this node is the Raft leader for tableName.
	IsLeader(tableName string) (bool, error)
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
	// FullInterval is how often a full snapshot is exported for every table.
	// Without it a bucket only ever accumulates incrementals and a follower
	// that needs a base has nothing to start from. Defaults to 6 hours.
	FullInterval time.Duration
	// IncrMaxChain caps how many incrementals may hang off the newest full
	// snapshot before the next export is forced to be a full one, bounding both
	// recovery time and the blast radius of a lost link. Defaults to 8.
	IncrMaxChain int
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
// GC treats the most recent full snapshot as the active anchor: it is always
// retained, and only incremental artefacts whose base index falls at or after
// the latest full tip are preserved as part of the active chain.
type snapshotArtifact interface {
	io.Writer
	Sync() error
}

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
	if cfg.FullInterval <= 0 {
		cfg.FullInterval = 6 * time.Hour
	}
	if cfg.IncrMaxChain <= 0 {
		cfg.IncrMaxChain = 8
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

	full := time.NewTicker(e.cfg.FullInterval)
	defer full.Stop()

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
		case <-full.C:
			e.exportFullAll(ctx)
		}
	}
}

// exportFullAll writes a fresh full snapshot for every table this node leads.
// Unlike the compaction path, which the Raft event listener has already gated
// on leadership, the interval has no gate of its own.
func (e *SnapshotExporter) exportFullAll(ctx context.Context) {
	names, err := e.tables.GetTableNames()
	if err != nil {
		e.log.Errorf("periodic full export: cannot list tables: %v", err)
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		leader, err := e.tables.IsLeader(name)
		if err != nil {
			e.log.Debugf("periodic full export: leadership of table %s unknown: %v", name, err)
			continue
		}
		if !leader {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, e.cfg.SnapshotTimeout)
		err = e.ExportFull(sctx, name)
		cancel()
		if err != nil {
			e.log.Errorf("periodic full export: table %s: %v", name, err)
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

	tipIndex, err := e.tables.Snapshot(ctx, tableName, sf)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	gcHorizon, err := e.tables.GCHorizon(ctx, tableName)
	if err != nil {
		return fmt.Errorf("read gc horizon: %w", err)
	}

	meta := Meta{
		Table:     tableName,
		Type:      SnapshotTypeFull,
		TipIndex:  tipIndex,
		NodeID:    e.cfg.NodeID,
		Format:    SnapshotFormat,
		GCHorizon: gcHorizon,
	}
	if err := e.publishArtifact(ctx, meta, sf, sf.File); err != nil {
		return err
	}
	e.log.Infof("exported full snapshot for table %s at index %d", tableName, tipIndex)
	return nil
}

// ExportIncremental takes an incremental snapshot of tableName capturing
// changes since the latest committed tip (full or incremental) and uploads it
// to the bucket. If no prior artefact exists the incremental is taken from
// the beginning (base index 0).
//
// When that tip has already fallen to or below the GC horizon the base is
// raised to just above the horizon rather than the export being abandoned for
// a full — see the reasoning inline.
func (e *SnapshotExporter) ExportIncremental(ctx context.Context, tableName string) error {
	baseIndex, chain, err := e.chainState(ctx, tableName)
	if err != nil {
		return fmt.Errorf("find latest tip: %w", err)
	}
	return e.exportIncremental(ctx, tableName, baseIndex, chain)
}

func (e *SnapshotExporter) exportIncremental(ctx context.Context, tableName string, requestedBase uint64, chain int) error {
	if chain >= e.cfg.IncrMaxChain {
		e.log.Infof("incremental chain for table %s reached %d links; exporting a full snapshot instead", tableName, chain)
		return e.ExportFull(ctx, tableName)
	}

	sf, err := replicationSnapshot.NewTemp()
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	resp, err := e.tables.IncrementalSnapshot(ctx, tableName, sf, requestedBase)
	if err != nil {
		return fmt.Errorf("incremental snapshot: %w", err)
	}
	if resp.TipIndex <= resp.BaseIndex {
		e.log.Debugf("incremental export: no new data for table %s since index %d (applied index %d)", tableName, resp.BaseIndex, resp.TipIndex)
		return nil
	}

	meta := Meta{
		Table:     tableName,
		Type:      SnapshotTypeIncremental,
		BaseIndex: resp.BaseIndex,
		TipIndex:  resp.TipIndex,
		NodeID:    e.cfg.NodeID,
		Format:    SnapshotFormat,
		GCHorizon: resp.GCHorizon,
	}
	if err := e.publishArtifact(ctx, meta, sf, sf.File); err != nil {
		return err
	}
	e.log.Infof("exported incremental snapshot for table %s (%d→%d)", tableName, resp.BaseIndex, resp.TipIndex)
	return nil
}

// publishArtifact makes a generated artifact durable and visible. Metadata is
// the commit marker and is always published last. If any later step fails after
// an artifact object was uploaded, the object is removed so it cannot evade
// metadata-driven GC.
func (e *SnapshotExporter) publishArtifact(ctx context.Context, meta Meta, source snapshotArtifact, raw *os.File) (err error) {
	final, err := (&armadapb.Command{
		Table:       []byte(meta.Table),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &meta.TipIndex,
	}).MarshalVT()
	if err != nil {
		return fmt.Errorf("marshal dummy command: %w", err)
	}
	if _, err := source.Write(final); err != nil {
		return fmt.Errorf("write dummy command: %w", err)
	}
	if err := source.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	fi, err := raw.Stat()
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}
	meta.SizeBytes = fi.Size()
	meta.SHA256, err = fileSHA256(raw)
	if err != nil {
		return fmt.Errorf("sha256: %w", err)
	}
	meta.CreatedAt = time.Now().UTC()

	var snapKey, metaKey string
	switch meta.Type {
	case SnapshotTypeFull:
		snapKey = FullSnapKey(meta.Table, meta.TipIndex)
		metaKey = FullMetaKey(meta.Table, meta.TipIndex)
	case SnapshotTypeIncremental:
		snapKey = IncrSnapKey(meta.Table, meta.BaseIndex, meta.TipIndex)
		metaKey = IncrMetaKey(meta.Table, meta.BaseIndex, meta.TipIndex)
	default:
		return fmt.Errorf("unknown snapshot type %q", meta.Type)
	}

	exists, err := e.cfg.Bucket.Exists(ctx, metaKey)
	if err != nil {
		return fmt.Errorf("check existing snapshot: %w", err)
	}
	if exists {
		e.log.Debugf("snapshot for table %s at index %d already committed, skipping", meta.Table, meta.TipIndex)
		return nil
	}
	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := e.cfg.Bucket.Upload(ctx, snapKey, raw); err != nil {
		e.cleanupArtifact(ctx, snapKey)
		return fmt.Errorf("upload snapshot: %w", err)
	}
	if err := e.uploadMeta(ctx, metaKey, meta); err != nil {
		e.cleanupArtifact(ctx, snapKey)
		return err
	}
	return nil
}

func (e *SnapshotExporter) cleanupArtifact(ctx context.Context, snapKey string) {
	if err := e.cfg.Bucket.Delete(ctx, snapKey); err != nil && !errors.Is(err, objfs.ErrNotExist) {
		e.log.Warnf("cleanup of artifact %s failed: %v", snapKey, err)
	}
}

// chainState returns the highest committed tip index across all artefacts for
// tableName, and how many incremental links currently hang off the newest full
// snapshot. Both are zero when no artefacts exist yet.
func (e *SnapshotExporter) chainState(ctx context.Context, tableName string) (tip uint64, chain int, err error) {
	metas, err := e.ListMeta(ctx, tableName)
	if err != nil {
		return 0, 0, err
	}
	if len(metas) == 0 {
		return 0, 0, nil
	}
	var latestFullTip uint64
	for _, m := range metas {
		if m.Type == SnapshotTypeFull && m.TipIndex > latestFullTip {
			latestFullTip = m.TipIndex
		}
	}
	for _, m := range metas {
		if m.Type == SnapshotTypeIncremental && m.BaseIndex >= latestFullTip {
			chain++
		}
	}
	return metas[len(metas)-1].TipIndex, chain, nil
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
// seeking to the start before reading. The file is left positioned at EOF
// after the call; callers that need to re-read must seek themselves.
func fileSHA256(f io.ReadSeeker) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

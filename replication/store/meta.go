// Copyright JAMF Software, LLC

// Package store provides shared-storage snapshot export and garbage collection
// for Armada leader clusters (proposal 005 – Phase A).
//
// The package defines the object-key scheme and the JSON commit-signal (Meta) that
// governs the lifecycle of snapshot artefacts in the configured blob store.
package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// SnapshotType distinguishes full from incremental snapshot artefacts.
type SnapshotType string

const (
	// SnapshotTypeFull denotes a complete table snapshot.
	SnapshotTypeFull SnapshotType = "full"
	// SnapshotTypeIncremental denotes a delta snapshot starting from a prior base.
	SnapshotTypeIncremental SnapshotType = "incr"

	// SnapshotFormat identifies the wire format of inter-cluster snapshot artefacts:
	// a sequence of length-prefixed, snappy-compressed armadapb.Command protobuf messages.
	SnapshotFormat = "armada-command-v1"
)

// Meta is the commit signal written to shared storage after a snapshot artefact has
// been fully uploaded and its SHA-256 verified. Followers treat the absence of a
// corresponding .meta file as an incomplete (uncommitted) artefact and ignore the
// associated .snap file.
type Meta struct {
	Table     string       `json:"table"`
	Type      SnapshotType `json:"type"`
	BaseIndex uint64       `json:"base_index"`
	TipIndex  uint64       `json:"tip_index"`
	SizeBytes int64        `json:"size_bytes"`
	SHA256    string       `json:"sha256"`
	CreatedAt time.Time    `json:"created_at"`
	NodeID    string       `json:"node_id"`
	Format    string       `json:"format"`
}

// Object key helpers ─────────────────────────────────────────────────────────

// FullSnapKey returns the object key for a full snapshot artefact.
//
//	snapshots/{table}/full/{tip_index}.snap
func FullSnapKey(tableName string, tipIndex uint64) string {
	return fmt.Sprintf("snapshots/%s/full/%d.snap", tableName, tipIndex)
}

// FullMetaKey returns the object key for a full snapshot meta commit file.
//
//	snapshots/{table}/full/{tip_index}.snap.meta
func FullMetaKey(tableName string, tipIndex uint64) string {
	return FullSnapKey(tableName, tipIndex) + ".meta"
}

// IncrSnapKey returns the object key for an incremental snapshot artefact.
//
//	snapshots/{table}/incr/{base_index}_{tip_index}.snap
func IncrSnapKey(tableName string, baseIndex, tipIndex uint64) string {
	return fmt.Sprintf("snapshots/%s/incr/%d_%d.snap", tableName, baseIndex, tipIndex)
}

// IncrMetaKey returns the object key for an incremental snapshot meta commit file.
//
//	snapshots/{table}/incr/{base_index}_{tip_index}.snap.meta
func IncrMetaKey(tableName string, baseIndex, tipIndex uint64) string {
	return IncrSnapKey(tableName, baseIndex, tipIndex) + ".meta"
}

// LeaseKey returns the object key for a downloader lease file. Lease files
// prevent GC from deleting an artefact that is currently being downloaded.
//
//	snapshots/{table}/.lease/{node_id}
func LeaseKey(tableName, nodeID string) string {
	return fmt.Sprintf("snapshots/%s/.lease/%s", tableName, nodeID)
}

// GCLogKey returns the object key for a GC tombstone audit-log entry.
//
//	gc/{timestamp}.log
func GCLogKey(t time.Time) string {
	return fmt.Sprintf("gc/%s.log", t.UTC().Format(time.RFC3339Nano))
}

// Meta marshaling helpers ─────────────────────────────────────────────────────

func (m Meta) String() string {
	return fmt.Sprintf("Meta{table=%s, type=%s, base=%d, tip=%d, size=%d, sha256=%s, created_at=%s, node_id=%s, format=%s}",
		m.Table, m.Type, m.BaseIndex, m.TipIndex, m.SizeBytes, m.SHA256, m.CreatedAt.Format(time.RFC3339), m.NodeID, m.Format)
}

func marshalMeta(m Meta) ([]byte, error) {
	return json.Marshal(m)
}

func unmarshalMeta(data []byte, m *Meta) error {
	return json.Unmarshal(data, m)
}

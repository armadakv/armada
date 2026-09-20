// Copyright JAMF Software, LLC

package store

import (
	"context"
	"fmt"
	"io"

	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/armada/storage/table/fsm"
)

// engineTableService adapts a tableProvider (storage.Engine) to the
// TableSnapshotService interface expected by SnapshotExporter.
type engineTableService struct {
	tables tableProvider
	nodeID uint64
}

// tableProvider is the minimal subset of storage.Engine used by engineTableService.
type tableProvider interface {
	GetTables() ([]table.Table, error)
	GetTable(name string) (table.ActiveTable, error)
	GetLeaderID(shardID uint64) (uint64, uint64, bool, error)
}

// NewEngineTableService wraps e in a TableSnapshotService. nodeID is this
// node's Raft replica ID, used to answer IsLeader.
// storage.Engine satisfies tableProvider out of the box.
func NewEngineTableService(e tableProvider, nodeID uint64) TableSnapshotService {
	return &engineTableService{tables: e, nodeID: nodeID}
}

// IsLeader reports whether this node is the Raft leader of tableName's shard.
// Compaction-driven exports are already leader-gated by the caller, but the
// interval-driven full export has no such gate of its own and every member
// would otherwise race to upload the same artefact.
func (s *engineTableService) IsLeader(tableName string) (bool, error) {
	t, err := s.tables.GetTable(tableName)
	if err != nil {
		return false, fmt.Errorf("get table %s: %w", tableName, err)
	}
	leaderID, _, valid, err := s.tables.GetLeaderID(t.ClusterID)
	if err != nil {
		return false, err
	}
	return valid && leaderID == s.nodeID, nil
}

// GCHorizon returns the table's MVCC garbage-collection horizon.
func (s *engineTableService) GCHorizon(ctx context.Context, tableName string) (uint64, error) {
	t, err := s.tables.GetTable(tableName)
	if err != nil {
		return 0, fmt.Errorf("get table %s: %w", tableName, err)
	}
	resp, err := t.GCHorizon(ctx)
	if err != nil {
		return 0, err
	}
	return resp.Index, nil
}

func (s *engineTableService) GetTableNames() ([]string, error) {
	tables, err := s.tables.GetTables()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	return names, nil
}

func (s *engineTableService) Snapshot(ctx context.Context, tableName string, w io.Writer) (uint64, error) {
	t, err := s.tables.GetTable(tableName)
	if err != nil {
		return 0, fmt.Errorf("get table %s: %w", tableName, err)
	}
	resp, err := t.Snapshot(ctx, w)
	if err != nil {
		return 0, err
	}
	return resp.Index, nil
}

func (s *engineTableService) IncrementalSnapshot(ctx context.Context, tableName string, w io.Writer, sinceIndex uint64) (*fsm.SnapshotResponse, error) {
	t, err := s.tables.GetTable(tableName)
	if err != nil {
		return nil, fmt.Errorf("get table %s: %w", tableName, err)
	}
	return t.IncrementalSnapshot(ctx, w, sinceIndex)
}

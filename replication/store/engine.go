// Copyright JAMF Software, LLC

package store

import (
	"context"
	"fmt"
	"io"

	"github.com/armadakv/armada/storage/table"
)

// engineTableService adapts a tableProvider (storage.Engine) to the
// TableSnapshotService interface expected by SnapshotExporter.
type engineTableService struct {
	tables tableProvider
}

// tableProvider is the minimal subset of storage.Engine used by engineTableService.
type tableProvider interface {
	GetTables() ([]table.Table, error)
	GetTable(name string) (table.ActiveTable, error)
}

// NewEngineTableService wraps e in a TableSnapshotService.
// storage.Engine satisfies tableProvider out of the box.
func NewEngineTableService(e tableProvider) TableSnapshotService {
	return &engineTableService{tables: e}
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

func (s *engineTableService) IncrementalSnapshot(ctx context.Context, tableName string, w io.Writer, sinceIndex uint64) (uint64, error) {
	t, err := s.tables.GetTable(tableName)
	if err != nil {
		return 0, fmt.Errorf("get table %s: %w", tableName, err)
	}
	resp, err := t.IncrementalSnapshot(ctx, w, sinceIndex)
	if err != nil {
		return 0, err
	}
	return resp.Index, nil
}

// Copyright JAMF Software, LLC

// Package armadaserver provides the gRPC server implementations for all Armada APIs,
// including the user-facing KV, Tables, and Cluster services, the cross-cluster Replication
// and Snapshot services, the Maintenance (backup/restore/reset) service, and the REST server
// for metrics, health checks, and pprof profiling.
package armadaserver

import (
	"context"
	"io"
	"iter"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/raftpb"
	"github.com/armadakv/armada/storage/table"
)

type KVService interface {
	Range(ctx context.Context, req *armadapb.RangeRequest) (*armadapb.RangeResponse, error)
	Put(ctx context.Context, req *armadapb.PutRequest) (*armadapb.PutResponse, error)
	Delete(ctx context.Context, req *armadapb.DeleteRangeRequest) (*armadapb.DeleteRangeResponse, error)
	Txn(ctx context.Context, req *armadapb.TxnRequest) (*armadapb.TxnResponse, error)
	IterateRange(ctx context.Context, req *armadapb.RangeRequest) (iter.Seq[*armadapb.RangeResponse], error)
}

type SnapshotService interface {
	Snapshot(ctx context.Context, writer io.Writer) error
}

type TableService interface {
	GetTables() ([]table.Table, error)
	GetTable(name string) (table.ActiveTable, error)
	Restore(name string, reader io.Reader) error
	CreateTable(name string) (table.Table, error)
	DeleteTable(name string) error
}

type ClusterService interface {
	MemberList(context.Context, *armadapb.MemberListRequest) (*armadapb.MemberListResponse, error)
	Status(context.Context, *armadapb.StatusRequest) (*armadapb.StatusResponse, error)
}

type ConfigService func() map[string]any

type LogReaderService interface {
	QueryRaftLog(ctx context.Context, clusterID uint64, logRange raft.LogRange, maxSize uint64) ([]raftpb.Entry, error)
}

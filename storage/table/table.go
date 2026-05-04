// Copyright JAMF Software, LLC

package table

import (
	"context"
	"io"
	"iter"
	"time"

	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/client"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/armadapb"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table/fsm"
	"github.com/armadakv/armada/storage/table/key"
)

type raftHandler interface {
	SyncRead(ctx context.Context, id uint64, req interface{}) (interface{}, error)
	StaleRead(id uint64, req interface{}) (interface{}, error)
	SyncPropose(ctx context.Context, session *client.Session, bytes []byte) (sm.Result, error)
	GetNoOPSession(id uint64) *client.Session
}

// MaxValueLen 2MB max value.
const MaxValueLen = 2 * 1024 * 1024

// Table stored representation of a table.
type Table struct {
	Name      string `json:"name"`
	ClusterID uint64 `json:"cluster_id"`
	RecoverID uint64 `json:"recover_id"`
}

// AsActive returns ActiveTable wrapper of this table.
func (t Table) AsActive(host raftHandler) ActiveTable {
	return ActiveTable{nh: host, session: host.GetNoOPSession(t.ClusterID), Table: t}
}

// ActiveTable could be queried and new proposals could be made through it.
type ActiveTable struct {
	Table
	nh      raftHandler
	session *client.Session
}

func readTable[S any](t *ActiveTable, ctx context.Context, linearizable bool, req any) (S, error) {
	var (
		err error
		val interface{}
	)
	if linearizable {
		val, err = t.nh.SyncRead(ctx, t.ClusterID, req)
	} else {
		val, err = t.nh.StaleRead(t.ClusterID, req)
	}
	if err != nil {
		return *new(S), err
	}
	return val.(S), nil
}

func proposeTable[S any](t *ActiveTable, ctx context.Context, cmd *armadapb.Command) (S, uint64, error) {
	bytes, err := cmd.MarshalVT()
	if err != nil {
		return *new(S), 0, err
	}
	var res sm.Result
	for {
		res, err = t.nh.SyncPropose(ctx, t.session, bytes)
		if err == nil {
			break
		}
		if !raft.IsTempError(err) {
			return *new(S), 0, err
		}
		select {
		case <-ctx.Done():
			return *new(S), 0, err
		case <-time.After(50 * time.Millisecond):
		}
	}
	pr := &armadapb.CommandResult{}
	if err := pr.UnmarshalVTUnsafe(res.Data); err != nil {
		return *new(S), 0, err
	}
	if len(pr.Responses) == 0 {
		return *new(S), 0, serrors.ErrNoResultFound
	}
	switch r := pr.Responses[0].Response.(type) {
	case S:
		return r, pr.Revision, nil
	default:
		return *new(S), 0, serrors.ErrUnknownResultType
	}
}

// Range performs a Range query in the Raft data, supplied context must have a deadline set.
func (t *ActiveTable) Range(ctx context.Context, req *armadapb.RangeRequest) (*armadapb.RangeResponse, error) {
	if len(req.Key) > key.LatestVersionLen {
		return nil, serrors.ErrKeyLengthExceeded
	}
	if len(req.RangeEnd) > key.LatestVersionLen {
		return nil, serrors.ErrKeyLengthExceeded
	}

	response, err := readTable[*armadapb.ResponseOp_Range](t, ctx, req.Linearizable, &armadapb.RequestOp_Range{
		Key:       req.Key,
		RangeEnd:  req.RangeEnd,
		Limit:     req.Limit,
		KeysOnly:  req.KeysOnly,
		CountOnly: req.CountOnly,
	})
	if err != nil {
		return nil, err
	}
	return &armadapb.RangeResponse{
		Kvs:   response.Kvs,
		Count: response.Count,
		More:  response.More,
	}, nil
}

// Put performs a Put proposal into the Raft, supplied context must have a deadline set.
func (t *ActiveTable) Put(ctx context.Context, req *armadapb.PutRequest) (*armadapb.PutResponse, error) {
	if len(req.Key) == 0 {
		return nil, serrors.ErrEmptyKey
	}
	if len(req.Key) > key.LatestVersionLen {
		return nil, serrors.ErrKeyLengthExceeded
	}
	if len(req.Value) > MaxValueLen {
		return nil, serrors.ErrValueLengthExceeded
	}
	cmd := &armadapb.Command{
		Type:  armadapb.Command_PUT,
		Table: req.Table,
		Kv: &armadapb.KeyValue{
			Key:   req.Key,
			Value: req.Value,
		},
		PrevKvs: req.PrevKv,
	}
	r, rev, err := proposeTable[*armadapb.ResponseOp_ResponsePut](t, ctx, cmd)
	if err != nil {
		return nil, err
	}
	return &armadapb.PutResponse{PrevKv: r.ResponsePut.PrevKv, Header: &armadapb.ResponseHeader{Revision: rev}}, nil
}

// Delete performs a DeleteRange proposal into the Raft, supplied context must have a deadline set.
func (t *ActiveTable) Delete(ctx context.Context, req *armadapb.DeleteRangeRequest) (*armadapb.DeleteRangeResponse, error) {
	if len(req.Key) == 0 {
		return nil, serrors.ErrEmptyKey
	}
	if len(req.Key) > key.LatestVersionLen {
		return nil, serrors.ErrKeyLengthExceeded
	}
	cmd := &armadapb.Command{
		Type:  armadapb.Command_DELETE,
		Table: req.Table,
		Kv: &armadapb.KeyValue{
			Key: req.Key,
		},
		PrevKvs:  req.PrevKv,
		RangeEnd: req.RangeEnd,
		Count:    req.Count,
	}
	r, rev, err := proposeTable[*armadapb.ResponseOp_ResponseDeleteRange](t, ctx, cmd)
	if err != nil {
		return nil, err
	}
	return &armadapb.DeleteRangeResponse{Deleted: r.ResponseDeleteRange.Deleted, PrevKvs: r.ResponseDeleteRange.PrevKvs, Header: &armadapb.ResponseHeader{Revision: rev}}, nil
}

func (t *ActiveTable) Txn(ctx context.Context, req *armadapb.TxnRequest) (*armadapb.TxnResponse, error) {
	// Do not propose read-only transactions through the log
	if req.IsReadonly() {
		return readTable[*armadapb.TxnResponse](t, ctx, true, req)
	}

	cmd := &armadapb.Command{
		Type:  armadapb.Command_TXN,
		Table: req.Table,
		Txn: &armadapb.Txn{
			Compare: req.Compare,
			Success: req.Success,
			Failure: req.Failure,
		},
	}

	bytes, err := cmd.MarshalVT()
	if err != nil {
		return nil, err
	}
	res, err := t.nh.SyncPropose(ctx, t.session, bytes)
	if err != nil {
		return nil, err
	}
	txr := &armadapb.CommandResult{}
	if err := txr.UnmarshalVTUnsafe(res.Data); err != nil {
		return nil, err
	}
	return &armadapb.TxnResponse{
		Succeeded: fsm.UpdateResult(res.Value) == fsm.ResultSuccess,
		Responses: txr.Responses,
		Header:    &armadapb.ResponseHeader{Revision: txr.Revision},
	}, nil
}

// Iterator returns open pebble.Iterator it is an API consumer responsibility to close it.
func (t *ActiveTable) Iterator(ctx context.Context, req *armadapb.RangeRequest) (iter.Seq[*armadapb.ResponseOp_Range], error) {
	return readTable[iter.Seq[*armadapb.ResponseOp_Range]](t, ctx, req.Linearizable, fsm.IteratorRequest{RangeOp: &armadapb.RequestOp_Range{
		Key:       req.Key,
		RangeEnd:  req.RangeEnd,
		Limit:     req.Limit,
		KeysOnly:  req.KeysOnly,
		CountOnly: req.CountOnly,
	}})
}

// Snapshot streams snapshot to the provided writer.
func (t *ActiveTable) Snapshot(ctx context.Context, writer io.Writer) (*fsm.SnapshotResponse, error) {
	return readTable[*fsm.SnapshotResponse](t, ctx, true, fsm.SnapshotRequest{Writer: writer, Stopper: ctx.Done()})
}

// IncrementalSnapshot streams only the changes (puts and deletes) with seqno > sinceIndex to the provided writer.
// The caller must ensure sinceIndex is above the table's GC horizon, otherwise the delta may be incomplete.
func (t *ActiveTable) IncrementalSnapshot(ctx context.Context, writer io.Writer, sinceIndex uint64) (*fsm.SnapshotResponse, error) {
	return readTable[*fsm.SnapshotResponse](t, ctx, true, fsm.IncrementalSnapshotRequest{Writer: writer, Stopper: ctx.Done(), SinceIndex: sinceIndex})
}

// LocalIndex returns local index.
func (t *ActiveTable) LocalIndex(ctx context.Context, linearizable bool) (*fsm.IndexResponse, error) {
	return readTable[*fsm.IndexResponse](t, ctx, linearizable, fsm.LocalIndexRequest{})
}

// LeaderIndex returns leader index.
func (t *ActiveTable) LeaderIndex(ctx context.Context, linearizable bool) (*fsm.IndexResponse, error) {
	return readTable[*fsm.IndexResponse](t, ctx, linearizable, fsm.LeaderIndexRequest{})
}

// GCHorizon returns the current GC horizon of the table's FSM. Any MVCC
// revision strictly below this value has been (or will be) reclaimed by GC.
// Reads at a revision below the horizon may return ErrCompacted.
func (t *ActiveTable) GCHorizon(ctx context.Context) (*fsm.IndexResponse, error) {
	return readTable[*fsm.IndexResponse](t, ctx, false, fsm.GCHorizonRequest{})
}

// Reset resets the leader index to 0.
func (t *ActiveTable) Reset(ctx context.Context) error {
	li := uint64(0)
	cmd := &armadapb.Command{
		Type:        armadapb.Command_DUMMY,
		Table:       []byte(t.Name),
		LeaderIndex: &li,
	}
	bts, err := cmd.MarshalVT()
	if err != nil {
		return err
	}
	_, err = t.nh.SyncPropose(ctx, t.session, bts)
	return err
}

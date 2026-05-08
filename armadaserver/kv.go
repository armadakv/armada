// Copyright JAMF Software, LLC

package armadaserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage"
	serrors "github.com/armadakv/armada/storage/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// keyContainsNullByte reports whether a user key contains a null (0x00) byte.
// Null bytes are forbidden because the V2 physical-key encoding uses a 0x00
// separator between the user-key bytes and the MVCC sequence number; allowing
// null bytes would make the physical-key prefix ambiguous and break GC range
// deletions.
func keyContainsNullByte(k []byte) bool {
	return bytes.IndexByte(k, 0) >= 0
}

// rangeEndContainsNullByte is like keyContainsNullByte but allows the
// single-byte \x00 sentinel, which is the conventional "all keys >= key"
// wildcard for range_end fields.
func rangeEndContainsNullByte(k []byte) bool {
	if len(k) == 1 && k[0] == 0 {
		return false
	}
	return bytes.IndexByte(k, 0) >= 0
}

// rangeKeyContainsNullByte is like keyContainsNullByte but allows the
// single-byte \x00 sentinel as a range start key, which represents
// "start from the beginning of the keyspace" in a range scan.
func rangeKeyContainsNullByte(k []byte) bool {
	if len(k) == 1 && k[0] == 0 {
		return false
	}
	return bytes.IndexByte(k, 0) >= 0
}

// validateTxnKeys checks all keys embedded in a TxnRequest for null bytes.
func validateTxnKeys(req *armadapb.TxnRequest) error {
	for _, cmp := range req.Compare {
		if keyContainsNullByte(cmp.Key) {
			return status.Errorf(codes.InvalidArgument, "key must not contain null bytes")
		}
		if rangeEndContainsNullByte(cmp.RangeEnd) {
			return status.Errorf(codes.InvalidArgument, "range_end must not contain null bytes")
		}
	}
	for _, op := range req.Success {
		if err := validateRequestOpKeys(op); err != nil {
			return err
		}
	}
	for _, op := range req.Failure {
		if err := validateRequestOpKeys(op); err != nil {
			return err
		}
	}
	return nil
}

func validateRequestOpKeys(op *armadapb.RequestOp) error {
	if r := op.GetRequestRange(); r != nil {
		if rangeKeyContainsNullByte(r.Key) {
			return status.Errorf(codes.InvalidArgument, "key must not contain null bytes")
		}
		if rangeEndContainsNullByte(r.RangeEnd) {
			return status.Errorf(codes.InvalidArgument, "range_end must not contain null bytes")
		}
	}
	if p := op.GetRequestPut(); p != nil {
		if keyContainsNullByte(p.Key) {
			return status.Errorf(codes.InvalidArgument, "key must not contain null bytes")
		}
	}
	if d := op.GetRequestDeleteRange(); d != nil {
		if keyContainsNullByte(d.Key) {
			return status.Errorf(codes.InvalidArgument, "key must not contain null bytes")
		}
		if rangeEndContainsNullByte(d.RangeEnd) {
			return status.Errorf(codes.InvalidArgument, "range_end must not contain null bytes")
		}
	}
	return nil
}

// KVServer implements KV service from proto/regatta.proto.
type KVServer struct {
	armadapb.UnimplementedKVServer
	Storage KVService
}

// Range implements proto/regatta.proto KV.Range method.
// Currently, only subset of functionality is implemented.
// The versioning functionality is not available.
func (s *KVServer) Range(ctx context.Context, req *armadapb.RangeRequest) (*armadapb.RangeResponse, error) {
	if req.GetLimit() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "limit must be a positive number")
	} else if req.GetKeysOnly() && req.GetCountOnly() {
		return nil, status.Error(codes.InvalidArgument, "keys_only and count_only must not be set at the same time")
	} else if req.GetMinModRevision() > 0 {
		return nil, status.Error(codes.Unimplemented, "min_mod_revision not implemented")
	} else if req.GetMaxModRevision() > 0 {
		return nil, status.Error(codes.Unimplemented, "max_mod_revision not implemented")
	} else if req.GetMinCreateRevision() > 0 {
		return nil, status.Error(codes.Unimplemented, "min_create_revision not implemented")
	} else if req.GetMaxCreateRevision() > 0 {
		return nil, status.Error(codes.Unimplemented, "max_create_revision not implemented")
	}

	if len(req.GetTable()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "table must be set")
	}

	if len(req.GetKey()) == 0 && len(req.GetRangeEnd()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "key must be set")
	}

	if rangeKeyContainsNullByte(req.GetKey()) {
		return nil, status.Error(codes.InvalidArgument, "key must not contain null bytes")
	}

	if rangeEndContainsNullByte(req.GetRangeEnd()) {
		return nil, status.Error(codes.InvalidArgument, "range_end must not contain null bytes")
	}

	val, err := s.Storage.Range(ctx, req)
	if err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			return nil, status.Error(codes.NotFound, "table not found")
		}
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return val, nil
}

// IterateRange gets the keys in the range from the key-value store.
func (s *KVServer) IterateRange(req *armadapb.RangeRequest, srv armadapb.KV_IterateRangeServer) error {
	if req.GetLimit() < 0 {
		return status.Errorf(codes.InvalidArgument, "limit must be a positive number")
	} else if req.GetKeysOnly() && req.GetCountOnly() {
		return status.Error(codes.InvalidArgument, "keys_only and count_only must not be set at the same time")
	} else if req.GetMinModRevision() > 0 {
		return status.Error(codes.Unimplemented, "min_mod_revision not implemented")
	} else if req.GetMaxModRevision() > 0 {
		return status.Error(codes.Unimplemented, "max_mod_revision not implemented")
	} else if req.GetMinCreateRevision() > 0 {
		return status.Error(codes.Unimplemented, "min_create_revision not implemented")
	} else if req.GetMaxCreateRevision() > 0 {
		return status.Error(codes.Unimplemented, "max_create_revision not implemented")
	}

	if len(req.GetTable()) == 0 {
		return status.Error(codes.InvalidArgument, "table must be set")
	}

	if len(req.GetKey()) == 0 {
		return status.Error(codes.InvalidArgument, "key must be set")
	}

	if keyContainsNullByte(req.GetKey()) {
		return status.Error(codes.InvalidArgument, "key must not contain null bytes")
	}

	if rangeEndContainsNullByte(req.GetRangeEnd()) {
		return status.Error(codes.InvalidArgument, "range_end must not contain null bytes")
	}

	ctx := srv.Context()
	r, err := s.Storage.IterateRange(ctx, req)
	if err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			return status.Error(codes.NotFound, "table not found")
		}
		if serrors.IsSafeToRetry(err) {
			return status.Error(codes.Unavailable, err.Error())
		}
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	pull, stop := iter.Pull(r)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			response, valid := pull()
			if !valid {
				return nil
			}
			if err := srv.Send(response); err != nil {
				return status.Error(codes.Internal, err.Error())
			}
		}
	}
}

// Put implements proto/regatta.proto KV.Put method.
func (s *KVServer) Put(ctx context.Context, req *armadapb.PutRequest) (*armadapb.PutResponse, error) {
	if len(req.GetTable()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "table must be set")
	}

	if len(req.GetKey()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "key must be set")
	}

	if keyContainsNullByte(req.GetKey()) {
		return nil, status.Errorf(codes.InvalidArgument, "key must not contain null bytes")
	}

	r, err := s.Storage.Put(ctx, req)
	if err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			return nil, status.Error(codes.NotFound, "table not found")
		}
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return r, nil
}

// DeleteRange implements proto/regatta.proto KV.DeleteRange method.
func (s *KVServer) DeleteRange(ctx context.Context, req *armadapb.DeleteRangeRequest) (*armadapb.DeleteRangeResponse, error) {
	if len(req.GetTable()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "table must be set")
	}

	if len(req.GetKey()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "key must be set")
	}

	if keyContainsNullByte(req.GetKey()) {
		return nil, status.Errorf(codes.InvalidArgument, "key must not contain null bytes")
	}

	if rangeEndContainsNullByte(req.GetRangeEnd()) {
		return nil, status.Errorf(codes.InvalidArgument, "range_end must not contain null bytes")
	}

	r, err := s.Storage.Delete(ctx, req)
	if err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			return nil, status.Error(codes.NotFound, "table not found")
		}
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return r, nil
}

// Txn processes multiple requests in a single transaction.
// A txn request increments the revision of the key-value store
// and generates events with the same revision for every completed request.
// It is allowed to modify the same key several times within one txn (the result will be the last Op that modified the key).
func (s *KVServer) Txn(ctx context.Context, req *armadapb.TxnRequest) (*armadapb.TxnResponse, error) {
	if len(req.GetTable()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "table must be set")
	}

	if err := validateTxnKeys(req); err != nil {
		return nil, err
	}

	r, err := s.Storage.Txn(ctx, req)
	if err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			return nil, status.Error(codes.NotFound, "table not found")
		}
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return r, nil
}

func NewForwardingKVServer(storage KVService, client armadapb.KVClient, q *storage.IndexNotificationQueue) *ForwardingKVServer {
	return &ForwardingKVServer{
		KVServer: KVServer{Storage: storage},
		client:   client,
		q:        q,
	}
}

type propagationQueue interface {
	Add(ctx context.Context, table string, revision uint64) <-chan error
}

// ForwardingKVServer forwards the write operations to the leader cluster.
type ForwardingKVServer struct {
	KVServer
	client armadapb.KVClient
	q      propagationQueue
}

// Put implements proto/regatta.proto KV.Put method.
func (r *ForwardingKVServer) Put(ctx context.Context, req *armadapb.PutRequest) (*armadapb.PutResponse, error) {
	put, err := r.client.Put(ctx, req)
	if err != nil {
		if s, ok := status.FromError(err); ok {
			return nil, status.Error(s.Code(), fmt.Sprintf("leader error: %v", s.Err()))
		}
		return nil, status.Error(codes.FailedPrecondition, "forward error")
	}

	return put, <-r.q.Add(ctx, string(req.Table), put.Header.Revision)
}

// DeleteRange implements proto/regatta.proto KV.DeleteRange method.
func (r *ForwardingKVServer) DeleteRange(ctx context.Context, req *armadapb.DeleteRangeRequest) (*armadapb.DeleteRangeResponse, error) {
	del, err := r.client.DeleteRange(ctx, req)
	if err != nil {
		if s, ok := status.FromError(err); ok {
			return nil, status.Error(s.Code(), fmt.Sprintf("leader error: %v", s.Err()))
		}
		return nil, status.Error(codes.FailedPrecondition, "forward error")
	}
	return del, <-r.q.Add(ctx, string(req.Table), del.Header.Revision)
}

// Txn processes multiple requests in a single transaction.
// A txn request increments the revision of the key-value store
// and generates events with the same revision for every completed request.
// It is allowed to modify the same key several times within one txn (the result will be the last Op that modified the key).
// Readonly transactions allowed using follower API.
func (r *ForwardingKVServer) Txn(ctx context.Context, req *armadapb.TxnRequest) (*armadapb.TxnResponse, error) {
	if req.IsReadonly() {
		return r.KVServer.Txn(ctx, req)
	}
	txn, err := r.client.Txn(ctx, req)
	if err != nil {
		if s, ok := status.FromError(err); ok {
			return nil, status.Error(s.Code(), fmt.Sprintf("leader error: %v", s.Err()))
		}
		return nil, status.Error(codes.FailedPrecondition, "forward error")
	}
	return txn, <-r.q.Add(ctx, string(req.Table), txn.Header.Revision)
}

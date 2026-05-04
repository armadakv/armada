// Copyright JAMF Software, LLC

package armadaserver

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/util/iterx"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	table1Name   = []byte("table_1")
	key1Name     = []byte("key_1")
	table1Value1 = []byte("table_1/value_1")
)

func TestKVServer_Range(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Get key")
	storage.On("Range", mock.Anything, &armadapb.RangeRequest{
		Table: table1Name,
		Key:   key1Name,
	}).Return(&armadapb.RangeResponse{}, nil)
	_, err := kv.Range(context.Background(), &armadapb.RangeRequest{
		Table: table1Name,
		Key:   key1Name,
	})

	r.NoError(err)
}

func TestKVServer_RangeError(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Get with empty table name")
	_, err := kv.Range(context.Background(), &armadapb.RangeRequest{
		Table: []byte{},
		Key:   key1Name,
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "table must be set").Error())

	t.Log("Get with empty key name")
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table: table1Name,
		Key:   []byte{},
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "key must be set").Error())

	t.Log("Get with negative limit")
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table: table1Name,
		Key:   key1Name,
		Limit: -1,
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "limit must be a positive number").Error())

	t.Log("Get with both CountOnly and KeysOnly")
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table:     table1Name,
		Key:       key1Name,
		KeysOnly:  true,
		CountOnly: true,
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "keys_only and count_only must not be set at the same time").Error())

	t.Log("Get kv from non-existing table")
	storage = &mockKVService{}
	storage.On("Range", mock.Anything, mock.Anything).Return((*armadapb.RangeResponse)(nil), errors.ErrTableNotFound)
	kv.Storage = storage
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table: []byte("non_existing_table"),
		Key:   key1Name,
	})
	r.EqualError(err, status.Errorf(codes.NotFound, "table not found").Error())

	t.Log("Get unknown error")
	storage = &mockKVService{}
	storage.On("Range", mock.Anything, mock.Anything).Return((*armadapb.RangeResponse)(nil), fmt.Errorf("unknown"))
	kv.Storage = storage
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	})
	r.EqualError(err, status.Errorf(codes.FailedPrecondition, "unknown").Error())

	t.Log("Get retry-safe error")
	storage = &mockKVService{}
	storage.On("Range", mock.Anything, mock.Anything).Return((*armadapb.RangeResponse)(nil), raft.ErrSystemBusy)
	kv.Storage = storage
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	})
	r.EqualError(err, status.Error(codes.Unavailable, raft.ErrSystemBusy.Error()).Error())
}

func TestKVServer_RangeUnimplemented(t *testing.T) {
	r := require.New(t)
	kv := KVServer{
		Storage: &mockKVService{},
	}

	t.Log("Get kv with unimplemented min_mod_revision")
	_, err := kv.Range(context.Background(), &armadapb.RangeRequest{
		Table:          table1Name,
		Key:            key1Name,
		MinModRevision: 1,
	})
	r.EqualError(err, status.Errorf(codes.Unimplemented, "min_mod_revision not implemented").Error())

	t.Log("Get kv with unimplemented max_mod_revision")
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table:          table1Name,
		Key:            key1Name,
		MaxModRevision: 1,
	})
	r.EqualError(err, status.Errorf(codes.Unimplemented, "max_mod_revision not implemented").Error())

	t.Log("Get kv with unimplemented min_create_revision")
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table:             table1Name,
		Key:               key1Name,
		MinCreateRevision: 1,
	})
	r.EqualError(err, status.Errorf(codes.Unimplemented, "min_create_revision not implemented").Error())

	t.Log("Get kv with unimplemented max_create_revision")
	_, err = kv.Range(context.Background(), &armadapb.RangeRequest{
		Table:             table1Name,
		Key:               key1Name,
		MaxCreateRevision: 1,
	})
	r.EqualError(err, status.Errorf(codes.Unimplemented, "max_create_revision not implemented").Error())
}

func TestKVServer_IterateRangeError(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Get with empty table name")
	err := kv.IterateRange(&armadapb.RangeRequest{
		Table: []byte{},
		Key:   key1Name,
	}, nil)
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "table must be set").Error())

	t.Log("Get with empty key name")
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   []byte{},
	}, nil)
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "key must be set").Error())

	t.Log("Get with negative limit")
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   key1Name,
		Limit: -1,
	}, nil)
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "limit must be a positive number").Error())

	t.Log("Get with both CountOnly and KeysOnly")
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table:     table1Name,
		Key:       key1Name,
		KeysOnly:  true,
		CountOnly: true,
	}, nil)
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "keys_only and count_only must not be set at the same time").Error())

	t.Log("Get kv from non-existing table")
	storage = &mockKVService{}
	storage.On("IterateRange", mock.Anything, mock.Anything).Return((iter.Seq[*armadapb.RangeResponse])(nil), errors.ErrTableNotFound)
	kv.Storage = storage
	srv := &mockIterateRangeServer{}
	srv.On("Context").Return(context.Background())
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: []byte("non_existing_table"),
		Key:   key1Name,
	}, srv)
	r.EqualError(err, status.Errorf(codes.NotFound, "table not found").Error())

	t.Log("Get unknown error")
	storage = &mockKVService{}
	storage.On("IterateRange", mock.Anything, mock.Anything).Return((iter.Seq[*armadapb.RangeResponse])(nil), fmt.Errorf("unknown"))
	kv.Storage = storage
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	}, srv)
	r.EqualError(err, status.Errorf(codes.FailedPrecondition, "unknown").Error())

	t.Log("Get unknown send error")
	storage = &mockKVService{}
	storage.On("IterateRange", mock.Anything, mock.Anything).Return(iterx.From(&armadapb.RangeResponse{}), nil)
	kv.Storage = storage

	srv.On("Send", mock.Anything).Return(fmt.Errorf("uknown send error"))
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	}, srv)
	r.EqualError(err, status.Errorf(codes.Internal, "uknown send error").Error())

	t.Log("Get retry-safe error")
	storage = &mockKVService{}
	storage.On("IterateRange", mock.Anything, mock.Anything).Return((iter.Seq[*armadapb.RangeResponse])(nil), raft.ErrSystemBusy)
	kv.Storage = storage
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	}, srv)
	r.EqualError(err, status.Error(codes.Unavailable, raft.ErrSystemBusy.Error()).Error())
}

func TestKVServer_IterateRangeUnimplemented(t *testing.T) {
	r := require.New(t)
	kv := KVServer{
		Storage: &mockKVService{},
	}

	t.Log("Get kv with unimplemented min_mod_revision")
	err := kv.IterateRange(&armadapb.RangeRequest{
		Table:          table1Name,
		Key:            key1Name,
		MinModRevision: 1,
	}, nil)
	r.EqualError(err, status.Errorf(codes.Unimplemented, "min_mod_revision not implemented").Error())

	t.Log("Get kv with unimplemented max_mod_revision")
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table:          table1Name,
		Key:            key1Name,
		MaxModRevision: 1,
	}, nil)
	r.EqualError(err, status.Errorf(codes.Unimplemented, "max_mod_revision not implemented").Error())

	t.Log("Get kv with unimplemented min_create_revision")
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table:             table1Name,
		Key:               key1Name,
		MinCreateRevision: 1,
	}, nil)
	r.EqualError(err, status.Errorf(codes.Unimplemented, "min_create_revision not implemented").Error())

	t.Log("Get kv with unimplemented max_create_revision")
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table:             table1Name,
		Key:               key1Name,
		MaxCreateRevision: 1,
	}, nil)
	r.EqualError(err, status.Errorf(codes.Unimplemented, "max_create_revision not implemented").Error())
}

func TestKVServer_IterateRange(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("IterateRange single message")
	storage.On("IterateRange", mock.Anything, mock.Anything).Return(iterx.From(&armadapb.RangeResponse{}), nil)
	srv := &mockIterateRangeServer{}
	srv.On("Context").Return(context.Background())
	srv.On("Send", mock.AnythingOfType("*armadapb.RangeResponse")).Return(nil).Once()
	err := kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   key1Name,
	}, srv)
	r.NoError(err)

	t.Log("IterateRange multi messages")
	storage.On("IterateRange", mock.Anything, mock.Anything).Return(iterx.From(&armadapb.RangeResponse{}, &armadapb.RangeResponse{}, &armadapb.RangeResponse{}))
	srv = &mockIterateRangeServer{}
	srv.On("Context").Return(context.Background())
	srv.On("Send", mock.AnythingOfType("*armadapb.RangeResponse")).Return(nil).Times(3)
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   key1Name,
	}, srv)
	r.NoError(err)

	t.Log("IterateRange canceled context")
	storage.On("IterateRange", mock.Anything, mock.Anything).Return(iterx.From(&armadapb.RangeResponse{}, &armadapb.RangeResponse{}, &armadapb.RangeResponse{}))
	srv = &mockIterateRangeServer{}
	ctx, cancelFunc := context.WithCancel(context.Background())
	cancelFunc()
	srv.On("Context").Return(ctx)
	srv.On("Send", mock.AnythingOfType("*armadapb.RangeResponse")).Return(nil).Times(0)
	err = kv.IterateRange(&armadapb.RangeRequest{
		Table: table1Name,
		Key:   key1Name,
	}, srv)
	r.NoError(err)
}

func TestKVServer_PutError(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Put with empty table name")
	_, err := kv.Put(context.Background(), &armadapb.PutRequest{
		Table: []byte{},
		Key:   key1Name,
		Value: table1Value1,
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "table must be set").Error())

	t.Log("Put with empty key name")
	_, err = kv.Put(context.Background(), &armadapb.PutRequest{
		Table: table1Name,
		Key:   []byte{},
		Value: table1Value1,
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "key must be set").Error())

	t.Log("Put with non-existing table")
	storage.On("Put", mock.Anything, mock.Anything).Return((*armadapb.PutResponse)(nil), errors.ErrTableNotFound)
	_, err = kv.Put(context.Background(), &armadapb.PutRequest{
		Table: []byte("non_existing_table"),
		Key:   key1Name,
		Value: table1Value1,
	})
	r.EqualError(err, status.Errorf(codes.NotFound, "table not found").Error())

	t.Log("Put unknown error")
	storage = &mockKVService{}
	storage.On("Put", mock.Anything, mock.Anything).Return((*armadapb.PutResponse)(nil), fmt.Errorf("unknown"))
	kv.Storage = storage
	_, err = kv.Put(context.Background(), &armadapb.PutRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	})
	r.EqualError(err, status.Errorf(codes.FailedPrecondition, "unknown").Error())

	t.Log("Put retry-safe error")
	storage = &mockKVService{}
	storage.On("Put", mock.Anything, mock.Anything).Return((*armadapb.PutResponse)(nil), raft.ErrSystemBusy)
	kv.Storage = storage
	_, err = kv.Put(context.Background(), &armadapb.PutRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	})
	r.EqualError(err, status.Error(codes.Unavailable, raft.ErrSystemBusy.Error()).Error())
}

func TestKVServer_Put(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Put new kv")
	storage.On("Put", mock.Anything, &armadapb.PutRequest{
		Table: table1Name,
		Key:   key1Name,
		Value: table1Value1,
	}).Return(&armadapb.PutResponse{}, nil)
	_, err := kv.Put(context.Background(), &armadapb.PutRequest{
		Table: table1Name,
		Key:   key1Name,
		Value: table1Value1,
	})
	r.NoError(err)
}

func TestKVServer_DeleteRangeError(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Delete with empty table name")
	_, err := kv.DeleteRange(context.Background(), &armadapb.DeleteRangeRequest{
		Table: []byte{},
		Key:   key1Name,
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "table must be set").Error())

	t.Log("Delete with empty key name")
	_, err = kv.DeleteRange(context.Background(), &armadapb.DeleteRangeRequest{
		Table: table1Name,
		Key:   []byte{},
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "key must be set").Error())

	t.Log("Delete with non-existing table")
	storage.On("Delete", mock.Anything, mock.Anything).Return((*armadapb.DeleteRangeResponse)(nil), errors.ErrTableNotFound)
	_, err = kv.DeleteRange(context.Background(), &armadapb.DeleteRangeRequest{
		Table: []byte("non_existing_table"),
		Key:   key1Name,
	})
	r.EqualError(err, status.Errorf(codes.NotFound, "table not found").Error())

	t.Log("Delete unknown error")
	storage = &mockKVService{}
	storage.On("Delete", mock.Anything, mock.Anything).Return((*armadapb.DeleteRangeResponse)(nil), fmt.Errorf("unknown"))
	kv.Storage = storage
	_, err = kv.DeleteRange(context.Background(), &armadapb.DeleteRangeRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	})
	r.EqualError(err, status.Errorf(codes.FailedPrecondition, "unknown").Error())

	t.Log("Delete retry-safe error")
	storage = &mockKVService{}
	storage.On("Delete", mock.Anything, mock.Anything).Return((*armadapb.DeleteRangeResponse)(nil), raft.ErrSystemBusy)
	kv.Storage = storage
	_, err = kv.DeleteRange(context.Background(), &armadapb.DeleteRangeRequest{
		Table: table1Name,
		Key:   []byte("foo"),
	})
	r.EqualError(err, status.Error(codes.Unavailable, raft.ErrSystemBusy.Error()).Error())
}

func TestKVServer_DeleteRange(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Delete existing kv")
	storage.On("Delete", mock.Anything, &armadapb.DeleteRangeRequest{
		Table: table1Name,
		Key:   key1Name,
	}).Return(&armadapb.DeleteRangeResponse{Deleted: 1}, nil)
	drresp, err := kv.DeleteRange(context.Background(), &armadapb.DeleteRangeRequest{
		Table: table1Name,
		Key:   key1Name,
	})
	r.NoError(err)
	r.Equal(int64(1), drresp.GetDeleted())
}

func TestKVServer_TxnError(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Txn with empty table name")
	_, err := kv.Txn(context.Background(), &armadapb.TxnRequest{
		Table: []byte{},
	})
	r.EqualError(err, status.Errorf(codes.InvalidArgument, "table must be set").Error())

	t.Log("Txn with non-existing table")
	storage.On("Txn", mock.Anything, mock.Anything).Return((*armadapb.TxnResponse)(nil), errors.ErrTableNotFound)
	_, err = kv.Txn(context.Background(), &armadapb.TxnRequest{
		Table: []byte("non_existing_table"),
	})
	r.EqualError(err, status.Errorf(codes.NotFound, "table not found").Error())

	t.Log("Txn unknown error")
	storage = &mockKVService{}
	storage.On("Txn", mock.Anything, mock.Anything).Return((*armadapb.TxnResponse)(nil), fmt.Errorf("unknown"))
	kv.Storage = storage
	_, err = kv.Txn(context.Background(), &armadapb.TxnRequest{
		Table: table1Name,
	})
	r.EqualError(err, status.Errorf(codes.FailedPrecondition, "unknown").Error())

	t.Log("Txn retry-safe error")
	storage = &mockKVService{}
	storage.On("Txn", mock.Anything, mock.Anything).Return((*armadapb.TxnResponse)(nil), raft.ErrSystemBusy)
	kv.Storage = storage
	_, err = kv.Txn(context.Background(), &armadapb.TxnRequest{
		Table: table1Name,
	})
	r.EqualError(err, status.Error(codes.Unavailable, raft.ErrSystemBusy.Error()).Error())
}

func TestKVServer_Txn(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	kv := KVServer{
		Storage: storage,
	}

	t.Log("Txn with correct params")
	storage.On("Txn", mock.Anything, &armadapb.TxnRequest{
		Table: table1Name,
		Success: []*armadapb.RequestOp{
			{
				Request: &armadapb.RequestOp_RequestRange{
					RequestRange: &armadapb.RequestOp_Range{},
				},
			},
		},
	}).Return(&armadapb.TxnResponse{}, nil)
	_, err := kv.Txn(context.Background(), &armadapb.TxnRequest{
		Table: table1Name,
		Success: []*armadapb.RequestOp{
			{
				Request: &armadapb.RequestOp_RequestRange{
					RequestRange: &armadapb.RequestOp_Range{},
				},
			},
		},
	})
	r.NoError(err)
}

func TestForwardingKVServer_Put(t *testing.T) {
	r := require.New(t)
	client := &mockClient{}
	kv := ForwardingKVServer{
		KVServer: KVServer{
			Storage: &mockKVService{},
		},
		client: client,
		q:      fakeQueue{},
	}
	ctx := context.Background()
	req := &armadapb.PutRequest{
		Table: table1Name,
		Key:   key1Name,
	}
	client.On("Put", ctx, req, mock.Anything).Return(&armadapb.PutResponse{Header: &armadapb.ResponseHeader{Revision: 1}}, nil)
	t.Log("Put kv")
	resp, err := kv.Put(ctx, req)
	r.NoError(err)
	r.Equal(uint64(1), resp.Header.Revision)
}

func TestForwardingKVServer_DeleteRange(t *testing.T) {
	r := require.New(t)
	client := &mockClient{}
	kv := ForwardingKVServer{
		KVServer: KVServer{
			Storage: &mockKVService{},
		},
		client: client,
		q:      fakeQueue{},
	}
	ctx := context.Background()
	req := &armadapb.DeleteRangeRequest{
		Table: table1Name,
		Key:   key1Name,
	}
	client.On("DeleteRange", ctx, req, mock.Anything).Return(&armadapb.DeleteRangeResponse{Header: &armadapb.ResponseHeader{Revision: 1}}, nil)
	t.Log("Delete existing kv")
	resp, err := kv.DeleteRange(ctx, req)
	r.NoError(err)
	r.Equal(uint64(1), resp.Header.Revision)
}

func TestForwardingKVServer_Txn(t *testing.T) {
	r := require.New(t)
	storage := &mockKVService{}
	client := &mockClient{}
	kv := ForwardingKVServer{
		KVServer: KVServer{
			Storage: storage,
		},
		client: client,
		q:      fakeQueue{},
	}

	ctx := context.Background()
	req := &armadapb.TxnRequest{
		Success: []*armadapb.RequestOp{
			{
				Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{
					Key: key1Name,
				}},
			},
		},
	}
	t.Log("Writable Txn")
	client.On("Txn", ctx, req, mock.Anything).Return(&armadapb.TxnResponse{Header: &armadapb.ResponseHeader{Revision: 1}}, nil)
	resp, err := kv.Txn(ctx, req)
	r.NoError(err)
	r.Equal(uint64(1), resp.Header.Revision)

	req = &armadapb.TxnRequest{
		Table: table1Name,
		Success: []*armadapb.RequestOp{
			{
				Request: &armadapb.RequestOp_RequestRange{RequestRange: &armadapb.RequestOp_Range{
					Key: key1Name,
				}},
			},
		},
	}
	ctx = context.Background()
	storage.On("Txn", ctx, req).Return(&armadapb.TxnResponse{}, nil)
	t.Log("Readonly Txn")
	_, err = kv.Txn(ctx, req)
	r.NoError(err)
}

// mockKVService implements trivial storage for testing purposes.
type mockKVService struct {
	mock.Mock
}

func (s *mockKVService) Range(ctx context.Context, req *armadapb.RangeRequest) (*armadapb.RangeResponse, error) {
	called := s.Called(ctx, req)
	return called.Get(0).(*armadapb.RangeResponse), called.Error(1)
}

func (s *mockKVService) IterateRange(ctx context.Context, req *armadapb.RangeRequest) (iter.Seq[*armadapb.RangeResponse], error) {
	called := s.Called(ctx, req)
	return called.Get(0).(iter.Seq[*armadapb.RangeResponse]), called.Error(1)
}

func (s *mockKVService) Put(ctx context.Context, req *armadapb.PutRequest) (*armadapb.PutResponse, error) {
	called := s.Called(ctx, req)
	return called.Get(0).(*armadapb.PutResponse), called.Error(1)
}

func (s *mockKVService) Delete(ctx context.Context, req *armadapb.DeleteRangeRequest) (*armadapb.DeleteRangeResponse, error) {
	called := s.Called(ctx, req)
	return called.Get(0).(*armadapb.DeleteRangeResponse), called.Error(1)
}

func (s *mockKVService) Txn(ctx context.Context, req *armadapb.TxnRequest) (*armadapb.TxnResponse, error) {
	called := s.Called(ctx, req)
	return called.Get(0).(*armadapb.TxnResponse), called.Error(1)
}

type mockIterateRangeServer struct {
	mock.Mock
}

func (m *mockIterateRangeServer) Send(response *armadapb.RangeResponse) error {
	return m.Mock.Called(response).Error(0)
}

func (m *mockIterateRangeServer) SetHeader(md metadata.MD) error {
	return m.Mock.Called(md).Error(0)
}

func (m *mockIterateRangeServer) SendHeader(md metadata.MD) error {
	return m.Mock.Called(md).Error(0)
}

func (m *mockIterateRangeServer) SetTrailer(md metadata.MD) {
	m.Called(md)
}

func (m *mockIterateRangeServer) Context() context.Context {
	return m.Mock.Called().Get(0).(context.Context)
}

func (m *mockIterateRangeServer) SendMsg(mes any) error {
	return m.Mock.Called(mes).Error(0)
}

func (m *mockIterateRangeServer) RecvMsg(mes any) error {
	return m.Mock.Called(mes).Error(0)
}

type mockClient struct {
	mock.Mock
}

func (m *mockClient) Range(ctx context.Context, in *armadapb.RangeRequest, opts ...grpc.CallOption) (*armadapb.RangeResponse, error) {
	called := m.Called(ctx, in, opts)
	return called.Get(0).(*armadapb.RangeResponse), called.Error(1)
}

func (m *mockClient) IterateRange(ctx context.Context, in *armadapb.RangeRequest, opts ...grpc.CallOption) (armadapb.KV_IterateRangeClient, error) {
	called := m.Called(ctx, in, opts)
	return called.Get(0).(armadapb.KV_IterateRangeClient), called.Error(1)
}

func (m *mockClient) Put(ctx context.Context, in *armadapb.PutRequest, opts ...grpc.CallOption) (*armadapb.PutResponse, error) {
	called := m.Called(ctx, in, opts)
	return called.Get(0).(*armadapb.PutResponse), called.Error(1)
}

func (m *mockClient) DeleteRange(ctx context.Context, in *armadapb.DeleteRangeRequest, opts ...grpc.CallOption) (*armadapb.DeleteRangeResponse, error) {
	called := m.Called(ctx, in, opts)
	return called.Get(0).(*armadapb.DeleteRangeResponse), called.Error(1)
}

func (m *mockClient) Txn(ctx context.Context, in *armadapb.TxnRequest, opts ...grpc.CallOption) (*armadapb.TxnResponse, error) {
	called := m.Called(ctx, in, opts)
	return called.Get(0).(*armadapb.TxnResponse), called.Error(1)
}

type fakeQueue struct{}

func (f fakeQueue) Add(ctx context.Context, table string, revision uint64) <-chan error {
	i := make(chan error)
	close(i)
	return i
}

// Copyright JAMF Software, LLC

package regattaserver

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/armadakv/armada/regattapb"
	serrors "github.com/armadakv/armada/storage/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type TablesServer struct {
	regattapb.UnimplementedTablesServer
	Tables   TableService
	AuthFunc func(ctx context.Context) (context.Context, error)
}

func (t *TablesServer) Create(ctx context.Context, req *regattapb.CreateTableRequest) (*regattapb.CreateTableResponse, error) {
	if len(req.Name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "name must be set")
	}
	table, err := t.Tables.CreateTable(req.Name)
	if err != nil {
		if errors.Is(err, serrors.ErrTableExists) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &regattapb.CreateTableResponse{Id: strconv.FormatUint(table.ClusterID, 10)}, nil
}

func (t *TablesServer) Delete(ctx context.Context, req *regattapb.DeleteTableRequest) (*regattapb.DeleteTableResponse, error) {
	if len(req.Name) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "name must be set")
	}
	if err := t.Tables.DeleteTable(req.Name); err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &regattapb.DeleteTableResponse{}, nil
}

func (t *TablesServer) List(ctx context.Context, _ *regattapb.ListTablesRequest) (*regattapb.ListTablesResponse, error) {
	ts, err := t.Tables.GetTables()
	if err != nil {
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	resp := &regattapb.ListTablesResponse{Tables: make([]*regattapb.TableInfo, len(ts))}
	for i, table := range ts {
		resp.Tables[i] = &regattapb.TableInfo{
			Name: table.Name,
			Id:   strconv.FormatUint(table.ClusterID, 10),
		}
	}
	slices.SortFunc(resp.Tables, func(a, b *regattapb.TableInfo) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return resp, nil
}

func (t *TablesServer) AuthFuncOverride(ctx context.Context, _ string) (context.Context, error) {
	return t.AuthFunc(ctx)
}

type ReadonlyTablesServer struct {
	TablesServer
}

func (t *ReadonlyTablesServer) Create(context.Context, *regattapb.CreateTableRequest) (*regattapb.CreateTableResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Create not implemented for follower")
}

func (t *ReadonlyTablesServer) Delete(context.Context, *regattapb.DeleteTableRequest) (*regattapb.DeleteTableResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Delete not implemented for follower")
}

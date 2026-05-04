// Copyright JAMF Software, LLC

package armadaserver

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/armadakv/armada/armadapb"
	serrors "github.com/armadakv/armada/storage/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type TablesServer struct {
	armadapb.UnimplementedTablesServer
	Tables   TableService
	AuthFunc func(ctx context.Context) (context.Context, error)
}

func (t *TablesServer) Create(ctx context.Context, req *armadapb.CreateTableRequest) (*armadapb.CreateTableResponse, error) {
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
	return &armadapb.CreateTableResponse{Id: strconv.FormatUint(table.ClusterID, 10)}, nil
}

func (t *TablesServer) Delete(ctx context.Context, req *armadapb.DeleteTableRequest) (*armadapb.DeleteTableResponse, error) {
	if len(req.Name) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "name must be set")
	}
	if err := t.Tables.DeleteTable(req.Name); err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &armadapb.DeleteTableResponse{}, nil
}

func (t *TablesServer) List(ctx context.Context, _ *armadapb.ListTablesRequest) (*armadapb.ListTablesResponse, error) {
	ts, err := t.Tables.GetTables()
	if err != nil {
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	resp := &armadapb.ListTablesResponse{Tables: make([]*armadapb.TableInfo, len(ts))}
	for i, table := range ts {
		resp.Tables[i] = &armadapb.TableInfo{
			Name: table.Name,
			Id:   strconv.FormatUint(table.ClusterID, 10),
		}
	}
	slices.SortFunc(resp.Tables, func(a, b *armadapb.TableInfo) int {
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

func (t *ReadonlyTablesServer) Create(context.Context, *armadapb.CreateTableRequest) (*armadapb.CreateTableResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Create not implemented for follower")
}

func (t *ReadonlyTablesServer) Delete(context.Context, *armadapb.DeleteTableRequest) (*armadapb.DeleteTableResponse, error) {
	return nil, status.Error(codes.Unimplemented, "method Delete not implemented for follower")
}

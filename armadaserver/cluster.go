// Copyright JAMF Software, LLC

package armadaserver

import (
	"context"
	"encoding/json"

	"github.com/armadakv/armada/armadapb"
	serrors "github.com/armadakv/armada/storage/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type ClusterServer struct {
	armadapb.UnimplementedClusterServer
	Cluster ClusterService
	Config  ConfigService
}

func (c *ClusterServer) MemberList(ctx context.Context, req *armadapb.MemberListRequest) (*armadapb.MemberListResponse, error) {
	res, err := c.Cluster.MemberList(ctx, req)
	if err != nil {
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return res, nil
}

func (c *ClusterServer) Status(ctx context.Context, req *armadapb.StatusRequest) (*armadapb.StatusResponse, error) {
	res, err := c.Cluster.Status(ctx, req)
	if req.Config {
		cfg := c.Config()
		b, err := json.Marshal(cfg)
		if err != nil {
			return nil, err
		}
		st := structpb.Struct{}
		if err = json.Unmarshal(b, &st); err != nil {
			return nil, err
		}
		res.Config = &st
	}
	if err != nil {
		if serrors.IsSafeToRetry(err) {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return res, nil
}

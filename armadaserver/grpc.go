// Copyright JAMF Software, LLC

package armadaserver

import (
	"net"
	"time"

	_ "github.com/armadakv/armada/armadaserver/encoding/gzip"
	_ "github.com/armadakv/armada/armadaserver/encoding/proto"
	_ "github.com/armadakv/armada/armadaserver/encoding/snappy"
	_ "github.com/armadakv/armada/armadaserver/encoding/zstd"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

// defaultOpts default GRPC server options
// * Allow for earlier keepalives.
// * Allow keepalives without stream.
// * Allow for handlers to return before shutting down.
var defaultOpts = []grpc.ServerOption{
	grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 30 * time.Second, PermitWithoutStream: true}),
	grpc.WaitForHandlers(true),
}

// Server is server where gRPC services can be registered in.
type Server struct {
	*grpc.Server
	listener net.Listener
	log      *zap.SugaredLogger
}

// NewServer returns initialized gRPC server.
func NewServer(l net.Listener, logger *zap.SugaredLogger, opts ...grpc.ServerOption) *Server {
	rs := new(Server)
	rs.listener = l
	rs.log = logger
	rs.Server = grpc.NewServer(append(defaultOpts, opts...)...)
	reflection.Register(rs.Server)
	return rs
}

func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Serve starts underlying gRPC server.
func (s *Server) Serve() error {
	s.log.Infof("serve gRPC on: %s", s.listener.Addr())
	return s.Server.Serve(s.listener)
}

// Shutdown stops underlying gRPC server.
func (s *Server) Shutdown() {
	s.log.Infof("stopping gRPC on: %s", s.Addr())
	s.GracefulStop()
	s.Stop()
	s.log.Infof("stopped gRPC on: %s", s.Addr())
}

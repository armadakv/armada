package armadaserver

import (
	"context"
	"net"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// MixedServer serves both gRPC and plain HTTP handlers on the same listener.
type MixedServer struct {
	grpcServer *Server
	httpServer *http.Server
	listener   net.Listener
	log        *zap.SugaredLogger
}

// NewMixedServer constructs a server that multiplexes gRPC and HTTP traffic
// over the same listener.
func NewMixedServer(listener net.Listener, grpcServer *Server, httpHandler http.Handler, logger *zap.SugaredLogger) *MixedServer {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		httpHandler.ServeHTTP(w, r)
	})
	return &MixedServer{
		grpcServer: grpcServer,
		httpServer: &http.Server{
			Handler:  handler,
			ErrorLog: zap.NewStdLog(logger.Desugar()),
		},
		listener: listener,
		log:      logger,
	}
}

// Serve starts serving mixed traffic.
func (s *MixedServer) Serve() error {
	s.log.Infof("serve replication HTTP+gRPC on: %s", s.listener.Addr())
	return s.httpServer.Serve(s.listener)
}

// Shutdown stops both HTTP and gRPC servers.
func (s *MixedServer) Shutdown() {
	s.log.Infof("stopping replication HTTP+gRPC on: %s", s.listener.Addr())
	_ = s.httpServer.Shutdown(context.Background())
	s.grpcServer.Shutdown()
	s.log.Infof("stopped replication HTTP+gRPC on: %s", s.listener.Addr())
}

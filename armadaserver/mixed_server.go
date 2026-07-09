// Copyright Armada Contributors

package armadaserver

import (
	"context"
	"net"
	"net/http"
	"time"

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
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           grpcHandlerFunc(grpcServer.Server, httpHandler),
		ErrorLog:          zap.NewStdLog(logger.Desugar()),
	}
	// Enable cleartext HTTP/2 (h2c / prior-knowledge) so that gRPC clients
	// connecting over plain TCP (non-TLS replication addresses) work correctly.
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Protocols = &p
	return &MixedServer{
		grpcServer: grpcServer,
		httpServer: srv,
		listener:   listener,
		log:        logger,
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
	s.grpcServer.Stop()
	s.log.Infof("stopped replication HTTP+gRPC on: %s", s.listener.Addr())
}

// Copyright JAMF Software, LLC

package quictransport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	sharedIdleTimeout = 30 * time.Second
	sharedKeepAlive   = 10 * time.Second
)

// Listener is the minimal interface for accepting QUIC connections.
// Both *quic.Listener (non-shared path) and *ALPNListener (shared path) satisfy it.
type Listener interface {
	Accept(ctx context.Context) (*quic.Conn, error)
	Close() error
}

// ALPNListener is a channel-backed Listener that receives connections routed
// by Shared's accept loop for a specific ALPN protocol.
type ALPNListener struct {
	ch       chan *quic.Conn
	closedCh chan struct{}
	once     sync.Once
}

func newALPNListener() *ALPNListener {
	return &ALPNListener{
		ch:       make(chan *quic.Conn, 16),
		closedCh: make(chan struct{}),
	}
}

// Accept blocks until a new connection for this ALPN arrives, the listener is
// closed, or ctx is cancelled.
func (l *ALPNListener) Accept(ctx context.Context) (*quic.Conn, error) {
	select {
	case conn, ok := <-l.ch:
		if !ok {
			return nil, errors.New("listener closed")
		}
		return conn, nil
	case <-l.closedCh:
		return nil, errors.New("listener closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close signals that no more connections will be consumed from this listener.
func (l *ALPNListener) Close() error {
	l.once.Do(func() { close(l.closedCh) })
	return nil
}

// Shared owns a single UDP socket and a *quic.Transport. It starts exactly one
// QUIC listener whose TLS config is built from all registered ALPNs. Incoming
// connections are routed to per-ALPN ALPNListeners by the negotiated protocol.
//
// Usage:
//
//	s, _ := New(addr, bufSize)
//	raftL, _ := s.ListenALPN(raftALPN, raftServerTLS)   // register before Serve
//	gossipL, _ := s.ListenALPN(gossipALPN, gossipServerTLS)
//	s.Serve()   // start the single listener — call after all ListenALPN calls
//	// ... use raftL and gossipL ...
//	s.Close()   // stop everything
type Shared struct {
	// Transport is the underlying *quic.Transport. Use it for dialling only;
	// do not call Transport.Listen — Serve handles that.
	Transport *quic.Transport
	conn      *net.UDPConn

	mu       sync.Mutex
	alpns    map[string]*ALPNListener
	tlsConfs map[string]*tls.Config
	ql       *quic.Listener
	served   bool
	cancel   context.CancelFunc
	done     chan struct{}
}

// New creates a new Shared transport bound to listenAddr.
// When bufferSize > 0 the UDP receive/send buffers are capped to that value.
func New(listenAddr string, bufferSize int) (*Shared, error) {
	pc, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return nil, err
	}
	udpConn := pc.(*net.UDPConn)
	if bufferSize > 0 {
		_ = udpConn.SetReadBuffer(bufferSize)
		_ = udpConn.SetWriteBuffer(bufferSize)
	}
	return &Shared{
		Transport: &quic.Transport{Conn: udpConn},
		conn:      udpConn,
		alpns:     make(map[string]*ALPNListener),
		tlsConfs:  make(map[string]*tls.Config),
		done:      make(chan struct{}),
	}, nil
}

// ListenALPN registers serverTLS for alpn and returns an ALPNListener that
// will receive connections negotiated with that protocol. Must be called before
// Serve; serverTLS must contain a certificate.
func (s *Shared) ListenALPN(alpn string, serverTLS *tls.Config) (*ALPNListener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.served {
		return nil, errors.New("quictransport: ListenALPN called after Serve")
	}
	cloned := serverTLS.Clone()
	cloned.NextProtos = []string{alpn}
	s.tlsConfs[alpn] = cloned
	l := newALPNListener()
	s.alpns[alpn] = l
	return l, nil
}

// Serve starts the shared QUIC listener and begins routing incoming connections
// to the ALPNListeners registered via ListenALPN. It must be called after all
// ListenALPN registrations and only once.
func (s *Shared) Serve() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.served {
		return errors.New("quictransport: Serve already called")
	}

	allALPNs := make([]string, 0, len(s.tlsConfs))
	for alpn := range s.tlsConfs {
		allALPNs = append(allALPNs, alpn)
	}
	tlsConfs := s.tlsConfs
	combinedTLS := &tls.Config{
		NextProtos: allALPNs,
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			for _, proto := range info.SupportedProtos {
				if cfg, ok := tlsConfs[proto]; ok {
					return cfg, nil
				}
			}
			return nil, nil
		},
	}

	ql, err := s.Transport.Listen(combinedTLS, &quic.Config{
		MaxIdleTimeout:  sharedIdleTimeout,
		KeepAlivePeriod: sharedKeepAlive,
	})
	if err != nil {
		return err
	}
	s.ql = ql

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.served = true

	go s.acceptLoop(ctx)
	return nil
}

func (s *Shared) acceptLoop(ctx context.Context) {
	defer close(s.done)
	for {
		conn, err := s.ql.Accept(ctx)
		if err != nil {
			return
		}
		alpn := conn.ConnectionState().TLS.NegotiatedProtocol
		s.mu.Lock()
		al, ok := s.alpns[alpn]
		s.mu.Unlock()
		if !ok {
			_ = conn.CloseWithError(0, "unknown ALPN")
			continue
		}
		select {
		case al.ch <- conn:
		case <-al.closedCh:
			_ = conn.CloseWithError(0, "listener closed")
		case <-ctx.Done():
			_ = conn.CloseWithError(0, "shutting down")
			return
		}
	}
}

// UDPConn returns the underlying *net.UDPConn. Callers can use it to apply
// OS-level buffer-size tuning (e.g. the capped-buffer wrapper in the raft
// transport).
func (s *Shared) UDPConn() *net.UDPConn { return s.conn }

// Close tears down the shared listener, stops the accept loop, closes the
// quic.Transport, and releases the UDP port. Call this only after all
// ALPNListeners and dialers that reference this transport have been stopped.
func (s *Shared) Close() error {
	if s.cancel != nil {
		s.cancel()
		if s.ql != nil {
			_ = s.ql.Close()
		}
		<-s.done
	}
	if err := s.Transport.Close(); err != nil {
		return err
	}
	return s.conn.Close()
}

// Copyright JAMF Software, LLC

package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/armadakv/armada/internal/quictransport"
	"github.com/hashicorp/memberlist"
	"github.com/quic-go/quic-go"
)

const (
	// gossipALPN is the ALPN identifier for the armada gossip QUIC protocol.
	gossipALPN = "armada-gossip-1"

	// Stream-type header bytes written at the start of every QUIC stream so
	// the receiving side knows how to interpret the payload.
	gossipPacketStreamType byte = 0x01
	gossipRelayStreamType  byte = 0x02

	// maxGossipFrameSize caps incoming gossip payloads.  4 MiB is far more
	// than any realistic memberlist message.
	maxGossipFrameSize = 4 * 1024 * 1024

	gossipDialTimeout  = 5 * time.Second
	gossipIdleTimeout  = 30 * time.Second
	gossipKeepAlive    = 10 * time.Second
	gossipPacketChanSz = 256
	gossipStreamChanSz = 64
)

// Compile-time check that QUICTransport implements memberlist.Transport.
var _ memberlist.Transport = (*QUICTransport)(nil)

// QUICTransport implements memberlist.Transport using QUIC.
//
// Both gossip probes/pushes (packet channel) and anti-entropy (stream channel)
// are multiplexed over QUIC streams on a single shared UDP socket.
// A one-byte header distinguishes packet streams (0x01) from relay streams (0x02).
//
// Outbound connections are pooled by peer address to avoid repeated TLS
// handshakes for high-frequency gossip operations.
type QUICTransport struct {
	advAddr   string
	serverTLS *tls.Config
	clientTLS *tls.Config

	quicTransport *quic.Transport
	udpConn       net.PacketConn
	listener      quictransport.Listener

	packetCh chan *memberlist.Packet
	streamCh chan net.Conn

	// connPool caches outbound QUIC connections keyed by remote "host:port".
	connMu   sync.Mutex
	connPool map[string]*quic.Conn

	// activeConns tracks all server-side accepted connections so that
	// Shutdown() can send graceful CONNECTION_CLOSE to each peer.
	activeConns sync.Map // *quic.Conn → struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// ownsTransport is true when this QUICTransport created the quicTransport
	// itself. Only the owner closes it and the underlying UDP socket.
	ownsTransport bool
}

// NewQUICTransport creates a QUIC transport that listens on advAddr.
//
// When shared is non-nil, the transport reuses the provided *quic.Transport
// (and its underlying UDP socket) instead of creating a new one.  The caller
// retains ownership of the shared transport and must close it after this
// transport has been shut down.
//
// When serverTLS / clientTLS are nil the transport generates a self-signed
// certificate so that all traffic is encrypted (though peer identity is not
// verified).  Pass non-nil configs for mutual TLS between cluster members.
func NewQUICTransport(advAddr string, serverTLS, clientTLS *tls.Config, shared *quictransport.Shared) (*QUICTransport, error) {
	if serverTLS == nil {
		var err error
		serverTLS, err = gossipSelfSignedTLSConfig()
		if err != nil {
			return nil, err
		}
	} else {
		serverTLS = serverTLS.Clone()
		serverTLS.NextProtos = append(serverTLS.NextProtos, gossipALPN)
	}

	if clientTLS == nil {
		clientTLS = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // lgtm[go/disabled-certificate-check]
			NextProtos:         []string{gossipALPN},
		}
	} else {
		clientTLS = clientTLS.Clone()
		clientTLS.NextProtos = append(clientTLS.NextProtos, gossipALPN)
	}

	var qt *quic.Transport
	var udpConn net.PacketConn
	ownsTransport := false
	var ln quictransport.Listener

	if shared != nil {
		qt = shared.Transport
		// Register gossip ALPN; Shared.Serve() starts the actual listener later.
		alpnListener, err := shared.ListenALPN(gossipALPN, serverTLS)
		if err != nil {
			return nil, err
		}
		ln = alpnListener
	} else {
		pc, err := net.ListenPacket("udp", advAddr)
		if err != nil {
			return nil, err
		}
		qt = &quic.Transport{Conn: pc}
		udpConn = pc
		ownsTransport = true
		l, err := qt.Listen(serverTLS, &quic.Config{
			MaxIdleTimeout:  gossipIdleTimeout,
			KeepAlivePeriod: gossipKeepAlive,
		})
		if err != nil {
			_ = udpConn.Close()
			return nil, err
		}
		ln = l
	}

	ctx, cancel := context.WithCancel(context.Background())
	t := &QUICTransport{
		advAddr:       advAddr,
		serverTLS:     serverTLS,
		clientTLS:     clientTLS,
		quicTransport: qt,
		udpConn:       udpConn,
		listener:      ln,
		packetCh:      make(chan *memberlist.Packet, gossipPacketChanSz),
		streamCh:      make(chan net.Conn, gossipStreamChanSz),
		connPool:      make(map[string]*quic.Conn),
		ctx:           ctx,
		cancel:        cancel,
		ownsTransport: ownsTransport,
	}

	t.wg.Add(1)
	go t.acceptLoop()

	return t, nil
}

// -----------------------------------------------------------------------
// memberlist.Transport implementation
// -----------------------------------------------------------------------

// FinalAdvertiseAddr returns the IP and port that this node should advertise
// to the rest of the cluster.
//
// The transport always uses its own bound address and port.  The ip/port
// arguments (from the memberlist config's AdvertiseAddr/AdvertisePort fields)
// are only honoured when both are non-zero so that an operator can explicitly
// override the advertised endpoint.  memberlist's DefaultLANConfig hard-codes
// AdvertisePort: 7946 even when a custom transport is provided; that default
// must not override the transport's actual port, which is why we only accept
// the override when ip is also explicitly set.
func (t *QUICTransport) FinalAdvertiseAddr(ip string, port int) (net.IP, int, error) {
	if ip != "" && port != 0 {
		return net.ParseIP(ip), port, nil
	}

	h, p, err := net.SplitHostPort(t.advAddr)
	if err != nil {
		return nil, 0, err
	}
	parsedPort, _ := strconv.Atoi(p)

	parsedIP := net.ParseIP(h)
	if parsedIP != nil && !parsedIP.IsUnspecified() {
		if ip != "" {
			parsedIP = net.ParseIP(ip)
		}
		return parsedIP, parsedPort, nil
	}

	// Unspecified (0.0.0.0) — walk interfaces to find a routable IPv4 address.
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, 0, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var candidate net.IP
			switch v := a.(type) {
			case *net.IPNet:
				candidate = v.IP
			case *net.IPAddr:
				candidate = v.IP
			}
			if candidate != nil && candidate.To4() != nil && !candidate.IsLoopback() {
				return candidate, parsedPort, nil
			}
		}
	}

	return parsedIP, parsedPort, nil
}

// WriteTo sends a gossip packet (probe / push-pull) to addr.
// It reuses a pooled QUIC connection to the peer when available.
func (t *QUICTransport) WriteTo(b []byte, addr string) (time.Time, error) {
	conn, err := t.pooledDial(addr)
	if err != nil {
		return time.Time{}, err
	}

	stream, err := conn.OpenStreamSync(t.ctx)
	if err != nil {
		t.evictConn(addr)
		return time.Time{}, err
	}
	defer stream.CancelRead(0)

	// Header: type byte + 4-byte big-endian payload length.
	hdr := make([]byte, 1+4)
	hdr[0] = gossipPacketStreamType
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(b)))

	sent := time.Now()
	if _, err := stream.Write(append(hdr, b...)); err != nil {
		t.evictConn(addr)
		return time.Time{}, err
	}
	_ = stream.Close()
	return sent, nil
}

// PacketCh returns the channel on which incoming gossip packets are delivered.
func (t *QUICTransport) PacketCh() <-chan *memberlist.Packet { return t.packetCh }

// DialTimeout opens a reliable stream connection to addr (used for anti-entropy).
// A fresh QUIC connection is dialled for each stream so that memberlist can
// close the connection independently.
func (t *QUICTransport) DialTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(t.ctx, timeout)
	defer cancel()

	conn, err := t.quicTransport.Dial(ctx, udpAddr, t.clientTLS, &quic.Config{
		MaxIdleTimeout:  gossipIdleTimeout,
		KeepAlivePeriod: gossipKeepAlive,
	})
	if err != nil {
		return nil, err
	}

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}

	// Tell the remote side this is a relay stream.
	if _, err := stream.Write([]byte{gossipRelayStreamType}); err != nil {
		stream.CancelWrite(0)
		_ = conn.CloseWithError(0, "")
		return nil, err
	}

	// The caller (memberlist) is responsible for closing the returned net.Conn.
	// Closing the quicStreamConn sends STREAM_FIN; the connection itself remains
	// alive until idle so the server can still read any buffered stream data.
	return &quicStreamConn{conn: conn, stream: stream, closeConn: false}, nil
}

// StreamCh returns the channel on which incoming relay streams are delivered.
func (t *QUICTransport) StreamCh() <-chan net.Conn { return t.streamCh }

// Shutdown stops the transport and releases all resources.
func (t *QUICTransport) Shutdown() error {
	t.cancel()
	// Close all active server-side connections gracefully before closing the listener.
	t.activeConns.Range(func(k, _ any) bool {
		_ = k.(*quic.Conn).CloseWithError(0, "")
		return true
	})
	if t.listener != nil {
		_ = t.listener.Close()
	}
	// Only close the quicTransport and UDP socket if we own them.
	if t.ownsTransport && t.quicTransport != nil {
		_ = t.quicTransport.Close()
	}
	t.wg.Wait()
	if t.ownsTransport && t.udpConn != nil {
		_ = t.udpConn.Close()
	}
	return nil
}

// -----------------------------------------------------------------------
// Accept loop
// -----------------------------------------------------------------------

func (t *QUICTransport) acceptLoop() {
	defer t.wg.Done()
	for {
		conn, err := t.listener.Accept(t.ctx)
		if err != nil {
			return
		}
		t.wg.Add(1)
		go t.serveConn(conn)
	}
}

func (t *QUICTransport) serveConn(conn *quic.Conn) {
	defer t.wg.Done()
	t.activeConns.Store(conn, struct{}{})
	defer t.activeConns.Delete(conn)
	for {
		stream, err := conn.AcceptStream(t.ctx)
		if err != nil {
			return
		}
		t.wg.Add(1)
		go func(s *quic.Stream) {
			defer t.wg.Done()
			t.serveStream(conn, s)
		}(stream)
	}
}

func (t *QUICTransport) serveStream(conn *quic.Conn, stream *quic.Stream) {
	var typeBuf [1]byte
	if _, err := io.ReadFull(stream, typeBuf[:]); err != nil {
		return
	}

	switch typeBuf[0] {
	case gossipPacketStreamType:
		t.servePacketStream(conn, stream)
	case gossipRelayStreamType:
		t.serveRelayStream(conn, stream)
	}
}

// servePacketStream reads a framed gossip packet and pushes it to packetCh.
func (t *QUICTransport) servePacketStream(conn *quic.Conn, stream *quic.Stream) {
	defer stream.CancelRead(0)

	var hdr [4]byte
	if _, err := io.ReadFull(stream, hdr[:]); err != nil {
		return
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size == 0 || size > maxGossipFrameSize {
		return
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(stream, buf); err != nil {
		return
	}

	select {
	case t.packetCh <- &memberlist.Packet{
		Buf:       buf,
		From:      conn.RemoteAddr(),
		Timestamp: time.Now(),
	}:
	case <-t.ctx.Done():
	}
}

// serveRelayStream wraps the stream as a net.Conn and pushes it to streamCh.
// The connection is NOT closed here; the consumer of streamCh is responsible
// for closing the stream only (closeConn: false).
func (t *QUICTransport) serveRelayStream(conn *quic.Conn, stream *quic.Stream) {
	select {
	case t.streamCh <- &quicStreamConn{conn: conn, stream: stream, closeConn: false}:
	case <-t.ctx.Done():
		stream.CancelRead(0)
	}
}

// -----------------------------------------------------------------------
// Connection pool
// -----------------------------------------------------------------------

// pooledDial returns a pooled QUIC connection to addr, creating one if needed.
func (t *QUICTransport) pooledDial(addr string) (*quic.Conn, error) {
	t.connMu.Lock()
	conn, ok := t.connPool[addr]
	t.connMu.Unlock()
	if ok {
		return conn, nil
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(t.ctx, gossipDialTimeout)
	defer cancel()

	conn, err = t.quicTransport.Dial(ctx, udpAddr, t.clientTLS, &quic.Config{
		MaxIdleTimeout:  gossipIdleTimeout,
		KeepAlivePeriod: gossipKeepAlive,
	})
	if err != nil {
		return nil, err
	}

	t.connMu.Lock()
	if existing, ok := t.connPool[addr]; ok {
		// Lost the race; discard our new connection.
		t.connMu.Unlock()
		_ = conn.CloseWithError(0, "")
		return existing, nil
	}
	t.connPool[addr] = conn
	t.connMu.Unlock()

	// Evict the entry when the connection is closed by either side.
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		select {
		case <-conn.Context().Done():
			t.evictConn(addr)
		case <-t.ctx.Done():
		}
	}()

	return conn, nil
}

func (t *QUICTransport) evictConn(addr string) {
	t.connMu.Lock()
	delete(t.connPool, addr)
	t.connMu.Unlock()
}

// -----------------------------------------------------------------------
// quicStreamConn — net.Conn over a QUIC stream
// -----------------------------------------------------------------------

// quicStreamConn wraps a quic.Stream and its parent quic.Conn to expose the
// net.Conn interface required by memberlist relay streams.
//
// When closeConn is true (client-side / DialTimeout), Close() shuts down both
// the stream and the QUIC connection.  When false (server-side / StreamCh),
// only the stream is closed so that serveConn can continue accepting further
// streams on the same connection.
type quicStreamConn struct {
	conn      *quic.Conn
	stream    *quic.Stream
	closeConn bool
}

func (c *quicStreamConn) Read(b []byte) (int, error)  { return c.stream.Read(b) }
func (c *quicStreamConn) Write(b []byte) (int, error) { return c.stream.Write(b) }
func (c *quicStreamConn) Close() error {
	_ = c.stream.Close()
	if c.closeConn {
		return c.conn.CloseWithError(0, "")
	}
	return nil
}
func (c *quicStreamConn) LocalAddr() net.Addr               { return c.conn.LocalAddr() }
func (c *quicStreamConn) RemoteAddr() net.Addr              { return c.conn.RemoteAddr() }
func (c *quicStreamConn) SetDeadline(t time.Time) error     { return c.stream.SetDeadline(t) }
func (c *quicStreamConn) SetReadDeadline(t time.Time) error { return c.stream.SetReadDeadline(t) }
func (c *quicStreamConn) SetWriteDeadline(t time.Time) error {
	return c.stream.SetWriteDeadline(t)
}

// -----------------------------------------------------------------------
// TLS helpers
// -----------------------------------------------------------------------

// gossipSelfSignedTLSConfig generates a fresh ECDSA P-256 self-signed
// certificate used when no explicit TLS config is provided.  Traffic is
// encrypted but peer identity is not verified.
func gossipSelfSignedTLSConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"armada-gossip"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
		}},
		NextProtos: []string{gossipALPN},
	}, nil
}

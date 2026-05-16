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
	"io"
	"math/big"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/armadakv/armada/raft/transport"
	"github.com/hashicorp/memberlist"
	"github.com/quic-go/quic-go"
)

const (
	// gossipALPN is the ALPN identifier for the armada gossip QUIC protocol.
	gossipALPN = "armada-gossip-1"

	// Stream-type header byte written at the start of every relay QUIC stream.
	gossipRelayStreamType byte = 0x02

	// maxGossipDatagramSize caps incoming gossip datagrams. Memberlist probe
	// messages are always small (a few hundred bytes), well under QUIC MTU.
	maxGossipDatagramSize = 65535

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
// Gossip probes and push-notifications (packet channel) are sent as QUIC
// datagrams — true fire-and-forget semantics that map naturally to what
// memberlist expects from its "UDP" path.  Anti-entropy push-pull (stream
// channel) uses QUIC streams for reliable, ordered delivery.  Both paths are
// multiplexed over a single shared UDP socket via ALPN "armada-gossip-1".
//
// Outbound connections are pooled by peer address to avoid repeated TLS
// handshakes for high-frequency gossip operations.
type QUICTransport struct {
	advAddr   string
	serverTLS *tls.Config
	clientTLS *tls.Config

	quicTransport *quic.Transport
	udpConn       net.PacketConn
	listener      transport.Listener

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
func NewQUICTransport(advAddr string, serverTLS, clientTLS *tls.Config, shared *transport.Shared) (*QUICTransport, error) {
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
	var ln transport.Listener

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
			EnableDatagrams: true,
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

// WriteTo sends a gossip packet (probe / push-pull notification) to addr as a
// QUIC datagram.  Datagrams are truly fire-and-forget: no per-message stream
// setup, no flow control — matching the UDP semantics memberlist expects on
// this path.  The underlying QUIC connection is pooled so the TLS handshake
// cost is paid only once per peer.
func (t *QUICTransport) WriteTo(b []byte, addr string) (time.Time, error) {
	conn, err := t.pooledDial(addr)
	if err != nil {
		return time.Time{}, err
	}

	sent := time.Now()
	if err := conn.SendDatagram(b); err != nil {
		// Connection may be dead — evict so the next probe gets a fresh one.
		t.evictConn(addr)
		return time.Time{}, err
	}
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
		EnableDatagrams: true,
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

	// Receive QUIC datagrams (gossip probes / push notifications).
	t.wg.Add(1)
	go t.serveDatagram(conn)

	// Receive QUIC streams (anti-entropy push-pull relay).
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

// serveDatagram receives QUIC datagrams from conn and delivers them to
// packetCh.  This is the "UDP probe" path used by memberlist for pings and
// acks — truly fire-and-forget, no per-message stream overhead.
func (t *QUICTransport) serveDatagram(conn *quic.Conn) {
	defer t.wg.Done()
	for {
		dgram, err := conn.ReceiveDatagram(t.ctx)
		if err != nil {
			return
		}
		if len(dgram) == 0 || len(dgram) > maxGossipDatagramSize {
			continue
		}
		buf := make([]byte, len(dgram))
		copy(buf, dgram)
		select {
		case t.packetCh <- &memberlist.Packet{
			Buf:       buf,
			From:      conn.RemoteAddr(),
			Timestamp: time.Now(),
		}:
		case <-t.ctx.Done():
			return
		}
	}
}

func (t *QUICTransport) serveStream(conn *quic.Conn, stream *quic.Stream) {
	var typeBuf [1]byte
	if _, err := io.ReadFull(stream, typeBuf[:]); err != nil {
		return
	}

	if typeBuf[0] == gossipRelayStreamType {
		t.serveRelayStream(conn, stream)
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
		EnableDatagrams: true,
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

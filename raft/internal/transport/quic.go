// Copyright 2017-2025 Lei Ni (nilei81@gmail.com) and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

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
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/quic-go/quic-go"

	"github.com/armadakv/armada/raft/config"
	"github.com/armadakv/armada/raft/internal/settings"
	"github.com/armadakv/armada/raft/raftio"
	pb "github.com/armadakv/armada/raft/raftpb"
)

const (
	// QUICTransportName is the name of the QUIC transport module.
	QUICTransportName = "go-quic-transport"

	// quicALPN is the ALPN identifier for the armada raft QUIC protocol.
	quicALPN = "armada-raft-1"

	// Stream type indicators — written as the first byte of every stream so the
	// receiver knows how to interpret the frames that follow.
	raftStreamType     byte = 0x01
	snapshotStreamType byte = 0x02

	// maxFrameSize caps incoming frames at 128 MiB to prevent runaway
	// allocations while still accommodating the largest possible raft message
	// batch (settings.MaxMessageBatchSize = settings.LargeEntitySize = 64 MiB)
	// and snapshot chunks (settings.SnapshotChunkSize = 2 MiB).
	maxFrameSize = 128 * 1024 * 1024
)

var (
	quicMaxIdleTimeout  = 30 * time.Second
	quicKeepAlivePeriod = 10 * time.Second
	perConnBufSize      = settings.PerConnectionSendBufSize
	recvBufSize         = settings.PerConnectionRecvBufSize
	// quicDialTimeout is a context deadline applied to every outbound QUIC dial.
	// Unlike HandshakeIdleTimeout (which only ticks down once the remote sends
	// its first crypto packet), a context deadline aborts the dial after the
	// specified wall-clock duration regardless of whether the remote responded
	// at all.  This matches the behaviour of TCP's net.DialTimeout and ensures
	// that dialling a port where nothing is listening (UDP void) fails in ~5 s
	// rather than blocking for the full MaxIdleTimeout (30 s).
	quicDialTimeout = time.Duration(dialTimeoutSecond) * time.Second
)

// Compile-time interface assertions.
var _ raftio.ITransport = (*QUIC)(nil)
var _ raftio.IConnection = (*QUICConnection)(nil)
var _ raftio.ISnapshotConnection = (*QUICSnapshotConnection)(nil)

// ---------------------------------------------------------------------------
// Wire helpers
// ---------------------------------------------------------------------------

// writeFrame writes a 4-byte big-endian length-prefixed frame to w.
func writeFrame(w io.Writer, data []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// readFrame reads a 4-byte big-endian length-prefixed frame from r.
// It returns io.EOF when the stream has been cleanly closed by the sender.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size == 0 {
		return nil, errors.New("quic transport: received zero-length frame")
	}
	if size > maxFrameSize {
		return nil, errors.Newf("quic transport: frame size %d exceeds limit %d", size, maxFrameSize)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ---------------------------------------------------------------------------
// QUICConnection — raft message batches
// ---------------------------------------------------------------------------

// QUICConnection sends raft message batches over a single QUIC stream.
type QUICConnection struct {
	conn   *quic.Conn
	stream *quic.Stream
	buf    []byte
}

// Close closes the stream and the underlying QUIC connection.
func (c *QUICConnection) Close() {
	if err := c.stream.Close(); err != nil {
		plog.Debugf("quic: failed to close raft stream: %v", err)
	}
	if err := c.conn.CloseWithError(0, ""); err != nil {
		plog.Debugf("quic: failed to close connection: %v", err)
	}
}

// SendMessageBatch serialises batch and sends it as a single framed message.
func (c *QUICConnection) SendMessageBatch(batch pb.MessageBatch) error {
	sz := batch.SizeUpperLimit()
	if len(c.buf) < sz {
		c.buf = make([]byte, sz)
	}
	data := pb.MustMarshalTo(&batch, c.buf)
	return writeFrame(c.stream, data)
}

// ---------------------------------------------------------------------------
// QUICSnapshotConnection — snapshot chunks
// ---------------------------------------------------------------------------

// QUICSnapshotConnection sends snapshot chunks over a single QUIC stream.
type QUICSnapshotConnection struct {
	conn   *quic.Conn
	stream *quic.Stream
}

// Close signals end-of-stream to the server, waits for the server's
// confirmation FIN, then tears down the QUIC connection.
//
// Calling conn.CloseWithError immediately after stream.Close would race with
// in-flight stream data: QUIC's CONNECTION_CLOSE is an immediate abort and
// does not wait for stream data to be delivered. We therefore read from the
// stream until EOF — which arrives once the server has processed every chunk
// and closed its own write side — before issuing CloseWithError.
func (c *QUICSnapshotConnection) Close() {
	// Close our write side (sends STREAM FIN to server).
	if err := c.stream.Close(); err != nil {
		plog.Debugf("quic: failed to close snapshot stream write side: %v", err)
	}
	// Wait for the server to close its write side (confirmation that all
	// chunks were received and handed to the chunk handler).
	c.waitForServerFIN()
	// Now safe to abort the connection — all data has been delivered.
	if err := c.conn.CloseWithError(0, ""); err != nil {
		plog.Debugf("quic: failed to close connection: %v", err)
	}
}

// SendChunk serialises chunk and sends it as a single framed message.
func (c *QUICSnapshotConnection) SendChunk(chunk pb.Chunk) error {
	buf := make([]byte, chunk.Size())
	data := pb.MustMarshalTo(&chunk, buf)
	return writeFrame(c.stream, data)
}

// waitForServerFIN reads from the stream until EOF or deadline, confirming
// that the server has received and processed all chunks. The server closes
// its write side after the last chunk is handled, which produces the EOF here.
func (c *QUICSnapshotConnection) waitForServerFIN() {
	_ = c.stream.SetReadDeadline(time.Now().Add(quicKeepAlivePeriod))
	_, _ = io.Copy(io.Discard, c.stream)
}

// ---------------------------------------------------------------------------
// QUIC transport
// ---------------------------------------------------------------------------

// QUIC is a QUIC-based transport module for exchanging Raft messages and
// snapshots between NodeHost instances.
//
// Wire protocol (per stream):
//
//	byte 0        — stream type: 0x01 = raft, 0x02 = snapshot
//	[4B len][payload] × N  — length-prefixed protobuf frames
//
// No CRC checksums or magic bytes are needed: QUIC guarantees in-order,
// reliable, integrity-protected delivery at the transport layer.
// Stream end is signalled by a QUIC FIN (stream.Close on the sender side),
// which surfaces as io.EOF on the receiver.
type QUIC struct {
	wg             sync.WaitGroup
	connWg         sync.WaitGroup
	requestHandler raftio.MessageHandler
	chunkHandler   raftio.ChunkHandler
	nhConfig       config.NodeHostConfig
	// quicTransport owns the shared UDP socket. We keep a reference so that
	// Close() can call quicTransport.Close() followed by udpConn.Close() to
	// guarantee the port is released even when createdConn is false inside the
	// quic-go Transport (which is the case when the Conn is passed in by us).
	quicTransport *quic.Transport
	udpConn       net.PacketConn
	listener      *quic.Listener
	cancelCtx     context.CancelFunc
	// activeConns tracks all connections accepted by the server-side listener.
	// We explicitly call CloseWithError on every entry during Close() — before
	// quicTransport.Close() — so that a graceful CONNECTION_CLOSE frame is
	// transmitted to each connected peer.  Without this, quicTransport.Close()
	// tears down connections abortively and the remote peer's send goroutine
	// never detects the failure, leaving it blocked writing into a dead
	// connection indefinitely.
	activeConns sync.Map // *quic.Conn → struct{}
}

// NewQUICTransport creates and returns a new QUIC transport module.
func NewQUICTransport(
	nhConfig config.NodeHostConfig,
	requestHandler raftio.MessageHandler,
	chunkHandler raftio.ChunkHandler,
) raftio.ITransport {
	return &QUIC{
		nhConfig:       nhConfig,
		requestHandler: requestHandler,
		chunkHandler:   chunkHandler,
	}
}

// Name returns the human-readable name of the transport module.
func (t *QUIC) Name() string { return QUICTransportName }

// Start binds the QUIC listener and begins accepting connections.
func (t *QUIC) Start() error {
	tlsConfig, err := t.serverTLSConfig()
	if err != nil {
		return err
	}
	// Create the UDP socket ourselves so we can close it explicitly in Close(),
	// guaranteeing that the port is released.  quic.ListenAddr hides the
	// Transport internally and listener.Close() does not close the UDP socket.
	pc, err := net.ListenPacket("udp", t.nhConfig.GetListenAddress())
	if err != nil {
		return err
	}
	udpConn := pc.(*net.UDPConn)
	// Wrap the connection to control the UDP receive/send buffer size.
	// When QUICUDPBufferSize is set, we cap any buffer-size request (including
	// those issued internally by quic-go) to that value. This prevents noisy
	// warnings on systems where the kernel enforces a low SO_RCVBUF maximum
	// (e.g. constrained CI environments). When the field is 0, the raw
	// connection is used and quic-go applies its own default (7 MiB).
	var packetConn net.PacketConn = udpConn
	if sz := t.nhConfig.QUICUDPBufferSize; sz > 0 {
		packetConn = newCappedUDPConn(udpConn, sz)
	}
	qt := &quic.Transport{Conn: packetConn}
	listener, err := qt.Listen(tlsConfig, &quic.Config{
		MaxIdleTimeout:  quicMaxIdleTimeout,
		KeepAlivePeriod: quicKeepAlivePeriod,
	})
	if err != nil {
		_ = udpConn.Close()
		return err
	}
	t.udpConn = udpConn
	t.quicTransport = qt
	t.listener = listener

	ctx, cancel := context.WithCancel(context.Background())
	t.cancelCtx = cancel

	// Accept loop.
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		for {
			conn, err := t.listener.Accept(ctx)
			if err != nil {
				return
			}
			t.connWg.Add(1)
			go func(c *quic.Conn) {
				defer t.connWg.Done()
				t.serveConn(c, ctx)
			}(conn)
		}
	}()

	return nil
}

// Close shuts down the transport: stops the accept loop, waits for all
// in-flight connection goroutines to finish, then releases the UDP port.
func (t *QUIC) Close() error {
	// 1. Cancel the shared context. This unblocks listener.Accept(ctx) so the
	//    accept loop goroutine exits, and unblocks conn.AcceptStream(ctx) in
	//    every serveConn goroutine so those loops exit too.
	if t.cancelCtx != nil {
		t.cancelCtx()
	}

	// 2. Send a graceful CONNECTION_CLOSE to every peer we have accepted a
	//    connection from.  This must happen before quicTransport.Close()
	//    because Transport.Close() tears down connections abortively —
	//    without sending CONNECTION_CLOSE — which leaves the remote peer's
	//    send goroutine blocked writing into a dead connection until its
	//    MaxIdleTimeout expires (up to 30 s).  By sending CloseWithError
	//    here, while the UDP socket is still open, the remote receives a
	//    proper CONNECTION_CLOSE frame and immediately unblocks.
	t.activeConns.Range(func(k, _ any) bool {
		_ = k.(*quic.Conn).CloseWithError(0, "")
		return true
	})

	// 3. Close the listener and the quic.Transport immediately after cancelling
	//    the context. This is the critical step that unblocks any goroutine
	//    stuck in a blocking stream read (e.g. io.ReadFull inside readFrame /
	//    serveRaftStream). quic-go does not propagate a context cancellation to
	//    open streams — only closing the Transport aborts them with an error.
	//    We must do this before waiting, otherwise serveConn goroutines that
	//    own active streams will never return.
	if t.listener != nil {
		_ = t.listener.Close()
	}
	if t.quicTransport != nil {
		_ = t.quicTransport.Close()
	}

	// 3. Wait for the accept loop goroutine to exit.
	t.wg.Wait()

	// 4. Wait for all serveConn goroutines (and the stream goroutines they
	//    own) to finish. By this point every stream read has been aborted by
	//    the Transport closure above, so these goroutines should exit promptly.
	t.connWg.Wait()

	// 5. Close the raw UDP socket to release the port. The Transport has
	//    already been closed above; this final close ensures the OS port is
	//    freed immediately even if the Transport's internal reference counting
	//    would otherwise delay it.
	if t.udpConn != nil {
		_ = t.udpConn.Close()
	}
	return nil
}

// GetConnection dials a new QUIC connection to target and opens a raft stream.
func (t *QUIC) GetConnection(ctx context.Context, target string) (raftio.IConnection, error) {
	conn, stream, err := t.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	if _, err := stream.Write([]byte{raftStreamType}); err != nil {
		stream.CancelWrite(0)
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	return &QUICConnection{
		conn:   conn,
		stream: stream,
		buf:    make([]byte, perConnBufSize),
	}, nil
}

// GetSnapshotConnection dials a new QUIC connection to target and opens a
// snapshot stream.
func (t *QUIC) GetSnapshotConnection(ctx context.Context, target string) (raftio.ISnapshotConnection, error) {
	conn, stream, err := t.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	if _, err := stream.Write([]byte{snapshotStreamType}); err != nil {
		stream.CancelWrite(0)
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	return &QUICSnapshotConnection{conn: conn, stream: stream}, nil
}

// dial establishes a QUIC connection to target and opens a bidirectional stream.
//
// It reuses the shared quic.Transport (and therefore the same UDP socket as the
// listener) for outbound connections.  This avoids creating a new OS UDP socket
// per outgoing connection.
//
// A context deadline of quicDialTimeout is applied so that dialling a port
// where nothing is listening fails promptly.  HandshakeIdleTimeout alone is
// insufficient because quic-go only starts that timer once the remote has sent
// its first crypto packet; against a UDP void it would block for the full
// MaxIdleTimeout (30 s).  A context cancellation, by contrast, aborts the dial
// immediately regardless of handshake progress.
func (t *QUIC) dial(ctx context.Context, target string) (*quic.Conn, *quic.Stream, error) {
	tlsConfig, err := t.clientTLSConfig(target)
	if err != nil {
		return nil, nil, err
	}
	addr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, nil, errors.Wrap(err, "quic: resolve address")
	}
	dialCtx, cancel := context.WithTimeout(ctx, quicDialTimeout)
	defer cancel()
	conn, err := t.quicTransport.Dial(dialCtx, addr, tlsConfig, &quic.Config{
		MaxIdleTimeout:  quicMaxIdleTimeout,
		KeepAlivePeriod: quicKeepAlivePeriod,
	})
	if err != nil {
		return nil, nil, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, nil, err
	}
	return conn, stream, nil
}

// serveConn accepts streams from conn and dispatches each to its own goroutine.
// It uses an inline WaitGroup so that all stream goroutines finish before the
// connection is torn down.
func (t *QUIC) serveConn(conn *quic.Conn, ctx context.Context) {
	t.activeConns.Store(conn, struct{}{})
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		t.activeConns.Delete(conn)
		_ = conn.CloseWithError(0, "")
	}()
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		wg.Add(1)
		go func(s *quic.Stream) {
			defer wg.Done()
			defer s.CancelRead(0)
			t.serveStream(s)
		}(stream)
	}
}

// serveStream reads the stream-type byte and routes to the appropriate handler.
func (t *QUIC) serveStream(stream *quic.Stream) {
	var typeBuf [1]byte
	if _, err := io.ReadFull(stream, typeBuf[:]); err != nil {
		if err != io.EOF {
			plog.Errorf("quic: failed to read stream type byte: %v", err)
		}
		return
	}
	switch typeBuf[0] {
	case raftStreamType:
		t.serveRaftStream(stream)
	case snapshotStreamType:
		t.serveSnapshotStream(stream)
	default:
		plog.Errorf("quic: unknown stream type 0x%02x, dropping stream", typeBuf[0])
	}
}

// serveRaftStream reads length-prefixed MessageBatch frames until the stream
// is closed or an error occurs.
func (t *QUIC) serveRaftStream(stream *quic.Stream) {
	for {
		data, err := readFrame(stream)
		if err != nil {
			if err != io.EOF {
				plog.Errorf("quic: raft stream read error: %v", err)
			}
			return
		}
		batch := pb.MessageBatch{}
		if err := batch.Unmarshal(data); err != nil {
			plog.Errorf("quic: failed to unmarshal MessageBatch: %v", err)
			return
		}
		t.requestHandler(batch)
	}
}

// serveSnapshotStream reads length-prefixed Chunk frames until the stream is
// closed or an error occurs.
//
// After the loop exits (whether cleanly via io.EOF or due to an error) the
// server closes its own write side of the stream. This FIN travels back to
// the sender and is used by QUICSnapshotConnection.Close() as a confirmation
// that every chunk has been handed to the chunk handler — allowing the sender
// to safely call conn.CloseWithError without racing with undelivered data.
func (t *QUIC) serveSnapshotStream(stream *quic.Stream) {
	// Always close the write side on exit so the sender's waitForServerFIN
	// unblocks regardless of whether the loop completed successfully or not.
	defer func() {
		if err := stream.Close(); err != nil {
			plog.Debugf("quic: failed to close snapshot stream write side: %v", err)
		}
	}()
	for {
		data, err := readFrame(stream)
		if err != nil {
			if err != io.EOF {
				plog.Errorf("quic: snapshot stream read error: %v", err)
			}
			return
		}
		chunk := pb.Chunk{}
		if err := chunk.Unmarshal(data); err != nil {
			plog.Errorf("quic: failed to unmarshal Chunk: %v", err)
			return
		}
		if !t.chunkHandler(chunk) {
			plog.Errorf("quic: chunk rejected %s", chunkKey(chunk))
			return
		}
	}
}

// ---------------------------------------------------------------------------
// TLS helpers
// ---------------------------------------------------------------------------

func (t *QUIC) serverTLSConfig() (*tls.Config, error) {
	if t.nhConfig.ServerTLS != nil {
		cfg := t.nhConfig.ServerTLS.Clone()
		cfg.NextProtos = append(cfg.NextProtos, quicALPN)
		return cfg, nil
	}
	return selfSignedTLSConfig()
}

func (t *QUIC) clientTLSConfig(_ string) (*tls.Config, error) {
	if t.nhConfig.ClientTLS != nil {
		cfg := t.nhConfig.ClientTLS.Clone()
		cfg.NextProtos = append(cfg.NextProtos, quicALPN)
		return cfg, nil
	}
	// When no ClientTLS is configured the server uses a self-signed certificate.
	// We accept it unconditionally here; the channel is still encrypted.
	return &tls.Config{ //nolint:gosec
		InsecureSkipVerify: true, // lgtm[go/disabled-certificate-check]
		NextProtos:         []string{quicALPN},
	}, nil
}

// selfSignedTLSConfig generates a fresh ECDSA P-256 self-signed certificate.
// It is used as the server TLS identity when MutualTLS is not configured,
// allowing QUIC to establish an encrypted (though unauthenticated) channel.
func selfSignedTLSConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, errors.Wrap(err, "quic: generate ECDSA key")
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"armada-raft"}},
		NotBefore:             time.Now().Add(-time.Minute), // small clock-skew tolerance
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, errors.Wrap(err, "quic: create self-signed certificate")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
		}},
		NextProtos: []string{quicALPN},
	}, nil
}

// cappedUDPConn wraps a *net.UDPConn and caps SetReadBuffer / SetWriteBuffer
// calls to a configured maximum. This prevents the QUIC library from
// requesting a buffer size larger than the OS kernel permits, which would
// otherwise produce a noisy log warning on systems with a low SO_RCVBUF limit.
type cappedUDPConn struct {
	*net.UDPConn
	maxSize int
}

// newCappedUDPConn returns a cappedUDPConn that enforces maxSize on every
// SetReadBuffer / SetWriteBuffer call.
func newCappedUDPConn(c *net.UDPConn, maxSize int) *cappedUDPConn {
	return &cappedUDPConn{UDPConn: c, maxSize: maxSize}
}

// SetReadBuffer caps the requested size and delegates to the underlying conn.
func (c *cappedUDPConn) SetReadBuffer(n int) error {
	if n > c.maxSize {
		n = c.maxSize
	}
	return c.UDPConn.SetReadBuffer(n)
}

// SetWriteBuffer caps the requested size and delegates to the underlying conn.
func (c *cappedUDPConn) SetWriteBuffer(n int) error {
	if n > c.maxSize {
		n = c.maxSize
	}
	return c.UDPConn.SetWriteBuffer(n)
}

// Copyright Armada Contributors

package armadaserver

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestWatchServer_Unimplemented(t *testing.T) {
	err := (&WatchServer{}).Watch(nil)
	require.Equal(t, codes.Unimplemented, status.Code(err))
}

// startWatchServer serves the KV and Watch services of a fresh engine over an
// in-memory connection.
func startWatchServer(t *testing.T, tables ...string) (armadapb.KVClient, armadapb.WatchClient) {
	t.Helper()
	engine := newInMemTestEngine(t, tables...)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	armadapb.RegisterKVServer(srv, &KVServer{Storage: engine})
	armadapb.RegisterWatchServer(srv, &WatchServer{Watches: engine})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return armadapb.NewKVClient(conn), armadapb.NewWatchClient(conn)
}

func openWatch(t *testing.T, c armadapb.WatchClient) armadapb.Watch_WatchClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	stream, err := c.Watch(ctx)
	require.NoError(t, err)
	return stream
}

func createWatch(t *testing.T, stream armadapb.Watch_WatchClient, req *armadapb.WatchCreateRequest) *armadapb.WatchResponse {
	t.Helper()
	require.NoError(t, stream.Send(&armadapb.WatchRequest{RequestUnion: &armadapb.WatchRequest_CreateRequest{CreateRequest: req}}))
	resp, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, resp.Created)
	return resp
}

func TestWatchServer_LiveEvents(t *testing.T) {
	kv, wc := startWatchServer(t, "t")
	ctx := context.Background()
	stream := openWatch(t, wc)

	created := createWatch(t, stream, &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("k"), RangeEnd: []byte("l"), PrevKv: true})
	require.False(t, created.Canceled)
	id := created.WatchId
	require.NotZero(t, id)

	put1, err := kv.Put(ctx, &armadapb.PutRequest{Table: []byte("t"), Key: []byte("k1"), Value: []byte("v1")})
	require.NoError(t, err)
	_, err = kv.Put(ctx, &armadapb.PutRequest{Table: []byte("t"), Key: []byte("other"), Value: []byte("x")})
	require.NoError(t, err)
	put2, err := kv.Put(ctx, &armadapb.PutRequest{Table: []byte("t"), Key: []byte("k1"), Value: []byte("v2")})
	require.NoError(t, err)
	del, err := kv.DeleteRange(ctx, &armadapb.DeleteRangeRequest{Table: []byte("t"), Key: []byte("k"), RangeEnd: []byte("l")})
	require.NoError(t, err)

	var events []*armadapb.Event
	for len(events) < 3 {
		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, id, resp.WatchId)
		require.NotEmpty(t, resp.Events)
		require.GreaterOrEqual(t, resp.Header.Revision, uint64(resp.Events[len(resp.Events)-1].Kv.ModRevision))
		events = append(events, resp.Events...)
	}

	require.Equal(t, armadapb.Event_PUT, events[0].Type)
	require.Equal(t, "v1", string(events[0].Kv.Value))
	require.Equal(t, int64(put1.Header.Revision), events[0].Kv.ModRevision)
	require.Equal(t, int64(put1.Header.Revision), events[0].Kv.CreateRevision)
	require.Nil(t, events[0].PrevKv)

	require.Equal(t, armadapb.Event_PUT, events[1].Type)
	require.Equal(t, int64(put2.Header.Revision), events[1].Kv.ModRevision)
	require.Equal(t, int64(put1.Header.Revision), events[1].Kv.CreateRevision)
	require.Equal(t, "v1", string(events[1].PrevKv.Value))

	require.Equal(t, armadapb.Event_DELETE, events[2].Type)
	require.Equal(t, int64(del.Header.Revision), events[2].Kv.ModRevision)
	require.Equal(t, "v2", string(events[2].PrevKv.Value))
}

func TestWatchServer_CreateErrors(t *testing.T) {
	_, wc := startWatchServer(t, "t")
	stream := openWatch(t, wc)

	tests := []struct {
		name   string
		req    *armadapb.WatchCreateRequest
		reason string
	}{
		{"no table", &armadapb.WatchCreateRequest{Key: []byte("a")}, "table must be set"},
		{"no key", &armadapb.WatchCreateRequest{Table: []byte("t")}, "key must be set"},
		{"null byte", &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("a\x00b")}, "key must not contain null bytes"},
		{"negative start", &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("a"), StartRevision: -1}, "start_revision must not be negative"},
		{"unknown table", &armadapb.WatchCreateRequest{Table: []byte("missing"), Key: []byte("a")}, "table not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := createWatch(t, stream, tt.req)
			require.True(t, resp.Canceled)
			require.Equal(t, tt.reason, resp.CancelReason)
		})
	}
}

func TestWatchServer_StartRevision(t *testing.T) {
	kv, wc := startWatchServer(t, "t")
	stream := openWatch(t, wc)
	put, err := kv.Put(context.Background(), &armadapb.PutRequest{Table: []byte("t"), Key: []byte("a"), Value: []byte("1")})
	require.NoError(t, err)
	rev := int64(put.Header.Revision)

	past := createWatch(t, stream, &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("a"), StartRevision: rev})
	require.True(t, past.Canceled)
	require.Equal(t, "watching from a past revision is not supported", past.CancelReason)

	future := createWatch(t, stream, &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("a"), StartRevision: rev + 1})
	require.False(t, future.Canceled)
}

func TestWatchServer_IDsAndCancel(t *testing.T) {
	kv, wc := startWatchServer(t, "t")
	stream := openWatch(t, wc)

	first := createWatch(t, stream, &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("a"), WatchId: 7})
	require.Equal(t, int64(7), first.WatchId)
	require.False(t, first.Canceled)

	dup := createWatch(t, stream, &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("b"), WatchId: 7})
	require.True(t, dup.Canceled)
	require.Equal(t, int64(7), dup.WatchId)

	// The rejected duplicate leaves the original watcher in place.
	_, err := kv.Put(context.Background(), &armadapb.PutRequest{Table: []byte("t"), Key: []byte("a"), Value: []byte("1")})
	require.NoError(t, err)
	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, int64(7), resp.WatchId)
	require.Len(t, resp.Events, 1)

	require.NoError(t, stream.Send(&armadapb.WatchRequest{RequestUnion: &armadapb.WatchRequest_CancelRequest{CancelRequest: &armadapb.WatchCancelRequest{WatchId: 7}}}))
	resp, err = stream.Recv()
	require.NoError(t, err)
	require.True(t, resp.Canceled)
	require.Equal(t, int64(7), resp.WatchId)

	// The ID is free again.
	again := createWatch(t, stream, &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("a"), WatchId: 7})
	require.False(t, again.Canceled)
}

// TestWatchServer_ConcurrentWrites creates a watcher while writes are in
// flight. Batches applied before the state machine noticed the watcher are
// not recorded and must be read back from the store, so the watcher still
// sees every write from its first event on, in order.
func TestWatchServer_ConcurrentWrites(t *testing.T) {
	kv, wc := startWatchServer(t, "t")
	ctx := context.Background()

	const writes = 600
	revs := make(chan int64, writes)
	errs := make(chan error, 1)
	go func() {
		defer close(revs)
		for range writes {
			res, err := kv.Put(ctx, &armadapb.PutRequest{Table: []byte("t"), Key: []byte("a"), Value: []byte("v")})
			if err != nil {
				errs <- err
				return
			}
			revs <- int64(res.Header.Revision)
		}
	}()

	// Let some writes happen before the watcher exists.
	first := <-revs
	stream := openWatch(t, wc)
	created := createWatch(t, stream, &armadapb.WatchCreateRequest{Table: []byte("t"), Key: []byte("a")})
	require.False(t, created.Canceled)

	written := []int64{first}
	for r := range revs {
		written = append(written, r)
	}
	select {
	case err := <-errs:
		require.NoError(t, err)
	default:
	}
	last := written[len(written)-1]

	var got []int64
	for len(got) == 0 || got[len(got)-1] < last {
		resp, err := stream.Recv()
		require.NoError(t, err)
		require.False(t, resp.Canceled, resp.CancelReason)
		for _, ev := range resp.Events {
			got = append(got, ev.Kv.ModRevision)
		}
	}

	// Every write from the first received event on was delivered, in order.
	start := slices.Index(written, got[0])
	require.NotEqual(t, -1, start, "first event %d is a write", got[0])
	require.Equal(t, written[start:], got)
}

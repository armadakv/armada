// Copyright Armada Contributors

package watch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table/fsm"
	"github.com/stretchr/testify/require"
)

const testTable = "t"

type testResolver struct {
	mu     sync.Mutex
	shards map[string]uint64
}

func (r *testResolver) resolve(table string) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.shards[table]; ok {
		return id, nil
	}
	return 0, serrors.ErrTableNotFound
}

func (r *testResolver) set(table string, shard uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if shard == 0 {
		delete(r.shards, table)
		return
	}
	r.shards[table] = shard
}

// testStore plays the MVCC store: it keeps every committed event.
type testStore struct {
	mu     sync.Mutex
	events []*armadapb.Event
	floor  uint64
}

func (s *testStore) add(events ...*armadapb.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
}

func (s *testStore) scan(_ uint64, req fsm.WatchScanRequest) (*fsm.WatchScanResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.From <= s.floor {
		return &fsm.WatchScanResponse{Floor: s.floor, Compacted: true}, nil
	}
	w := &Watcher{key: req.Key, rangeEnd: req.RangeEnd}
	res := &fsm.WatchScanResponse{Floor: s.floor, Through: req.To}
	for _, ev := range s.events {
		rev := uint64(ev.Kv.ModRevision)
		if rev >= req.From && rev <= req.To && w.contains(ev.Kv.Key) {
			res.Events = append(res.Events, ev)
		}
	}
	return res, nil
}

var testStores sync.Map

func storeOf(r *Registry) *testStore {
	s, _ := testStores.Load(r)
	return s.(*testStore)
}

// newTestHub returns a registry with an open hub for shard 1 at revision rev.
func newTestHub(t *testing.T, rev uint64) (*Registry, *Hub, *testResolver) {
	t.Helper()
	res := &testResolver{shards: map[string]uint64{testTable: 1}}
	st := &testStore{}
	r := NewRegistry(res.resolve, st.scan)
	testStores.Store(r, st)
	t.Cleanup(r.Close)
	h := r.Observer(1, testTable).(*Hub)
	h.Committed(fsm.Commit{Revision: rev})
	require.Eventually(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.open && h.revision == rev
	}, time.Second, time.Millisecond)
	return r, h, res
}

func put(k, v string, create, mod int64) *armadapb.Event {
	return &armadapb.Event{Type: armadapb.Event_PUT, Kv: &armadapb.KeyValue{Key: []byte(k), Value: []byte(v), CreateRevision: create, ModRevision: mod}}
}

func del(k string, create, mod int64) *armadapb.Event {
	return &armadapb.Event{Type: armadapb.Event_DELETE, Kv: &armadapb.KeyValue{Key: []byte(k), CreateRevision: create, ModRevision: mod}}
}

func commit(rev uint64, events ...*armadapb.Event) fsm.Commit {
	return fsm.Commit{Revision: rev, Captured: true, Events: events}
}

// received holds messages taken from a sink but not yet returned by next.
var received sync.Map

// next returns the next message from the sink, draining it the way a stream does.
func next(t *testing.T, s *Sink) Message {
	t.Helper()
	if v, ok := received.Load(s); ok {
		if q := v.([]Message); len(q) > 0 {
			received.Store(s, q[1:])
			return q[0]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	q, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("no message received: %v", err)
	}
	received.Store(s, q[1:])
	return q[0]
}

func requireNoMessage(t *testing.T, s *Sink) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if v, ok := received.Load(s); ok {
		require.Empty(t, v.([]Message))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Empty(t, s.queue)
}

func keys(m Message) []string {
	var res []string
	for _, ev := range m.Events {
		res = append(res, string(ev.Kv.Key))
	}
	return res
}

func watchOn(t *testing.T, r *Registry, opts Options) (*Watcher, *Sink) {
	t.Helper()
	s := NewSink(0)
	opts.Table = testTable
	w, err := r.Watch(opts, s)
	require.NoError(t, err)
	m := next(t, s)
	require.True(t, m.Created)
	require.Equal(t, opts.ID, m.WatchID)
	return w, s
}

func TestHub_LiveDelivery(t *testing.T) {
	r, h, _ := newTestHub(t, 5)
	_, single := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	_, prefix := watchOn(t, r, Options{ID: 2, Key: []byte("k"), RangeEnd: []byte("l")})
	_, all := watchOn(t, r, Options{ID: 3, Key: []byte{0}, RangeEnd: []byte{0}})

	h.Committed(commit(7, put("a", "1", 6, 6), put("k1", "1", 6, 6), put("k2", "1", 7, 7), put("z", "1", 7, 7)))

	m := next(t, single)
	require.Equal(t, []string{"a"}, keys(m))
	require.Equal(t, uint64(7), m.Revision)
	require.Equal(t, uint64(1), m.ShardID)
	require.Equal(t, []string{"k1", "k2"}, keys(next(t, prefix)))
	require.Equal(t, []string{"a", "k1", "k2", "z"}, keys(next(t, all)))
}

func TestHub_FiltersAndPrevKV(t *testing.T) {
	r, h, _ := newTestHub(t, 1)
	_, noPut := watchOn(t, r, Options{ID: 1, Key: []byte("a"), NoPut: true})
	_, noDel := watchOn(t, r, Options{ID: 2, Key: []byte("a"), NoDelete: true})
	_, withPrev := watchOn(t, r, Options{ID: 3, Key: []byte("a"), PrevKV: true})
	_, noPrev := watchOn(t, r, Options{ID: 4, Key: []byte("a")})

	p := put("a", "2", 1, 2)
	p.PrevKv = &armadapb.KeyValue{Key: []byte("a"), Value: []byte("1"), CreateRevision: 1, ModRevision: 1}
	h.Committed(commit(2, p))
	h.Committed(commit(3, del("a", 1, 3)))

	m := next(t, noPut)
	require.Equal(t, armadapb.Event_DELETE, m.Events[0].Type)
	m = next(t, noDel)
	require.Equal(t, armadapb.Event_PUT, m.Events[0].Type)
	requireNoMessage(t, noDel)
	require.NotNil(t, next(t, withPrev).Events[0].PrevKv)
	require.Nil(t, next(t, noPrev).Events[0].PrevKv)
	require.NotNil(t, p.PrevKv, "shared events are not modified")
}

func TestHub_StartRevision(t *testing.T) {
	r, h, _ := newTestHub(t, 5)
	_, future := watchOn(t, r, Options{ID: 1, Key: []byte("a"), StartRevision: 8})

	h.Committed(commit(7, put("a", "1", 7, 7)))
	h.Committed(commit(8, put("a", "2", 7, 8)))
	m := next(t, future)
	require.Len(t, m.Events, 1)
	require.Equal(t, int64(8), m.Events[0].Kv.ModRevision)

	_, err := r.Watch(Options{Table: testTable, Key: []byte("a"), StartRevision: 8}, NewSink(0))
	require.ErrorIs(t, err, ErrHistoricalRevision)
}

func TestHub_Cancel(t *testing.T) {
	r, h, _ := newTestHub(t, 1)
	w, s := watchOn(t, r, Options{ID: 9, Key: []byte("a")})
	w.Cancel()
	m := next(t, s)
	require.True(t, m.Canceled)
	require.Equal(t, int64(9), m.WatchID)
	w.Cancel()
	h.Committed(commit(2, put("a", "1", 2, 2)))
	requireNoMessage(t, s)
}

func TestHub_CancelOnRewrite(t *testing.T) {
	r, h, _ := newTestHub(t, 10)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	h.Committed(fsm.Commit{Revision: 20, Rewritten: true})
	m := next(t, s)
	require.True(t, m.Canceled)
	require.Equal(t, reasonRewritten, m.Reason)

	// A revision that goes backwards is a rewrite as well.
	_, s = watchOn(t, r, Options{ID: 2, Key: []byte("a")})
	h.Committed(commit(15))
	require.Equal(t, reasonRewritten, next(t, s).Reason)
}

// commitTo stores the events and reports the commit to the hub, captured or not.
func commitTo(r *Registry, h *Hub, captured bool, rev uint64, events ...*armadapb.Event) {
	storeOf(r).add(events...)
	c := fsm.Commit{Revision: rev, Captured: captured}
	if captured {
		c.Events = events
	}
	h.Committed(c)
}

// collect reads event messages until n events arrived and returns their revisions.
func collect(t *testing.T, s *Sink, n int) []int64 {
	t.Helper()
	var revs []int64
	for len(revs) < n {
		m := next(t, s)
		require.False(t, m.Canceled, "unexpected cancel: %s", m.Reason)
		for _, ev := range m.Events {
			revs = append(revs, ev.Kv.ModRevision)
		}
	}
	return revs
}

func seq(from, to int64) []int64 {
	var res []int64
	for r := from; r <= to; r++ {
		res = append(res, r)
	}
	return res
}

func TestHub_CatchUpMissedRevisions(t *testing.T) {
	r, h, _ := newTestHub(t, 10)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	_, future := watchOn(t, r, Options{ID: 2, Key: []byte("a"), StartRevision: 100})

	commitTo(r, h, true, 11, put("a", "1", 11, 11))
	// Revisions 12 and 13 were not recorded: the watcher reads them from the store.
	commitTo(r, h, false, 13, put("a", "2", 11, 12), put("b", "1", 13, 13), put("a", "3", 11, 13))
	commitTo(r, h, true, 14, put("a", "4", 11, 14))

	require.Equal(t, seq(11, 14), collect(t, s, 4))
	requireNoMessage(t, s)
	requireNoMessage(t, future)
}

func TestHub_CatchUpSlowConsumer(t *testing.T) {
	r, h, _ := newTestHub(t, 1)
	s := NewSink(1)
	_, err := r.Watch(Options{ID: 1, Table: testTable, Key: []byte("a")}, s)
	require.NoError(t, err)
	require.True(t, next(t, s).Created)

	// The sink holds one message: the rest is read back from the store as
	// the stream drains.
	for rev := uint64(2); rev <= 40; rev++ {
		commitTo(r, h, true, rev, put("a", "v", 2, int64(rev)))
	}
	require.Equal(t, seq(2, 40), collect(t, s, 39))

	// Once caught up, the watcher is live again.
	commitTo(r, h, true, 41, put("a", "v", 2, 41))
	require.Equal(t, []int64{41}, collect(t, s, 1))
	requireNoMessage(t, s)
}

func TestHub_CatchUpCompacted(t *testing.T) {
	r, h, _ := newTestHub(t, 10)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	storeOf(r).floor = 30
	commitTo(r, h, false, 40, put("a", "1", 40, 40))
	m := next(t, s)
	require.True(t, m.Canceled)
	require.Equal(t, reasonCompacted, m.Reason)
	require.Equal(t, uint64(31), m.CompactRevision)
}

func TestHub_Capturing(t *testing.T) {
	r, h, _ := newTestHub(t, 1)
	require.False(t, h.Capturing(), "nothing is recorded without watchers")
	w, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	require.True(t, h.Capturing())
	w.Cancel()
	require.True(t, next(t, s).Canceled)
	require.False(t, h.Capturing())
}

func TestHub_Closed(t *testing.T) {
	r, h, _ := newTestHub(t, 1)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	h.Closed()
	m := next(t, s)
	require.True(t, m.Canceled)
	require.Equal(t, reasonClosed, m.Reason)

	_, err := r.Watch(Options{Table: testTable, Key: []byte("a")}, NewSink(0))
	require.ErrorIs(t, err, ErrNotServed)

	// A restarted state machine gets a new hub.
	h2 := r.Observer(1, testTable).(*Hub)
	require.NotSame(t, h, h2)
	h2.Committed(fsm.Commit{Revision: 1})
	require.Eventually(t, func() bool {
		_, err := r.Watch(Options{Table: testTable, Key: []byte("a")}, NewSink(0))
		return err == nil
	}, time.Second, time.Millisecond)
}

func TestRegistry_CheckServing(t *testing.T) {
	r, _, res := newTestHub(t, 1)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})

	res.set(testTable, 2)
	r.checkServing()
	m := next(t, s)
	require.True(t, m.Canceled)
	require.Equal(t, reasonReplaced, m.Reason)

	res.set(testTable, 1)
	_, s = watchOn(t, r, Options{ID: 2, Key: []byte("a")})
	res.set(testTable, 0)
	r.checkServing()
	require.Equal(t, reasonDeleted, next(t, s).Reason)
}

func TestRegistry_NotServed(t *testing.T) {
	r, _, res := newTestHub(t, 1)
	res.set("other", 7)
	_, err := r.Watch(Options{Table: "other", Key: []byte("a")}, NewSink(0))
	require.ErrorIs(t, err, ErrNotServed)
	_, err = r.Watch(Options{Table: "missing", Key: []byte("a")}, NewSink(0))
	require.ErrorIs(t, err, serrors.ErrTableNotFound)
}

func TestRegistry_Close(t *testing.T) {
	r, _, _ := newTestHub(t, 1)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	r.Close()
	require.Equal(t, reasonShutdown, next(t, s).Reason)
	_, err := r.Watch(Options{Table: testTable, Key: []byte("a")}, NewSink(0))
	require.ErrorIs(t, err, ErrClosed)
	require.Nil(t, r.Observer(1, testTable))
}

func TestHub_Overflow(t *testing.T) {
	r, h, _ := newTestHub(t, 1)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})

	// Block the loop on the hub lock right after it took the first commit, so
	// that the following commits fill the queue and then overflow.
	h.mu.Lock()
	commitTo(r, h, true, 2, put("a", "v", 2, 2))
	require.Eventually(t, func() bool {
		h.qmu.Lock()
		defer h.qmu.Unlock()
		return len(h.pending) == 0
	}, time.Second, time.Millisecond)
	last := uint64(maxPendingCommits + 12)
	for rev := uint64(3); rev <= last; rev++ {
		commitTo(r, h, true, rev, put("a", "v", 2, int64(rev)))
	}
	h.qmu.Lock()
	require.True(t, h.overflow)
	h.qmu.Unlock()
	h.mu.Unlock()

	// Queued commits are delivered live, the overflowed ones from the store.
	require.Equal(t, seq(2, int64(last)), collect(t, s, int(last-1)))
	requireNoMessage(t, s)
}

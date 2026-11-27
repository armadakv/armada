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

// newTestHub returns a registry with an open hub for shard 1 at revision rev.
func newTestHub(t *testing.T, rev uint64) (*Registry, *Hub, *testResolver) {
	t.Helper()
	res := &testResolver{shards: map[string]uint64{testTable: 1}}
	r := NewRegistry(res.resolve)
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

// next returns the next message from the sink.
func next(t *testing.T, s *Sink) Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			m := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return m
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("no message received")
		case <-s.ready:
		}
	}
}

func requireNoMessage(t *testing.T, s *Sink) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
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

func TestHub_CancelOnMissedRevisions(t *testing.T) {
	r, h, _ := newTestHub(t, 10)
	_, s := watchOn(t, r, Options{ID: 1, Key: []byte("a")})
	_, future := watchOn(t, r, Options{ID: 2, Key: []byte("a"), StartRevision: 100})
	h.Committed(fsm.Commit{Revision: 20})
	m := next(t, s)
	require.True(t, m.Canceled)
	require.Equal(t, reasonBehind, m.Reason)
	requireNoMessage(t, future)
}

func TestHub_CancelSlowConsumer(t *testing.T) {
	r, h, _ := newTestHub(t, 1)
	s := NewSink(1)
	_, err := r.Watch(Options{ID: 1, Table: testTable, Key: []byte("a")}, s)
	require.NoError(t, err)
	h.Committed(commit(2, put("a", "1", 2, 2)))
	h.Committed(commit(3, put("a", "2", 2, 3)))

	require.True(t, next(t, s).Created)
	require.Len(t, next(t, s).Events, 1, "a message larger than the limit fits an empty sink")
	m := next(t, s)
	require.True(t, m.Canceled)
	require.Equal(t, reasonBehind, m.Reason)
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
	h.Committed(commit(2, put("a", "v", 2, 2)))
	require.Eventually(t, func() bool {
		h.qmu.Lock()
		defer h.qmu.Unlock()
		return len(h.pending) == 0
	}, time.Second, time.Millisecond)
	for i := range maxPendingCommits + 10 {
		rev := uint64(i + 3)
		h.Committed(commit(rev, put("a", "v", 2, int64(rev))))
	}
	h.mu.Unlock()

	var delivered int
	for {
		m := next(t, s)
		if m.Canceled {
			require.Equal(t, reasonBehind, m.Reason)
			break
		}
		delivered++
	}
	require.Equal(t, 1+maxPendingCommits, delivered, "queued commits are delivered before the overflow")
}

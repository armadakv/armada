// Copyright Armada Contributors

package watch

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage/table/fsm"
)

const (
	// maxPendingCommits bounds the commits a hub queues before it stops
	// recording them individually and remembers only that revisions were missed.
	maxPendingCommits = 1024
	// ringSize is the number of recent captured commits a hub keeps, so that a
	// watcher that caught up from the store can switch to the live path
	// without reading the store again.
	ringSize = 64
	// catchUpBytes bounds the events read from the store per catch-up step.
	catchUpBytes = 1024 * 1024
	// catchUpRetry is the delay before a failed catch-up step is retried.
	catchUpRetry = 100 * time.Millisecond
)

// Hub delivers the events of one table shard to its watchers. It implements
// fsm.Observer for the shard's state machine.
//
// A watcher is synced while it is fed from the commits the state machine
// reports. When it cannot be, because events were not recorded, the hub fell
// behind the state machine, or the watcher's stream is too slow, it becomes
// unsynced and reads its events back from the store from the first revision
// it has not received. Once it reaches the revisions the hub still keeps in
// memory, it is synced again. No event is lost or duplicated on the way.
type Hub struct {
	shardID  uint64
	table    string
	registry *Registry
	// count is the number of watchers; the state machine records events only
	// while it is non-zero.
	count atomic.Int64

	// The state machine hands commits over through this queue. When the queue
	// is full, later commits are folded into an overflow marker, so the apply
	// path never waits and ordering is preserved.
	qmu         sync.Mutex
	pending     []fsm.Commit
	overflow    bool
	ovRevision  uint64
	ovRewritten bool
	closing     bool
	wake        chan struct{}
	stop        chan struct{}
	done        chan struct{}

	mu sync.Mutex
	// open is set while the shard's state machine is open.
	open bool
	// revision is the revision of the last commit the hub has processed.
	revision uint64
	// floor is the highest revision whose events are no longer available,
	// as of the last commit.
	floor uint64
	// ring holds the most recent captured commits. Together they cover every
	// revision in (ringBase, revision].
	ring     []fsm.Commit
	ringBase uint64
	watchers map[*Watcher]struct{}
	matcher  matcher
}

func newHub(r *Registry, shardID uint64, table string) *Hub {
	h := &Hub{
		shardID:  shardID,
		table:    table,
		registry: r,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		watchers: make(map[*Watcher]struct{}),
		matcher:  newMatcher(),
	}
	go h.loop()
	return h
}

// Capturing implements fsm.Observer. Batches applied while nobody watches are
// not recorded; a watcher that needs them reads them from the store.
func (h *Hub) Capturing() bool {
	return h.count.Load() > 0
}

// Committed implements fsm.Observer.
func (h *Hub) Committed(c fsm.Commit) {
	h.qmu.Lock()
	defer h.qmu.Unlock()
	if h.closing {
		return
	}
	if h.overflow || len(h.pending) >= maxPendingCommits {
		h.overflow = true
		h.ovRevision = c.Revision
		h.ovRewritten = h.ovRewritten || c.Rewritten
	} else {
		h.pending = append(h.pending, c)
	}
	h.signal()
}

// Closed implements fsm.Observer. The hub is unregistered at once, so that a
// restarted state machine gets a new hub, and then drains its queue.
func (h *Hub) Closed() {
	h.registry.remove(h)
	h.qmu.Lock()
	defer h.qmu.Unlock()
	h.closing = true
	h.signal()
}

func (h *Hub) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Hub) loop() {
	defer close(h.done)
	for {
		select {
		case <-h.stop:
			h.mu.Lock()
			h.open = false
			h.cancelAllLocked(reasonShutdown, 0)
			h.mu.Unlock()
			return
		case <-h.wake:
		}

		h.qmu.Lock()
		pending := h.pending
		h.pending = nil
		overflow, ovRevision, ovRewritten := h.overflow, h.ovRevision, h.ovRewritten
		h.overflow, h.ovRevision, h.ovRewritten = false, 0, false
		closing := h.closing
		h.qmu.Unlock()

		h.mu.Lock()
		for _, c := range pending {
			h.applyLocked(c)
		}
		if overflow {
			h.applyLocked(fsm.Commit{Revision: ovRevision, Rewritten: ovRewritten})
		}
		if closing {
			h.open = false
			h.cancelAllLocked(reasonClosed, 0)
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
	}
}

// applyLocked processes one commit.
func (h *Hub) applyLocked(c fsm.Commit) {
	h.open = true
	h.floor = c.Floor
	switch {
	case c.Rewritten || c.Revision < h.revision:
		h.cancelAllLocked(reasonRewritten, c.Floor+1)
		h.resetRingLocked(c.Revision)
	case !c.Captured:
		if c.Revision > h.revision {
			h.missedLocked(c.Revision)
		}
		h.resetRingLocked(c.Revision)
	default:
		h.dispatchLocked(c)
		h.ring = append(h.ring, c)
		if len(h.ring) > ringSize {
			h.ringBase = h.ring[0].Revision
			h.ring = h.ring[1:]
		}
	}
	h.revision = c.Revision
}

func (h *Hub) resetRingLocked(rev uint64) {
	h.ring = nil
	h.ringBase = rev
}

// missedLocked handles revisions up to rev whose events were not recorded:
// the synced watchers that were waiting for any of them catch up from the store.
func (h *Hub) missedLocked(rev uint64) {
	for w := range h.watchers {
		if w.state == stateSynced && h.positionLocked(w) <= rev {
			h.demoteLocked(w, h.positionLocked(w))
		}
	}
}

// positionLocked returns the next revision a synced watcher will receive.
func (h *Hub) positionLocked(w *Watcher) uint64 {
	return max(h.revision+1, w.minRev)
}

func (h *Hub) dispatchLocked(c fsm.Commit) {
	if len(h.watchers) == 0 || len(c.Events) == 0 {
		return
	}
	var (
		order []*Watcher
		byW   = make(map[*Watcher][]*armadapb.Event)
	)
	for _, ev := range c.Events {
		h.matcher.each(ev.Kv.Key, func(w *Watcher) {
			if w.state != stateSynced || !w.wants(ev) {
				return
			}
			evs, seen := byW[w]
			if !seen {
				order = append(order, w)
			}
			byW[w] = append(evs, w.view(ev))
		})
	}
	for _, w := range order {
		evs := byW[w]
		m := Message{WatchID: w.id, ShardID: h.shardID, Revision: c.Revision, Events: evs}
		if !w.sink.offer(m, eventsSize(evs)) {
			// The stream is too slow: the watcher reads this commit and
			// everything after it back from the store once there is room.
			h.demoteLocked(w, h.positionLocked(w))
		}
	}
}

// demoteLocked makes a watcher read its events from the store from next on.
func (h *Hub) demoteLocked(w *Watcher, next uint64) {
	w.state = stateUnsynced
	w.next = next
	if !w.catchingUp {
		w.catchingUp = true
		go h.catchUp(w)
	}
}

// promoteLocked switches an unsynced watcher to the live path when the
// commits it still needs are in memory, delivering them from the ring. It
// reports whether the watcher is synced.
func (h *Hub) promoteLocked(w *Watcher) bool {
	if w.next <= h.ringBase {
		return false
	}
	for _, c := range h.ring {
		if c.Revision < w.next {
			continue
		}
		evs := w.filter(c.Events)
		if len(evs) == 0 {
			w.next = c.Revision + 1
			continue
		}
		if !w.sink.offer(Message{WatchID: w.id, ShardID: h.shardID, Revision: c.Revision, Events: evs}, eventsSize(evs)) {
			return false
		}
		w.next = c.Revision + 1
	}
	w.state = stateSynced
	w.catchingUp = false
	w.minRev = max(w.minRev, w.next)
	return true
}

// catchUp reads an unsynced watcher's events from the store until it can be
// promoted.
func (h *Hub) catchUp(w *Watcher) {
	for {
		h.mu.Lock()
		if w.state != stateUnsynced {
			w.catchingUp = false
			h.mu.Unlock()
			return
		}
		if h.promoteLocked(w) {
			h.mu.Unlock()
			return
		}
		from, to := w.next, h.revision
		h.mu.Unlock()

		if !w.sink.waitRoom(w.stop) {
			h.endCatchUp(w)
			return
		}
		res, err := h.registry.scan(w.stop, h.shardID, fsm.WatchScanRequest{
			Key:      w.key,
			RangeEnd: w.rangeEnd,
			From:     from,
			To:       to,
			PrevKV:   w.prevKV,
			MaxBytes: catchUpBytes,
		})
		switch {
		case err != nil:
			select {
			case <-w.stop:
				h.endCatchUp(w)
				return
			case <-time.After(catchUpRetry):
			}
			continue
		case res.Compacted:
			h.cancel(w, reasonCompacted, res.Floor+1)
			h.endCatchUp(w)
			return
		}
		if !h.deliverCatchUp(w, from, res) {
			h.endCatchUp(w)
			return
		}
	}
}

// deliverCatchUp queues the events of one catch-up step. It reports whether
// the watcher is still unsynced and catching up.
func (h *Hub) deliverCatchUp(w *Watcher, from uint64, res *fsm.WatchScanResponse) bool {
	evs := w.filter(res.Events)
	for {
		h.mu.Lock()
		if w.state != stateUnsynced || w.next != from {
			h.mu.Unlock()
			return w.state == stateUnsynced
		}
		if len(evs) == 0 || w.sink.offer(Message{WatchID: w.id, ShardID: h.shardID, Revision: res.Through, Events: evs}, eventsSize(evs)) {
			w.next = res.Through + 1
			h.mu.Unlock()
			return true
		}
		h.mu.Unlock()
		if !w.sink.waitRoom(w.stop) {
			return false
		}
	}
}

func (h *Hub) endCatchUp(w *Watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.catchingUp = false
}

// add registers a watcher.
func (h *Hub) add(opts Options, sink *Sink) (*Watcher, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.open {
		return nil, ErrNotServed
	}
	w := newWatcher(h, opts, sink)
	historical := false
	switch {
	case opts.StartRevision == 0:
		w.minRev = h.revision + 1
	case opts.StartRevision <= h.floor:
		return nil, &CompactedError{CompactRevision: h.floor + 1}
	case opts.StartRevision > h.revision:
		w.minRev = opts.StartRevision
	default:
		w.minRev = opts.StartRevision
		historical = true
	}
	h.watchers[w] = struct{}{}
	h.count.Add(1)
	h.matcher.add(w)
	sink.Push(Message{WatchID: w.id, ShardID: h.shardID, Revision: h.revision, Created: true})
	if historical {
		// The store is authoritative about the floor: the catch-up read
		// cancels the watcher if its revisions are gone.
		h.demoteLocked(w, opts.StartRevision)
	}
	return w, nil
}

func (h *Hub) cancel(w *Watcher, reason string, compact uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelLocked(w, reason, compact)
}

func (h *Hub) cancelLocked(w *Watcher, reason string, compact uint64) {
	if w.state == stateCanceled {
		return
	}
	w.state = stateCanceled
	close(w.stop)
	delete(h.watchers, w)
	h.count.Add(-1)
	h.matcher.remove(w)
	w.sink.Push(Message{
		WatchID:         w.id,
		ShardID:         h.shardID,
		Revision:        h.revision,
		Canceled:        true,
		CompactRevision: compact,
		Reason:          reason,
	})
}

func (h *Hub) cancelAllLocked(reason string, compact uint64) {
	for w := range h.watchers {
		h.cancelLocked(w, reason, compact)
	}
}

// cancelAll cancels every watcher of the hub.
func (h *Hub) cancelAll(reason string, compact uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelAllLocked(reason, compact)
}

// watching reports whether the hub has watchers.
func (h *Hub) watching() bool {
	return h.count.Load() > 0
}

// shutdown stops the hub, canceling its watchers.
func (h *Hub) shutdown() {
	close(h.stop)
	<-h.done
}

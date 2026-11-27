// Copyright Armada Contributors

package watch

import (
	"sync"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage/table/fsm"
)

// maxPendingCommits bounds the commits a hub queues before it stops recording
// them individually and remembers only that revisions were missed.
const maxPendingCommits = 1024

// Hub delivers the events of one table shard to its watchers. It implements
// fsm.Observer for the shard's state machine.
type Hub struct {
	shardID  uint64
	table    string
	registry *Registry

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

// Capturing implements fsm.Observer. Until history can be read back from the
// store, every batch is captured: a batch that is not captured would leave
// its watchers without a way to recover the events.
func (h *Hub) Capturing() bool {
	return true
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
	switch {
	case c.Rewritten || c.Revision < h.revision:
		h.cancelAllLocked(reasonRewritten, 0)
	case !c.Captured:
		if c.Revision > h.revision {
			h.missedLocked(c.Revision)
		}
	default:
		h.dispatchLocked(c)
	}
	h.revision = c.Revision
}

// missedLocked handles revisions up to rev whose events were not recorded.
// Watchers that were waiting for any of them can no longer be served.
func (h *Hub) missedLocked(rev uint64) {
	for w := range h.watchers {
		if h.positionLocked(w) <= rev {
			h.cancelLocked(w, reasonBehind, 0)
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
			h.cancelLocked(w, reasonBehind, 0)
		}
	}
}

// add registers a watcher.
func (h *Hub) add(opts Options, sink *Sink) (*Watcher, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.open {
		return nil, ErrNotServed
	}
	w := newWatcher(h, opts, sink)
	switch {
	case opts.StartRevision == 0:
		w.minRev = h.revision + 1
	case opts.StartRevision > h.revision:
		w.minRev = opts.StartRevision
	default:
		return nil, ErrHistoricalRevision
	}
	h.watchers[w] = struct{}{}
	h.matcher.add(w)
	sink.Push(Message{WatchID: w.id, ShardID: h.shardID, Revision: h.revision, Created: true})
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
	delete(h.watchers, w)
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
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.watchers) > 0
}

// shutdown stops the hub, canceling its watchers.
func (h *Hub) shutdown() {
	close(h.stop)
	<-h.done
}

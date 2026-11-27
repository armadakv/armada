// Copyright Armada Contributors

package watch

import (
	"bytes"

	"github.com/armadakv/armada/armadapb"
)

type watcherState uint8

const (
	// stateSynced watchers are fed by the live path.
	stateSynced watcherState = iota
	// stateCanceled watchers receive nothing more.
	stateCanceled
)

// Watcher is a registered watch. It is created by Registry.Watch.
type Watcher struct {
	id       int64
	hub      *Hub
	sink     *Sink
	key      []byte
	rangeEnd []byte
	noPut    bool
	noDelete bool
	prevKV   bool
	// minRev is the first revision the watcher wants; earlier events are
	// skipped. A watcher created for a future revision waits for it this way.
	minRev uint64

	// state is guarded by hub.mu.
	state watcherState
}

func newWatcher(h *Hub, opts Options, sink *Sink) *Watcher {
	w := &Watcher{
		id:       opts.ID,
		hub:      h,
		sink:     sink,
		key:      bytes.Clone(opts.Key),
		noPut:    opts.NoPut,
		noDelete: opts.NoDelete,
		prevKV:   opts.PrevKV,
	}
	if len(opts.RangeEnd) > 0 {
		w.rangeEnd = bytes.Clone(opts.RangeEnd)
	}
	return w
}

// ID returns the watcher's ID.
func (w *Watcher) ID() int64 {
	return w.id
}

// Cancel unregisters the watcher. A canceled message is queued unless the
// watcher was already canceled.
func (w *Watcher) Cancel() {
	w.hub.cancel(w, "", 0)
}

// single reports whether the watcher watches one key.
func (w *Watcher) single() bool {
	return w.rangeEnd == nil
}

// contains reports whether key is in the watched range.
func (w *Watcher) contains(key []byte) bool {
	if w.single() {
		return bytes.Equal(w.key, key)
	}
	if bytes.Compare(key, w.key) < 0 {
		return false
	}
	if len(w.rangeEnd) == 1 && w.rangeEnd[0] == 0 {
		return true
	}
	return bytes.Compare(key, w.rangeEnd) < 0
}

// wants reports whether the watcher wants the event.
func (w *Watcher) wants(ev *armadapb.Event) bool {
	if uint64(ev.Kv.ModRevision) < w.minRev {
		return false
	}
	switch ev.Type {
	case armadapb.Event_PUT:
		return !w.noPut
	case armadapb.Event_DELETE:
		return !w.noDelete
	}
	return false
}

// view returns the event as the watcher sees it. Events are shared between
// watchers and never modified.
func (w *Watcher) view(ev *armadapb.Event) *armadapb.Event {
	if w.prevKV || ev.PrevKv == nil {
		return ev
	}
	return &armadapb.Event{Type: ev.Type, Kv: ev.Kv}
}

// matcher finds the watchers of a key: single-key watchers by key, range
// watchers by checking their bounds.
type matcher struct {
	keys   map[string]map[*Watcher]struct{}
	ranges map[*Watcher]struct{}
}

func newMatcher() matcher {
	return matcher{keys: make(map[string]map[*Watcher]struct{}), ranges: make(map[*Watcher]struct{})}
}

func (m *matcher) add(w *Watcher) {
	if !w.single() {
		m.ranges[w] = struct{}{}
		return
	}
	ws, ok := m.keys[string(w.key)]
	if !ok {
		ws = make(map[*Watcher]struct{})
		m.keys[string(w.key)] = ws
	}
	ws[w] = struct{}{}
}

func (m *matcher) remove(w *Watcher) {
	if !w.single() {
		delete(m.ranges, w)
		return
	}
	if ws, ok := m.keys[string(w.key)]; ok {
		delete(ws, w)
		if len(ws) == 0 {
			delete(m.keys, string(w.key))
		}
	}
}

func (m *matcher) each(key []byte, f func(*Watcher)) {
	for w := range m.keys[string(key)] {
		f(w)
	}
	for w := range m.ranges {
		if w.contains(key) {
			f(w)
		}
	}
}

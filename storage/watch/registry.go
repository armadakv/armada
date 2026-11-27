// Copyright Armada Contributors

package watch

import (
	"errors"
	"sync"
	"time"

	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table/fsm"
	"go.uber.org/zap"
)

const (
	// defaultServingInterval is how often the registry checks that the watched
	// tables are still served by the shards their watchers are registered on.
	defaultServingInterval = time.Second
	// maxConcurrentScans bounds the catch-up reads running at once on a node.
	maxConcurrentScans = 16
)

// Resolver returns the shard currently serving a table.
type Resolver func(table string) (shardID uint64, err error)

// Scanner reads events back from a shard's store on this node.
type Scanner func(shardID uint64, req fsm.WatchScanRequest) (*fsm.WatchScanResponse, error)

// Registry owns the hubs of all table shards on a node.
type Registry struct {
	resolve  Resolver
	scanner  Scanner
	scans    chan struct{}
	log      *zap.SugaredLogger
	interval time.Duration

	mu     sync.Mutex
	hubs   map[uint64]*Hub
	closed bool

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewRegistry returns a registry resolving tables with resolve and reading
// history with scan.
func NewRegistry(resolve Resolver, scan Scanner) *Registry {
	return &Registry{
		resolve:  resolve,
		scanner:  scan,
		scans:    make(chan struct{}, maxConcurrentScans),
		log:      zap.S().Named("watch"),
		interval: defaultServingInterval,
		hubs:     make(map[uint64]*Hub),
		stop:     make(chan struct{}),
	}
}

// Observer returns the observer for a shard's state machine. It is called
// when the state machine opens.
func (r *Registry) Observer(shardID uint64, table string) fsm.Observer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if h, ok := r.hubs[shardID]; ok {
		return h
	}
	h := newHub(r, shardID, table)
	r.hubs[shardID] = h
	return h
}

// remove unregisters a hub whose state machine closed.
func (r *Registry) remove(h *Hub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.hubs[h.shardID]; ok && cur == h {
		delete(r.hubs, h.shardID)
	}
}

// scan reads history from a shard, waiting for a free scan slot.
func (r *Registry) scan(stop <-chan struct{}, shardID uint64, req fsm.WatchScanRequest) (*fsm.WatchScanResponse, error) {
	select {
	case r.scans <- struct{}{}:
	case <-stop:
		return nil, ErrClosed
	case <-r.stop:
		return nil, ErrClosed
	}
	defer func() { <-r.scans }()
	return r.scanner(shardID, req)
}

// Watch registers a watcher whose messages are queued into sink.
func (r *Registry) Watch(opts Options, sink *Sink) (*Watcher, error) {
	shardID, err := r.resolve(opts.Table)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	h, ok := r.hubs[shardID]
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if !ok {
		return nil, ErrNotServed
	}
	return h.add(opts, sink)
}

// Start starts checking that watched tables keep their serving shards.
func (r *Registry) Start() {
	r.wg.Go(r.monitor)
}

func (r *Registry) monitor() {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.checkServing()
		}
	}
}

// checkServing cancels the watchers of hubs that no longer serve their table:
// the table was deleted, recreated, or its shard was replaced by a recovery.
func (r *Registry) checkServing() {
	r.mu.Lock()
	hubs := make([]*Hub, 0, len(r.hubs))
	for _, h := range r.hubs {
		hubs = append(hubs, h)
	}
	r.mu.Unlock()

	for _, h := range hubs {
		if !h.watching() {
			continue
		}
		shardID, err := r.resolve(h.table)
		switch {
		case errors.Is(err, serrors.ErrTableNotFound):
			h.cancelAll(reasonDeleted, 0)
		case err != nil:
			r.log.Debugf("unable to resolve table %s: %v", h.table, err)
		case shardID != h.shardID:
			h.cancelAll(reasonReplaced, 0)
		}
	}
}

// Close stops the registry and cancels every watcher.
func (r *Registry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	hubs := r.hubs
	r.hubs = make(map[uint64]*Hub)
	r.mu.Unlock()

	close(r.stop)
	r.wg.Wait()
	for _, h := range hubs {
		h.shutdown()
	}
}

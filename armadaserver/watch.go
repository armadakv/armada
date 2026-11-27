// Copyright Armada Contributors

package armadaserver

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/armadakv/armada/armadapb"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/armadakv/armada/storage/watch"
)

// WatchService registers watchers on tables.
type WatchService interface {
	Watch(opts watch.Options, sink *watch.Sink) (*watch.Watcher, error)
	WatchHeader(shardID, revision uint64) *armadapb.ResponseHeader
}

// DefaultMaxWatchersPerStream is the default limit of watchers on one stream.
const DefaultMaxWatchersPerStream = 1000

// WatchServer implements the Watch service from proto/armada.proto.
type WatchServer struct {
	armadapb.UnimplementedWatchServer
	Watches WatchService
	// SinkLimit is the number of event bytes queued per stream before its
	// watchers are considered too slow. Zero uses watch.DefaultSinkLimit.
	SinkLimit int
	// MaxResponseSize is the target size of one watch response. Larger
	// messages are split between revisions, or within a revision for
	// watchers that allow fragments. Zero uses DefaultMaxGRPCSize.
	MaxResponseSize int
	// MaxWatchersPerStream limits the watchers of one stream. Zero uses
	// DefaultMaxWatchersPerStream; a negative value means no limit.
	MaxWatchersPerStream int
}

func (s *WatchServer) maxResponseSize() int {
	if s.MaxResponseSize > 0 {
		return s.MaxResponseSize
	}
	return DefaultMaxGRPCSize
}

func (s *WatchServer) maxWatchers() int {
	if s.MaxWatchersPerStream == 0 {
		return DefaultMaxWatchersPerStream
	}
	return s.MaxWatchersPerStream
}

// Watch implements the Watch service. Create and cancel requests are handled
// in order as they arrive; responses of all watchers on the stream are sent
// in the order they were produced.
func (s *WatchServer) Watch(srv armadapb.Watch_WatchServer) error {
	if s.Watches == nil {
		return s.UnimplementedWatchServer.Watch(srv)
	}
	ctx, cancel := context.WithCancel(srv.Context())
	defer cancel()

	st := &watchStream{
		server:    s,
		srv:       srv,
		sink:      watch.NewSink(s.SinkLimit),
		watchers:  make(map[int64]*watch.Watcher),
		fragments: make(map[int64]bool),
		nextID:    1,
	}
	defer st.close()

	recvErr := make(chan error, 1)
	go func() {
		err := st.recvLoop()
		if err != nil {
			recvErr <- err
			cancel()
		}
	}()

	err := st.sendLoop(ctx)
	select {
	case rerr := <-recvErr:
		return rerr
	default:
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, watch.ErrSinkClosed) {
		return nil
	}
	return err
}

type watchStream struct {
	server *WatchServer
	srv    armadapb.Watch_WatchServer
	sink   *watch.Sink

	mu       sync.Mutex
	watchers map[int64]*watch.Watcher
	// fragments records the watchers that allow a revision to be split.
	fragments map[int64]bool
	nextID    int64
	closed    bool
}

// recvLoop handles requests until the client closes its side of the stream.
func (st *watchStream) recvLoop() error {
	for {
		req, err := st.srv.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch r := req.RequestUnion.(type) {
		case *armadapb.WatchRequest_CreateRequest:
			st.create(r.CreateRequest)
		case *armadapb.WatchRequest_CancelRequest:
			st.cancel(r.CancelRequest.GetWatchId())
		case *armadapb.WatchRequest_ProgressRequest:
			st.requestProgress()
		}
	}
}

func (st *watchStream) sendLoop(ctx context.Context) error {
	// dropping holds the watchers whose revision did not fit a response: they
	// get nothing more until their cancellation, so no revision is skipped.
	dropping := make(map[int64]bool)
	for {
		msgs, err := st.sink.Next(ctx)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Canceled && !m.Created {
				// A rejected create carries the requested ID, which may belong
				// to another watcher, so only a registered watcher is forgotten.
				st.forget(m.WatchID)
				delete(dropping, m.WatchID)
			} else if dropping[m.WatchID] && !m.Created {
				continue
			}
			responses, ok := st.responses(m)
			for _, resp := range responses {
				if err := st.srv.Send(resp); err != nil {
					return err
				}
			}
			if !ok {
				dropping[m.WatchID] = true
				st.tooLarge(m.WatchID)
			}
		}
	}
}

func (st *watchStream) response(m watch.Message, events []*armadapb.Event, fragment bool) *armadapb.WatchResponse {
	return &armadapb.WatchResponse{
		Header:          st.server.Watches.WatchHeader(m.ShardID, m.Revision),
		WatchId:         m.WatchID,
		Created:         m.Created,
		Canceled:        m.Canceled,
		CompactRevision: int64(m.CompactRevision),
		CancelReason:    m.Reason,
		Fragment:        fragment,
		Events:          events,
	}
}

// responses converts a message to responses that fit the maximum response
// size. A message is split between revisions when needed; a single revision
// that does not fit is split into fragments if the watcher allows it. When it
// does not, the responses before that revision are returned with ok == false
// and the watcher has to be canceled.
func (st *watchStream) responses(m watch.Message) ([]*armadapb.WatchResponse, bool) {
	limit := st.server.maxResponseSize()
	if len(m.Events) == 0 || eventsBytes(m.Events) <= limit {
		return []*armadapb.WatchResponse{st.response(m, m.Events, false)}, true
	}
	st.mu.Lock()
	fragments := st.fragments[m.WatchID]
	st.mu.Unlock()

	var (
		res   []*armadapb.WatchResponse
		batch []*armadapb.Event
		size  int
	)
	flush := func() {
		if len(batch) > 0 {
			res = append(res, st.response(m, batch, false))
			batch, size = nil, 0
		}
	}
	for _, rev := range splitByRevision(m.Events) {
		revSize := eventsBytes(rev)
		if size+revSize > limit {
			flush()
		}
		if revSize <= limit {
			batch = append(batch, rev...)
			size += revSize
			continue
		}
		if !fragments {
			return res, false
		}
		// Fragments of one revision: all but the last are marked.
		var part []*armadapb.Event
		partSize := 0
		for _, ev := range rev {
			evSize := ev.SizeVT() + eventFraming
			if len(part) > 0 && partSize+evSize > limit {
				res = append(res, st.response(m, part, true))
				part, partSize = nil, 0
			}
			part = append(part, ev)
			partSize += evSize
		}
		res = append(res, st.response(m, part, false))
	}
	flush()
	return res, true
}

// eventFraming approximates the encoding overhead of one event in a response.
const eventFraming = 8

func eventsBytes(events []*armadapb.Event) int {
	n := 0
	for _, ev := range events {
		n += ev.SizeVT() + eventFraming
	}
	return n
}

// splitByRevision groups events, which are ordered by revision, by revision.
func splitByRevision(events []*armadapb.Event) [][]*armadapb.Event {
	var res [][]*armadapb.Event
	start := 0
	for i := 1; i <= len(events); i++ {
		if i == len(events) || events[i].Kv.ModRevision != events[start].Kv.ModRevision {
			res = append(res, events[start:i])
			start = i
		}
	}
	return res
}

// tooLarge cancels a watcher whose revision does not fit a response.
func (st *watchStream) tooLarge(id int64) {
	st.mu.Lock()
	w := st.watchers[id]
	st.mu.Unlock()
	if w != nil {
		w.CancelWithReason(watch.ReasonResponseTooLarge)
	}
}

func (st *watchStream) requestProgress() {
	st.mu.Lock()
	watchers := make([]*watch.Watcher, 0, len(st.watchers))
	for _, w := range st.watchers {
		if w != nil {
			watchers = append(watchers, w)
		}
	}
	st.mu.Unlock()
	for _, w := range watchers {
		w.RequestProgress()
	}
}

func (st *watchStream) create(req *armadapb.WatchCreateRequest) {
	id, ok := st.reserve(req.GetWatchId(), req.GetFragment())
	if !ok {
		st.reject(req.GetWatchId(), "watch_id is already in use on this stream, or the stream has too many watchers")
		return
	}
	opts, reason := watchOptions(id, req)
	if reason != "" {
		st.release(id)
		st.reject(id, reason)
		return
	}
	w, err := st.server.Watches.Watch(opts, st.sink)
	if err != nil {
		st.release(id)
		var compacted *watch.CompactedError
		if errors.As(err, &compacted) {
			st.sink.Push(watch.Message{WatchID: id, Created: true, Canceled: true, CompactRevision: compacted.CompactRevision, Reason: err.Error()})
			return
		}
		st.reject(id, watchErrorReason(err))
		return
	}
	if !st.bind(id, w) {
		w.Cancel()
	}
}

// reserve claims a watch ID; a zero ID asks for the next free one.
func (st *watchStream) reserve(id int64, fragment bool) (int64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return id, false
	}
	if limit := st.server.maxWatchers(); limit > 0 && len(st.watchers) >= limit {
		return id, false
	}
	if id == 0 {
		for {
			id = st.nextID
			st.nextID++
			if _, used := st.watchers[id]; !used {
				break
			}
		}
	} else if _, used := st.watchers[id]; used {
		return id, false
	}
	st.watchers[id] = nil
	if fragment {
		st.fragments[id] = true
	}
	return id, true
}

func (st *watchStream) bind(id int64, w *watch.Watcher) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return false
	}
	if cur, ok := st.watchers[id]; ok && cur == nil {
		st.watchers[id] = w
	}
	return true
}

func (st *watchStream) release(id int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if w, ok := st.watchers[id]; ok && w == nil {
		delete(st.watchers, id)
		delete(st.fragments, id)
	}
}

// forget drops a watcher whose canceled message is being sent.
func (st *watchStream) forget(id int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if w, ok := st.watchers[id]; ok && w != nil {
		delete(st.watchers, id)
		delete(st.fragments, id)
	}
}

func (st *watchStream) cancel(id int64) {
	st.mu.Lock()
	w := st.watchers[id]
	st.mu.Unlock()
	if w != nil {
		w.Cancel()
	}
}

// reject answers a create request that could not be served.
func (st *watchStream) reject(id int64, reason string) {
	st.sink.Push(watch.Message{WatchID: id, Created: true, Canceled: true, Reason: reason})
}

func (st *watchStream) close() {
	st.sink.Close()
	st.mu.Lock()
	st.closed = true
	watchers := st.watchers
	st.watchers = nil
	st.mu.Unlock()
	for _, w := range watchers {
		if w != nil {
			w.Cancel()
		}
	}
}

// watchOptions validates a create request and converts it to watch options.
// It returns a non-empty reason when the request is invalid.
func watchOptions(id int64, req *armadapb.WatchCreateRequest) (watch.Options, string) {
	switch {
	case len(req.GetTable()) == 0:
		return watch.Options{}, "table must be set"
	case len(req.GetKey()) == 0 && len(req.GetRangeEnd()) == 0:
		return watch.Options{}, "key must be set"
	case rangeKeyContainsNullByte(req.GetKey(), req.GetRangeEnd()):
		return watch.Options{}, "key must not contain null bytes"
	case rangeEndContainsNullByte(req.GetRangeEnd()):
		return watch.Options{}, "range_end must not contain null bytes"
	case len(req.GetKey()) > key.LatestVersionLen || len(req.GetRangeEnd()) > key.LatestVersionLen:
		return watch.Options{}, serrors.ErrKeyLengthExceeded.Error()
	case req.GetStartRevision() < 0:
		return watch.Options{}, "start_revision must not be negative"
	}
	opts := watch.Options{
		ID:             id,
		Table:          string(req.GetTable()),
		Key:            req.GetKey(),
		RangeEnd:       req.GetRangeEnd(),
		StartRevision:  uint64(req.GetStartRevision()),
		PrevKV:         req.GetPrevKv(),
		ProgressNotify: req.GetProgressNotify(),
	}
	if len(opts.Key) == 0 {
		// An empty key with a range end starts at the beginning of the keyspace.
		opts.Key = []byte{0}
	}
	for _, f := range req.GetFilters() {
		switch f {
		case armadapb.WatchCreateRequest_NOPUT:
			opts.NoPut = true
		case armadapb.WatchCreateRequest_NODELETE:
			opts.NoDelete = true
		default:
			return watch.Options{}, "unknown filter"
		}
	}
	return opts, ""
}

func watchErrorReason(err error) string {
	switch {
	case errors.Is(err, serrors.ErrTableNotFound):
		return "table not found"
	default:
		return err.Error()
	}
}

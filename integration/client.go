package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armadakv/armada/armadapb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// wholeKeyspace is the [key, range_end) pair that selects every key: both ends
// are the single zero byte.
var wholeKeyspaceStart, wholeKeyspaceEnd = []byte{0}, []byte{0}

// Leader is the node the harness writes through.
func (e *Env) Leader() *Node { return e.Leaders.Nodes[0] }

// Follower is the node the harness reads the follower cluster through.
func (e *Env) Follower() *Node { return e.Followers.Nodes[0] }

func key(i int) []byte   { return []byte(fmt.Sprintf("key-%08d", i)) }
func value(i int) []byte { return []byte(fmt.Sprintf("value-%08d", i)) }

// ── readiness ────────────────────────────────────────────────────────────────

// awaitLeaderTable creates the table and waits until its shard accepts
// proposals.
//
// The gate has to be a write. Create publishes the table entry before its raft
// shard exists ("shard not found"), and the shard then answers stale reads
// before it can accept proposals ("request dropped as the shard is not ready"),
// so neither the metadata nor a Range proves the cluster is usable.
func (e *Env) awaitLeaderTable(ctx context.Context) {
	t := e.t
	t.Helper()
	tables := armadapb.NewTablesClient(e.Leader().Conn(t))
	kv := armadapb.NewKVClient(e.Leader().Conn(t))

	deadline := time.Now().Add(e.cfg.scale(3 * time.Minute))
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = func() error {
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if _, err := tables.Create(callCtx, &armadapb.CreateTableRequest{Name: e.Table}); err != nil {
				if status.Code(err) != codes.AlreadyExists {
					return fmt.Errorf("create table: %w", err)
				}
			}
			// The canary is deleted again so it never shows up in a key count.
			canary := []byte("__ready__")
			if _, err := kv.Put(callCtx, &armadapb.PutRequest{
				Table: []byte(e.Table), Key: canary, Value: []byte("1"),
			}); err != nil {
				return fmt.Errorf("canary put: %w", err)
			}
			if _, err := kv.DeleteRange(callCtx, &armadapb.DeleteRangeRequest{
				Table: []byte(e.Table), Key: canary,
			}); err != nil {
				return fmt.Errorf("canary delete: %w", err)
			}
			return nil
		}()
		if lastErr == nil {
			shard, _ := e.ShardID(ctx, e.Leader())
			infof("leader table %q ready on shard %d", e.Table, shard)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("leader cluster never served table %q: %v", e.Table, lastErr)
}

// ── reads ────────────────────────────────────────────────────────────────────

// ShardID reports which raft shard is currently serving the table on this node,
// read straight out of a response header rather than from table metadata. It
// changes when a full recovery swaps a freshly loaded shard in, and must not
// change when an incremental delta is applied into the live one.
func (e *Env) ShardID(ctx context.Context, n *Node) (uint64, error) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := armadapb.NewKVClient(n.Conn(e.t)).Range(callCtx, &armadapb.RangeRequest{
		Table:     []byte(e.Table),
		Key:       wholeKeyspaceStart,
		RangeEnd:  wholeKeyspaceEnd,
		CountOnly: true,
	})
	if err != nil {
		return 0, err
	}
	return resp.GetHeader().GetShardId(), nil
}

// CountKeys returns the number of keys the node currently serves for the table.
func (e *Env) CountKeys(ctx context.Context, n *Node) (int64, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := armadapb.NewKVClient(n.Conn(e.t)).Range(callCtx, &armadapb.RangeRequest{
		Table:     []byte(e.Table),
		Key:       wholeKeyspaceStart,
		RangeEnd:  wholeKeyspaceEnd,
		CountOnly: true,
	})
	if err != nil {
		return 0, err
	}
	return resp.GetCount(), nil
}

// AppliedIndex returns the node's raft applied index for the table. On the
// leader cluster it is the authoritative "how far has the source got"; on the
// follower it is the follower's own shard progress, which is what has to keep
// moving for replication to be alive.
func (e *Env) AppliedIndex(ctx context.Context, n *Node) (uint64, error) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := armadapb.NewClusterClient(n.Conn(e.t)).Status(callCtx, &armadapb.StatusRequest{})
	if err != nil {
		return 0, err
	}
	ts, ok := resp.GetTables()[e.Table]
	if !ok {
		return 0, fmt.Errorf("node %s does not report table %q", n.Name, e.Table)
	}
	return ts.GetRaftAppliedIndex(), nil
}

// ── writes ───────────────────────────────────────────────────────────────────

// Load writes keys [from, to) through the leader cluster.
//
// Every Put is one raft entry, so the number of keys written is also how far
// the raft log advances — which is what drives compaction and therefore the GC
// horizon that the recovery scenarios depend on.
func (e *Env) Load(ctx context.Context, from, to int) {
	t := e.t
	t.Helper()
	if to <= from {
		return
	}
	started := time.Now()
	kv := armadapb.NewKVClient(e.Leader().Conn(t))

	next := int64(from)
	var failed atomic.Int64
	var firstErr atomic.Value
	var wg sync.WaitGroup
	for range e.cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1) - 1)
				if i >= to {
					return
				}
				callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, err := kv.Put(callCtx, &armadapb.PutRequest{
					Table: []byte(e.Table), Key: key(i), Value: value(i),
				})
				cancel()
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err)
				}
			}
		}()
	}
	wg.Wait()

	n := to - from
	if f := failed.Load(); f > 0 {
		t.Fatalf("load of keys %d..%d: %d/%d puts failed, first error: %v",
			from, to-1, f, n, firstErr.Load())
	}
	if to > e.written {
		e.written = to
	}
	infof("wrote keys %d..%d (%d puts, concurrency %d) in %s",
		from, to-1, n, e.cfg.Concurrency, time.Since(started).Round(time.Millisecond))
}

// ── verification ─────────────────────────────────────────────────────────────

// VerifyKeys checks that the node holds exactly the keys [from, to) with the
// expected values, and describes any gap it finds as contiguous runs.
//
// Counting keys is not enough: a recovery that drops one range and duplicates
// another still counts correctly. This is the assertion that would catch a
// short delta, which is the failure mode an incremental artefact exported
// below the GC horizon produces.
func (e *Env) VerifyKeys(ctx context.Context, n *Node, from, to int) error {
	got, err := e.scanKeys(ctx, n)
	if err != nil {
		return fmt.Errorf("scan %s: %w", n.Name, err)
	}
	var missing []int
	var wrong []string
	for i := from; i < to; i++ {
		v, ok := got[string(key(i))]
		switch {
		case !ok:
			missing = append(missing, i)
		case v != string(value(i)):
			if len(wrong) < 5 {
				wrong = append(wrong, fmt.Sprintf("%s=%q want %q", key(i), v, value(i)))
			}
		}
	}
	extra := len(got) - ((to - from) - len(missing))
	if len(missing) == 0 && len(wrong) == 0 && extra == 0 {
		return nil
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d expected keys missing, runs: %s",
			len(missing), to-from, describeRuns(missing)))
	}
	if len(wrong) > 0 {
		parts = append(parts, fmt.Sprintf("wrong values: %s", strings.Join(wrong, ", ")))
	}
	if extra != 0 {
		parts = append(parts, fmt.Sprintf("%d key(s) outside [%d,%d) present", extra, from, to))
	}
	return fmt.Errorf("node %s: %s", n.Name, strings.Join(parts, "; "))
}

// scanKeys streams the whole table off one node.
func (e *Env) scanKeys(ctx context.Context, n *Node) (map[string]string, error) {
	callCtx, cancel := context.WithTimeout(ctx, e.cfg.scale(3*time.Minute))
	defer cancel()
	stream, err := armadapb.NewKVClient(n.Conn(e.t)).IterateRange(callCtx, &armadapb.RangeRequest{
		Table:    []byte(e.Table),
		Key:      wholeKeyspaceStart,
		RangeEnd: wholeKeyspaceEnd,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, kv := range resp.GetKvs() {
			out[string(kv.GetKey())] = string(kv.GetValue())
		}
	}
}

// describeRuns collapses a sorted list of indexes into contiguous runs, so that
// "200-219" is reported instead of twenty separate numbers. A single long run
// points at a lost delta or a truncated stream; scattered singletons point at
// dropped individual writes.
func describeRuns(idx []int) string {
	if len(idx) == 0 {
		return "none"
	}
	sort.Ints(idx)
	var runs []string
	start, prev := idx[0], idx[0]
	flush := func() {
		if start == prev {
			runs = append(runs, fmt.Sprintf("%d", start))
		} else {
			runs = append(runs, fmt.Sprintf("%d-%d (%d keys)", start, prev, prev-start+1))
		}
	}
	for _, i := range idx[1:] {
		if i == prev+1 {
			prev = i
			continue
		}
		flush()
		start, prev = i, i
	}
	flush()
	if len(runs) > 10 {
		return fmt.Sprintf("%v and %d more runs", runs[:10], len(runs)-10)
	}
	return fmt.Sprintf("%v", runs)
}

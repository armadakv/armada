package integration

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── polling ──────────────────────────────────────────────────────────────────

// Await polls cond until it holds, failing the test with `what` on timeout.
func (e *Env) Await(ctx context.Context, what string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	if e.TryAwait(ctx, timeout, cond) {
		return
	}
	e.t.Fatalf("timed out after %s waiting for %s", e.cfg.scale(timeout), what)
}

// TryAwait polls cond until it holds, reporting whether it did. Use it for
// preconditions that a scenario can proceed without.
func (e *Env) TryAwait(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	e.t.Helper()
	deadline := time.Now().Add(e.cfg.scale(timeout))
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// AwaitConverged waits until the follower serves the same number of keys as the
// leader, reporting the gap on failure.
func (e *Env) AwaitConverged(ctx context.Context, timeout time.Duration) {
	t := e.t
	t.Helper()
	want, err := e.CountKeys(ctx, e.Leader())
	if err != nil {
		t.Fatalf("count leader keys: %v", err)
	}
	var lastGot int64 = -1
	ok := e.TryAwait(ctx, timeout, func() bool {
		got, err := e.CountKeys(ctx, e.Follower())
		if err != nil {
			return false
		}
		lastGot = got
		return got == want
	})
	if !ok {
		t.Errorf("follower did not converge within %s: leader has %d keys, follower has %d",
			e.cfg.scale(timeout), want, lastGot)
		return
	}
	infof("follower converged to %d keys", want)
}

// ── log marks ────────────────────────────────────────────────────────────────

// LogMark records how much of each node's log had been written at a point in
// time, so that later assertions can be scoped to one scenario.
//
// Marks are offsets rather than in-band markers because the harness cannot
// write into a container's own log stream, and they are re-read live on every
// query so a mark is usable inside a polling loop.
type LogMark struct {
	group *Group
	at    map[string]int
	when  time.Time
	label string
}

// Mark takes a log mark across every node in the group.
func (g *Group) Mark(ctx context.Context, label string) LogMark {
	m := LogMark{group: g, at: map[string]int{}, when: time.Now(), label: label}
	for _, n := range g.Nodes {
		m.at[n.Name] = len(n.Logs(ctx))
	}
	return m
}

// PerNode returns each node's log written since the mark.
func (m LogMark) PerNode(ctx context.Context) map[string]string {
	out := make(map[string]string, len(m.group.Nodes))
	for _, n := range m.group.Nodes {
		full := n.Logs(ctx)
		off := m.at[n.Name]
		if off > len(full) {
			// The container was recreated, so its log restarted from zero and
			// the old offset is meaningless. Everything present is new.
			off = 0
		}
		out[n.Name] = full[off:]
	}
	return out
}

// Count is how many lines match across every node in the group.
func (m LogMark) Count(ctx context.Context, pattern string) int {
	total := 0
	for _, n := range m.CountPerNode(ctx, pattern) {
		total += n
	}
	return total
}

// CountPerNode attributes matches to individual nodes. This is what makes
// "exactly one node loaded the snapshot" checkable: the whole point of
// learner-first recovery is that the work is not replicated across peers.
func (m LogMark) CountPerNode(ctx context.Context, pattern string) map[string]int {
	out := map[string]int{}
	for name, text := range m.PerNode(ctx) {
		if n := strings.Count(text, pattern); n > 0 {
			out[name] = n
		}
	}
	return out
}

// Nodes lists the nodes whose log matched, sorted by name.
func (m LogMark) Nodes(ctx context.Context, pattern string) []string {
	per := m.CountPerNode(ctx, pattern)
	names := make([]string, 0, len(per))
	for name := range per {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Has reports whether any node matched.
func (m LogMark) Has(ctx context.Context, pattern string) bool {
	for _, text := range m.PerNode(ctx) {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

// Lines returns every matching line, prefixed with the node it came from.
func (m LogMark) Lines(ctx context.Context, pattern string) []string {
	var out []string
	per := m.PerNode(ctx)
	for _, name := range sortedKeys(per) {
		for line := range strings.SplitSeq(per[name], "\n") {
			if strings.Contains(line, pattern) {
				out = append(out, name+": "+strings.TrimSpace(line))
			}
		}
	}
	return out
}

// ── log assertions ───────────────────────────────────────────────────────────

// MustContain asserts at least one node logged the pattern.
func (m LogMark) MustContain(ctx context.Context, t *testing.T, pattern, label string) {
	t.Helper()
	per := m.CountPerNode(ctx, pattern)
	if len(per) == 0 {
		t.Errorf("%s: no node logged %q since %s", label, pattern, m.label)
		return
	}
	infof("✓ %s (%s)", label, describeCounts(per))
}

// MustNotContain asserts no node logged the pattern.
func (m LogMark) MustNotContain(ctx context.Context, t *testing.T, pattern, label string) {
	t.Helper()
	per := m.CountPerNode(ctx, pattern)
	if len(per) > 0 {
		t.Errorf("%s: unexpected %q (%s)", label, pattern, describeCounts(per))
		for _, l := range m.Lines(ctx, pattern) {
			infof("    %s", l)
		}
		return
	}
	infof("✓ %s", label)
}

// MustCount asserts an exact number of matches across the group.
func (m LogMark) MustCount(ctx context.Context, t *testing.T, pattern string, want int, label string) {
	t.Helper()
	per := m.CountPerNode(ctx, pattern)
	got := 0
	for _, n := range per {
		got += n
	}
	if got != want {
		t.Errorf("%s: want %d occurrences of %q, got %d (%s)", label, want, pattern, got, describeCounts(per))
		for _, l := range m.Lines(ctx, pattern) {
			infof("    %s", l)
		}
		return
	}
	infof("✓ %s (%s)", label, describeCounts(per))
}

// AwaitLog waits for a pattern to appear on any node since the mark.
func (e *Env) AwaitLog(ctx context.Context, m LogMark, pattern string, timeout time.Duration, what string) bool {
	e.t.Helper()
	if e.TryAwait(ctx, timeout, func() bool { return m.Has(ctx, pattern) }) {
		return true
	}
	e.t.Errorf("timed out after %s waiting for %s (log pattern %q)", e.cfg.scale(timeout), what, pattern)
	return false
}

// ── structured log readings ──────────────────────────────────────────────────

// Negotiation is one "recovering from X snapshot object=… (base=… tip=…)" line:
// the artefact a follower node actually chose.
//
// Parsing it beats substring-matching for the artefact type. A follower that
// silently degrades to the leader's on-demand live snapshot still converges, so
// every other assertion passes while the shared store does nothing — which is
// how a bad artefact filter hid itself for several runs of the shell harness.
type Negotiation struct {
	Node      string
	Type      string // "full", "incr" or "live"
	ObjectKey string
	BaseIndex uint64
	TipIndex  uint64
}

func (n Negotiation) String() string {
	return fmt.Sprintf("%s: %s %s (base=%d tip=%d)", n.Node, n.Type, n.ObjectKey, n.BaseIndex, n.TipIndex)
}

var negotiationRE = regexp.MustCompile(
	`recovering from (\w+) snapshot object=(\S+) \(base=(\d+) tip=(\d+)\)`)

// Negotiations lists every artefact chosen since the mark, in log order per
// node.
func (m LogMark) Negotiations(ctx context.Context) []Negotiation {
	var out []Negotiation
	per := m.PerNode(ctx)
	for _, name := range sortedKeys(per) {
		for _, g := range negotiationRE.FindAllStringSubmatch(per[name], -1) {
			out = append(out, Negotiation{
				Node:      name,
				Type:      g[1],
				ObjectKey: g[2],
				BaseIndex: mustUint(g[3]),
				TipIndex:  mustUint(g[4]),
			})
		}
	}
	return out
}

// Swap is one completed recovery: the moment the table starts being served
// from the recovered shard.
type Swap struct {
	Node        string
	ShardID     uint64
	SourceIndex uint64
	Artefact    string
	TipIndex    uint64
}

func (s Swap) String() string {
	return fmt.Sprintf("%s: shard %d at source index %d (artefact %s tip %d)",
		s.Node, s.ShardID, s.SourceIndex, s.Artefact, s.TipIndex)
}

var swapRE = regexp.MustCompile(
	`recovery complete, serving shard is now (\d+) at source index (\d+) \(artefact (\w+) tip (\d+)\)`)

// Swaps lists every completed recovery since the mark.
//
// More than one means the follower recovered, failed to resume log
// replication, and recovered again — each round allocating a fresh shard. That
// churn is the regression this harness exists to catch.
func (m LogMark) Swaps(ctx context.Context) []Swap {
	var out []Swap
	per := m.PerNode(ctx)
	for _, name := range sortedKeys(per) {
		for _, g := range swapRE.FindAllStringSubmatch(per[name], -1) {
			out = append(out, Swap{
				Node:        name,
				ShardID:     mustUint(g[1]),
				SourceIndex: mustUint(g[2]),
				Artefact:    g[3],
				TipIndex:    mustUint(g[4]),
			})
		}
	}
	return out
}

// LearnerShards returns the recovery shard ids that were seeded with learners
// since the mark, mapped to the nodes that logged each one.
//
// Counting seeding *episodes by shard* is what makes the incremental assertion
// meaningful. An incremental artefact is replayed straight into the live shard
// and never seeds anything, but a recovery chain may legitimately continue past
// a delta into a full — at which point learners do appear, for that full's
// shard. Comparing shard count against the number of non-incremental artefacts
// distinguishes "the delta wrongly built a shard" from "the chain moved on".
func (m LogMark) LearnerShards(ctx context.Context) map[uint64][]string {
	out := map[uint64][]string{}
	per := m.PerNode(ctx)
	for _, name := range sortedKeys(per) {
		for _, g := range learnerRE.FindAllStringSubmatch(per[name], -1) {
			id := mustUint(g[1])
			out[id] = append(out[id], name)
		}
	}
	return out
}

// Both the coordinator ("node N added to recovery shard S as a learner") and
// the peer ("joined recovery shard S as a learner") name the shard.
var learnerRE = regexp.MustCompile(`recovery shard (\d+) as a learner`)

// ── small helpers ────────────────────────────────────────────────────────────

func describeCounts(per map[string]int) string {
	if len(per) == 0 {
		return "no matches"
	}
	parts := make([]string, 0, len(per))
	for _, name := range sortedKeys(per) {
		parts = append(parts, fmt.Sprintf("%s×%d", name, per[name]))
	}
	return strings.Join(parts, " ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustUint(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

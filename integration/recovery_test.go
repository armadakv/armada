package integration

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	armadatable "github.com/armadakv/armada/storage/table"
)

// TestRecovery drives the RFC 005 recovery paths against two real three-node
// clusters.
//
// Each scenario owns its own environment. Recovery cases deliberately wipe
// data, rebuild clusters, kill processes, and alter leader configuration; sharing
// those side effects made a later scenario diagnose an earlier one's leftovers
// instead of its own behaviour.
//
// This costs additional container startup time, but makes both the full suite
// and individual subtests deterministic:
//
//	go test -run '^TestRecovery/incremental$' ./...
func TestRecovery(t *testing.T) {
	run := func(name string, scenario func(*Env, *testing.T, context.Context)) {
		t.Run(name, func(t *testing.T) {
			env := New(t)
			scenario(env, t, context.Background())
		})
	}

	run("baseline", (*Env).scenarioBaseline)
	run("full", (*Env).scenarioFull)
	run("incremental", (*Env).scenarioIncremental)
	run("powerloss", (*Env).scenarioPowerloss)
	run("direct", (*Env).scenarioDirect)
}

// use points the harness's own helpers at the running subtest and restores the
// previous one on return.
//
// Env holds a *testing.T so that helpers can fail without every call site
// threading one through, but t.FailNow must be called from the goroutine of the
// test it belongs to — leaving Env pointing at the parent would abort the wrong
// test and mis-attribute the failure. The scenarios never run in parallel, so a
// single swap is enough.
func (e *Env) use(t *testing.T) func() {
	prev := e.t
	e.t = t
	restoreLog := setActive(t)
	return func() {
		restoreLog()
		e.t = prev
	}
}

// seed writes the baseline data set and waits for the follower to catch up. It
// is idempotent so that any scenario can be run on its own.
func (e *Env) seed(t *testing.T, ctx context.Context) {
	t.Helper()
	e.requireLeader(t, ctx)
	if e.seeded {
		return
	}
	e.Load(ctx, 0, e.cfg.Keys)
	got, err := e.CountKeys(ctx, e.Leader())
	if err != nil {
		t.Fatalf("count leader keys: %v", err)
	}
	if got != int64(e.cfg.Keys) {
		t.Fatalf("leader holds %d keys after writing %d", got, e.cfg.Keys)
	}
	e.AwaitConverged(ctx, 3*time.Minute)
	e.seeded = true
}

// requireLeader fails the scenario immediately if the leader cluster is not
// answering.
//
// An unavailable leader makes the scenario's result meaningless. Checking up
// front turns a later recovery timeout into an immediate, honest failure.
func (e *Env) requireLeader(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := e.CountKeys(ctx, e.Leader()); err != nil {
		t.Fatalf("leader cluster is not answering on %s: %v", e.Leader().APIAddr, err)
	}
}

// ── baseline ─────────────────────────────────────────────────────────────────

// scenarioBaseline establishes that plain tail replication works. Everything
// else builds on it, and a failure here means none of the recovery diagnoses
// below can be trusted.
func (e *Env) scenarioBaseline(t *testing.T, ctx context.Context) {
	defer e.use(t)()
	e.seed(t, ctx)
	if err := e.VerifyKeys(ctx, e.Follower(), 0, e.written); err != nil {
		t.Errorf("follower contents after tail replication: %v", err)
	} else {
		infof("✓ follower holds every key with the expected value")
	}
	infof("shared store: %s", e.DescribeStore())
}

// ── full ─────────────────────────────────────────────────────────────────────

// scenarioFull checks that a full recovery is learner-first: one node loads the
// artefact, the peers join the recovery shard as non-voting learners and are
// fed by raft's own snapshot path, then they are promoted and the shard is
// swapped in.
func (e *Env) scenarioFull(t *testing.T, ctx context.Context) {
	defer e.use(t)()
	e.seed(t, ctx)
	e.AwaitFullExport(ctx)
	e.CheckStore(t)

	// Wiping follower state forces a full recovery: no local shard, so nothing
	// to apply a delta onto.
	if err := e.Followers.Stop(ctx); err != nil {
		t.Fatalf("stop follower cluster: %v", err)
	}
	e.Followers.WipeData(t)
	mark := e.Followers.Mark(ctx, "full")
	if err := e.Followers.Start(ctx); err != nil {
		t.Fatalf("restart follower cluster: %v", err)
	}

	// Wait for the swap, not just for the key count. A follower can reach the
	// right key count purely by tailing the leader's log when the leader has
	// not compacted yet, which says nothing about learner-first having worked.
	if !e.AwaitLog(ctx, mark, "recovery complete, serving shard is now", 7*time.Minute,
		"learner-first recovery to complete the shard swap") {
		return
	}
	e.AwaitConverged(ctx, 5*time.Minute)

	// Insist on a shared-store artefact. Degrading to the leader's on-demand
	// live snapshot still converges, so every other assertion here would pass
	// while the shared store did nothing.
	negotiations := mark.Negotiations(ctx)
	switch {
	case len(negotiations) == 0:
		t.Errorf("no node logged an artefact negotiation; the recovery path is unknown")
	case len(negotiations) > 1:
		t.Errorf("want exactly one node to load the snapshot, got %d negotiations: %v",
			len(negotiations), negotiations)
	case negotiations[0].Type != armadatable.ArtifactFull:
		t.Errorf("want the FULL path from the shared store, negotiated %v", negotiations[0])
	default:
		infof("✓ took the FULL path from the shared store: %v", negotiations[0])
		infof("✓ exactly one node loaded the snapshot (%s)", negotiations[0].Node)
	}

	// More than one swap means the follower recovered, failed to resume log
	// replication, and recovered again — each round allocating a fresh shard.
	if swaps := mark.Swaps(ctx); len(swaps) != 1 {
		t.Errorf("want exactly one shard swap (no re-recovery churn), got %d: %v", len(swaps), swaps)
	} else {
		infof("✓ recovery ran exactly once: %v", swaps[0])
	}

	mark.MustContain(ctx, t, "as a learner", "peers joined the recovery shard as non-voting learners")
	mark.MustContain(ctx, t, "promoted to voter", "learners were promoted to voters")

	if err := e.VerifyKeys(ctx, e.Follower(), 0, e.written); err != nil {
		t.Errorf("follower contents after full recovery: %v", err)
	} else {
		infof("✓ recovered follower holds every key with the expected value")
	}
}

// ── incremental ──────────────────────────────────────────────────────────────

// scenarioIncremental checks that a follower inside the delta window applies an
// incremental artefact into its live shard, without building a recovery shard.
//
// The window is narrow, and narrow for two independent reasons that pull in
// opposite directions:
//
//	legal   — a delta must exist whose base is at or below the follower's index
//	          and whose tip is beyond it. Bases are the tips of previous
//	          exports and keep advancing, so the window shuts for good as soon
//	          as any artefact is published past the follower.
//	needed  — the follower must also sit at or below the leader's GC horizon,
//	          otherwise ordinary log replication just tails the gap and no
//	          recovery happens at all.
//
// Those two cannot be satisfied by writing a fixed amount of data, because
// where the horizon and the next export land depends on when raft happens to
// compact. So the scenario writes in small chunks and watches the shared store
// until both conditions hold, then stops. If the window shuts first it says so
// with the numbers it measured, rather than leaving a bare "took the wrong
// path".
func (e *Env) scenarioIncremental(t *testing.T, ctx context.Context) {
	defer e.use(t)()
	e.seed(t, ctx)

	// Silence the full-snapshot ticker for the duration of the setup, and put
	// it back afterwards so the later scenarios still find a fresh base.
	restore := e.cfg.FullInterval
	e.SetLeaderFullInterval(ctx, 30*time.Minute)
	t.Cleanup(func() {
		// Cleanups run after the scenario's own defers, so the harness is
		// pointing back at the parent test by now; aim it at this subtest again
		// so a failure to restore is reported against the scenario that caused
		// it.
		defer e.use(t)()
		e.SetLeaderFullInterval(context.Background(), restore)
	})

	shardBefore, err := e.ShardID(ctx, e.Follower())
	if err != nil {
		t.Fatalf("read follower shard id: %v", err)
	}
	infof("follower shard before: %d", shardBefore)

	e.AwaitConverged(ctx, 3*time.Minute)
	// No API exposes the follower's source watermark, so use the leader's
	// applied index while the follower is converged and the leader idle.
	//
	// This is an *upper* bound, not an equality: the follower's watermark
	// tracks the last replicated command, while the leader's applied index can
	// include entries beyond it. The error direction is deliberate — an
	// overestimated index makes usableDelta stricter, so the scenario can skip
	// when it could in fact have run, but it can never claim the window was
	// open when it was not. A false skip costs coverage; a false pass would
	// cost the point of the test.
	followerAt, err := e.AppliedIndex(ctx, e.Leader())
	if err != nil {
		t.Fatalf("read leader applied index: %v", err)
	}
	infof("follower is caught up at index <=%d (leader applied index while idle); store: %s",
		followerAt, e.DescribeStore())

	if err := e.Followers.Stop(ctx); err != nil {
		t.Fatalf("stop follower cluster: %v", err)
	}
	delta, why := e.openDeltaWindow(ctx, followerAt)

	mark := e.Followers.Mark(ctx, "incremental")
	if err := e.Followers.Start(ctx); err != nil {
		t.Fatalf("restart follower cluster: %v", err)
	}
	e.AwaitConverged(ctx, 5*time.Minute)

	// Contents first: whichever path was taken, the data has to be right, and
	// a skipped precondition must not skip that check.
	if err := e.VerifyKeys(ctx, e.Follower(), 0, e.written); err != nil {
		t.Errorf("follower contents after recovery: %v", err)
	} else {
		infof("✓ follower holds every key with the expected value")
	}

	negotiations := mark.Negotiations(ctx)
	if delta == nil {
		t.Skipf("could not open the delta window for a follower at index <=%d.\n    %s\n    the follower instead negotiated: %v",
			followerAt, why, negotiations)
	}

	// The decision under test is the first one the follower makes. What the
	// chain does afterwards is a separate, legitimate question: one delta only
	// carries the follower as far as its tip, and if the leader compacted past
	// that tip in the meantime the next link can only be a full.
	switch {
	case len(negotiations) == 0:
		t.Errorf("%s covered the follower at index %d but it negotiated no artefact at all",
			delta.Name(), followerAt)
		return
	case negotiations[0].Type != armadatable.ArtifactIncremental:
		t.Errorf("%s covered the follower at index %d but it took the %s path instead; negotiated: %v",
			delta.Name(), followerAt, negotiations[0].Type, negotiations)
		return
	}

	infof("✓ took the INCREMENTAL path: %v", negotiations[0])
	if len(negotiations) > 1 {
		infof("the chain continued past the delta (%v); the delta's tip left the follower "+
			"below the leader's gc horizon, so tailing the log was not an option",
			negotiations[1:])
	}

	// The defining property: a delta is replayed into the existing shard, so
	// the table keeps its shard id. A full recovery would have swapped in a
	// new one.
	//
	// This only holds when the delta was the whole chain. Once a full link
	// follows, the shard id is *expected* to change, and reading it races that
	// link's swap — so in that case the delta's innocence is established from
	// the seeded-shard count below instead, which does not depend on timing.
	if len(negotiations) == 1 {
		if shardAfter, err := e.ShardID(ctx, e.Follower()); err != nil {
			t.Errorf("read follower shard id: %v", err)
		} else if shardAfter != shardBefore {
			t.Errorf("shard id changed from %d to %d: a recovery shard was built for a delta",
				shardBefore, shardAfter)
		} else {
			infof("✓ shard id unchanged (%d): no recovery shard built", shardAfter)
		}
	}

	// An incremental seeds no learners. Asserting a flat zero would be wrong
	// though, because a later full link in the same chain legitimately seeds
	// its own shard — so require one seeded shard per non-incremental artefact
	// and no more. A delta that wrongly built a shard shows up as the extra.
	wantSeeded := 0
	for _, n := range negotiations {
		if n.Type != armadatable.ArtifactIncremental {
			wantSeeded++
		}
	}
	seeded := mark.LearnerShards(ctx)
	switch {
	case len(seeded) == wantSeeded && wantSeeded == 0:
		infof("✓ no learner seeding for an incremental")
	case len(seeded) == wantSeeded:
		infof("✓ learner seeding only for the %d non-incremental link(s) in the chain (shards %v)",
			wantSeeded, sortedShards(seeded))
	default:
		t.Errorf("want %d seeded recovery shard(s) (one per non-incremental artefact), got %d: %v",
			wantSeeded, len(seeded), seeded)
	}
}

// openDeltaWindow writes to the leader, a chunk at a time, until a follower
// stopped at followerAt both must and can recover from a delta. It returns the
// delta, or nil and the measured reason the window could not be opened.
//
// The leader must be running with the full-snapshot ticker silenced, otherwise
// a ticker full can land past followerAt and shut the window mid-loop.
func (e *Env) openDeltaWindow(ctx context.Context, followerAt uint64) (*Artefact, string) {
	e.t.Helper()
	from := e.written
	for at := from; at < from+e.cfg.IncrGap; at += e.cfg.IncrChunk {
		e.Load(ctx, at, min(at+e.cfg.IncrChunk, from+e.cfg.IncrGap))

		// Exports are asynchronous, so give the exporter a moment to react to
		// the compaction this chunk may have triggered before judging the store.
		e.TryAwait(ctx, 20*time.Second, func() bool {
			return e.usableDelta(followerAt) != nil && e.HorizonEstimate() > followerAt
		})

		delta, horizon := e.usableDelta(followerAt), e.HorizonEstimate()
		switch {
		case delta != nil && horizon > followerAt:
			infof("delta window open: %s covers a follower at index %d, which is now below the leader's gc horizon %d",
				delta.Name(), followerAt, horizon)
			return delta, ""
		case delta == nil && e.NewestArtefactTip() > followerAt:
			return nil, fmt.Sprintf(
				"the newest artefact tip is already %d, past the follower at %d, so every future delta's base will be past it too and the window is shut for good.\n"+
					"    gc horizon ~%d; store: %s\n    %s",
				e.NewestArtefactTip(), followerAt, horizon, e.DescribeStore(),
				strings.Join(e.explainDeltas(followerAt), "\n    "))
		}
	}
	return nil, fmt.Sprintf(
		"wrote %d keys without the follower at %d being both below the gc horizon (~%d) and covered by a delta.\n    store: %s\n    %s",
		e.cfg.IncrGap, followerAt, e.HorizonEstimate(), e.DescribeStore(),
		strings.Join(e.explainDeltas(followerAt), "\n    "))
}

// usableDelta returns the delta the negotiator would pick for a follower at the
// given index, or nil when none can serve it.
//
// It mirrors the production rules rather than guessing, which is what lets the
// scenario demand the incremental path instead of merely hoping for it:
//
//   - SelectBestSnapshot picks the highest BaseIndex at or below the follower
//     whose TipIndex is beyond it, and a chained delta (base > 0) therefore
//     always outranks a full (base 0) when both are candidates.
//   - An incremental only holds a complete delta above the GC horizon recorded
//     at export time; at or below it the compacted versions are simply missing
//     and the delta would be silently short, so the negotiator falls back to a
//     full. An artefact with no recorded horizon has unknown provenance and is
//     rejected for the same reason.
//   - ResumableSnapshots then drops anything whose tip is already below the
//     live horizon, since a follower restored to that tip would land below the
//     horizon again and need a second recovery immediately.
func (e *Env) usableDelta(followerIndex uint64) *Artefact {
	horizon := e.HorizonEstimate()
	var best *Artefact
	for _, a := range e.ChainedIncrementals() {
		if a.BaseIndex > followerIndex || a.TipIndex <= followerIndex {
			continue
		}
		if a.GCHorizon == 0 || a.BaseIndex <= a.GCHorizon {
			continue
		}
		if a.TipIndex < horizon {
			continue
		}
		if best == nil || a.BaseIndex > best.BaseIndex {
			best = &a
		}
	}
	return best
}

// explainDeltas reports, delta by delta, which of the negotiator's rules
// rejects it for a follower at followerIndex — or that it is usable.
//
// "No delta covers the follower" is not a diagnosis: there are four separate
// reasons a delta can be unusable, they fail at three different layers, and
// which one fired is the whole of what a skipped run has to say. An artefact
// whose base reaches the follower but whose tip does not move it, and one that
// has since dropped below the live horizon, are completely different problems.
func (e *Env) explainDeltas(followerIndex uint64) []string {
	deltas := e.ChainedIncrementals()
	if len(deltas) == 0 {
		return []string{"the store holds no chained delta at all — every export so far was a full or a base-0 incremental"}
	}
	horizon := e.HorizonEstimate()
	out := make([]string, 0, len(deltas))
	for _, a := range deltas {
		switch {
		case a.BaseIndex > followerIndex:
			out = append(out, fmt.Sprintf("%s: base %d is past the follower at %d, so applying it would skip data",
				a.Name(), a.BaseIndex, followerIndex))
		case a.TipIndex <= followerIndex:
			out = append(out, fmt.Sprintf("%s: base %d does reach the follower at %d, but tip %d is not past it, so it would carry the follower nowhere (SelectBestSnapshot skips it)",
				a.Name(), a.BaseIndex, followerIndex, a.TipIndex))
		case a.GCHorizon == 0:
			out = append(out, fmt.Sprintf("%s: no gc horizon recorded at export, so its provenance is unknown and the negotiator will not trust it",
				a.Name()))
		case a.BaseIndex <= a.GCHorizon:
			out = append(out, fmt.Sprintf("%s: base %d is at or below the gc horizon %d recorded at export, so the delta may be silently short",
				a.Name(), a.BaseIndex, a.GCHorizon))
		case a.TipIndex < horizon:
			out = append(out, fmt.Sprintf("%s: tip %d has fallen below the live gc horizon ~%d, so restoring to it would land below the horizon again (ResumableSnapshots drops it)",
				a.Name(), a.TipIndex, horizon))
		default:
			out = append(out, fmt.Sprintf("%s: usable", a.Name()))
		}
	}
	return out
}

// sortedShards renders a shard-id set for a log line.
func sortedShards(m map[uint64][]string) []uint64 {
	out := make([]uint64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// ── powerloss ────────────────────────────────────────────────────────────────

// scenarioPowerloss SIGKILLs the node that is actually driving a recovery,
// mid-flight, and requires it to resume from the durable journal rather than
// abandon the recovery and start over.
func (e *Env) scenarioPowerloss(t *testing.T, ctx context.Context) {
	defer e.use(t)()
	e.seed(t, ctx)
	e.AwaitFullExport(ctx)

	if err := e.Followers.Stop(ctx); err != nil {
		t.Fatalf("stop follower cluster: %v", err)
	}
	e.Followers.WipeData(t)
	mark := e.Followers.Mark(ctx, "powerloss")
	if err := e.Followers.Start(ctx); err != nil {
		t.Fatalf("restart follower cluster: %v", err)
	}

	// Pull the plug on the coordinator specifically, not on whichever node
	// happens to be listed first: the coordinator is the only node holding the
	// recovery journal's lease and the only one whose loss exercises resume.
	victim := -1
	found := e.TryAwait(ctx, 4*time.Minute, func() bool {
		for i, n := range e.Followers.Nodes {
			if mark.CountPerNode(ctx, "recovering from")[n.Name] > 0 {
				victim = i
				return true
			}
		}
		return false
	})
	if !found {
		t.Fatalf("no recovery started within the timeout, cannot test power loss")
	}
	coordinator := e.Followers.Nodes[victim]
	infof("recovery coordinator is %s; SIGKILLing it mid-recovery", coordinator.Name)
	if err := e.Followers.Kill(ctx, victim); err != nil {
		t.Fatalf("kill %s: %v", coordinator.Name, err)
	}
	time.Sleep(2 * time.Second)
	if err := e.Followers.Revive(ctx, victim); err != nil {
		t.Fatalf("revive %s: %v", coordinator.Name, err)
	}
	infof("revived %s", coordinator.Name)

	e.AwaitConverged(ctx, 7*time.Minute)

	mark.MustContain(ctx, t, "recovery complete, serving shard is now", "recovery finished after the kill")
	// A restart must resume from the journal — reusing its shard id and its
	// staged download — not tear the recovery down and allocate a new one.
	mark.MustNotContain(ctx, t, "abandoning recovery in phase", "recovery was resumed, not abandoned")

	if shard, err := e.ShardID(ctx, e.Follower()); err != nil {
		t.Errorf("follower serves no shard after power loss: %v", err)
	} else {
		infof("✓ follower serves shard %d after power loss", shard)
	}
	if err := e.VerifyKeys(ctx, e.Follower(), 0, e.written); err != nil {
		t.Errorf("follower contents after power loss: %v", err)
	} else {
		infof("✓ follower holds every key with the expected value")
	}
}

// ── direct ───────────────────────────────────────────────────────────────────

// scenarioDirect runs the recovery with the follower reading the shared bucket
// itself instead of proxying through the leader.
//
// This path used to be handed the HTTP-only "snapshots-live/{table}" key and
// fail with ErrNotExist, so direct mode could not recover at all; it is worth
// covering separately.
func (e *Env) scenarioDirect(t *testing.T, ctx context.Context) {
	defer e.use(t)()
	e.seed(t, ctx)
	e.AwaitFullExport(ctx)

	// Restore proxy mode afterwards so the scenario is reorderable.
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = e.Followers.Stop(cleanupCtx)
		e.Followers.WipeData(t)
		e.Followers.SetSnapshotSource(SourceProxy)
		e.Followers.Rebuild(cleanupCtx)
		_ = e.Followers.Start(cleanupCtx)
	})

	if err := e.Followers.Stop(ctx); err != nil {
		t.Fatalf("stop follower cluster: %v", err)
	}
	e.Followers.WipeData(t)
	// The snapshot source is a flag, and argv is fixed at container creation,
	// so the containers have to be rebuilt rather than restarted.
	e.Followers.SetSnapshotSource(SourceDirect)
	e.Followers.Rebuild(ctx)
	mark := e.Followers.Mark(ctx, "direct")
	if err := e.Followers.Start(ctx); err != nil {
		t.Fatalf("start follower cluster in direct mode: %v", err)
	}

	e.AwaitConverged(ctx, 6*time.Minute)

	negotiations := mark.Negotiations(ctx)
	if len(negotiations) == 0 {
		t.Errorf("direct-mode follower negotiated no artefact")
	} else {
		infof("✓ direct-mode follower negotiated %v", negotiations)
	}
	for _, n := range negotiations {
		if n.Type == armadatable.ArtifactLive {
			t.Errorf("direct mode fell back to the leader's live snapshot: %v", n)
		}
	}
	// Regression guard: the live fallback's object key is an HTTP route, not a
	// bucket object, so a direct-mode follower reaching for it always fails.
	mark.MustNotContain(ctx, t, "snapshots-live", "did not reach for the HTTP-only live key")

	if err := e.VerifyKeys(ctx, e.Follower(), 0, e.written); err != nil {
		t.Errorf("follower contents after direct-mode recovery: %v", err)
	} else {
		infof("✓ follower holds every key with the expected value")
	}
}

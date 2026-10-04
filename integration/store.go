package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/replication/store"
)

// Artefact is one committed snapshot in the shared store, together with the
// on-disk paths the harness verifies it against.
type Artefact struct {
	store.Meta
	MetaPath string
	SnapPath string
}

// Name is the artefact's identity as it appears in object keys and log lines:
// "full/1162" or "incr/2009_2060".
func (a Artefact) Name() string {
	if a.Type == store.SnapshotTypeIncremental {
		return fmt.Sprintf("incr/%d_%d", a.BaseIndex, a.TipIndex)
	}
	return fmt.Sprintf("full/%d", a.TipIndex)
}

// Chained reports whether this is a real delta off a prior base rather than a
// whole-keyspace base-0 export. A base-0 incremental is rejected by the
// negotiator as unsound for a non-empty follower, so it cannot drive the
// incremental path.
func (a Artefact) Chained() bool {
	return a.Type == store.SnapshotTypeIncremental && a.BaseIndex > 0
}

// Artefacts lists every committed artefact for the harness table.
//
// Only artefacts with a .meta commit file count: the exporter writes the .snap
// first and the .meta only after the upload is verified, so a .snap without a
// .meta is an artefact in flight and followers must ignore it.
func (e *Env) Artefacts() ([]Artefact, error) {
	var out []Artefact
	for _, kind := range []string{"full", "incr"} {
		dir := filepath.Join(e.StoreDir, "snapshots", e.Table, kind)
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".snap.meta") {
				continue
			}
			metaPath := filepath.Join(dir, ent.Name())
			raw, err := os.ReadFile(metaPath)
			if err != nil {
				return nil, err
			}
			var m store.Meta
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("%s: %w", metaPath, err)
			}
			out = append(out, Artefact{
				Meta:     m,
				MetaPath: metaPath,
				SnapPath: strings.TrimSuffix(metaPath, ".meta"),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		if out[i].BaseIndex != out[j].BaseIndex {
			return out[i].BaseIndex < out[j].BaseIndex
		}
		return out[i].TipIndex < out[j].TipIndex
	})
	return out, nil
}

// artefactsOfType filters by snapshot type, ignoring read errors the way a
// polling predicate wants to.
func (e *Env) artefactsOfType(typ store.SnapshotType) []Artefact {
	all, err := e.Artefacts()
	if err != nil {
		return nil
	}
	var out []Artefact
	for _, a := range all {
		if a.Type == typ {
			out = append(out, a)
		}
	}
	return out
}

// Fulls returns the committed full artefacts. Every incremental chain is
// anchored on one of these.
func (e *Env) Fulls() []Artefact { return e.artefactsOfType(store.SnapshotTypeFull) }

// Incrementals returns the committed incremental artefacts, base-0 ones
// included.
func (e *Env) Incrementals() []Artefact {
	return e.artefactsOfType(store.SnapshotTypeIncremental)
}

// ChainedIncrementals returns only the deltas that chain off a prior base.
func (e *Env) ChainedIncrementals() []Artefact {
	var out []Artefact
	for _, a := range e.Incrementals() {
		if a.Chained() {
			out = append(out, a)
		}
	}
	return out
}

// NewestChainedIncrTip is the highest tip index among chained deltas, i.e. how
// far a follower can be carried by the current chain.
func (e *Env) NewestChainedIncrTip() uint64 {
	var best uint64
	for _, a := range e.ChainedIncrementals() {
		if a.TipIndex > best {
			best = a.TipIndex
		}
	}
	return best
}

// NewestArtefactTip is the highest tip index in the store, and therefore the
// base the leader's next export will chain off.
//
// Once it passes a stopped follower's index, no future delta can cover that
// follower: every subsequent base is above it. That makes this the definitive
// "the delta window has shut" signal.
func (e *Env) NewestArtefactTip() uint64 {
	var best uint64
	all, err := e.Artefacts()
	if err != nil {
		return 0
	}
	for _, a := range all {
		if a.TipIndex > best {
			best = a.TipIndex
		}
	}
	return best
}

// HorizonEstimate is the leader's MVCC GC horizon as of its most recent
// export.
//
// The horizon is not exposed by any API, but every artefact records the value
// that was live when it was published, so the newest recording is the closest
// observable reading. A follower at or below it cannot catch up by tailing the
// log and must recover from an artefact — which is the second half of what the
// incremental scenario has to establish.
func (e *Env) HorizonEstimate() uint64 {
	var best uint64
	all, err := e.Artefacts()
	if err != nil {
		return 0
	}
	for _, a := range all {
		if a.GCHorizon > best {
			best = a.GCHorizon
		}
	}
	return best
}

// DescribeStore renders the store for a log line.
func (e *Env) DescribeStore() string {
	all, err := e.Artefacts()
	if err != nil {
		return fmt.Sprintf("<unreadable: %v>", err)
	}
	if len(all) == 0 {
		return "(no artefacts)"
	}
	parts := make([]string, 0, len(all))
	for _, a := range all {
		parts = append(parts, fmt.Sprintf("%s(gc=%d,%dB)", a.Name(), a.GCHorizon, a.SizeBytes))
	}
	return strings.Join(parts, " ")
}

// CheckStore verifies every committed artefact against its own commit signal:
// the .snap file must exist, its length must match SizeBytes, and its SHA-256
// must match the recorded digest.
//
// The follower verifies the same digest after downloading, but it verifies what
// it received; this verifies what was published. A truncated or mis-digested
// upload that the follower then rejects looks, from the follower's side, like a
// negotiation failure — checking the store directly tells the two apart.
func (e *Env) CheckStore(t *testing.T) {
	t.Helper()
	all, err := e.Artefacts()
	if err != nil {
		t.Errorf("read shared store: %v", err)
		return
	}
	for _, a := range all {
		fi, err := os.Stat(a.SnapPath)
		if err != nil {
			t.Errorf("artefact %s is committed but its data is missing: %v", a.Name(), err)
			continue
		}
		if fi.Size() != a.SizeBytes {
			t.Errorf("artefact %s: on-disk size %d, meta says %d", a.Name(), fi.Size(), a.SizeBytes)
			continue
		}
		if a.SHA256 == "" {
			t.Errorf("artefact %s has no recorded sha256", a.Name())
			continue
		}
		sum, err := fileSHA256(a.SnapPath)
		if err != nil {
			t.Errorf("artefact %s: %v", a.Name(), err)
			continue
		}
		if sum != a.SHA256 {
			t.Errorf("artefact %s: sha256 %s, meta says %s", a.Name(), sum, a.SHA256)
			continue
		}
		if a.Format != store.SnapshotFormat {
			t.Errorf("artefact %s: format %q, want %q", a.Name(), a.Format, store.SnapshotFormat)
		}
	}
	infof("shared store verified: %d artefact(s) — %s", len(all), e.DescribeStore())
}

// AwaitFullExport waits for a full snapshot to be published. A full artefact is
// the base of every incremental chain, so nothing else in the suite is
// meaningful until one exists.
func (e *Env) AwaitFullExport(ctx context.Context) {
	t := e.t
	t.Helper()
	e.Await(ctx, "a full snapshot to be exported", 2*time.Minute, func() bool {
		return len(e.Fulls()) > 0
	})
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

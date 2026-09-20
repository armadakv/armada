// Copyright Armada Contributors

package table

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/storage/cluster"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/table/fsm"
)

// RecoveryPhase is the durable stage of a table recovery. The phase is written
// to the Raft-replicated metadata store *before* the side effect it authorises,
// so that a node which loses power at any instant can work out what it was
// doing and finish it.
type RecoveryPhase string

const (
	// PhaseDownloading — the artifact identity is pinned; bytes are being fetched.
	PhaseDownloading RecoveryPhase = "downloading"
	// PhaseLoading — the staged artifact is replayed into the target shard.
	PhaseLoading RecoveryPhase = "loading"
	// PhaseSeeding — peers join the recovery shard as learners and catch up.
	PhaseSeeding RecoveryPhase = "seeding"
	// PhasePromoting — caught-up learners are promoted to voters one at a time.
	PhasePromoting RecoveryPhase = "promoting"
	// PhaseSwapping — the recovery shard becomes the table's serving shard.
	PhaseSwapping RecoveryPhase = "swapping"
)

// Artifact types recorded in RecoveryArtifact.Type.
const (
	// ArtifactFull is a full snapshot from the shared object store.
	ArtifactFull = "full"
	// ArtifactIncremental is a delta snapshot from the shared object store.
	ArtifactIncremental = "incr"
	// ArtifactLive is an on-demand snapshot streamed from the leader.
	ArtifactLive = "live"
	// ArtifactLocal is a locally supplied stream (operator restore).
	ArtifactLocal = "local"
)

// RecoveryArtifact pins the identity of the snapshot a recovery is built from.
// It is written before the first byte is downloaded: the leader may garbage
// collect or publish artifacts while a recovery is in flight, and switching
// artifacts mid-load would mix two points in time into one half-filled shard.
type RecoveryArtifact struct {
	ObjectKey string `json:"object_key"`
	Type      string `json:"type"`
	BaseIndex uint64 `json:"base_index"`
	TipIndex  uint64 `json:"tip_index"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	// StagePath is the node-local path of the staged artifact. When set, a
	// recovery interrupted during PhaseLoading can reopen it and replay from
	// the beginning; replay is idempotent because every record carries its own
	// LeaderIndex, which the FSM uses as the MVCC seqno.
	StagePath string `json:"stage_path,omitempty"`
	// Legacy marks a maintenance backup predating the mandatory terminal
	// source-progress marker.
	Legacy bool `json:"legacy,omitempty"`
}

// RecoveryRecord is the durable journal entry for one table recovery. It lives
// in the same Raft-replicated metadata store as /tables/{name}, under
// /recovery/{table}, so the reconcile loop reads table metadata and recovery
// phase in the same pass.
type RecoveryRecord struct {
	Table       string        `json:"table"`
	Phase       RecoveryPhase `json:"phase"`
	RecoveryID  uint64        `json:"recovery_id"`
	Coordinator uint64        `json:"coordinator"`
	// Members is the intended voter set captured when recovery begins. Learners
	// records only successful admissions and therefore must not define quorum.
	Members   []uint64         `json:"members,omitempty"`
	Learners  []uint64         `json:"learners"`
	Promoted  []uint64         `json:"promoted"`
	SourceID  uint64           `json:"source_id"`
	SeedIndex uint64           `json:"seed_index"`
	Artifact  RecoveryArtifact `json:"artifact"`
	// Incarnation identifies the process that created the record. A live
	// artifact is point-in-time and has no stable identity, so its staged copy
	// is meaningless to a later process: replaying it installs whatever the
	// leader happened to hold when it was first fetched. Comparing this against
	// the running manager's own incarnation is what tells the two apart.
	Incarnation string    `json:"incarnation,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const (
	recoveryKeyPrefix   = "/recovery/"
	recoveryGossipTopic = "recovery/"
)

// RecoveryResult is the storage-side outcome of a recovery drive attempt.
// It lets callers distinguish an already-completed recovery from the absence of
// work and from a recovery that still needs transport bytes or later retries.
type RecoveryResult uint8

const (
	RecoveryNoRecovery RecoveryResult = iota
	RecoveryNeedsDownload
	RecoveryPending
	RecoveryCompleted
)

var (
	// ErrRecoveryNeedsDownload is returned by DriveRecovery when the journal is
	// still in PhaseDownloading: fetching the bytes belongs to the caller that
	// owns the object-store or HTTP transport, not to the storage layer.
	ErrRecoveryNeedsDownload = errors.New("recovery artifact is not staged yet")

	// ErrRecoveryConflict is returned when another node already owns the
	// recovery of this table.
	ErrRecoveryConflict = errors.New("recovery already owned by another coordinator")

	// errNoRecovery is the internal sentinel for "no journal record".
	errNoRecovery = errors.New("no recovery record")

	// ErrRecoveryPending means the current phase cannot advance yet; the next
	// reconcile tick should try again. It is not a failure.
	ErrRecoveryPending = errors.New("recovery step pending")
)

func recoveryKey(table string) string { return recoveryKeyPrefix + table }

func recoveryProgressKey(table string, recoveryID, nodeID uint64) string {
	return fmt.Sprintf("%s%s/progress/%d/%d", recoveryKeyPrefix, table, recoveryID, nodeID)
}

// RecoveryBus is the gossip surface used to accelerate recovery coordination.
// Every step it carries is also reachable from the reconcile tick, so a lost
// message only costs latency — the journal, not gossip, is the source of truth.
type RecoveryBus interface {
	Broadcast(m cluster.Message)
	SendTo(n cluster.Node, m cluster.Message) error
	WatchPrefix(prefix string, f func(cluster.Message))
	Nodes() []cluster.Node
}

// recoveryGossip is the payload of every recovery/* gossip message.
type recoveryGossip struct {
	Table      string `json:"table"`
	RecoveryID uint64 `json:"recovery_id"`
	NodeID     uint64 `json:"node_id"`
	RaftAddr   string `json:"raft_addr,omitempty"`
	Applied    uint64 `json:"applied,omitempty"`
}

// AttachRecoveryBus wires the gossip cluster into the manager and subscribes to
// the recovery topic. It must be called before Start.
func (m *Manager) AttachRecoveryBus(b RecoveryBus) {
	m.bus = b
	b.WatchPrefix(recoveryGossipTopic, m.handleRecoveryGossip)
}

// ─── journal primitives ──────────────────────────────────────────────────────

type recoveryEntry struct {
	rec RecoveryRecord
	ver uint64
}

func (m *Manager) getRecovery(name string) (RecoveryRecord, uint64, error) {
	p, err := m.store.Get(recoveryKey(name))
	if err != nil {
		if errors.Is(err, kv.ErrNotExist) {
			return RecoveryRecord{}, 0, errNoRecovery
		}
		return RecoveryRecord{}, 0, err
	}
	rec := RecoveryRecord{}
	if err := json.Unmarshal([]byte(p.Value), &rec); err != nil {
		return RecoveryRecord{}, 0, fmt.Errorf("decode recovery record for %q: %w", name, err)
	}
	return rec, p.Ver, nil
}

// putRecovery writes rec at the given version and stamps it. It takes a pointer
// so the caller keeps the timestamp it just committed — recoverySeed measures
// its patience against exactly that value.
func (m *Manager) putRecovery(rec *RecoveryRecord, ver uint64) (uint64, error) {
	rec.UpdatedAt = time.Now().UTC()
	bts, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	p, err := m.store.Set(recoveryKey(rec.Table), string(bts), ver)
	if err != nil {
		return 0, err
	}
	return p.Ver, nil
}

func (m *Manager) deleteRecovery(rec RecoveryRecord, ver uint64) error {
	err := m.store.Delete(recoveryKey(rec.Table), ver)
	if err != nil && !errors.Is(err, kv.ErrNotExist) {
		return err
	}
	m.clearProgress(rec)
	return nil
}

func (m *Manager) listRecoveries() ([]recoveryEntry, error) {
	pairs, err := m.store.GetAll(recoveryKeyPrefix + "*")
	if err != nil {
		return nil, err
	}
	out := make([]recoveryEntry, 0, len(pairs))
	for _, p := range pairs {
		rec := RecoveryRecord{}
		if err := json.Unmarshal([]byte(p.Value), &rec); err != nil {
			m.log.Warnf("skipping undecodable recovery record %s: %v", p.Key, err)
			continue
		}
		if rec.Table == "" {
			rec.Table = strings.TrimPrefix(p.Key, recoveryKeyPrefix)
		}
		out = append(out, recoveryEntry{rec: rec, ver: p.Ver})
	}
	return out, nil
}

func (m *Manager) setPhase(rec *RecoveryRecord, ver *uint64, phase RecoveryPhase) error {
	rec.Phase = phase
	next, err := m.putRecovery(rec, *ver)
	if err != nil {
		return err
	}
	*ver = next
	return nil
}

// ─── progress ────────────────────────────────────────────────────────────────

// reportProgress publishes this node's applied index for a recovery shard. It
// is written to the replicated store (durable, reliable) and gossiped to the
// coordinator (fast). The store copy is what makes catch-up detection immune to
// a dropped gossip message.
func (m *Manager) reportProgress(rec RecoveryRecord, applied uint64) {
	key := recoveryProgressKey(rec.Table, rec.RecoveryID, m.cfg.NodeID)
	prev, err := m.store.Get(key)
	if err != nil && !errors.Is(err, kv.ErrNotExist) {
		m.log.Warnf("[%s] cannot read recovery progress: %v", rec.Table, err)
		return
	}
	if prev.Value == strconv.FormatUint(applied, 10) {
		return
	}
	if _, err := m.store.Set(key, strconv.FormatUint(applied, 10), prev.Ver); err != nil {
		m.log.Warnf("[%s] cannot record recovery progress: %v", rec.Table, err)
		return
	}
	m.sendRecoveryGossip(rec.Coordinator, "progress", recoveryGossip{
		Table:      rec.Table,
		RecoveryID: rec.RecoveryID,
		NodeID:     m.cfg.NodeID,
		Applied:    applied,
	})
}

// progressOf returns the highest applied index reported by nodeID, taking the
// maximum of the durable record and any newer gossip report.
func (m *Manager) progressOf(rec RecoveryRecord, nodeID uint64) uint64 {
	var best uint64
	if p, err := m.store.Get(recoveryProgressKey(rec.Table, rec.RecoveryID, nodeID)); err == nil {
		if v, err := strconv.ParseUint(p.Value, 10, 64); err == nil {
			best = v
		}
	}
	if v, ok := m.progress.Load(progressCacheKey(rec.Table, rec.RecoveryID, nodeID)); ok {
		best = max(best, v.(uint64))
	}
	return best
}

func progressCacheKey(table string, recoveryID, nodeID uint64) string {
	return table + "/" + strconv.FormatUint(recoveryID, 10) + "/" + strconv.FormatUint(nodeID, 10)
}

func (m *Manager) clearProgress(rec RecoveryRecord) {
	pairs, err := m.store.GetAll(fmt.Sprintf("%s%s/progress/%d/*", recoveryKeyPrefix, rec.Table, rec.RecoveryID))
	if err != nil {
		return
	}
	for _, p := range pairs {
		_ = m.store.Delete(p.Key, p.Ver)
	}
	for id := range m.members {
		m.progress.Delete(progressCacheKey(rec.Table, rec.RecoveryID, id))
	}
}

// ─── gossip ──────────────────────────────────────────────────────────────────

func (m *Manager) sendRecoveryGossip(target uint64, verb string, payload recoveryGossip) {
	if m.bus == nil {
		return
	}
	bts, err := json.Marshal(&payload)
	if err != nil {
		return
	}
	msg := cluster.Message{Key: recoveryGossipTopic + payload.Table + "/" + verb, Payload: bts}
	if target == 0 {
		m.bus.Broadcast(msg)
		return
	}
	for _, n := range m.bus.Nodes() {
		if n.NodeID != target {
			continue
		}
		if err := m.bus.SendTo(n, msg); err != nil {
			m.log.Debugf("recovery gossip %s to node %d failed: %v", verb, target, err)
		}
		return
	}
}

// handleRecoveryGossip turns an incoming message into either a progress-cache
// update or a nudge of the recovery loop. Running every state transition
// through the single reconcile path — rather than acting inline — keeps one
// implementation of the phase machine and makes a lost message merely slow.
func (m *Manager) handleRecoveryGossip(msg cluster.Message) {
	verb := path.Base(msg.Key)
	g := recoveryGossip{}
	if err := json.Unmarshal(msg.Payload, &g); err != nil {
		return
	}
	switch verb {
	case "progress":
		if g.Table != "" && g.RecoveryID != 0 && g.NodeID != 0 {
			// Gossip is merely an acceleration path. Never let a delayed report
			// from an older shard generation satisfy the current recovery.
			rec, ok, err := m.RecoveryStatus(g.Table)
			if err == nil && ok && rec.RecoveryID == g.RecoveryID {
				m.progress.Store(progressCacheKey(g.Table, g.RecoveryID, g.NodeID), g.Applied)
			}
		}
	case "invite":
		// Not the coordinator and no local replica yet: tell the coordinator
		// our raft address so it can propose the AddNonVoting. A peer cannot
		// add itself — RequestAddNonVoting needs the shard to be local first.
		if g.NodeID == m.cfg.NodeID || m.nh.HasNodeInfo(g.RecoveryID, m.cfg.NodeID) {
			return
		}
		m.sendRecoveryGossip(g.NodeID, "accept", recoveryGossip{
			Table:      g.Table,
			RecoveryID: g.RecoveryID,
			NodeID:     m.cfg.NodeID,
			RaftAddr:   m.members[m.cfg.NodeID],
		})
		return
	}
	m.nudgeRecovery()
}

func (m *Manager) nudgeRecovery() {
	select {
	case m.recoveryNudge <- struct{}{}:
	default:
	}
}

// ─── public entry points ─────────────────────────────────────────────────────

// RecoveryStatus returns the durable recovery record for name, if any.
func (m *Manager) RecoveryStatus(name string) (RecoveryRecord, bool, error) {
	rec, _, err := m.getRecovery(name)
	if errors.Is(err, errNoRecovery) {
		return RecoveryRecord{}, false, nil
	}
	if err != nil {
		return RecoveryRecord{}, false, err
	}
	return rec, true, nil
}

// BeginRecovery pins art as the artifact for name's recovery and enters
// PhaseDownloading. It is idempotent for an identical artifact so a retrying
// caller does not renegotiate, and refuses to take over a recovery owned by
// another node.
func (m *Manager) BeginRecovery(name string, sourceID uint64, art RecoveryArtifact) error {
	if !m.beginRecoveryWork(name) {
		return ErrRecoveryPending
	}
	defer m.endRecoveryWork(name)
	return m.beginRecoveryLocked(name, sourceID, art)
}

func (m *Manager) beginRecoveryLocked(name string, sourceID uint64, art RecoveryArtifact) error {
	rec, ver, err := m.getRecovery(name)
	switch {
	case errors.Is(err, errNoRecovery):
		rec := RecoveryRecord{
			Table:       name,
			Phase:       PhaseDownloading,
			Coordinator: m.cfg.NodeID,
			Members:     sortedMemberIDs(m.members),
			SourceID:    sourceID,
			Artifact:    art,
			Incarnation: m.incarnation,
		}
		_, err = m.putRecovery(&rec, 0)
		return err
	case err != nil:
		return err
	}
	if rec.Coordinator != m.cfg.NodeID {
		return ErrRecoveryConflict
	}
	if rec.Phase != PhaseDownloading {
		// Already past negotiation; the existing artifact stays pinned.
		return nil
	}
	if rec.Artifact == art && rec.SourceID == sourceID {
		return nil
	}
	rec.Artifact = art
	rec.SourceID = sourceID
	_, err = m.putRecovery(&rec, ver)
	return err
}

// CompleteDownload records that the staged artifact at stagePath is complete
// and verified, then drives the recovery as far as it can go. For a full
// recovery it allocates the recovery shard ID here — after the bytes are safe
// on disk but before the shard is started, so a crash cannot leak an ID.
func (m *Manager) CompleteDownload(ctx context.Context, name, stagePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.beginRecoveryWork(name) {
		return ErrRecoveryPending
	}
	defer m.endRecoveryWork(name)
	return m.completeDownloadLocked(ctx, name, stagePath)
}

func (m *Manager) completeDownloadLocked(ctx context.Context, name, stagePath string) error {
	rec, ver, err := m.getRecovery(name)
	if err != nil {
		return err
	}
	if rec.Coordinator != m.cfg.NodeID {
		return ErrRecoveryConflict
	}
	if rec.Phase == PhaseDownloading {
		rec.Artifact.StagePath = stagePath
		if rec.Artifact.Type != ArtifactIncremental {
			recoveryID, err := m.incAndGetIDSeq()
			if err != nil {
				return err
			}
			rec.RecoveryID = recoveryID
		}
		if err := m.setPhase(&rec, &ver, PhaseLoading); err != nil {
			return err
		}
		if err := m.pinRecoverID(name, rec.RecoveryID); err != nil {
			return err
		}
	}
	return m.driveRecoveryLocked(ctx, name, nil)
}

// claimRecovery transfers ownership of a recovery to this node. It is only safe
// before a recovery shard exists, i.e. while the record is still downloading.
func (m *Manager) claimRecovery(rec RecoveryRecord, ver uint64) error {
	if !m.beginRecoveryWork(rec.Table) {
		return ErrRecoveryPending
	}
	defer m.endRecoveryWork(rec.Table)
	current, currentVer, err := m.getRecovery(rec.Table)
	if err != nil {
		return err
	}
	if currentVer != ver {
		return kv.ErrVersionMismatch
	}
	m.log.Infof("[%s] claiming recovery from coordinator %d (phase %s)", current.Table, current.Coordinator, current.Phase)
	current.Coordinator = m.cfg.NodeID
	_, err = m.putRecovery(&current, currentVer)
	return err
}

// AbandonRecovery tears down an in-flight recovery: the recovery shard (if any)
// is stopped and tombstoned for cleanup, and the journal record is removed.
func (m *Manager) AbandonRecovery(name string) error {
	if !m.beginRecoveryWork(name) {
		return ErrRecoveryPending
	}
	defer m.endRecoveryWork(name)
	rec, ver, err := m.getRecovery(name)
	if errors.Is(err, errNoRecovery) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.abandonRecoveryLocked(rec, ver)
}

// abandonRecovery is for callers that do not already hold the table recovery
// slot. It first serializes and validates the exact journal version; no cleanup
// side effect is allowed before that compare-and-swap boundary.
func (m *Manager) abandonRecovery(rec RecoveryRecord, ver uint64) error {
	if !m.beginRecoveryWork(rec.Table) {
		return ErrRecoveryPending
	}
	defer m.endRecoveryWork(rec.Table)
	return m.abandonRecoveryLocked(rec, ver)
}

func (m *Manager) abandonRecoveryLocked(target RecoveryRecord, expectedVer uint64) error {
	rec, ver, err := m.getRecovery(target.Table)
	if errors.Is(err, errNoRecovery) {
		return nil
	}
	if err != nil {
		return err
	}
	if ver != expectedVer || rec.RecoveryID != target.RecoveryID || rec.Coordinator != target.Coordinator {
		return kv.ErrVersionMismatch
	}

	// Removing the exact record is the durable retirement transition. Only after
	// this CAS succeeds may this operation touch the old shard or staged bytes.
	if err := m.deleteRecovery(rec, ver); err != nil {
		return err
	}
	m.log.Warnf("[%s] abandoned recovery in phase %s (coordinator %d)", rec.Table, rec.Phase, rec.Coordinator)
	if rec.RecoveryID > tableIDsRangeStart && m.shardRunning(rec.RecoveryID) {
		if err := m.stopTable(rec.RecoveryID); err != nil {
			m.log.Warnf("[%s] could not stop abandoned recovery shard %d: %v", rec.Table, rec.RecoveryID, err)
		}
	}
	if rec.RecoveryID != 0 {
		if err := m.clearRecoverID(rec.Table, rec.RecoveryID); err != nil {
			m.log.Warnf("[%s] could not clear recover id: %v", rec.Table, err)
		}
	}
	m.removeStaged(rec)
	return nil
}

// DriveRecovery advances the recovery of name as far as it can in one call.
// It returns ErrRecoveryNeedsDownload when the artifact is not staged yet.
func (m *Manager) DriveRecovery(ctx context.Context, name string) error {
	return m.driveRecovery(ctx, name, nil)
}

// DriveRecoveryResult is the explicit recovery completion API. It is useful to
// transport callers that must only advance their source watermark after the
// manager has actually swapped or applied the recovery.
func (m *Manager) DriveRecoveryResult(ctx context.Context, name string) (RecoveryResult, error) {
	if !m.beginRecoveryWork(name) {
		return RecoveryPending, nil
	}
	defer m.endRecoveryWork(name)
	if _, _, err := m.getRecovery(name); errors.Is(err, errNoRecovery) {
		return RecoveryNoRecovery, nil
	} else if err != nil {
		return RecoveryPending, err
	}
	if err := m.driveRecoveryLocked(ctx, name, nil); err != nil {
		switch {
		case errors.Is(err, ErrRecoveryNeedsDownload):
			return RecoveryNeedsDownload, nil
		case errors.Is(err, ErrRecoveryPending):
			return RecoveryPending, nil
		default:
			return RecoveryPending, err
		}
	}
	return RecoveryCompleted, nil
}

// ─── phase machine ───────────────────────────────────────────────────────────

// driveRecovery runs the coordinator side of the phase machine. in, when
// non-nil, supplies a one-shot reader for the loading phase (operator restore);
// otherwise the staged file named by the journal is reopened.
func (m *Manager) driveRecovery(ctx context.Context, name string, in io.Reader) error {
	if !m.beginRecoveryWork(name) {
		return ErrRecoveryPending
	}
	defer m.endRecoveryWork(name)
	return m.driveRecoveryLocked(ctx, name, in)
}

// driveRecoveryLocked is driveRecovery for a caller that already holds the
// table's in-process recovery slot.
func (m *Manager) driveRecoveryLocked(ctx context.Context, name string, in io.Reader) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, ver, err := m.getRecovery(name)
		if errors.Is(err, errNoRecovery) {
			return nil
		}
		if err != nil {
			return err
		}
		if rec.Coordinator != m.cfg.NodeID {
			return ErrRecoveryConflict
		}

		switch rec.Phase {
		case PhaseDownloading:
			return ErrRecoveryNeedsDownload
		case PhaseLoading:
			err = m.recoveryLoad(ctx, &rec, &ver, in)
			in = nil
		case PhaseSeeding:
			err = m.recoverySeed(ctx, &rec, &ver)
		case PhasePromoting:
			err = m.recoveryPromote(ctx, &rec, &ver)
		case PhaseSwapping:
			err = m.recoverySwap(&rec, &ver)
		default:
			return fmt.Errorf("unknown recovery phase %q for table %q", rec.Phase, name)
		}
		if err != nil {
			return err
		}
	}
}

func (m *Manager) beginRecoveryWork(name string) bool {
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	if _, busy := m.recoveryInflight[name]; busy {
		return false
	}
	if m.recoveryInflight == nil {
		m.recoveryInflight = make(map[string]struct{})
	}
	m.recoveryInflight[name] = struct{}{}
	return true
}

func (m *Manager) endRecoveryWork(name string) {
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	delete(m.recoveryInflight, name)
}

// recoveryLoad replays the staged artifact into the target shard. Replay always
// restarts from the beginning of the artifact: every PUT/DELETE carries its own
// LeaderIndex, which becomes the MVCC seqno, so re-applying a command rewrites
// the identical physical key@seqno. That idempotence is what makes a crashed
// load safely restartable into the same half-filled shard — and it is the only
// workable scheme, because the artifact is a snappy stream whose record
// boundaries are not addressable by byte offset.
func (m *Manager) recoveryLoad(ctx context.Context, rec *RecoveryRecord, ver *uint64, in io.Reader) error {
	target := rec.RecoveryID
	if target == 0 {
		tbl, _, err := m.getTableVersion(rec.Table)
		if err != nil {
			return err
		}
		if tbl.ClusterID == 0 {
			return fmt.Errorf("incremental recovery of table %q has no live shard", rec.Table)
		}
		target = tbl.ClusterID
	} else {
		if err := m.ensureRecoveryShard(rec.Table, rec.RecoveryID); err != nil {
			return err
		}
		if err := m.waitForLeader(ctx, rec.RecoveryID); err != nil {
			return err
		}
	}

	reader := in
	if reader == nil {
		sf, err := m.openStaged(*rec)
		switch {
		case errors.Is(err, errStagedMissing) && rec.Artifact.ObjectKey != "":
			// The artifact identity is still pinned, so re-fetching is safe:
			// it cannot silently switch to a different point in time.
			m.log.Warnf("[%s] staged artifact is gone, restarting the download", rec.Table)
			rec.Artifact.StagePath = ""
			return m.setPhase(rec, ver, PhaseDownloading)
		case errors.Is(err, errStagedMissing):
			// A locally supplied stream cannot be re-read after a crash.
			if aerr := m.abandonRecoveryLocked(*rec, *ver); aerr != nil {
				return aerr
			}
			return fmt.Errorf("recovery of table %q cannot resume: its source stream is gone", rec.Table)
		case err != nil:
			return err
		}
		defer func() { _ = sf.Close() }()
		reader = sf
	}

	if err := m.readIntoTable(ctx, target, rec.Table, reader, !rec.Artifact.Legacy); err != nil {
		return err
	}

	if rec.RecoveryID == 0 {
		// Incremental replay lands directly in the live shard: no shard to
		// seed, promote or swap.
		if err := m.deleteRecovery(*rec, *ver); err != nil {
			return err
		}
		m.removeStaged(*rec)
		return nil
	}

	// Capture a Raft snapshot so learners receive the loaded state through
	// Dragonboat's own SST snapshot path instead of replaying every command.
	// That is the entire point of learner-first recovery.
	m.requestRecoverySnapshot(ctx, rec.RecoveryID)

	idx, err := m.localIndex(rec.RecoveryID)
	if err != nil {
		return err
	}
	rec.SeedIndex = idx
	// Advance the phase before discarding the staged file: the reverse order
	// would leave a crash stuck in "loading" with nothing to load.
	if err := m.setPhase(rec, ver, PhaseSeeding); err != nil {
		return err
	}
	m.removeStaged(*rec)
	return nil
}

// recoverySeed makes every other member a non-voting replica of the recovery
// shard and waits for them to catch up. Membership changes must be proposed by
// a member, and the coordinator is the only voter — a peer cannot add itself.
func (m *Manager) recoverySeed(ctx context.Context, rec *RecoveryRecord, ver *uint64) error {
	if len(rec.Members) == 0 {
		// Records written before Members existed use the configured member set.
		// Persist it now so retries retain one stable quorum definition.
		rec.Members = sortedMemberIDs(m.members)
		next, err := m.putRecovery(rec, *ver)
		if err != nil {
			return err
		}
		*ver = next
	}
	if err := m.ensureRecoveryShard(rec.Table, rec.RecoveryID); err != nil {
		return err
	}
	mem, err := m.shardMembership(ctx, rec.RecoveryID)
	if err != nil {
		m.log.Warnf("[%s] cannot read recovery shard membership: %v", rec.Table, err)
		return ErrRecoveryPending
	}

	// Raft membership is the truth; rec.Learners is a hint that survives restarts.
	learners := make([]uint64, 0, len(rec.Members))
	changed := false
	for _, id := range recoveryLearners(*rec) {
		if _, removed := mem.Removed[id]; removed {
			continue
		}
		if _, voter := mem.Nodes[id]; voter {
			learners = append(learners, id)
			continue
		}
		if _, nonVoting := mem.NonVotings[id]; nonVoting {
			learners = append(learners, id)
			continue
		}
		addCtx, cancel := context.WithTimeout(ctx, m.recoveryStepTimeout)
		err := m.nh.SyncRequestAddNonVoting(addCtx, rec.RecoveryID, id, m.members[id], mem.ConfigChangeID)
		cancel()
		if err != nil {
			m.log.Warnf("[%s] adding node %d as learner failed: %v", rec.Table, id, err)
			continue
		}
		m.log.Infof("[%s] node %d added to recovery shard %d as a learner", rec.Table, id, rec.RecoveryID)
		learners = append(learners, id)
		changed = true
		m.sendRecoveryGossip(id, "admit", recoveryGossip{
			Table:      rec.Table,
			RecoveryID: rec.RecoveryID,
			NodeID:     id,
		})
		// OrderedConfigChange is on: every change needs a fresh index.
		if mem, err = m.shardMembership(ctx, rec.RecoveryID); err != nil {
			break
		}
	}

	if changed || !slices.Equal(rec.Learners, learners) {
		rec.Learners = learners
		next, err := m.putRecovery(rec, *ver)
		if err != nil {
			return err
		}
		*ver = next
	}

	// Invite anyone who has not shown up; a peer that missed this still joins
	// from its own reconcile tick by reading rec.Learners.
	m.sendRecoveryGossip(0, "invite", recoveryGossip{
		Table:      rec.Table,
		RecoveryID: rec.RecoveryID,
		NodeID:     m.cfg.NodeID,
		RaftAddr:   m.members[m.cfg.NodeID],
	})

	caughtUp := m.caughtUpLearners(*rec)
	intendedLearners := recoveryLearners(*rec)
	if len(caughtUp) == len(intendedLearners) {
		return m.setPhase(rec, ver, PhasePromoting)
	}

	m.log.Debugf("[%s] seeding shard %d: %d/%d learners caught up to index %d (waited %s of %s)",
		rec.Table, rec.RecoveryID, len(caughtUp), len(intendedLearners), rec.SeedIndex,
		time.Since(rec.UpdatedAt).Truncate(time.Second), m.recoverySeedTimeout)

	// A learner that never reports must not block the table forever, but it
	// must also never be promoted unseen: an uncaught-up voter would stall the
	// shard, because promoting voter #2 makes quorum 2-of-2.
	if time.Since(rec.UpdatedAt) < m.recoverySeedTimeout {
		return ErrRecoveryPending
	}
	if len(caughtUp)+1 < recoveryQuorum(*rec) {
		m.log.Warnf("[%s] only %d/%d learners caught up; waiting for a safe majority",
			rec.Table, len(caughtUp), len(intendedLearners))
		return ErrRecoveryPending
	}
	m.log.Warnf("[%s] seeding timed out; promoting %d of %d learners", rec.Table, len(caughtUp), len(intendedLearners))
	return m.setPhase(rec, ver, PhasePromoting)
}

// caughtUpLearners returns the admitted learners whose reported applied index
// has reached the index the coordinator loaded to. It deliberately receives the
// full record so the report key is scoped to this recovery generation.
func (m *Manager) caughtUpLearners(rec RecoveryRecord) []uint64 {
	var out []uint64
	for _, id := range rec.Learners {
		if m.progressOf(rec, id) >= rec.SeedIndex {
			out = append(out, id)
		}
	}
	return out
}

func recoveryLearners(rec RecoveryRecord) []uint64 {
	learners := make([]uint64, 0, len(rec.Members))
	for _, id := range rec.Members {
		if id != rec.Coordinator {
			learners = append(learners, id)
		}
	}
	return learners
}

func recoveryQuorum(rec RecoveryRecord) int {
	members := len(rec.Members)
	if members == 0 {
		return 1
	}
	return members/2 + 1
}

// recoveryPromote promotes caught-up learners to voters, one at a time, each
// with a fresh config-change index, appending to Promoted after each success.
func (m *Manager) recoveryPromote(ctx context.Context, rec *RecoveryRecord, ver *uint64) error {
	if err := m.ensureRecoveryShard(rec.Table, rec.RecoveryID); err != nil {
		return err
	}
	for _, id := range m.caughtUpLearners(*rec) {
		if slices.Contains(rec.Promoted, id) {
			continue
		}
		mem, err := m.shardMembership(ctx, rec.RecoveryID)
		if err != nil {
			return ErrRecoveryPending
		}
		if _, voter := mem.Nodes[id]; !voter {
			addCtx, cancel := context.WithTimeout(ctx, m.recoveryStepTimeout)
			err := m.nh.SyncRequestAddReplica(addCtx, rec.RecoveryID, id, m.members[id], mem.ConfigChangeID)
			cancel()
			if err != nil {
				m.log.Warnf("[%s] promoting node %d failed: %v", rec.Table, id, err)
				return ErrRecoveryPending
			}
			m.log.Infof("[%s] node %d promoted to voter on recovery shard %d", rec.Table, id, rec.RecoveryID)
		}
		rec.Promoted = append(rec.Promoted, id)
		next, err := m.putRecovery(rec, *ver)
		if err != nil {
			return err
		}
		*ver = next
	}
	return m.setPhase(rec, ver, PhaseSwapping)
}

// recoverySwap makes the recovery shard the table's serving shard and retires
// the old one through the existing cleanup tombstone path.
func (m *Manager) recoverySwap(rec *RecoveryRecord, ver *uint64) error {
	tbl, tblVer, err := m.getTableVersion(rec.Table)
	if err != nil && !errors.Is(err, serrors.ErrTableNotFound) {
		return err
	}
	old := tbl.ClusterID
	if tbl.ClusterID != rec.RecoveryID {
		tbl.Name = rec.Table
		tbl.ClusterID = rec.RecoveryID
		tbl.RecoverID = 0
		tbl.Recovered = true
		if err := m.setTableVersion(tbl, tblVer); err != nil {
			return err
		}
	} else {
		old = 0
	}

	if err := m.deleteRecovery(*rec, *ver); err != nil {
		return err
	}
	m.removeStaged(*rec)
	m.sendRecoveryGossip(0, "done", recoveryGossip{Table: rec.Table, RecoveryID: rec.RecoveryID})

	if old > tableIDsRangeStart && m.shardRunning(old) {
		if err := m.stopTable(old); err != nil {
			m.log.Warnf("[%s] could not retire old shard %d: %v", rec.Table, old, err)
		}
	}
	// Report the source index the new shard actually landed on, not just the
	// shard id. Whether recovery made progress is decided entirely by this
	// number: log replication resumes from it, and if it is at or below the
	// leader's GC horizon the leader answers USE_SNAPSHOT and the whole
	// recovery runs again. A value well short of the artefact tip is the
	// signature of that loop.
	leaderIndex, idxErr := m.leaderIndex(rec.RecoveryID)
	if idxErr != nil {
		m.log.Infof("[%s] recovery complete, serving shard is now %d (source index unavailable: %v)",
			rec.Table, rec.RecoveryID, idxErr)
	} else {
		m.log.Infof("[%s] recovery complete, serving shard is now %d at source index %d (artefact %s tip %d)",
			rec.Table, rec.RecoveryID, leaderIndex, rec.Artifact.Type, rec.Artifact.TipIndex)
	}
	return nil
}

// ─── reconcile ───────────────────────────────────────────────────────────────

// recoveryLoop drives recovery records independently of the table reconcile
// loop: a load can take minutes and must not hold up shard reconciliation.
func (m *Manager) recoveryLoop() {
	t := time.NewTicker(m.recoveryInterval)
	defer t.Stop()
	for {
		select {
		case <-m.closed:
			return
		case <-t.C:
		case <-m.recoveryNudge:
		}
		if ls, ok := m.store.(leaderStore); ok && !ls.HasLeader() {
			continue
		}
		if err := m.reconcileRecovery(); err != nil {
			m.log.Errorf("recovery reconcile failed: %v", err)
		}
	}
}

// Note: reconcileServingMembership deliberately does NOT run here. This loop
// ticks every recoveryInterval AND on every gossip nudge, and learner progress
// reports nudge it many times a second, so a membership read per table on each
// pass is far too much Raft traffic. It belongs on the slow reconcile loop with
// the rest of the desired-state reconciliation.

// reconcileRecovery is the single place that decides, for every in-flight
// recovery, what this node should be doing. It is deliberately the only caller
// of startRecoveryShard: diffTables must never auto-start a recovery shard,
// because starting it as a voter while an AddNonVoting entry for this replica
// is still unapplied is a panic, not an error.
func (m *Manager) reconcileRecovery() error {
	recs, err := m.listRecoveries()
	if err != nil {
		return err
	}

	for _, e := range recs {
		if e.rec.Coordinator == m.cfg.NodeID {
			m.scheduleRecovery(e.rec.Table)
			continue
		}

		// Everything below is for a node that does not own the record. Two
		// roles apply here and they are not mutually exclusive: this node may
		// be a learner of the recovery shard, and it may also be the table's
		// lease holder. Do the learner half unconditionally — the coordinator
		// finishes seeding only once every learner has reported its progress,
		// so a lease holder that did the ownership check instead would stall
		// seeding forever and wedge the table.
		m.joinRecoveryAsLearner(e.rec)

		if m.holdsTableLease(e.rec.Table) {
			// This node owns the table but the record names someone else.
			switch {
			case !m.coordinatorLive(e.rec.Coordinator):
				// The coordinator is the recovery shard's only voter, so once
				// that node is gone the shard can never reach quorum and nobody
				// can resume the recovery. Start over.
				if err := m.abandonRecovery(e.rec, e.ver); err != nil {
					m.log.Warnf("[%s] abandoning orphaned recovery failed: %v", e.rec.Table, err)
				}
			case e.rec.Phase == PhaseDownloading:
				// Only the lease holder's replication worker fetches artifacts,
				// so a record left downloading under a coordinator that no
				// longer holds the lease can never advance: that node's worker
				// is idle and this node's worker refuses to act on a record it
				// does not own. Claim it. The artifact identity is pinned, so
				// taking ownership cannot switch the point in time.
				if err := m.claimRecovery(e.rec, e.ver); err != nil {
					m.log.Warnf("[%s] claiming stalled download failed: %v", e.rec.Table, err)
				}
			case time.Since(e.rec.UpdatedAt) > m.recoverySeedTimeout:
				// Past the download phase a recovery shard already exists with
				// the old coordinator as its only voter, so this node cannot
				// take it over. A live coordinator is normally left to finish,
				// but one that has not advanced a phase in this long is wedged.
				m.log.Warnf("[%s] recovery stuck in phase %s under coordinator %d for %s; starting over",
					e.rec.Table, e.rec.Phase, e.rec.Coordinator,
					time.Since(e.rec.UpdatedAt).Truncate(time.Second))
				if err := m.abandonRecovery(e.rec, e.ver); err != nil {
					m.log.Warnf("[%s] abandoning wedged recovery failed: %v", e.rec.Table, err)
				}
			default:
				// A live coordinator mid-recovery: let it finish.
			}
		}
	}
	return nil
}

// scheduleRecovery keeps reconciliation responsive: potentially unbounded
// artifact replay happens in per-table lifecycle-tracked work, never on the
// coordinator loop. beginRecoveryWork is both the deduplication gate and the
// serialization boundary for every operation on this table's journal.
func (m *Manager) scheduleRecovery(name string) {
	if !m.beginRecoveryWork(name) {
		return
	}
	m.wg.Go(func() {
		defer m.endRecoveryWork(name)
		rec, ver, err := m.getRecovery(name)
		if errors.Is(err, errNoRecovery) {
			return
		}
		if err != nil {
			m.log.Warnf("[%s] recovery read failed: %v", name, err)
			return
		}
		if rec.Coordinator != m.cfg.NodeID {
			return
		}
		// A live artifact has no stable identity across process incarnations, so
		// retire this exact durable record rather than replay stale bytes.
		if rec.Artifact.Type == ArtifactLive && rec.Incarnation != m.incarnation {
			m.log.Infof("[%s] discarding a live recovery staged by a previous process (phase %s); renegotiating",
				rec.Table, rec.Phase)
			if err := m.abandonRecoveryLocked(rec, ver); err != nil {
				m.log.Warnf("[%s] discarding stale live recovery failed: %v", rec.Table, err)
			}
			return
		}
		parent := m.lifeCtx
		if parent == nil {
			parent = context.Background()
		}
		err = m.driveRecoveryLocked(parent, name, nil)
		switch {
		case err == nil,
			errors.Is(err, ErrRecoveryPending),
			errors.Is(err, ErrRecoveryNeedsDownload),
			errors.Is(err, context.Canceled):
		default:
			m.log.Warnf("[%s] recovery step failed: %v", name, err)
		}
	})
}

// joinRecoveryAsLearner is the peer side. The local role marker is written
// before the replica starts so that a restart between the two still starts
// non-voting.
func (m *Manager) joinRecoveryAsLearner(rec RecoveryRecord) {
	if rec.RecoveryID == 0 || !slices.Contains(rec.Learners, m.cfg.NodeID) {
		return
	}
	if !m.shardRunning(rec.RecoveryID) {
		if err := m.writeNonVotingMarker(rec.RecoveryID); err != nil {
			m.log.Errorf("[%s] cannot record learner role: %v", rec.Table, err)
			return
		}
		if err := m.startRecoveryShard(rec.Table, rec.RecoveryID, true); err != nil {
			m.log.Warnf("[%s] cannot join recovery shard %d as learner: %v", rec.Table, rec.RecoveryID, err)
			return
		}
		m.log.Infof("[%s] joined recovery shard %d as a learner", rec.Table, rec.RecoveryID)
	}
	idx, err := m.localIndex(rec.RecoveryID)
	if err != nil {
		return
	}
	m.reportProgress(rec, idx)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (m *Manager) holdsTableLease(name string) bool {
	p, err := m.store.Get(storedTableName(name) + "/lease")
	if err != nil {
		return false
	}
	l := Lease{}
	if err := json.Unmarshal([]byte(p.Value), &l); err != nil {
		return false
	}
	return l.ID == m.cfg.NodeID && l.Until.After(time.Now())
}

// coordinatorLive reports whether the node driving a recovery is still a member
// of the gossip cluster.
//
// Holding the table lease is not on its own evidence that the coordinator died.
// recreateFollowerTable deletes the table entry together with its lease, so the
// lease legitimately changes hands while the previous holder is mid-recovery;
// abandoning on the lease alone tears down a healthy recovery and restarts it
// from scratch, which is exactly the thrash this guard prevents.
//
// With no bus attached there is no liveness signal at all, so fall back to
// treating the coordinator as gone and let the lease decide.
func (m *Manager) coordinatorLive(nodeID uint64) bool {
	if nodeID == m.cfg.NodeID {
		return true
	}
	if m.bus == nil {
		return false
	}
	for _, n := range m.bus.Nodes() {
		if n.NodeID == nodeID {
			return true
		}
	}
	return false
}

func (m *Manager) shardRunning(shardID uint64) bool {
	nhi := m.nh.GetNodeHostInfo(raft.NodeHostInfoOption{SkipLogInfo: true})
	if nhi == nil {
		return false
	}
	for _, s := range nhi.ShardInfoList {
		if s.ShardID == shardID {
			return true
		}
	}
	return false
}

func (m *Manager) shardMembership(ctx context.Context, shardID uint64) (*raft.Membership, error) {
	mctx, cancel := context.WithTimeout(ctx, m.recoveryStepTimeout)
	defer cancel()
	return m.nh.SyncGetShardMembership(mctx, shardID)
}

// leaderIndex reads the source (leader) index the shard has applied up to. This
// is the watermark ordinary log replication resumes from, distinct from the
// shard's own Raft index that localIndex returns.
func (m *Manager) leaderIndex(shardID uint64) (uint64, error) {
	v, err := m.nh.StaleRead(shardID, fsm.LeaderIndexRequest{})
	if err != nil {
		return 0, err
	}
	res, ok := v.(*fsm.IndexResponse)
	if !ok {
		return 0, serrors.ErrUnknownResultType
	}
	return res.Index, nil
}

func (m *Manager) localIndex(shardID uint64) (uint64, error) {
	v, err := m.nh.StaleRead(shardID, fsm.LocalIndexRequest{})
	if err != nil {
		return 0, err
	}
	res, ok := v.(*fsm.IndexResponse)
	if !ok {
		return 0, serrors.ErrUnknownResultType
	}
	return res.Index, nil
}

// ensureRecoveryShard starts the coordinator's recovery shard if it is not
// already running. It is a genuinely single-member shard so that it elects
// itself immediately and can propose the membership changes that bring the
// learners in.
func (m *Manager) ensureRecoveryShard(name string, id uint64) error {
	if m.shardRunning(id) {
		return nil
	}
	// Make the shard known before it exists. diffTables stops any running shard
	// it does not recognise, so starting first and recording second gives the
	// reconcile loop a window in which it tears the recovery shard down again.
	if err := m.pinRecoverID(name, id); err != nil {
		return err
	}
	return m.startRecoveryShard(name, id, false)
}

func (m *Manager) startRecoveryShard(name string, id uint64, nonVoting bool) error {
	if m.nh.HasNodeInfo(id, m.cfg.NodeID) {
		marker, err := m.readNonVotingMarker(id)
		if err != nil {
			return fmt.Errorf("read recovery role marker for shard %d: %w", id, err)
		}
		return m.startReplica(name, id, replicaStartOptions{Mode: replicaRestart, IsNonVoting: nonVoting || marker})
	}
	if nonVoting {
		// A non-voting replica must join an existing shard — passing it as an
		// initial member panics in the Raft layer.
		return m.startReplica(name, id, replicaStartOptions{Mode: replicaJoin, IsNonVoting: true})
	}
	return m.startReplica(name, id, replicaStartOptions{
		Mode:    replicaBootstrap,
		Members: map[uint64]raft.Target{m.cfg.NodeID: m.members[m.cfg.NodeID]},
	})
}

func (m *Manager) requestRecoverySnapshot(ctx context.Context, shardID uint64) {
	snapCtx, cancel := context.WithTimeout(ctx, m.recoveryStepTimeout)
	defer cancel()
	// ErrRejected simply means a snapshot already exists at this index, which
	// is the normal outcome on a crash-retry.
	if _, err := m.nh.SyncRequestSnapshot(snapCtx, shardID, raft.SnapshotOption{}); err != nil && !errors.Is(err, raft.ErrRejected) {
		m.log.Warnf("[%d] could not capture seeding snapshot: %v", shardID, err)
	}
}

// pinRecoverID records the recovery shard on the table entry so the reconcile
// loop treats it as known and does not stop it. The journal stays authoritative
// if the two ever disagree.
func (m *Manager) pinRecoverID(name string, recoveryID uint64) error {
	tbl, ver, err := m.getTableVersion(name)
	if errors.Is(err, serrors.ErrTableNotFound) {
		if recoveryID == 0 {
			return nil
		}
		tbl = Table{Name: name}
	} else if err != nil {
		return err
	}
	if tbl.RecoverID == recoveryID {
		return nil
	}
	tbl.Name = name
	tbl.RecoverID = recoveryID
	return m.setTableVersion(tbl, ver)
}

// clearRecoverID clears only the recovery ID retired by this operation. A new
// recovery may have already pinned a different ID after the old journal was
// removed, and must never be erased by old cleanup.
func (m *Manager) clearRecoverID(name string, recoveryID uint64) error {
	tbl, ver, err := m.getTableVersion(name)
	if errors.Is(err, serrors.ErrTableNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if tbl.RecoverID != recoveryID {
		return nil
	}
	tbl.RecoverID = 0
	return m.setTableVersion(tbl, ver)
}

// errStagedMissing means the staged artifact named by the journal is not on
// disk any more.
var errStagedMissing = errors.New("staged artifact missing")

// openStaged reopens the staged artifact as a framed, snappy-decompressing
// reader of armadapb.Command records.
func (m *Manager) openStaged(rec RecoveryRecord) (io.ReadCloser, error) {
	if rec.Artifact.StagePath == "" {
		return nil, errStagedMissing
	}
	sf, err := snapshot.OpenFile(rec.Artifact.StagePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errStagedMissing
		}
		return nil, fmt.Errorf("open staged artifact: %w", err)
	}
	return sf, nil
}

func (m *Manager) removeStaged(rec RecoveryRecord) {
	if rec.Artifact.StagePath == "" || rec.Artifact.Type == ArtifactLocal {
		return
	}
	_ = os.Remove(rec.Artifact.StagePath)
	_ = os.Remove(rec.Artifact.StagePath + snapshot.OffsetSuffix)
}

func sortedMemberIDs(members map[uint64]string) []uint64 {
	ids := make([]uint64, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// reconcileServingMembership drives a table's serving shard toward the voter
// set it is supposed to have.
//
// raft.initial-members is the authoritative, complete member list — a node's
// replica id is its position in it (see cmd/armada/common.go) — so the desired
// state is simply "every member is a voter of the serving shard". Reconciling
// toward that covers both ways a shard ends up short:
//
//   - A learner recoverySeed left behind. Seeding deliberately stops waiting
//     once a majority has caught up and swaps the shard in anyway, so one slow
//     node cannot hold the table hostage; recoveryPromote then promotes only
//     the learners that reported, and recoverySwap deletes the journal record.
//     Nothing else would finish the job, and the table would serve permanently
//     one voter short with nothing logged as an error.
//   - A member that never made it into the shard at all, for instance because
//     its AddNonVoting was lost before seeding timed out.
//
// One SyncRequestAddReplica covers both: the Raft layer promotes an existing
// non-voting replica, inheriting its progress, or adds a missing one outright.
//
// This is quorum-safe in a way the same call is not during seeding. Seeding
// starts from a single voter, so admitting an uncaught-up second voter makes
// the quorum 2-of-2 and can stall the shard — hence recoverySeed's refusal to
// promote a learner that has not reported. Here the shard already has a
// majority of caught-up voters and one more member does not raise the quorum
// above what they satisfy; promotionKeepsQuorum states that rather than relying
// on it happening to be true for three nodes.
func (m *Manager) reconcileServingMembership() {
	parent := m.lifeCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, m.recoveryStepTimeout)
	defer cancel()

	m.mtx.RLock()
	tables, err := m.getTables()
	m.mtx.RUnlock()
	if err != nil {
		return
	}

	for _, tbl := range tables {
		if tbl.ClusterID <= tableIDsRangeStart || !m.shardRunning(tbl.ClusterID) {
			continue
		}
		// Membership changes must be proposed by the shard's leader.
		if leaderID, _, ok, lerr := m.nh.GetLeaderID(tbl.ClusterID); lerr != nil || !ok || leaderID != m.cfg.NodeID {
			continue
		}
		mem, merr := m.shardMembership(ctx, tbl.ClusterID)
		if merr != nil {
			continue
		}
		for _, id := range sortedMemberIDs(m.members) {
			if _, voter := mem.Nodes[id]; voter {
				continue
			}
			// Raft blacklists a removed replica id permanently, so retrying
			// would only be a hot loop of rejected proposals.
			if _, removed := mem.Removed[id]; removed {
				continue
			}
			if !promotionKeepsQuorum(len(mem.Nodes)) {
				break
			}
			addCtx, addCancel := context.WithTimeout(ctx, m.recoveryStepTimeout)
			aerr := m.nh.SyncRequestAddReplica(addCtx, tbl.ClusterID, id, m.members[id], mem.ConfigChangeID)
			addCancel()
			if aerr != nil {
				m.log.Warnf("[%s] making node %d a voter of serving shard %d failed: %v",
					tbl.Name, id, tbl.ClusterID, aerr)
				break
			}
			m.log.Infof("[%s] node %d is now a voter of serving shard %d", tbl.Name, id, tbl.ClusterID)
			// OrderedConfigChange is on: the next change needs a fresh index.
			if mem, merr = m.shardMembership(ctx, tbl.ClusterID); merr != nil {
				break
			}
		}
	}
}

// promotionKeepsQuorum reports whether admitting one more voter to a shard that
// currently has voters members leaves the existing voters able to commit alone.
//
// Seeding starts from a single voter, so admitting a second makes the quorum
// 2-of-2 and one uncaught-up member stalls the shard — which is why
// recoverySeed refuses to promote a learner that has not reported progress.
// Once the shard has two or more voters, adding another does not raise the
// quorum above what they already satisfy, so a lagging straggler cannot hold up
// commits and it is safe to finish the job.
func promotionKeepsQuorum(voters int) bool {
	if voters <= 0 {
		return false
	}
	return voters >= (voters+1)/2+1
}

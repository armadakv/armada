---
title: "RFC 005: Resilient & Resumable Snapshot Recovery via Shared Storage"
description: "Proposal to improve inter-cluster snapshot recovery on follower clusters using shared blob storage, resumable transfers, incremental snapshots, and learner-first shard promotion."
section: "proposals"
order: 6
---

Proposal status: Accepted

## Summary

Replace the current fragile, non-resumable gRPC snapshot transfer path between leader and follower clusters with a durable shared-storage pipeline backed by a pluggable blob store (S3, GCS, NFS, etc.). On the leader side, snapshots are exported periodically to shared storage with incremental variants and a garbage-collection lifecycle. On the follower side, recovery becomes resumable, is performed via a learner shard that is promoted only after it has fully caught up, and stale-but-consistent reads are served throughout the process.

## Motivation

Inter-cluster (leader → follower) snapshot recovery today is a single, non-resumable gRPC stream (`Snapshot.Stream`). The full flow is:

```
leader: table.Snapshot() ──► stream SnapshotChunks over gRPC ──► follower: save to tmp file ──► engine.Restore()
```

This design has several compounding weaknesses:

* **Not resumable.** If the gRPC stream drops midway the follower discards the temp file and restarts from scratch.
* **Leader bears full I/O per follower.** The snapshot is re-created on demand for every requesting follower shard; no sharing between concurrent recoveries.
* **No freshness negotiation.** The follower always requests a brand-new snapshot even when a recent one on shared storage would suffice.
* **Recovery is slow and resource-intensive.** `readIntoTable` replays the entire snapshot as batched Raft proposals through the consensus protocol on a single-node recovery shard. For large tables this serialises all data through the Raft log a second time, making recovery an O(data) CPU and I/O operation on top of the snapshot transfer itself. Reads continue to be served from the old shard during this window (the `ClusterID` is only swapped in metadata after `readIntoTable` completes), but the recovery node is under heavy load for a long time.
* **Incremental path is opportunistic only.** `streamIncremental` falls back to full automatically, but incremental artefacts are never stored persistently and cannot be reused across follower nodes or retries.
* **No GC of stale artefacts.** Ephemeral snapshots are not explicitly garbage-collected when a transfer is interrupted.

## Design

### Shared Storage Abstraction

[**objfs**](https://github.com/armadakv/objfs) (`github.com/armadakv/objfs`) is used as the blob store interface. It provides:

* A common `Bucket` interface (`Upload`, `Get`, `GetRange`, `Attributes`, `Iter`, `Delete`).
* Concrete backends: S3, GCS, Azure, Swift, filesystem (suitable for NFS mounts), in-memory (for tests).
* Per-backend YAML configuration consumed by `objstore.NewBucket`.

No custom abstraction on top of `objstore.Bucket` is needed.

#### Object key scheme

```
snapshots/
  {table_name}/
    full/
      {leader_index}.snap          # full snapshot at raft index N
      {leader_index}.snap.meta     # JSON: size, sha256, created_at, node_id
    incr/
      {base_index}_{tip_index}.snap
      {base_index}_{tip_index}.snap.meta
    .lease/
      {node_id}                    # soft lease written by downloaders; prevents GC races
```

All artefacts are immutable once written. A `.meta` file is written **after** the snapshot is fully flushed and its sha256 verified; its presence is the commit signal. Followers ignore any artefact that lacks a corresponding `.meta` file.

`.meta` JSON schema:

```json
{
  "table":       "orders",
  "type":        "full",
  "base_index":  0,
  "tip_index":   184320,
  "size_bytes":  1073741824,
  "sha256":      "abc123...",
  "created_at":  "2026-05-17T10:00:00Z",
  "node_id":     "leader-node-1",
  "format":      "armada-command-v1",
  "gc_horizon":  183001
}
```

`gc_horizon` records the table's MVCC garbage-collection horizon at export time.
An incremental artefact only carries a *complete* delta for a follower strictly
above that index; at or below it the compacted versions are simply absent and
the delta is silently short. Recording the horizon in the artefact makes it
self-describing, so a follower reading the bucket directly — with no access to
the leader's live table — can still make that judgement.

### Leader Side

#### SnapshotExporter

A new background component (`storage/snapshot/exporter.go`) runs on the leader alongside the table manager:

```go
type ExporterConfig struct {
    Bucket          objfs.Bucket
    NodeID          string        // written into Meta.NodeID
    SnapshotTimeout time.Duration // per-export budget
    FullInterval    time.Duration // e.g. 6h
    IncrMaxChain    int           // links per full before the next export is forced full
}
```

**Full export loop:** an interval ticker calls `ExportFull` for every table this
node leads. Unlike the compaction path the ticker has no leadership gate of its
own, so the exporter asks `TableSnapshotService.IsLeader` before each table;
without that every member races to upload the same artefact.

Driving `ExportFull` is what makes the bucket usable at all. Incrementals are
only ever a delta from some base, so a bucket that accumulates nothing but
`incr/` objects gives a recovering follower nothing to start from.

**Incremental export:** triggered by Raft log compaction, which only fires on
the leader. The exporter asks the table FSM for a delta and receives its
`base_index`, `gc_horizon`, and `tip_index` from the same Pebble snapshot view.
When the requested base is at or below that view's horizon, the FSM rebases it
strictly above the horizon so compacted versions and tombstones cannot be
silently omitted. The chain off the newest full is still capped by
`IncrMaxChain`, bounding recovery time and the blast radius of a lost link.

#### GC Worker

```go
type GCConfig struct {
    Bucket    objfs.Bucket
    Retention time.Duration
    Interval  time.Duration
}
```

GC algorithm:

1. Walk all `.meta` files under `snapshots/` via `bucket.List`.
2. For each table, sort artefacts by `tip_index` descending.
3. Skip the whole table while any *live* lease file exists under `.lease/`.
   Leases older than `store.LeaseTTL` are treated as abandoned. They are not
   physically deleted during collection because `objfs` lacks conditional
   delete and a refresh could otherwise race with deletion.
4. Delete artefacts whose `created_at < now − Retention`, **except**: the most recent full snapshot (always retained) and any incremental artefact whose `base_index ≥ latest-full tip_index` (active incremental chain).
5. When a new full snapshot is committed, schedule immediate deletion of all incremental artefacts whose `tip_index < new_full.base_index`.
6. Write a tombstone entry to `gc/{timestamp}.log` before each deletion for auditability.

#### Extended Snapshot RPCs

One new RPC is added to the `Snapshot` service in `proto/replication.proto`:

```protobuf
service Snapshot {
  rpc Stream (SnapshotRequest)      returns (stream SnapshotChunk); // existing
  rpc Query  (SnapshotQueryRequest) returns (SnapshotQueryResponse); // new
}

message SnapshotQueryRequest {
  string table          = 1;
  uint64 follower_index = 2; // current applied index on the follower
  uint64 cluster_id     = 3; // source shard incarnation the follower believes in
}

message SnapshotQueryResponse {
  enum SnapshotType { NONE = 0; FULL = 1; INCREMENTAL = 2; }
  SnapshotType type = 1;
  uint64 base_index = 2;
  uint64 tip_index  = 3;
  string object_key = 4;
  bytes  sha256     = 5;
  int64  size_bytes = 6;
}
```

**There is no `Presign` RPC.** Snapshot artefacts are fetched over the leader's
existing HTTP endpoint (`GET /snapshots/{object...}`), which already serves
`Range` requests through `http.ServeFileFS` and is therefore resumable with no
protocol work. Where the blob backend can mint a time-limited URL, the handler
answers with a `307` redirect to it instead of streaming the bytes; the
follower's HTTP getter follows redirects and carries its `Range` header across,
so resumption is unaffected and the leader stops being a bandwidth bottleneck.
The handler uses `objfs.PresignedGet`, falling back to streaming when the
configured backend cannot sign URLs.

`Query` resolves metadata and establishes a short-lived bridge lease while the
follower starts its transfer. Both direct and proxy downloads own a complete
GC-lease lifecycle: they acquire, renew, and release a lease around the
transfer. Proxy lease operations use a dedicated leader HTTP endpoint keyed by
the follower's raft address, so renewals continue to reach the leader even when
the artifact GET follows a presigned object-store redirect.

Snapshot selection filters every candidate for applicability, freshness, and
incremental provenance before ranking it. An incremental is valid when its base
is strictly above the horizon captured in the artifact's export view; it is not
gated on the follower being above the leader's current horizon.

### Follower Side

#### Two distinct recovery paths

The correct recovery path depends on whether a live shard already exists with the required base state:

**Incremental path** — only valid when the local shard is live and its applied
index is at or past `incr.base_index`. A delta carries the latest version of
every user key changed since its base, each stamped with its own `LeaderIndex`;
applying it to a shard anywhere in `[base, tip)` rewrites versions the follower
already holds identically and adds the newer ones. It **cannot** bootstrap an
empty recovery shard — that would produce a partial dataset. The incremental
path therefore operates entirely on the running shard and never creates one.

The follower re-checks both conditions locally even though `SelectBestSnapshot`
already enforces them remotely: the resolver is untrusted.

**Full path** — required when no usable incremental exists (the follower is
behind the GC horizon, the table is new, or the chain is broken). It loads the
snapshot on exactly one node and lets the other members receive it through
Dragonboat's own Raft snapshot path.

#### RecoveryCoordinator

`worker.recover()` becomes a thin driver: negotiate → pin the artefact in the
journal → download (resumable, checksum-verified) → hand the staged file to the
storage side. The phase machine itself lives in `storage/table/recovery.go`,
next to the journal and the `NodeHost`, because every phase past the download
manipulates Raft shards.

All three recovery entry points — the replication worker, the operator-facing
maintenance `Restore` RPC, and `RestoreLegacy` — converge on that one
coordinator once the bytes are on local disk. The operator path records the
artefact as `local` (no object key, no checksum) and is thereby crash-safe and
learner-seeded too, which is a strict improvement for large tables: today they
replay through every voter.

```
Phase 0 – Negotiate
  ├─ Query(table, localAppliedIndex, sourceID)
  ├─ NONE                               → on-demand live snapshot from the leader
  ├─ INCREMENTAL, shard live, idx ≥ base → Incremental Path (I1–I3)
  └─ FULL                               → Full Path (F1–F7)

One recovery pass applies one selected artifact. It then returns to ordinary
`Log.Replicate`; only an actual `USE_SNAPSHOT` response triggers another pass.

━━━ Incremental Path ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Phase I1 – Download
  ├─ Journal {phase: downloading, artifact, coordinator: self}
  ├─ Direct and proxy modes acquire and renew a GC lease for the transfer duration
  ├─ HTTP GET with validated Range, or bucket.GetRange, into {DataDir}/snapshots-staging
  └─ Verify sha256 and size; on mismatch discard and renegotiate

Phase I2 – Replay Delta Commands into the Live Shard
  ├─ Journal {phase: loading, recovery_id: 0}
  └─ readIntoTable against the existing ClusterID

Phase I3 – Resume Log Replication
  └─ Delete the journal record; ordinary Log.Replicate tails from tip_index+1

━━━ Full Path ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Phase F1 – Download                         (as I1)

Phase F2 – Load into the Recovery Shard
  ├─ Allocate RecoveryID and journal {phase: loading, recovery_id}
  ├─ Record Table.RecoverID so the reconcile loop treats the shard as known
  ├─ Start a genuinely single-member shard: it elects itself immediately and
  │    can therefore propose the membership changes that follow
  ├─ readIntoTable(RecoveryID) — an O(data) Raft replay, unavoidable because the
  │    inter-cluster format is a stream of armadapb.Command, not Pebble SSTs,
  │    and the follower FSM must stamp its own sysLocalIndex
  └─ RequestSnapshot(RecoveryID) so peers get state by snapshot, never by replay
       (tolerate the documented rejection when a snapshot already exists at the
        same index — the normal outcome on a crash retry)

Phase F3 – Seed Learners
  ├─ Journal {phase: seeding, seed_index}
  ├─ For every member absent from SyncGetShardMembership():
  │    SyncRequestAddNonVoting(recoveryID, peer, addr, configChangeIndex)
  │    The coordinator addresses peers from cfg.InitialMembers, so gossip is an
  │    accelerator and never the source of truth.
  └─ Each admitted peer starts StartOnDiskReplica(nil, join=true, IsNonVoting)

Phase F4 – Wait for Catch-up
  └─ Each learner publishes its applied index; the coordinator waits for
     appliedIndex ≥ seed_index

Phase F5 – Promote
  ├─ Journal {phase: promoting}
  └─ One learner at a time, each with a fresh configChangeIndex, appending to
     Promoted after each success

Phase F6 – Swap
  ├─ Journal {phase: swapping}
  ├─ CAS Table.ClusterID = recoveryID, RecoverID = 0
  ├─ Delete the journal record
  └─ stopTable(old) → existing /cleanup/… tombstone and grace period
```

Throughout the full path the old `ClusterID` keeps serving reads unchanged:
`GetTable` routes via `ClusterID`, which is not updated until F6.

##### Why peers cannot add themselves

An earlier draft had each peer call `SyncRequestAddNonVoting(recoveryID, self)`
on its own NodeHost. That cannot work: `RequestAddNonVoting` looks the shard up
locally and returns `ErrShardNotFound` when it is not already present. A
membership change must be proposed by a member — here the coordinator, which is
the leader of its single-node recovery shard. Hence the invite/accept/admit
exchange below rather than a single `started` broadcast.

##### Why the quorum gate in F4 is load-bearing

The coordinator starts as the *only* voter, so promoting voter #2 makes quorum
2-of-2. Promoting a learner that has not caught up therefore stalls the shard
outright. F4 is a correctness gate, not an optimisation: a learner that never
reports is left as a learner for the reconcile loop to promote later, and is
never promoted unseen.

##### Suppressing auto-start of the recovery shard

`diffTables` used to start any shard named by `Table.RecoverID`. It must not:
the role a replica takes (single-voter coordinator vs. non-voting learner) is
not derivable from the table entry, and starting a learner as a voter is a
**panic** in the Raft layer, not an error. Recovery shard IDs are therefore
*known but never auto-started* — known so the reconcile loop does not stop the
coordinator's own shard, never auto-started so only the recovery loop, which has
the journal in hand, decides the role.

For the same reason `tablesReady()` no longer requires `RecoverID` shards to be
present. A recovery shard legitimately exists on only some nodes, so requiring
it wedged engine readiness for the whole recovery.

#### Recovery Journal

The durability spine is a record at `/recovery/{table}` in the same
Raft-replicated metadata store (shard 1000) as `/tables/{name}`, so the
reconcile loop reads table metadata and recovery phase in one pass.

```go
type RecoveryRecord struct {
    Table       string
    Phase       RecoveryPhase    // downloading | loading | seeding | promoting | swapping
    RecoveryID  uint64           // 0 on the incremental path
    Coordinator uint64           // node ID owning this recovery
    Learners    []uint64         // added as non-voting
    Promoted    []uint64         // promoted to voter
    SourceID    uint64           // leader shard incarnation
    SeedIndex   uint64           // index learners must reach
    Artifact    RecoveryArtifact // object key, type, indexes, sha256, size, stage path
    UpdatedAt   time.Time
}
```

Writes use the store's existing CAS discipline (`Get` → `Set(key, value, ver)`).

**Ordering rule: the journal write always precedes the side effect it
authorises.** Two consequences are worth spelling out.

*Pinning the artefact before downloading* matters because the leader may GC or
publish artefacts while a recovery is in flight; switching artefacts mid-flight
would mix two points in time into one half-loaded shard. Once the phase is past
`downloading` the artefact stays pinned even if a newer one is offered.

*Writing the journal before `startTable`* inverts the old `Manager.restore`
order, which allocated a shard ID, started the shard, and only then recorded it
— leaking sequence numbers on a crash.

| Step | Durable write | Then side effect |
|---|---|---|
| Negotiated | `{downloading, artifact, coordinator: self}` | start download |
| Download verified | allocate `RecoveryID` (full only), `{loading, recovery_id}` | — |
| — | `Table.RecoverID = RecoveryID` | `startRecoveryShard`, then replay |
| Load done | `{seeding, seed_index}` | `RequestSnapshot` |
| Each learner added | append `Learners` | admit the peer |
| All caught up | `{promoting}` | promote, one at a time |
| Each promotion | append `Promoted` | — |
| All promoted | `{swapping}` | CAS `ClusterID`; delete record; `stopTable(old)` |

#### Power-loss rules

Every phase is re-enterable from a reconcile tick:

* **downloading** — resume from the local `.part`/`.offset` sidecar. If the
  partial file is gone, restart from byte 0 for the *same* object key. If the
  object no longer exists, delete the record and renegotiate.
* **loading** — replay the staged artefact **from the beginning**. This is safe
  and is the only workable scheme. Each `PUT`/`DELETE` carries its own
  `LeaderIndex`, which the FSM uses as the MVCC seqno, so re-applying a command
  rewrites the identical physical key@seqno; and the artefact is a snappy
  stream, so record boundaries are not byte-addressable and no consumed-offset
  watermark could be reliable anyway. The terminal-marker validation in
  `readSnapshotWithTerminal` is a pure stream check, unaffected by prior shard
  state.
* **seeding / promoting** — re-derive real membership from
  `SyncGetShardMembership(recoveryID)`. `Learners`/`Promoted` are hints; Raft
  membership is truth.
* **swapping** — re-read the table; if `ClusterID != RecoveryID`, CAS it; delete
  the record.
* **coordinator lost** — a lease holder may retire an unrecoverable recovery,
  but retirement is serialized with phase advancement. It first version-checks
  and removes the exact journal record; only then may it stop/tombstone that
  record's recovery shard, clear the matching `RecoverID`, and renegotiate.

#### The `IsNonVoting` restart hazard

A replica must decide `config.IsNonVoting` *before* it can read its persisted
membership, and the two error directions are not symmetric:

* Starting **non-voting** when membership says voter is **recoverable** — the
  Raft layer promotes the replica to follower when it restores a snapshot.
* Starting as a **voter** while the log still holds an unapplied `AddNonVoting`
  entry naming this replica **panics**.

Erring toward non-voting is therefore always safe, and a node-local,
CRC-checked marker at `{DataDir}/recovery-role/{shardID}` makes that choice
durable. It is node-local rather than replicated because it describes this
replica's own on-disk Raft state. A corrupt or unreadable marker reads as
*present*, again because that is the safe direction.

Promotion visibility alone does not prove that the original `AddNonVoting`
entry is no longer replayable from local Raft data. The marker therefore remains
for the lifetime of that local replica data and is removed only when cleanup
removes the replica itself. This is deliberately conservative: starting with a
stale non-voting marker self-corrects when persisted membership restores the
member as a voter, whereas starting as a voter before replaying `AddNonVoting`
panics.

This is the one part of the design that could not be settled by reading the Raft
implementation alone. It is verified by test: `TestLearnerRestartAfterPromotion`
stops a promoted replica, re-writes its marker, restarts it non-voting over a
voting membership, and asserts it converges back to voter without panicking.

#### Gossip Coordination Protocol

Learner join and promotion are coordinated over the existing `cluster.Cluster`
message bus (`hashicorp/memberlist` over QUIC). The listener is registered from
the storage side, not from `replication.Manager`: the handlers touch Raft shard
lifecycle, which is the table manager's business, and `replication.Manager` is
follower-only.

| Key | Sender → receiver | Payload | Receiver action |
|---|---|---|---|
| `recovery/{table}/invite` | coordinator → broadcast | `{table, recoveryID, nodeID, raftAddr}` | not coordinator and no local replica → reply `accept` |
| `recovery/{table}/accept` | peer → coordinator (`SendTo`) | `{recoveryID, nodeID, raftAddr}` | add the peer as non-voting, then `admit` |
| `recovery/{table}/admit` | coordinator → peer (`SendTo`) | `{recoveryID, nodeID}` | write the local role marker, **then** start the replica |
| `recovery/{table}/progress` | learner → coordinator (`SendTo`) | `{recoveryID, nodeID, appliedIndex}` | record catch-up |
| `recovery/{table}/done` | coordinator → broadcast | `{recoveryID}` | clear invite state |

The three-way invite/accept/admit replaces the earlier single `started`
broadcast for three reasons: the coordinator must propose the membership change
(a peer cannot add itself), it needs the peer's raft address, and the peer must
not start its replica until the `AddNonVoting` entry is committed.

**Gossip is best-effort; the journal is the source of truth.** Every step is
also reachable from the reconcile tick, so no message needs to be delivered:

* Coordinator, each tick while seeding: for every node in `cfg.InitialMembers`
  absent from `SyncGetShardMembership().NonVotings ∪ .Nodes`, call
  `SyncRequestAddNonVoting` directly. Replica IDs are static (the 1-based
  position in `--raft.initial-members`) and the manager already holds the full
  node-ID → raft-address map, so a lost `accept` self-heals.
* Peer, each tick: if the record lists this node in `Learners` and no local
  replica exists, write the marker and join. A lost `admit` self-heals.

Because of that, the handlers do not act inline — they update the progress cache
and nudge the recovery loop, keeping one implementation of the phase machine.

**Learner catch-up cannot use the gossip shard view.** `ShardView.Replicas`
comes from `pb.Membership.Addresses`, i.e. **voters only**; `NonVotings` and
`Witnesses` are separate maps that are never copied in, so a learner is
invisible there. Nor does any Raft API expose a peer's match index —
`ShardInfo` has no per-replica progress and `GetNodeHostInfo` is local-only.
Catch-up is therefore self-reported: each learner reads its applied index from
its own FSM and publishes it both to the replicated store (durable, reliable)
and over gossip (fast). The store copy is what makes catch-up detection immune
to a dropped message; the coordinator takes the maximum of the two.

Edge cases:

* *Peer down for the whole recovery* — it joins on its next reconcile tick if
  the record still exists, or simply starts the (now swapped-in) `ClusterID`
  shard normally afterwards.
* *Two would-be coordinators* — the table lease serialises it, and
  `Coordinator` is CAS-written, so the loser observes the conflict and backs
  off.
* *Peer already holds the shard as a voter* — the coordinator-side add checks
  `SyncGetShardMembership().Nodes` and the peer-side join checks
  `HasNodeInfo`. Either way it is a no-op, never a panic. With auto-start
  suppressed a peer can only hold the recovery shard as a voter *after* the
  swap, when the record is gone.

#### Resumable download

Downloads land in `{DataDir}/snapshots-staging/` rather than `os.TempDir()`, so
they survive a restart, alongside an `.offset` sidecar recording the verified
byte count. Data is fsynced before the sidecar is updated, so the sidecar can
never claim bytes that are not on disk; a sidecar ahead of the file is
distrusted and the download restarts.

The getter reports whether the requested range was actually honoured
(HTTP `206` vs `200`). Without that signal a resumed download would silently
concatenate a fresh copy onto a partial one.

The raw/framed split is explicit: bytes are written into a plain file unframed,
and only wrapped in the snappy framing reader when the completed artefact is
handed to the snapshot reader.

sha256 and size are verified against the values the leader advertised — both
were already computed and transported, and both were previously ignored, which
left a truncated artefact to fail much later as an unparseable command stream.
On mismatch the staged file and the journal record are discarded and the
recovery renegotiates. Verification is skipped for live snapshots, which carry
no checksum.

### Configuration

```
--shared-store.backend            none | filesystem | s3 | gcs | azblob
--shared-store.filesystem.directory
--shared-store.s3.bucket
--shared-store.gcs.bucket
--shared-store.azure.container / .account / .key
--shared-store.retention          48h   # GC window
--shared-store.gc-interval        1h
--shared-store.full-interval      6h    # periodic full export per led table
--shared-store.incr-max-chain     8     # links per full before forcing a full
--replication.snapshot-timeout    10m   # budget for one export
--replication.snapshot-source     auto | direct | proxy   (follower)
```

`direct` lets the follower read the bucket itself and renew its own GC lease;
`proxy` routes artefacts through the leader's HTTP endpoint and renews its lease
there by the follower's raft address. `auto` picks `direct` when a shared-store
backend is configured.

In `direct` mode the follower still needs the leader over HTTP for the live
on-demand snapshot fallback, which is an endpoint rather than an object in the
store. Both are wired, and the recovery path picks between them through separate
interfaces (`SnapshotObjectGetter` and `LiveSnapshotGetter`) rather than by
sniffing the object key — prefix-sniffing is what made `direct` mode unable to
recover at all.

### Rollout Plan

1. **Phase A** *(landed)* — `SnapshotExporter`, GC worker, blob store
   integration on the leader.
2. **Phase B** *(landed)* — `Query` RPC, resumable checksum-verified downloads,
   GC leases, periodic full export, GC-horizon guards, and the incremental
   recovery path.
3. **Phase C** *(landed)* — learner-first promotion, the recovery journal, and
   gossip coordination. Learner-first **replaces** the old in-place full
   recovery; there is no feature flag and no second path to maintain.
4. **Phase D** — GA: enable by default for new deployments; document the
   migration path for existing clusters.

## Alternatives

**objfs vs. `gocloud.dev/blob`.** The Go Cloud Development Kit blob package was considered. It was rejected because it has fewer production-grade backends, less operational adoption in the Go infrastructure ecosystem, and no built-in `GetRange` semantics needed for resumable downloads.

**A `Presign` RPC vs. a redirect from the existing HTTP endpoint.** A dedicated
`Snapshot.Presign` RPC was considered. It was rejected because the leader
already serves artefacts over HTTP with `Range` support, so a `307` redirect to
a pre-signed URL achieves the same offload with no new RPC, no new client code
(the HTTP client follows redirects and preserves `Range` by default), and a
clean degradation to streaming when the backend cannot sign. Always proxying
would make the leader a bandwidth bottleneck when several followers recover at
once; the redirect shifts the transfer directly to the object store wherever
the backend supports it.

**Incremental snapshots as WAL deltas vs. MVCC deltas.** Using raw Raft WAL
entries as incremental artefacts was considered. It was rejected because WAL
entries are already covered by the existing `Log.Replicate` path; duplicating
them in shared storage adds complexity without benefit. The incremental
artefact is instead the same `armadapb.Command` stream as a full snapshot,
produced by `table.IncrementalSnapshot`, which is what makes replay idempotent
and therefore crash-restartable.

**In-place shard replacement vs. learner-first promotion.** The current in-place approach (`Manager.Restore`) was considered as the baseline to keep. It was rejected for the new implementation because `readIntoTable` serialises the entire dataset back through the Raft log, and adding peers after the fact requires Dragonboat to re-snapshot from the recovery node. Learner-first promotion lets Dragonboat stream the recovered state to peers directly via its normal Raft snapshot path, which is already efficient.

**Gossip (`cluster.Cluster`) vs. a new internal gRPC RPC for learner coordination.** A dedicated internal gRPC call (e.g. `InternalService.JoinRecoveryShard`) was considered to signal peers. It was rejected because the gossip bus is already present on every follower node, provides reliable unicast (`SendTo` → `ml.SendReliable`) and broadcast, and requires no new transport or port. Adding a new internal gRPC RPC would increase surface area with no meaningful benefit for this use case.

## Unresolved Questions

* **OQ1 (resolved): Shared credentials for direct bucket access.** Both are
  supported and selected by `--replication.snapshot-source`. `direct` requires
  distributing storage credentials to followers and in exchange lets them read
  the bucket and renew a GC lease throughout a transfer; `proxy` keeps
  credentials leader-side and renews the follower's lease through the leader's
  replication HTTP endpoint. `auto` picks `direct` when a backend is configured.
* **OQ2: Default `incr-max-chain` value.** A value of 8 is proposed. Longer chains reduce full-snapshot export I/O but increase recovery time (applying N patches sequentially). This needs benchmarking against realistic table sizes.
* **OQ3: Recovery parallelism.** For multi-table followers,
  `max-recovery-in-flight` already bounds concurrent recoveries on the worker
  side. The storage-side coordinator additionally serialises per table, but
  nothing yet bounds how many tables a node may coordinate at once. Should
  `max-recovery-in-flight` be extended to cover it, or a separate limit added?
* **OQ4: Incremental snapshot opt-out.** Should operators be able to disable incremental consumption and always fall back to full snapshots for operational simplicity, or should incremental always be attempted when available?
* **OQ5: Read staleness during long recovery.** During recovery the old shard keeps serving reads (existing behaviour). If the old shard's log is far behind the GC horizon — which is typically what triggered recovery — reads may serve significantly stale data for the duration of the recovery window. Should a configurable maximum staleness threshold be enforced, after which write RPCs return `UNAVAILABLE` to signal the cluster is degraded, rather than silently serving arbitrarily old data?

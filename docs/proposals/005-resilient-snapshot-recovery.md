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
  "format":      "checkpoint-v1"
}
```

### Leader Side

#### SnapshotExporter

A new background component (`storage/snapshot/exporter.go`) runs on the leader alongside the table manager:

```go
type ExporterConfig struct {
    Bucket       objstore.Bucket
    FullInterval time.Duration // e.g. 6h
    IncrInterval time.Duration // e.g. 30m
    IncrMaxChain int           // max incremental chain length before forcing a new full
    Retention    time.Duration // GC window
}
```

**Full export loop:** calls `table.Snapshot(ctx)` to produce a Pebble checkpoint, streams the checkpoint files to `snapshots/{table}/full/{index}.snap` via `bucket.Upload`, then writes the `.meta` commit file.

**Incremental export loop:** determines `base_index` as the tip of the latest full or incremental artefact for the table, calls `table.IncrementalSnapshot(ctx, base_index)` to obtain delta SSTs, uploads them as `snapshots/{table}/incr/{base}_{tip}.snap`. If the incremental chain length reaches `IncrMaxChain`, the cycle is skipped and the next full export takes over.

#### GC Worker

```go
type GCWorker struct {
    Bucket    objstore.Bucket
    Retention time.Duration
    Interval  time.Duration
}
```

GC algorithm:

1. Walk all `.meta` files under `snapshots/` via `bucket.Iter`.
2. For each table, sort artefacts by `tip_index` descending.
3. Skip any artefact that has a lease file under `.lease/`.
4. Delete artefacts whose `created_at < now − Retention`, **except**: the most recent full snapshot (always retained) and any incremental artefact whose `base_index ≥ latest-full tip_index` (active incremental chain).
5. When a new full snapshot is committed, schedule immediate deletion of all incremental artefacts whose `tip_index < new_full.base_index`.
6. Write a tombstone entry to `gc/{timestamp}.log` before each deletion for auditability.

#### Extended Snapshot RPCs

Two new RPCs are added to the `Snapshot` service in `proto/replication.proto`:

```protobuf
service Snapshot {
  rpc Stream   (SnapshotRequest)       returns (stream SnapshotChunk); // existing
  rpc Query    (SnapshotQueryRequest)  returns (SnapshotQueryResponse); // new
  rpc Presign  (PresignRequest)        returns (PresignResponse);       // new
}

message SnapshotRequest {
  string table             = 1; // existing
  uint64 leader_index      = 2; // existing
  uint64 resume_from_chunk = 3; // new: 0 means start from beginning
}

message SnapshotQueryRequest {
  string table          = 1;
  uint64 follower_index = 2; // current applied index on the follower
}

message SnapshotQueryResponse {
  enum SnapshotType { NONE = 0; FULL = 1; INCREMENTAL = 2; }
  SnapshotType type      = 1;
  uint64 base_index      = 2;
  uint64 tip_index       = 3;
  string object_key      = 4;
  bytes  sha256          = 5;
  int64  size_bytes      = 6;
  bool   presign_capable = 7; // true when the bucket supports pre-signed URLs
}

message PresignRequest  { string object_key = 1; google.protobuf.Duration ttl = 2; }
message PresignResponse { string url = 1; }
```

The existing `Stream` RPC is kept for NFS-backed deployments and as a universal fallback.

### Follower Side

#### Two distinct recovery paths

The correct recovery path depends on whether a live shard already exists with the required base state:

**Incremental path** — only valid when the local shard is live and its applied index ≥ `incr.base_index`. Because an incremental snapshot is a set of Pebble SST delta files that must be ingested on top of existing state, it **cannot** bootstrap an empty recovery shard — that would produce a partial dataset and lead to data loss. The incremental path therefore operates entirely on the running shard without creating a new one.

**Full path** — required when no usable incremental exists (shard is behind the GC horizon, table is new, or incremental chain is broken). Uses a separate recovery shard (as today) but replaces the slow `readIntoTable` Raft-replay with direct Pebble SST ingest.

#### RecoveryCoordinator

Replaces `worker.recover()` in `replication/worker.go`. The coordinator selects the path at negotiation time and runs the appropriate phase sequence:

```
Phase 0 – Negotiate
  ├─ Call QuerySnapshot(table, localAppliedIndex)
  ├─ NONE                                       → back off and retry
  ├─ INCREMENTAL and localAppliedIndex ≥ base   → Incremental Path (Phases I1–I3)
  └─ FULL (or INCREMENTAL but shard is dead)    → Full Path (Phases F1–F7)

━━━ Incremental Path ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Phase I1 – Download Incremental Snapshot
  ├─ Write lease file to prevent GC races
  ├─ (S3/GCS) PresignSnapshot → HTTP GET with Range header (resumable)
  ├─ (NFS/fallback) gRPC SnapshotStream with chunk-level .ckpt sidecar
  └─ Verify sha256; remove lease file on success

Phase I2 – Replay Delta Commands into Live Shard
  └─ The downloaded artefact is a stream of armadapb.Command (PUT/DELETE)
     messages — the same wire format as the full snapshot.
     Propose them in batches via readIntoTable targeting the existing shard's
     ClusterID. Each batch is stamped with the shard's own Raft log index
     (sysLocalIndex) while the LeaderIndex carried in the final DUMMY command
     becomes the new sysLeaderIndex and MVCC seqno ceiling.

Phase I3 – Resume Log Replication
  └─ Resume Log.Replicate from tip_index+1 on the existing shard
     (no shard replacement; recovery is complete)

━━━ Full Path ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Phase F1 – Download Full Snapshot
  ├─ Write lease file to prevent GC races
  ├─ (S3/GCS) PresignSnapshot → HTTP GET with Range header (resumable)
  ├─ (NFS/fallback) gRPC SnapshotStream with chunk-level .ckpt sidecar
  └─ Verify sha256; remove lease file on success

Phase F2 – Replay Commands into Recovery Shard
  ├─ Start a new single-node Dragonboat shard with RecoveryID
  │    (same as today: startTable + waitForLeader)
  ├─ Call readIntoTable(RecoveryID, reader) to replay the downloaded
  │    armadapb.Command stream as batched Raft proposals.
  │    This is the same O(data) Raft-log replay as the current implementation.
  │    It is unavoidable: each batch must pass through the FSM so that
  │    sysLocalIndex is stamped with the recovery shard's own Raft index, and
  │    sysLeaderIndex / MVCC seqnos are set from the embedded LeaderIndex.
  │    Raw Pebble SSTs from the leader cannot be used here — they carry the
  │    leader's sysLocalIndex, which is meaningless to the recovery shard's
  │    Raft log, and the data format (intra-cluster Raft snapshots) is
  │    entirely different from the inter-cluster armadapb.Command stream.
  └─ After readIntoTable completes, call RequestSnapshot() on the recovery
       shard to force Dragonboat to materialise an intra-cluster snapshot
       (SST-based, via PrepareSnapshot/SaveSnapshot). This snapshot is what
       learner peers will receive — they never re-run readIntoTable.

Phase F3 – Catch-up via Log Replication
  └─ Resume Log.Replicate from tip_index+1 targeting RecoveryID

Phase F4 – Catch-up via Log Replication
  └─ Resume Log.Replicate from tip_index+1 targeting RecoveryID
     (existing replication worker re-targeted to RecoveryID)

Phase F5 – Notify Peers via Gossip
  ├─ Broadcast Message{Key: "recovery/{table}/started",
  │      Payload: {recoveryID, raftAddress, nodeID}} on cluster.Cluster
  └─ Each peer that receives the message and has a WatchPrefix("recovery/")
       calls SyncRequestAddNonVoting(recoveryID, self) to join as a learner

Phase F6 – Wait for Learners
  └─ Poll cluster.ShardInfo(recoveryID) until all expected replicas
       appear with an applied index ≥ recovery shard's index − threshold
     (shard view is propagated automatically via existing gossip broadcasts)

Phase F7 – Promote & Swap
  ├─ For each learner: SyncRequestAddNode(recoveryID, peer) → voter
  ├─ Broadcast Message{Key: "recovery/{table}/done", Payload: {recoveryID}}
  │    so peers can clean up their listener state
  ├─ Atomically update table metadata: ClusterID = recoveryID, RecoverID = 0
  └─ Schedule old shard for shutdown and cleanup (existing stopTable / cleanup path)
```

Throughout the full path (Phases F2–F7), the existing shard (old `ClusterID`) continues to serve reads unchanged — `GetTable` always routes via `ClusterID`, which is not updated until Phase F7.

The initial command replay in Phase F2 (`readIntoTable`) is the same O(data) cost as today — it cannot be avoided because the inter-cluster snapshot format is a stream of `armadapb.Command` protobuf messages, not raw Pebble SSTs, and the follower FSM must stamp its own `sysLocalIndex` (Raft log index) and `sysLeaderIndex` (leader MVCC seqno) independently. These are entirely separate from the intra-cluster Raft snapshot format (`PrepareSnapshot`/`SaveSnapshot`/`RecoverFromSnapshot`).

The key improvements over the current design are:
1. **Resumable download**: the command stream is stored durably in the blob store and can be resumed mid-transfer, unlike the current single-shot gRPC stream that discards progress on failure.
2. **Reusable artefact**: one export serves all followers; the leader does not re-generate the snapshot per requester.
3. **Peer distribution via intra-cluster snapshot**: after `readIntoTable` completes, `RequestSnapshot()` forces Dragonboat to materialise an SST-based intra-cluster snapshot. Learner peers receive this snapshot via Dragonboat's built-in Raft snapshot path — they never run `readIntoTable` themselves, eliminating the O(data) cost for every peer beyond the first.

#### Gossip Coordination Protocol

Learner join and promotion are coordinated through the existing `cluster.Cluster` message bus (backed by `hashicorp/memberlist` over QUIC):

| Message key | Direction | Payload |
|-------------|-----------|---------|
| `recovery/{table}/started` | recovery node → all peers | `{recoveryID uint64, raftAddr string, nodeID uint64}` |
| `recovery/{table}/done` | recovery node → all peers | `{recoveryID uint64}` |

Peers register a `WatchPrefix("recovery/")` listener in the `replication.Manager` on startup. On receiving a `started` message they call `SyncRequestAddNonVoting(recoveryID, self)` on their local `NodeHost`. On receiving `done` they deregister the learner join state.

`SendTo` (`ml.SendReliable`) provides reliable unicast over the existing QUIC stream channel. If a peer has not yet joined when the broadcast is sent, the `discover` loop and anti-entropy push-pull in memberlist ensure the message is delivered within one gossip round (≤ 5 s). If a peer misses the broadcast it can also detect the new shard by observing an unknown `recoveryID` in the gossip shard view and self-nominate as a learner.

#### Resumable gRPC Stream

For the gRPC `Stream` fallback, chunk-level checkpointing is introduced:

* After each `SnapshotChunk` is fsynced to the `.tmp` file, the chunk sequence number is written to a `.ckpt` sidecar.
* On restart, if a `.ckpt` exists, the follower sends `resume_from_chunk` in `SnapshotRequest`.
* The leader seeks to that chunk in the stored snapshot file and resumes from there.

For S3/GCS downloads, HTTP `Range` requests provide native resumability; only the byte offset needs to be persisted in `.ckpt`.

### Configuration

New settings live under `replication.snapshot-store` in Viper config, mirroring the objfs YAML embedding pattern:

```yaml
replication:
  snapshot-store:
    backend: s3          # s3 | gcs | azure | filesystem | none (disables feature)
    config: |            # backend-specific YAML passed verbatim to objstore
      bucket: my-armada-snapshots
      region: us-east-1
    full-interval: 6h
    incr-interval: 30m
    incr-max-chain: 8
    retention: 48h
    gc-interval: 1h
    presign-ttl: 1h
```

CLI flags follow the same dot-separated key convention (`replication.snapshot-store.backend`, etc.).

### Rollout Plan

Each phase is independently deployable and backward compatible with the existing `Stream` RPC. Old followers that have not been updated continue to use the existing path.

1. **Phase A** — Implement `SnapshotExporter`, GC worker, and blob store integration on the leader. Deploy with `backend: none`. No follower changes required.
2. **Phase B** — Implement `Query` and `Presign` RPCs; add `resume_from_chunk` to `Stream`. Wire up follower `RecoveryCoordinator` behind a feature flag (`replication.snapshot-store.enabled: false` by default).
3. **Phase C** — Implement learner-first shard promotion (Phases F5–F7) and gossip coordination. Enable in staging with `backend: filesystem` (NFS).
4. **Phase D** — GA: enable by default for new deployments; document the migration path for existing clusters.

## Alternatives

**objfs vs. `gocloud.dev/blob`.** The Go Cloud Development Kit blob package was considered. It was rejected because it has fewer production-grade backends, less operational adoption in the Go infrastructure ecosystem, and no built-in `GetRange` semantics needed for resumable downloads.

**Pre-signed URLs vs. always-proxy through the leader.** Always proxying snapshot data through the leader gRPC server was considered. This was rejected because the leader becomes a bandwidth bottleneck when several follower shards recover concurrently. Pre-signed URLs shift the transfer directly between the follower and the object store; the leader is only involved in signing (milliseconds of overhead).

**Incremental snapshots as WAL deltas vs. SST-level deltas.** Using raw Raft WAL entries as incremental artefacts was considered. It was rejected because WAL entries are already covered by the existing `Log.Replicate` path; duplicating them in shared storage adds complexity without benefit. SST-level incremental snapshots reuse the existing `table.IncrementalSnapshot` / Pebble SST ingest path with minimal new code in the FSM.

**In-place shard replacement vs. learner-first promotion.** The current in-place approach (`Manager.Restore`) was considered as the baseline to keep. It was rejected for the new implementation because `readIntoTable` serialises the entire dataset back through the Raft log, and adding peers after the fact requires Dragonboat to re-snapshot from the recovery node. Learner-first promotion lets Dragonboat stream the recovered state to peers directly via its normal Raft snapshot path, which is already efficient.

**Gossip (`cluster.Cluster`) vs. a new internal gRPC RPC for learner coordination.** A dedicated internal gRPC call (e.g. `InternalService.JoinRecoveryShard`) was considered to signal peers. It was rejected because the gossip bus is already present on every follower node, provides reliable unicast (`SendTo` → `ml.SendReliable`) and broadcast, and requires no new transport or port. Adding a new internal gRPC RPC would increase surface area with no meaningful benefit for this use case.

## Unresolved Questions

* **OQ1: Shared credentials for direct bucket access.** Should the blob store configuration be shared between leader and follower (so followers can skip the `Presign` round-trip for NFS mounts and read directly), or should it remain strictly leader-side with followers always going through the leader API? Direct follower access simplifies the hot path but requires distributing storage credentials to follower nodes.
* **OQ2: Default `incr-max-chain` value.** A value of 8 is proposed. Longer chains reduce full-snapshot export I/O but increase recovery time (applying N patches sequentially). This needs benchmarking against realistic table sizes.
* **OQ3: Recovery parallelism.** For multi-table followers, `max-recovery-in-flight` already bounds concurrent recoveries. Should this knob be extended to govern the new `RecoveryCoordinator` instances, or should a separate limit be introduced?
* **OQ4: Incremental snapshot opt-out.** Should operators be able to disable incremental consumption and always fall back to full snapshots for operational simplicity, or should incremental always be attempted when available?
* **OQ5: Read staleness during long recovery.** During recovery the old shard keeps serving reads (existing behaviour). If the old shard's log is far behind the GC horizon — which is typically what triggered recovery — reads may serve significantly stale data for the duration of the recovery window. Should a configurable maximum staleness threshold be enforced, after which write RPCs return `UNAVAILABLE` to signal the cluster is degraded, rather than silently serving arbitrarily old data?

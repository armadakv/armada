# Recovery Simplification Execution Plan

## Scope

This plan covers production recovery, replication, snapshot-store code, and their unit tests.

**Out of scope:** `integration/` and its separate Go module. It will be reviewed independently.

The work should be delivered in small, independently testable changes. Do not combine lifecycle refactoring, snapshot policy changes, and transport changes into one large rewrite.

## Goals

Reduce the number of components independently making recovery decisions:

- `storage/table` owns durable recovery state, shard lifecycle, membership, swapping, and cleanup.
- The replication worker owns remote negotiation, download, and verification only.
- Snapshot-store code owns artifact selection, publication, leases, and GC.
- Ordinary log replication remains the authority on whether a follower can resume from an artifact's tip.

The desired end state has:

1. one recovery lifecycle authority;
2. one shard-start policy;
3. one snapshot eligibility/ranking policy;
4. one download lifecycle;
5. one artifact publication path.

## Baseline

Before beginning, run:

```sh
go test ./storage/table ./replication ./replication/store ./replication/snapshot ./armadaserver ./cmd/armada -count=1
git diff --check
```

Preserve existing behavior covered by:

- `storage/table/recovery_test.go`
- `replication/worker_test.go`
- `replication/store/*_test.go`
- `armadaserver/replication_test.go`

Add regression tests with each correctness fix. Comments alone must not carry recovery-safety invariants.

---

## Phase 1: Serialize recovery lifecycle and cleanup

### Problem

Recovery progression and teardown are driven from more than one place:

- the replication worker resumes and completes downloads;
- the table-manager recovery loop advances phases;
- synchronous restore coordinates recovery separately;
- `abandonRecovery` performs destructive effects before version-checked journal deletion.

In particular, `abandonRecovery` can stop a shard, clear `RecoverID`, and delete staged data based on an old recovery record. A concurrently advancing recovery can then fail the final version check, after its newer state has already been damaged.

### Files

- `storage/table/recovery.go`
- `storage/table/manager.go`
- `storage/table/recovery_test.go`
- `replication/worker.go`
- `replication/worker_test.go`

### Changes

1. Use one per-table recovery-operation boundary for every operation that mutates, advances, replaces, or abandons a recovery:
   - `DriveRecovery`
   - `CompleteDownload`
   - local `Restore` / `coordinateRecovery`
   - `AbandonRecovery`
   - ownership claims
   - stale recovery cleanup

   The existing `beginRecoveryWork` / `endRecoveryWork` mechanism may be retained, but all of these operations must consistently use it.

2. Refactor abandonment into a serialized, version-safe operation:

   ```text
   acquire recovery slot
   read current recovery record and version
   validate the target recovery still matches
   persist a cancellation/retirement transition, or atomically remove the record
   perform cleanup authorized by that durable transition
   ```

   A journal-version conflict must happen **before** stopping shards, clearing metadata, or deleting staged data.

3. Do not introduce a broad new recovery framework. Retain the current durable phases where possible. A narrow internal helper such as `retireRecoveryLocked` is sufficient if it requires the per-table slot and re-reads the journal.

4. Provide one explicit storage-side completion result. The worker should not infer completion from a mixture of:
   - whether this invocation observed a journal record;
   - `forceRecovery`;
   - a nonzero source index.

   The exact API shape is flexible, but it should clearly distinguish:
   - no recovery exists;
   - recovery needs download;
   - recovery is pending;
   - recovery completed.

5. Keep layer ownership narrow:

   | Layer | Responsibility |
   |---|---|
   | Replication worker | query the leader, download, verify checksum/size, submit staged path |
   | Table manager | journal, phase transitions, shard lifecycle, membership, swap, cleanup |
   | Snapshot store | artifact selection, publication, leases, GC |

### Required tests

1. An old abandonment attempt races with advancement/swap:
   - it must not stop a newly serving shard;
   - it must not clear a newer `RecoverID`;
   - it must not delete a newer recovery journal.

2. A corrupt artifact clears only the matching active recovery.

3. The worker marks source restoration only after the table manager explicitly reports completion.

---

## Phase 2: Unify shard startup policy

### Problem

`startTable` and `startRecoveryShard` each construct raft configuration and independently choose bootstrap, restart, joining, role markers, and error handling.

A member that is offline through recovery can return after the journal has been deleted with no local log for the recovered shard. The ordinary `startTable` path then bootstraps all configured voters, although the recovery shard was originally bootstrapped only with its coordinator. NodeHost does not automatically convert that fresh bootstrap into a correct join.

### Files

- `storage/table/manager.go`
- `storage/table/recovery.go`
- `storage/table/recovery_role.go`
- `storage/table/recovery_test.go`
- `storage/table/manager_test.go`

### Changes

1. Replace the two startup implementations with a single lower-level helper using explicit intent:

   ```go
   type replicaStartMode int

   const (
       replicaBootstrap replicaStartMode = iota
       replicaRestart
       replicaJoin
   )

   type replicaStartOptions struct {
       Mode        replicaStartMode
       IsNonVoting bool
       Members     map[uint64]raft.Target
   }
   ```

   Exact names are not important. Explicit bootstrap/restart/join semantics are.

2. Make startup behavior unambiguous:

   | Situation | Start mode |
   |---|---|
   | New ordinary table | bootstrap with configured initial voters |
   | New recovery coordinator shard | bootstrap coordinator only |
   | Learner joining recovery | join as non-voting |
   | Existing local log | restart without bootstrap members |
   | Late member after recovery | join/restart from recovered serving membership; never bootstrap all voters |

3. Persist enough serving-shard origin information after a recovery journal is removed for a late member to select join/restart correctly.

   Do not rely on `HasNodeInfo == false` to imply ordinary bootstrap. Choose the smallest durable representation that resolves bootstrap versus join ambiguity.

4. Keep the non-voting marker protection conservative:
   - retain it for the lifetime of the local replica data; promotion visibility
     cannot prove the original `AddNonVoting` log entry is no longer replayable;
   - remove it only alongside the corresponding local replica data;
   - propagate unexpected marker read/open errors rather than interpreting them as absent.

### Required tests

1. Three-node recovery:
   - node 1 coordinates;
   - node 2 catches up and is promoted;
   - node 3 is offline through swap;
   - recovery journal is deleted;
   - node 3 starts afterward;
   - assert it joins/restarts the recovered shard without creating a conflicting bootstrap configuration.

2. A non-`os.IsNotExist` marker read failure is handled conservatively.

3. Promotion visibility does not remove a learner marker; removing the local
   replica data does.

---

## Phase 3: Scope progress by recovery generation and enforce quorum

### Problem

Recovery progress is stored in metadata and a gossip cache keyed only by table and node. Gossip carries `RecoveryID`, but handling does not use it. Progress from recovery A can satisfy recovery B's catch-up condition even though local Raft indexes from separate shards are unrelated.

`recoverySeed` can also advance when learner admissions failed because it compares caught-up learners only with learners admitted in the current pass. If both sets are empty, a later pass can promote and swap a single-voter shard in a multi-member cluster.

### Files

- `storage/table/recovery.go`
- `storage/table/recovery_test.go`

### Changes

1. Key durable progress and gossip cache entries by `(table, recoveryID, nodeID)`.

   Update:

   - `recoveryProgressKey`
   - `progressCacheKey`
   - `reportProgress`
   - `progressOf`
   - `clearProgress`
   - `caughtUpLearners`

2. Ignore or only use as a reconciliation nudge gossip progress for a recovery ID that is not currently active.

3. Make `caughtUpLearners` accept a full `RecoveryRecord`, not only a table name.

4. Base seeding and promotion eligibility on intended membership and a safe majority, not only on admission requests that happened to succeed in one invocation.

5. Preserve the intended availability policy:
   - recovery can proceed after timeout with a safe caught-up majority;
   - a multi-member deployment cannot swap with only its coordinator;
   - lagging members can be added later through serving-membership reconciliation.

### Required tests

1. A delayed progress report for recovery A must not count toward recovery B for the same table/node.

2. Failed or cancelled learner admissions must not permit promotion/swap with zero learners.

3. Timeout policy:
   - safe majority caught up: recovery can advance;
   - unsafe minority: recovery remains pending.

---

## Phase 4: Keep recovery coordination responsive

### Problem

The recovery reconciliation loop synchronously drives loading. A large or blocked `readIntoTable` can prevent recovery work for unrelated tables from being reconciled.

### Files

- `storage/table/recovery.go`
- `storage/table/recovery_test.go`

### Changes

1. Keep `reconcileRecovery` short:
   - inspect records;
   - schedule per-table work;
   - process learner join/progress;
   - handle liveness and ownership decisions.

2. Do not execute an unbounded `readIntoTable` from the coordination loop.

3. Use the existing per-table in-flight protection to run lifecycle-tracked recovery work.

4. The scheduler must:
   - stop on manager close;
   - avoid duplicate work for one table;
   - release its in-flight slot on return;
   - retain journal-driven retry semantics.

The scheduler is not a second source of truth. Durable journal state remains authoritative.

### Required test

Start a blocked recovery load for table A, then demonstrate that table B can still join/report learner progress and have its recovery reconciled.

---

## Phase 5: Make incremental snapshot completeness atomic with the snapshot view

### Problem

The exporter reads GC horizon, chooses/rebases the delta base, then independently creates an incremental snapshot. GC can advance and delete tombstones between these actions. The produced artifact can record the old horizon while omitting tombstones required by its claimed base.

### Files

- `storage/table/table.go`
- `storage/table/fsm/query.go`
- `storage/table/fsm/fsm.go`
- `replication/store/exporter.go`
- relevant FSM/table tests
- `replication/store/exporter_test.go`

### Changes

1. Extend the FSM incremental snapshot result to return metadata from the exact same Pebble view:

   ```go
   type SnapshotResponse struct {
       TipIndex  uint64
       BaseIndex uint64
       GCHorizon uint64
   }
   ```

   Adapt existing response types rather than introducing parallel types where practical.

2. Inside the incremental snapshot operation:
   - use one Pebble reader/snapshot;
   - read local index and `sysGCHorizon` from that reader;
   - validate or rebase requested base against that horizon;
   - emit records from that same reader;
   - return effective base, horizon, and tip.

3. Make the exporter use returned values directly in artifact metadata.

4. Remove the exporter-side sequence:

   ```text
   GCHorizon() -> choose/rebase base -> IncrementalSnapshot()
   ```

5. Do not create a generic snapshot transaction abstraction. This consistency guarantee belongs in the FSM snapshot operation.

### Required tests

1. Simulate GC advancing after initial base selection but before the snapshot view opens. Assert export either safely rebases/rejects or reports metadata matching the actual view.

2. Assert every nonzero-horizon incremental artifact satisfies:

   ```text
   BaseIndex > GCHorizon
   ```

---

## Phase 6: Centralize snapshot eligibility and ranking

### Problem

Bucket and RPC resolvers duplicate selection, provenance checks, fallback logic, and `fullSnapshots`. Both select the best artifact first, reject an invalid selected incremental, then retry using only full snapshots. That misses lower-ranked but valid incremental candidates.

The worker adds separate recovery-chain policy to compensate for query behavior.

### Files

- `replication/store/query.go`
- `replication/snapshot_query.go`
- `armadaserver/replication.go`
- `replication/snapshot_query_test.go`
- `armadaserver/replication_test.go`

### Changes

1. Add one pure store-level policy API. For example:

   ```go
   type SnapshotSelectionOptions struct {
       FollowerIndex                    uint64
       LiveGCHorizon                    uint64
       RequireKnownIncrementalHorizon   bool
   }

   func SelectRecoverableSnapshot(
       metas []Meta,
       options SnapshotSelectionOptions,
   ) (Meta, bool)
   ```

2. The policy must:
   1. discard stale/non-resumable artifacts;
   2. discard invalid incremental provenance;
   3. discard artifacts inapplicable to the follower;
   4. rank all remaining candidates using the existing best-candidate ordering.

3. Delete duplicate selection predicates and `fullSnapshots` helpers.

4. Keep transport-specific behavior outside the shared policy:
   - bucket resolver obtains its horizon information from metadata;
   - RPC resolver obtains live table horizon;
   - RPC maps errors to gRPC status codes;
   - both use the same selection helper.

5. Simplify worker recovery flow:
   - apply one selected artifact;
   - return to normal log replication;
   - recover again only if `Log.Replicate` actually returns `USE_SNAPSHOT`.

This should eliminate most of the special chain handling and context-dependent interpretation of `NONE`.

### Required tests

Add table-driven store-level tests:

1. Invalid highest-ranked incremental plus valid lower-ranked incremental selects the valid incremental.
2. Invalid incremental plus valid full selects the full.
3. No applicable artifact returns no selection.
4. Artifact tip below the horizon is rejected.
5. A follower is treated as able to tail only according to the actual log-replication contract.

Keep RPC/bucket tests thin: they should verify wiring and status mapping rather than reimplementing selection cases.

---

## Phase 7: Consolidate download lifecycle and make resumption durable

### Problem

Lease ownership, checkpointing, range handling, and verification are spread among query RPCs, worker code, staging code, and GC.

Current gaps:

- direct mode acquires a lease once; proxy mode writes a lease during query; neither renews over a long uninterrupted transfer;
- `Staged.Commit` is called only after `io.Copy` returns, so a process crash during an initial long download loses the whole partial transfer;
- sidecar contents and parent directory are not synced;
- HTTP treats any `206` response as proof that the requested offset was honoured.

### Files

- `replication/worker.go`
- `replication/snapshot_getter.go`
- `replication/snapshot/http_client.go`
- `replication/snapshot/snapshot.go`
- `replication/store/lease.go`
- `replication/store/gc.go`
- `armadaserver/replication.go`
- `replication/worker_test.go`
- `replication/snapshot_getter_test.go`
- new direct `replication/snapshot` tests as needed

### Changes

1. Introduce one small download operation responsible for:
   - acquiring a lease;
   - periodically refreshing it;
   - copying bytes;
   - periodically persisting a durable checkpoint;
   - restart behavior when range is ignored;
   - checksum and size verification;
   - releasing the lease.

2. Checkpoint during long copies and recoverable copy failures, not only when the copy returns successfully.

3. Harden `Staged.Commit`:
   - sync artifact data;
   - write temporary sidecar;
   - sync temporary sidecar;
   - rename it;
   - sync the containing directory.

4. Validate `Content-Range` before returning `ranged=true` for a nonzero requested offset.

5. Move proxy lease lifetime out of query-only behavior.
   - Query selects metadata.
   - Transfer lifetime owns acquire/renew/release.
   - If leader-side proxy leasing remains necessary, provide explicit acquire/renew/release support instead of relying on query retries.

6. Resolve the GC refresh race.
   - Do not blindly delete a stale lease key if it can be refreshed under that same key between list and delete.
   - Prefer deferring physical stale-lease cleanup unless conditional delete exists.
   - If physical cleanup is required, use generation/immutable lease keys.

### Required tests

1. Reader fails after N bytes:
   - checkpoint exists;
   - next attempt requests byte N;
   - final staged content is correct.

2. Reopen staged state after a simulated restart and resume only checkpointed bytes.

3. A mismatched `Content-Range` never appends to the staged prefix.

4. A long transfer refreshes its lease before `LeaseTTL`.

5. Lease refreshed between GC listing and deletion is not removed.

---

## Phase 8: Share artifact publication and use objfs presigning

### Problem

Full and incremental export duplicate terminal marker writing, sync, checksum, idempotency, upload, cleanup, and metadata publication. Their failure ordering differs: full export reads GC horizon after upload, so a horizon-read failure can leave an artifact object invisible to metadata-driven GC.

The custom `PresignedGetter` interface does not match the existing `objfs` capability and is not implemented by configured backends.

### Files

- `replication/store/exporter.go`
- `replication/store/exporter_test.go`
- `replication/store/http_handler.go`
- relevant HTTP handler tests

### Changes

1. Extract a narrow shared publication helper, for example:

   ```go
   func (e *SnapshotExporter) publishArtifact(
       ctx context.Context,
       meta Meta,
       source *snapshot.File,
   ) error
   ```

   It should own:
   - terminal `DUMMY` command handling where required;
   - sync;
   - size and checksum;
   - idempotency/meta check;
   - artifact upload;
   - cleanup on failure;
   - metadata-last publication.

2. Keep full/incremental snapshot generation and base selection separate. Do not build a generic exporter framework.

3. Obtain all required metadata before upload, or reliably delete the uploaded object for every later failure. In particular, fix the full-export horizon-read leak.

4. Delete local `PresignedGetter`.

5. Use pinned objfs support:

   ```go
   objfs.PresignedGet(ctx, bucket, objectKey, presignTTL)
   ```

   Fall back to streaming on unsupported signing capability.

### Required tests

1. Full-export horizon-read failure leaves no orphan object without metadata.
2. Metadata-upload failure attempts artifact cleanup.
3. An `objfs.Presigner`-capable bucket produces an HTTP redirect.
4. Unsupported presigning falls back to ordinary streaming.

---

## Phase 9: Documentation and validation

### Files

- `docs/operations_guide/replication.md`
- `docs/proposals/005-resilient-snapshot-recovery.md`
- generated CLI docs only if command help/flags change

### Changes

1. Update documentation to describe actual behavior:
   - incremental safety is determined by the export-time snapshot view;
   - stored artifacts do not by themselves prove logs remain available;
   - log replication decides whether another recovery is necessary;
   - leases protect active transfer duration and are renewed;
   - live snapshots are transport-specific, not a different Raft-recovery model.

2. Remove obsolete comments describing eliminated worker-side chain interpretation, query-owned leases, or duplicated startup assumptions.

3. Format and validate:

   ```sh
   gofmt -w <changed-go-files>
   go test ./storage/table ./replication ./replication/store ./replication/snapshot ./armadaserver ./cmd/armada -count=1
   git diff --check
   make lint
   ```

4. After focused tests pass, run where practical:

   ```sh
   go test ./... -count=1
   ```

Do not weaken recovery safety checks to accommodate unrelated existing test failures.

## Suggested delivery slices

Keep changes reviewable by delivering in this order:

1. recovery serialization, progress scoping, and quorum safety;
2. shard startup/handoff policy;
3. consistent incremental snapshot metadata;
4. shared snapshot selection policy;
5. durable download and lease lifecycle;
6. export publication and presigning cleanup.

Avoid a new all-purpose recovery framework. The simplification should come from reducing duplicated decision points while preserving existing architecture:

- cross-cluster replication continues to transfer logical commands;
- workers own remote transport;
- `storage/table` owns Raft replicas and recovery lifecycle;
- object storage transports artifacts and is not a Raft transport.

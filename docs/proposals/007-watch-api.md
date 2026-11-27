---
title: "RFC 007: Watch API"
description: "Add an etcd-style Watch API that streams key change events from the MVCC store, on leader and follower clusters."
section: "proposals"
order: 8
---

Proposal status: Proposed

## Summary

Add a `Watch` gRPC service that streams ordered PUT and DELETE events for a key
or key range of one table, starting either at the current revision or at a
caller-supplied historical revision.

Serve events from two sources that share one source of truth:

* a **live path**, where the table state machine publishes the events of each
  committed apply batch to an in-memory hub, and
* a **catch-up path**, where a watcher that starts in the past or falls behind
  reads its events back from the MVCC versions already stored in Pebble.

Every live event can be re-read from the store, so the apply path never blocks
on a watcher: a slow watcher is demoted to catch-up instead of applying
backpressure to Raft.

Watches are served locally by any replica of any cluster. Revisions are the
source leader sequence, so a client can resume a watch on a different node or a
different cluster with the revision it last saw.

**The initial implementation does not include leases, multi-table watches, a
separate watch retention setting, or point-in-time Range reads.**

## Motivation

Clients that need to react to changes currently poll with Range, which is
either slow to notice changes or expensive. Controllers, caches, and
configuration consumers need a change feed with a resumable position and a
clear signal when history is no longer available.

Armada previously listed Watch among the etcd features it leaves out. Three
things have changed:

* The V2 key schema retains every MVCC version of a key, with one tombstone
  per deleted key, so the store is already an event log.
* Every write is stamped with `updateContext.seqno()`, which is the source
  leader sequence on every cluster, so a revision means the same thing
  everywhere.
* SST blocks carry an MVCC sequence interval, so a scan for a narrow revision
  window can skip most of the store.

[RFC 004](004-split-cli-binaries.md) already reserves watches as an `arq`
responsibility.

## Design

### Semantics and scope

A watch covers one table and one key or half-open key range, with the same
`key`/`range_end` conventions as Range.

* Events are delivered in non-decreasing revision order. All events of one
  revision are delivered before any event of a later revision.
* There is **at most one event per key per revision**. A transaction that
  modifies a key several times produces one event carrying the final state at
  that revision, matching the single physical version the store keeps per
  `(logical key, modification sequence)`. Events within one revision are
  ordered by key.
* Revisions are sparse. Dummy entries, GC commands, membership changes, and
  writes that change nothing produce no events.
* A PUT event carries the key, value, `create_revision`, and `mod_revision`.
  A DELETE event carries the key, the deleted incarnation's `create_revision`,
  and `mod_revision` set to the deletion revision.
* An event is sent only after its apply batch is committed locally. A Range on
  the same node after receiving an event at revision `R` observes at least `R`.
* `start_revision = 0` starts after the node's current applied revision. A
  `start_revision` ahead of the node waits; this is the normal case when a
  client resumes on a node that is behind.
* A `start_revision` at or below the node's history floor is rejected with the
  first available revision. The client must re-list and start a new watch.

* `prev_kv` is exact for live events. For events replayed from history it is
  **best-effort**: it is omitted when the previous version has already been
  garbage collected. See [Previous key-values](#previous-key-values).
* Each watcher names exactly one table, as Range does. Revisions are per
  table and give no ordering across tables, so multi-table watches are out of
  scope. A client that needs several tables creates one watcher per table on
  the same stream.
* Watch has the same access rules as Range: whoever may read a table may watch
  it. There is no watch-specific permission.

Watches are best-effort with respect to node lifetime: a stream ends when the
node stops, and the client resumes elsewhere from its last seen revision.

### API

Add a `Watch` service to `proto/armada.proto` and an `Event` message to
`proto/mvcc.proto`. The shape follows etcd so that existing client watcher
logic ports directly; `table` is the Armada-specific addition.

```protobuf
service Watch {
  // Watch watches for events happening or that have happened. Several
  // watchers may share one stream.
  rpc Watch(stream WatchRequest) returns (stream WatchResponse);
}

message WatchRequest {
  oneof request_union {
    WatchCreateRequest create_request = 1;
    WatchCancelRequest cancel_request = 2;
    WatchProgressRequest progress_request = 3;
  }
}

message WatchCreateRequest {
  enum FilterType {
    NOPUT = 0;
    NODELETE = 1;
  }
  bytes table = 1;
  bytes key = 2;
  bytes range_end = 3;
  // start_revision is an inclusive revision to watch from. Zero means "now".
  int64 start_revision = 4;
  // progress_notify asks for periodic empty responses carrying the current
  // revision when there are no recent events.
  bool progress_notify = 5;
  repeated FilterType filters = 6;
  bool prev_kv = 7;
  // watch_id is chosen by the server when zero.
  int64 watch_id = 8;
  // fragment allows the server to split a large revision across responses.
  bool fragment = 9;
}

message WatchCancelRequest { int64 watch_id = 1; }
message WatchProgressRequest {}

message WatchResponse {
  ResponseHeader header = 1;
  int64 watch_id = 2;
  bool created = 3;
  bool canceled = 4;
  // compact_revision is set when the watch is canceled because the requested
  // revision is no longer available. It is the lowest revision that is.
  int64 compact_revision = 5;
  string cancel_reason = 6;
  bool fragment = 7;
  repeated mvcc.Event events = 8;
}

message Event {
  enum EventType {
    PUT = 0;
    DELETE = 1;
  }
  EventType type = 1;
  KeyValue kv = 2;
  KeyValue prev_kv = 3;
}
```

`header.revision` is the node's applied revision for the table when the
response was built. Progress responses have no events; a client may use their
revision as its resume position.

Apply the key validation used by `KVServer` (table required, no null bytes
outside the range sentinels). Reject negative `start_revision` as
`InvalidArgument`, an unknown table as `NotFound`.

### Event capture in the state machine

Add an event recorder to `updateContext`. The Put, Delete, PutBatch,
DeleteBatch, and transaction handlers append to it at the point where they
write a physical version. They already read the current record to preserve
creation revision, so the previous key-value is available without extra reads.

The recorder is keyed by `(sequence, user key)` and keeps the last write, so
the recorded events equal what the store holds after the batch. Two details
follow from existing apply behaviour:

* A replicated command sequence applies several source sequences in one Raft
  entry. Record each child under its own source sequence.
* Cross-cluster recovery replays snapshot commands that carry explicit source
  metadata and write at historical sequences. They are **not** events. They
  are applied to a recovery shard that no watcher is attached to; see
  [History floor](#history-floor).

`FSM.Update` checks an atomic watcher count for the table once per call. With
no watchers the recorder is nil and capture costs nothing. After
`ctx.Commit()` succeeds, publish one message
`(first revision, last revision, events)` to the hub and then report the
applied index as today.

Publishing is a non-blocking handoff. If the hub cannot accept the message it
records the revision range as a gap, and watchers that needed it catch up from
the store. The apply path performs no I/O and takes no lock that a watcher can
hold.

Replace the `af func(applied uint64)` argument of `fsm.New` with a small
publisher interface that carries both the applied index and the event batch,
and keep serving `table.Config.AppliedIndexListener` from it.

### Catch-up from the MVCC store

Add `NewMVCCSeqnoRangeFilter(lower, upper)` beside the existing GC filter in
`pebble/block_property.go`, and a watch-scan lookup handled by `FSM.Lookup`:

1. Take a Pebble snapshot and read the applied index and history floor from
   it. If the requested start is at or below the floor, return a compacted
   error carrying the first available revision. Reading both from the same
   view is what makes the check sound against a concurrent GC sweep, as in
   `commandIncrementalSnapshot`.
2. Iterate the watcher's key bounds with the sequence filter for a bounded
   revision window `[from, from+N)`, keeping versions whose modification
   sequence is inside the window. The filter is block-granular, so each key
   must still be checked.
3. Sort the window by `(revision, key)`, emit it, and continue with the next
   window until the snapshot's applied index is reached.

Size the window adaptively to keep the per-window buffer bounded.

### Previous key-values

On the live path the handlers already hold the record a write replaces, so
`prev_kv` is exact.

During catch-up, the previous version of a key is the next physical version in
the same prefix, if it still exists. GC removes a version below the horizon as
soon as a newer version of the key exists, so an event above the history floor
can have a predecessor below it that is already gone. The event is still
delivered, with `prev_kv` unset.

This is a documented limitation, not something to fix now. Making it exact
means GC must retain the version visible at the horizon for every recently
modified key, which changes version-retention rules at the core of the storage
engine. It can be added later without an API change: `prev_kv` simply becomes
populated in more cases.

### Watch hub

Add a `storage/watch` package. `table.Manager` owns one hub per table name so
that the hub outlives state machine restarts.

* Single-key watchers are indexed by key; range watchers by an interval tree.
* Each watcher has a bounded send buffer and a `next revision`. A watcher is
  either **synced** (fed by the live path) or **unsynced** (running catch-up).
* The hub keeps a short ring of recently published batches. An unsynced
  watcher is promoted once its `next revision` is inside the ring; the hub
  replays the remainder from the ring and then feeds it live. If it is not
  inside the ring, catch-up continues.
* A synced watcher whose buffer is full, or that is affected by a gap, becomes
  unsynced at its `next revision`. Nothing is dropped from the client's point
  of view.
* Progress notifications use the applied revision reported by the state
  machine. They are sent to a watcher only when it is synced and has nothing
  buffered.

The hub cancels watchers, with a reason, when:

* the table's serving shard changes: the table is deleted, recreated, or
  swapped to a recovery shard by cross-cluster recovery, or
* the node shuts down.

A replica that installs an intra-cluster Raft snapshot does **not** cancel its
watchers. The revisions it jumped over were never published to the hub, so the
hub records them as a gap and the watchers catch up from the recovered store.

### History floor

Events are available from the revision after the history floor:

```text
floor = max(gc horizon, history floor key)
```

The GC horizon is the existing `sysGCHorizon`, advanced by `Command_GC` when
the Raft log is compacted. The history window therefore follows Raft log
retention (`raft.snapshot-entries` and `raft.compaction-overhead`). No
separate watch retention setting is added.

The second term is new. Armada has two unrelated kinds of snapshot, and they
differ in exactly the property Watch depends on:

| | Intra-cluster (Raft) | Cross-cluster (replication) |
|---|---|---|
| Purpose | Bring a replica of the same shard up to date | Rebuild a follower cluster's table from the leader cluster |
| Format | Pebble checkpoint: the raw SSTs | Logical command stream, full or incremental |
| Content | Complete dataset: every MVCC version, tombstone, and system key | Latest version of each key only |
| History | Preserved, including both floor keys | Collapsed up to the artifact tip |
| Effect on watchers | Gap; catch up from the store | Shard swap; cancel |

**Intra-cluster snapshots need no special handling.** A checkpoint carries the
full Pebble state, so the receiving replica has the same history and the same
floor as the sender.

**Cross-cluster recovery raises the floor.** The artifact holds one version
per key, so the rebuilt table has no intermediate versions below the artifact
tip, while its GC horizon may be lower. Without a second floor a historical
watch there would silently skip events.

Persist a `sysHistoryFloor` system key. Recovery finishes loading an artifact
by proposing a command to the recovery shard that sets the key to the artifact
tip. The command goes through Raft, so every replica of the shard, including
learners seeded later by checkpoint, agrees on the floor. The key only moves
forward.

It cannot be folded into `sysGCHorizon`: the horizon is overwritten by each
`Command_GC` replicated from the leader cluster, and the first one after
recovery would carry a lower index than the artifact tip.

When recovery swaps the table to the recovery shard, the hub cancels that
table's watchers with `compact_revision = floor + 1`. Clients re-list and
start again, which is the correct response: the node's view of the table was
replaced.

### Server

Add `armadaserver/watch.go` with a `WatchServer`. Each stream runs one receive
loop and one send loop; watchers created on the stream share the send loop.

* Register the service on both the leader and follower API servers, behind the
  same interceptors as `KV`, so authentication and authorization are whatever
  applies to Range. Watches are never forwarded to the leader cluster.
* Split responses at the existing response size limit. When `fragment` is not
  set, a revision is never split across responses; a single revision that
  exceeds the limit cancels the watch with a reason asking for `fragment`.
* Map errors as `KVServer` does: `NotFound` for an unknown table,
  `Unavailable` for retryable storage errors.
* Add flags for the per-watcher buffer size, the progress interval, and the
  maximum number of watchers per stream.
* Add metrics: watcher count by state, demotions to catch-up, gaps, catch-up
  duration, events sent, and cancellations by reason.

### Clients and tooling

* `client/go` and the Java client gain a `Watch` call that resumes
  automatically from the last seen revision on stream failure and surfaces a
  compacted cancellation to the caller as a distinct error.
* Add watch cases to the client conformance specification.
* Add `arq watch`, following `etcdctl watch`.
* Update the API documentation and remove Watch from the list of unsupported
  features in the README.

### Implementation sequence

1. **API:** protobuf definitions, generated code, and an `Unimplemented`
   registration.
1. **Capture and hub:** event recorder, publisher interface, hub with live
   delivery only (`start_revision = 0`), cancellation on shard change.
1. **Catch-up:** sequence range filter, watch-scan lookup, synced/unsynced
   transitions, gap handling, compacted errors.
1. **History floor:** `sysHistoryFloor` and the recovery command that sets it.
1. **Server completeness:** progress, filters, `prev_kv`, fragmentation,
   limits, metrics.
1. **Clients and CLI.**

Do not expose historical `start_revision` before step 5 is complete.

### Validation plan

* **Equivalence:** for randomized workloads of puts, deletes, range deletes,
  batches, and transactions, the live stream must equal a catch-up scan of the
  same revision range. Include same-revision overwrites and
  create-then-delete inside one transaction.
* **Hub:** slow consumer demotion and re-promotion, gap handling, cancel on
  shard change, zero-watcher fast path.
* **`prev_kv`:** exact on the live path; unset, with the event still
  delivered, when catch-up finds the predecessor collected.
* **Scan:** window boundaries, sparse revisions, start at floor and floor+1,
  concurrent GC sweep.
* **Follower clusters:** revisions equal the leader's; replicated command
  sequences produce one batch per source sequence.
* **Integration:** watch across a Raft leader change, resume on a follower
  cluster with the same revision, compacted start revision. A replica that
  installs a Raft checkpoint mid-watch must deliver every event without
  cancelling. A follower cluster that recovers a table mid-watch must cancel
  with the artifact tip as the floor, and reject older start revisions
  afterwards on every replica.
* **Performance:** apply throughput with zero watchers must not regress;
  measure it with one wide watcher and with many single-key watchers.

## Alternatives

### Tail the Raft log

`Log.Replicate` already streams the log. Log entries hold commands, not
results: they do not say which keys a range delete removed or which
transaction branch ran. Follower log indices also differ from revisions.
Deriving events would mean re-executing commands against historical state.

### Store scans only

Serving live watchers by rescanning the store on every applied index is
correct and needs no capture in the state machine. Recent writes live in the
memtable, where block properties do not help, so each notification would cost
a scan of the watcher's range per watcher. This is the catch-up path; it is
too expensive as the only path.

### Live path only

Publishing from the state machine without catch-up cannot serve historical
start revisions and forces a choice between blocking apply and dropping
events for slow consumers.

### One server stream per watch

A unary-request, server-streaming RPC per watch is simpler on both sides.
Each watch then costs a stream, cancel means tearing the stream down, and
there is no in-band progress request. The wire shape is hard to change later,
so the bidirectional form is preferred.

### Derive live events from the Pebble batch

Decoding the apply batch after the handlers run guarantees equality with the
store by construction and leaves the handlers untouched. The batch does not
contain previous values, so `prev_kv` would need extra reads, and same-sequence
replacements appear as delete-then-set pairs that must be reconciled. Handler
capture with an equivalence test is preferred.

## Unresolved Questions

No blocking design decision remains. The following were considered and
settled:

* `prev_kv` is best-effort for replayed events and documented as such. Exact
  behaviour is a possible later addition.
* Authorization is the same as Range.
* Multi-table watches are out of scope, as multi-table Range is.
* Intra-cluster checkpoints preserve the complete dataset; only cross-cluster
  recovery raises the history floor.

Sizing remains to be measured, and does not change the design: the default
per-watcher buffer, the size of the hub's replay ring, and the catch-up window
all need numbers from the performance tests in the validation plan.

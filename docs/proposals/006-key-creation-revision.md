---
title: "RFC 006: Key Creation Revision"
description: "Implement creation-revision metadata and Range filters using read-optimized inline V2 key metadata."
section: "proposals"
order: 7
---

Proposal status: Proposed

## Summary

Track the creation revision of every logical key incarnation and implement
`min_create_revision` and `max_create_revision` filtering for Range, IterateRange,
and transaction Range operations.

Store creation revision directly in each physical V2 MVCC key. It is logically
one immutable property per key incarnation, but deliberately duplicated across
versions to avoid metadata lookups and value reads on the Range path.

Extend V2 directly: the existing V2 layout has not been released or deployed in
the wild. No V3 format, previous-V2 migration, or mixed-layout V2 read path is
required.

**The initial implementation will not include a creation-revision secondary
index, covering index, or range-key metadata representation.** These remain
separate, workload-driven optimizations rather than prerequisites for the feature.

## Motivation

Clients need to select currently live keys by when they were created, without
loading values for rejected keys. The public protobuf API already declares
creation-revision bounds and `KeyValue.create_revision`, but the server currently
rejects the filters as unimplemented.

Range read performance is the priority. Some additional storage and write work
are acceptable, particularly if they avoid per-candidate metadata lookups,
payload reads, or historical-version traversal.

The existing modification sequence cannot substitute for creation revision:
updates advance modification revision without creating a new key incarnation.
Creation also cannot be recovered reliably by finding the oldest retained
version, because GC may already have removed the original write.

## Design

### Semantics and scope

Creation revision is the sequence of the most recent transition from absent to
live, not the first-ever write to the key name and not a wall-clock timestamp.

| Operation | Creation revision | Modification revision |
|---|---|---|
| First Put at sequence 100 | 100 | 100 |
| Update at sequence 150 | 100 | 150 |
| Delete at sequence 200 | Retained in the tombstone; key is not returned | 200 |
| Recreate at sequence 250 | 250 | 250 |

Use `updateContext.seqno()` for both revisions. On followers this is the source
leader sequence, not the follower's local Raft apply index. Preserve each child
command's source sequence when applying replicated command sequences.

Creation bounds are inclusive; zero means unbounded. Reject negative bounds and
requests with a nonzero minimum greater than a nonzero maximum as
`InvalidArgument`. Filter the current visible incarnation only. A rejected current
version must never cause an older incarnation to be returned.

Return `create_revision` on ordinary and key-only KV responses, including
unfiltered reads and previous-KV responses. Populate the existing `mod_revision`
from the stored modification sequence as well. Count-only reads return no KVs.
No opt-in metadata flag is needed because metadata is available in the point key.

Keep existing public revision field types and protobuf tags. Public revision
metadata must fit the positive `int64` domain; guard conversions from internal
`uint64` sequences and reject invalid snapshot metadata rather than silently
wrapping into negative revisions.

Historical reads, modification-revision filters, and new transaction comparison
targets are outside this initial scope. Creation filtering is not a change feed:
it does not report updates to older keys or deleted keys.

### V2 physical layout

Replace the current V2 user-key layout with:

```text
[header: 4B][key type: 1B][user key][separator: 1B]
[inverted modification sequence: 8B][creation sequence: 8B][record kind: 1B]
```

The separator remains `0x00`; preserve the existing prohibition on null bytes
inside user keys. Modification sequence remains inverted big-endian so the newest
version sorts first. Creation sequence is ordinary big-endian. Record kind uses
`0` for a live value and `1` for a tombstone.

The logical-key prefix ends immediately after the separator. The complete
17-byte suffix is excluded from Pebble prefix grouping. Creation and kind must
not split one logical key into multiple prefix groups.

This adds nine raw bytes per physical version before compression and storage
framing. Keep user values opaque and unchanged; record kind, not value contents,
becomes authoritative for deletion. Empty values and a single zero-byte value
must remain valid live values. System keys use zero creation metadata and live
kind; they are excluded from user-key revision filtering.

Update the key model, encoders, decoders, length constants, and modification
sequence extraction helpers in `storage/table/key/`. Validate suffix length and
record kind. Helpers must no longer assume the final eight bytes are the
modification sequence.

### Prefix bounds and Pebble integration

Represent scan bounds as unsuffixed prefixes. For one user key, all versions lie
between its prefix ending in `0x00` and the exclusive bound obtained by replacing
that separator with `0x01`.

Update `Comparer.Split` to recognize both versioned keys and bare bounds;
inferring a suffix from total key length is insufficient. Supply a compatible
`ImmediateSuccessor` and keep bytewise comparison with modification sequence
first in the suffix.

Use `NextPrefix()` to skip older versions during ordinary point iteration.
Unsuffixed upper bounds are required: Pebble does not permit `NextPrefix()` with
a versioned MVCC upper bound. Reuse scratch storage where explicit seeks are
still needed.

Keep the columnar SST key schema consistent with the comparer. `DefaultOptions()`
already initializes schemas; replacing its splitter afterward requires rebuilding
`KeySchema` and `KeySchemas`. Prefer configuring the final comparer before schema
initialization in production.

Update the existing MVCC block-property collector to decode modification sequence
from its new position. Do not add creation-based block pruning in this phase:
skipping a newer version or tombstone could expose an older matching incarnation.

### Write application

All Put paths must inspect the current logical key, including ordinary Put,
PutBatch, and transaction Put:

1. Read through the indexed apply batch so earlier writes in the same batch,
   transaction, or replicated sequence are visible.
2. Preserve creation sequence when updating a live key.
3. Use the current command sequence when creating or recreating a key.
4. Write the value and its inline metadata atomically in the existing batch.

Plain Put currently avoids the previous-record read unless `prev_kv` is requested.
Creation tracking intentionally sacrifices that write optimization. Reuse the
record lookup when previous-KV output is also requested. An apply-local cache is
not required initially; add one only if measured write costs justify it.

Deletes write a tombstone with the deleted incarnation's creation sequence and
the deletion command's modification sequence. Deleting an absent or already
deleted key remains a no-op.

Maintain **one physical version per `(logical key, modification sequence)`**.
Multiple operations may affect a key at the same transaction sequence. If
creation or kind changes, explicitly delete the previous same-sequence physical
entry before setting its replacement; bytewise ordering would otherwise retain
multiple entries at that sequence.

Normal clients cannot choose creation metadata through Put. Explicit source
metadata is accepted only by the internal snapshot/restore command path.

### Read path and API plumbing

Reuse the existing creation-bound fields on `RangeRequest`. Add corresponding
fields to `RequestOp.Range` without reusing protobuf tags, regenerate
`armadapb/`, and thread the bounds through `ActiveTable.Range` and
`ActiveTable.Iterator`.

Remove only the creation-filter `Unimplemented` checks in `KVServer.Range` and
`KVServer.IterateRange`. Apply the same creation-bound validation to transaction
Range requests. Leave unsupported modification filters and comparison targets
unchanged.

Implement one consistent predicate in single-key lookup and range iteration:

1. Select the latest visible version.
2. Inspect record kind and creation bounds from the physical key.
3. Reject the entire logical key if deleted or outside the bounds.
4. Fetch/copy its value only when the request needs a value.

Keep key ordering, consistency modes, and follower write-propagation waits
unchanged. Do not add a metadata point lookup or enable range-key iteration for
ordinary reads.

Apply filtering before consuming `limit` or incrementing response counts.
`more` must indicate another matching live logical key, not another physical
version or a rejected key. Preserve existing streaming/count semantics while
counting only matches.

Include creation and modification fields in protobuf response-size accounting
so the existing response chunk limit remains correct. Populate metadata
consistently in single-key, ranged, key-only, transaction, and previous-KV results.

### Replication, snapshots, and GC

Live cross-cluster replication remains logical command replay. Derive creation
from the follower's current incarnation and the source command sequence; do not
copy local Dragonboat indices or introduce another replication mechanism.

Full and incremental command snapshots must explicitly carry both the source
creation sequence and modification sequence. Otherwise a collapsed latest-value
Put would incorrectly treat its modification sequence as creation.

Snapshot restoration must treat supplied creation metadata as authoritative,
including when applying an incremental Put over an existing live follower key.
That Put may represent a leader-side delete/recreate whose intermediate deletion
is absent from the collapsed delta. Preserve source modification sequence,
replay idempotence, and recovery progress watermarks.

Physical SST snapshots and checkpoints retain the raw extended keys. Update
their writers/readers and compatibility identifiers together with the format;
logical snapshot writers must also stop inferring tombstones from payload bytes.

Update GC version boundaries and sequence decoders. When constructing the start
of a strictly older-version range, canonicalize trailing creation/kind bytes to
the minimum ordering sentinel. Advancing modification sequence while copying the
current record's remaining metadata can miss an older incarnation at that
boundary.

Keep creation metadata on retained live versions even if the original creation
write is collected. Preserve existing GC horizons and snapshot fallback rules.

### Release and implementation sequence

Previous unreleased V2 data directories and artifacts are not supported by the
new layout; recreate development data rather than adding a migration path. Use
an incompatible comparer/storage identifier so accidental reuse fails clearly.
Do not expand existing V1 handling as part of this feature.

1. **Storage format:** extend V2, validate decoding, fix prefix bounds/splitter,
   initialize the matching columnar schema, and update sequence/GC helpers.
2. **Apply and recovery correctness:** implement incarnation tracking,
   same-sequence replacement, internal restore metadata, and snapshot propagation.
3. **API and reads:** thread creation bounds through all Range entrypoints,
   populate revision metadata, and implement metadata-only filtering with correct
   limits, counts, and chunk sizing.
4. **Release verification:** exercise replication/recovery and rerun benchmarks
   using the production implementation. Update
   [V2 key schema documentation](../v2_key_schema.md) and relevant API/query docs.

Do not activate an intermediate format whose decoders, writers, snapshots, and
GC disagree.

### Benchmark evidence

`pebble/creation_bench_test.go` compares inline metadata, combined point/range
iteration, and range-only metadata driving point seeks. It also includes a
metadata-free point-only control for ordinary reads.

For 10,000 keys, eight versions, 64-byte values, and a warm Pebble cache, median
times across three samples for a count query selecting 1% of keys were:

| Representation/access path | Median |
|---|---:|
| Inline creation metadata | 1.44 ms |
| Combined point/range iteration | 11.89 ms |
| Range-only metadata followed by point seeks | 3.54 ms |

Across the tested 1% count-query configurations, range-only filtering was
1.2-4.6 times slower than inline filtering. Point-only reads with separately
stored range metadata approximately matched the metadata-free control, but did
not return creation metadata.

The benchmark covers one/eight versions, 64/1,024-byte values, shuffled creation
order plus a clustered-order configuration, full/narrow key ranges, misses,
limited results, metadata/count/value output, and warm/disabled Pebble caches.
There are 290 cases with three samples each. Both candidate layouts include a
point-key deletion flag, isolating the creation-metadata comparison.

These are storage-level, flushed/compacted-SST measurements with result copying,
not protobuf/RPC or write benchmarks. Disabled Pebble cache does not mean cold
OS/device cache. Iterator block-byte counters exclude range-key blocks and blob
reads. The covering index was not benchmarked.

```bash
go test ./pebble -run '^$' -bench '^BenchmarkCreationRange$' -benchtime=150ms -count=3 -timeout=10m
```

### Validation plan

Add focused tests to existing key/FSM/server test files for:

* Encoding, prefix-key ordering, bare bounds, and latest-version selection.
* Creation, update, deletion, recreation, and repeated same-sequence operations.
* Indexed-batch visibility across PutBatch, transactions, and source sequences.
* Inclusive bounds, invalid bounds, single-key/ranged reads, and key/count-only
  operation without payload fetches.
* No resurrection of older incarnations; matching-key limit and `more` behavior.
* Revision metadata and response chunk sizes, including previous-KV responses.
* Empty and zero-byte values, source/local index divergence, and source metadata
  overriding stale follower incarnations during incremental restoration.
* Full/incremental logical snapshots, physical snapshots/checkpoints, and GC
  boundaries across incarnations.

Run targeted package tests before broader release checks:

```bash
make proto
go test ./storage/table/key ./storage/table/fsm ./storage/table ./pebble ./armadaserver ./replication/...
make fmt
make lint
make test
make test-integration-one SCENARIO=baseline
make test-integration-one SCENARIO=full
make test-integration-one SCENARIO=incremental
```

Extend recovery assertions to verify revisions as well as key/value convergence.
The [integration harness](../../integration/README.md) is a nested Go module;
root `go test ./...` does not run it. Re-run the creation benchmark after the
production path is implemented, checking ordinary Range as well as filtered
queries for latency, allocations, and unnecessary payload reads.

## Alternatives

### Creation metadata in values

A value envelope is simpler than a key-format extension, but filtering may
materialize separated values for rejected keys. It also undermines value-free
key/count-only queries. Inline key metadata better matches the read priority.

### One metadata point record per logical key

This avoids duplication across versions, but a naive implementation adds a
lookup per candidate key. A separate synchronized scan adds another access path
and consistency work. Neither is needed for the initial implementation.

### Pebble range keys

Range keys can store creation once per logical key and allow ordinary reads to
remain point-only if creation metadata is omitted from their responses. Combined
iteration or range-only metadata driving point seeks can avoid rejected payloads.

The measured per-key-span implementations are slower than optimized inline scans,
especially after using bare bounds and `NextPrefix()`. Storing creation solely in
range keys also makes always-returned creation metadata require range-key reads.
Do not use this representation initially.

### Creation-revision secondary or covering index

A creation-ordered index could avoid scanning most primary keys for a narrow
creation window over a large key range. A covering index would additionally
duplicate current values to avoid primary lookups.

The costs are extra writes, live-value duplication, compaction/cache pressure,
and restoring user-key ordering before returning results. Small limits do not
generally permit early termination of a creation-ordered scan.

**Omit the index from the initial implementation.** The benchmark does not prove
that an index is universally unnecessary; it was not included. Reconsider it only
if realistic broad, selective creation queries miss read-latency requirements
after inline metadata is implemented.

## Unresolved Questions

No blocking representation or initial-scope decision remains: use inline V2
metadata, return creation metadata on ordinary KV responses, and omit secondary
indexes and range-key creation metadata.

Larger-scale, cold-storage, concurrent read/write, and end-to-end Range
measurements remain necessary before generalizing the prototype results to all
workloads. They may motivate later optimizations, but do not expand this initial
implementation.

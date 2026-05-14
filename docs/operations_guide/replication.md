# Cross-Cluster Replication

Armada replicates data from a **leader cluster** to any number of **follower clusters** using
asynchronous, pull-based replication. This page describes how replication works in depth and how to
tune it for your deployment.

See [Architecture](../architecture.md) for a high-level overview of the hub-and-spoke topology.

---

## How Replication Works

### Pull-Based Model

Replication between clusters is **pull-based** — follower clusters periodically poll the leader
rather than the leader pushing to followers. This design choice has important implications:

* Adding a new follower cluster requires **no changes to the leader**.
* Temporary network outages between clusters **do not affect write availability** on the leader.
* Followers replicate at their own pace and can lag behind the leader.
* The leader does not need to track follower progress — each follower tracks its own position.

### Replication Lifecycle

The following describes the full lifecycle of a single replication cycle for one table:

```
Follower replication worker
         │
         │  1. Acquire table lease (only one node in the follower cluster runs this worker)
         │
         │  2. Read stored leader index from local state machine
         │
         │  3. Call leader Log.Replicate RPC with stored leader index
         │        │
         │        │  Leader LogServer
         │        │    a. Look up the Raft log entry at the requested index
         │        │    b. If the entry is still in the log:
         │        │         Convert raftpb.Entry → regattapb.Command
         │        │         Stream Command batches to follower (up to max-send-message-size-bytes)
         │        │    c. If the entry is below the GC horizon / was compacted:
         │        │         Return a "use snapshot" signal
         │        │
         │  4a. Log replay path (normal operation):
         │        Receive Command batch from leader
         │        Wrap individual Commands in a Command_SEQUENCE
         │        Re-propose the sequence into the follower's local Raft group for this table
         │        The follower FSM applies the entry and persists the leader index
         │
         │  4b. Snapshot recovery path (catch-up / new follower):
         │        Call leader Snapshot.Get RPC
         │        Stream full or incremental snapshot into a temporary recovery shard
         │        Apply a final dummy command carrying the latest leader index
         │        Atomically swap the recovery shard for the live table
         │
         └─ 5. Repeat from step 2 after poll-interval
```

### Logical Command Replication

Cross-cluster replication operates at the level of **logical commands**, not raw Raft log bytes.
The leader's `Log.Replicate` RPC converts its internal Raft entries into `regattapb.Command`
values and streams them to the follower. The follower re-proposes these commands into its own
independent Raft group for each table. This means:

* Each cluster maintains its **own independent Raft state** and local Raft indices.
* The follower stores the **source leader index** alongside its local Raft index so that the
  same logical write always carries the same MVCC revision across all regions.
* The follower can be behind the leader without affecting consistency guarantees *within* the follower.
* Clusters in different regions can use different Raft configurations (e.g. different replica counts)
  without affecting replication.

### Snapshot Fallback

If the requested log position is no longer available on the leader (because the Raft log was
compacted or the GC horizon has advanced), the leader signals the follower to perform a
**snapshot recovery** instead of a log replay. The follower then:

1. Receives a full or incremental snapshot from the `Snapshot` service.
2. Replays the streamed commands into a temporary recovery shard.
3. Atomically swaps the recovered shard for the live table, minimizing downtime.

Snapshot recovery is also used when a brand-new follower cluster is bootstrapped for the first
time (before it has any local state to resume from).

---

## Follower Write Forwarding

A follower cluster does not accept writes directly into its local Raft groups. Instead, the
follower **forwards write requests to the leader** and then waits for the resulting revision to
be applied locally before returning a response to the caller. This ensures that a client
connected to a follower sees its own writes immediately:

```
Client → Follower gRPC API
              │
              │  Forward Put/DeleteRange/Txn to leader
              │◄──────────────────────────────────────
              │  Leader returns the committed revision (leader index)
              │
              │  Wait on IndexNotificationQueue until the follower's
              │  local replication has applied that revision
              │
              └─ Return success to client
```

This means follower write latency includes the round-trip to the leader **plus** the time for
the follower to replicate that revision. Callers should account for this additional latency
when writing through a follower.

---

## Data Consistency Guarantees

| Property | Guarantee |
|----------|-----------|
| Within a single table on the leader | **Linearizable** — writes are serialized through the leader's Raft group |
| Within a single table on a follower | **Sequential consistency** — all writes are applied in the same order as on the leader |
| Across tables (any cluster) | **No guarantee** — tables are independent Raft groups |
| After a client write through a follower | **Read-your-writes** — the forwarding path waits for the revision to be locally applied |
| MVCC revisions across clusters | **Consistent** — the same revision number always refers to the same logical write on every cluster that has caught up to it |

---

## Per-Table Lease-Based Workers

Each follower runs one **replication worker** per table. Workers use a lease mechanism to ensure
that only one node in the follower cluster is actively replicating each table at any time.
The lease interval is controlled by `--replication.lease-interval`.

The worker set is reconciled against the current table list periodically. Tables that are
created on the leader are automatically picked up by the next reconciliation cycle on the
follower. The reconcile interval is controlled by `--replication.reconcile-interval`.

---

## Configuration Reference

### Follower Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--replication.leader-address` | `localhost:8444` | Address of the leader's Replication API |
| `--replication.poll-interval` | `1s` | How often the follower polls the leader for new log entries |
| `--replication.log-rpc-timeout` | `1m` | Timeout for each log replication RPC call |
| `--replication.snapshot-rpc-timeout` | `1h` | Timeout for a full snapshot recovery RPC |
| `--replication.max-recovery-in-flight` | `1` | Maximum number of concurrent snapshot recovery goroutines |
| `--replication.max-recv-message-size-bytes` | `8388608` (8 MiB) | Maximum size of a single replication message the follower will accept |
| `--replication.max-snapshot-recv-bytes-per-second` | `0` (unlimited) | Rate limit for snapshot reception in bytes per second |
| `--replication.lease-interval` | `15s` | How often workers renew their table leases |
| `--replication.reconcile-interval` | `30s` | How often the follower reconciles its worker set against the current table list |
| `--replication.keepalive-time` | `1m` | How often to send keepalive pings on the replication connection |
| `--replication.keepalive-timeout` | `10s` | How long to wait for a keepalive response before closing the connection |

### Leader Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--replication.address` | `http://0.0.0.0:8444` | Address for the leader's Replication API listener |
| `--replication.enabled` | `true` | Whether the Replication API is enabled |
| `--replication.max-send-message-size-bytes` | `4194304` (4 MiB) | Target maximum size of a single replication message sent by the leader |

---

## Tuning Tips

### Reducing Replication Lag

* Decrease `--replication.poll-interval` (e.g. `200ms`) to poll the leader more aggressively.
  Be mindful of the additional load this places on the leader.
* Increase `--replication.max-recv-message-size-bytes` on the follower and
  `--replication.max-send-message-size-bytes` on the leader to allow larger batches to be
  transferred in a single RPC round-trip.

### Improving Snapshot Throughput

* Leave `--replication.max-snapshot-recv-bytes-per-second` at `0` for maximum speed, or set
  a byte-per-second value to cap bandwidth consumption during recovery.
* Increase `--replication.max-recovery-in-flight` only if you have many tables to recover
  simultaneously and sufficient I/O capacity. The default of `1` is safe for most deployments.

### Unreliable or High-Latency Networks

* Increase `--replication.keepalive-time` and `--replication.keepalive-timeout` to tolerate
  transient network outages without dropping the replication connection prematurely.
* Increase `--replication.log-rpc-timeout` if the network round-trip time between the leader
  and follower is high.
* If the follower frequently falls behind the leader's GC horizon and triggers snapshot
  recovery, consider reducing the leader's compaction / GC frequency.

---

## Troubleshooting

### Follower is permanently stuck in snapshot recovery

The most common cause is that the follower's stored leader index has fallen so far behind the
leader that the Raft log entries are no longer available. Check:

1. Whether the follower has been offline for longer than the leader's Raft log retention window.
2. Whether `--replication.snapshot-rpc-timeout` is long enough for the snapshot transfer to
   complete (large datasets may require several hours).
3. Network bandwidth — low bandwidth combined with large snapshots can cause the RPC to time out
   before the snapshot is fully received.

### Replication lag is growing continuously

1. Check whether the replication worker has acquired a lease for the affected table. A worker
   that cannot acquire a lease will not replicate. Inspect the `armada_replication_leased` metric.
2. Check the leader's `armada_replication_index` metric versus the follower's applied leader
   index to quantify the lag.
3. Verify that the network path between the follower and leader's Replication API port (default
   `8444`) is open and not rate-limited.

### Follower writes are slow

Write forwarding latency = network RTT to leader + leader commit time + follower replication
catch-up time. To reduce it:

* Place follower clusters geographically close to the leader cluster.
* Reduce `--replication.poll-interval` on the follower so it applies the forwarded revision
  sooner.

---

## Monitoring Replication

See [Metrics and Observability](metrics_and_observability.md) for the Prometheus metrics
exposed by the replication subsystem, including `armada_replication_index` (per-table
leader index observed by the follower) and `armada_replication_leased` (lease status per table).

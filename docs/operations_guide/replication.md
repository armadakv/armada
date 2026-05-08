# Cross-Cluster Replication

Armada replicates data from a **leader cluster** to any number of **follower clusters** using
asynchronous, pull-based replication. This page describes how replication works and how to
tune it for your deployment.

See [Architecture](../architecture.md) for a high-level overview of the hub-and-spoke topology.

## How Replication Works

Replication between clusters is **pull-based** — follower clusters periodically poll the leader
rather than the leader pushing to followers. This means:

* Adding a new follower cluster requires no changes to the leader.
* Temporary network outages between clusters do not affect write availability on the leader.
* Followers replicate at their own pace and can lag behind the leader.

### Logical Command Replication

Cross-cluster replication operates at the level of **logical commands**, not raw Raft log entries.
The leader's `Log.Replicate` RPC converts Raft entries into `regattapb.Command` values and streams
them to the follower. The follower re-proposes these commands into its own local Raft group for each
table. This means:

* Each cluster maintains its own independent Raft state and local Raft indices.
* The follower stores the **source leader index** alongside its local Raft index so that the
  same logical write always carries the same MVCC revision across all regions.
* The follower can be behind the leader without affecting consistency guarantees *within* the follower.

### Snapshot Fallback

If the requested log position is no longer available on the leader (because the Raft log was
compacted or the GC horizon has advanced), the leader signals the follower to perform a
**snapshot recovery** instead of a log replay. The follower then:

1. Receives a full or incremental snapshot from the `Snapshot` service.
2. Replays the streamed commands into a temporary recovery shard.
3. Atomically swaps the recovered shard for the live table.

Snapshot recovery is the fallback for full catch-up after extended downtime or when a new
follower cluster is bootstrapped.

## Per-Table Lease-Based Workers

Each follower runs one **replication worker** per table. Workers use a lease mechanism to ensure
that only one node in the follower cluster is actively replicating each table at any time.
The lease interval is controlled by `--replication.lease-interval`.

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

## Tuning Tips

### Reducing Replication Lag

* Decrease `--replication.poll-interval` (e.g. `200ms`) to poll the leader more aggressively.
  Be mindful of the additional load this places on the leader.
* Increase `--replication.max-recv-message-size-bytes` on the follower and
  `--replication.max-send-message-size-bytes` on the leader to allow larger batches to be
  transferred in a single RPC round-trip.

### Improving Snapshot Throughput

* Increase `--replication.max-snapshot-recv-bytes-per-second` to remove the default unlimited
  rate and instead cap bandwidth, or leave it at `0` for maximum speed.
* Increase `--replication.max-recovery-in-flight` only if you have many tables to recover
  simultaneously and sufficient I/O capacity.

### Unreliable or High-Latency Networks

* Increase `--replication.keepalive-time` and `--replication.keepalive-timeout` to tolerate
  transient network outages without dropping the replication connection prematurely.
* Increase `--replication.log-rpc-timeout` if the network round-trip time between the leader
  and follower is high.

## Monitoring Replication

See [Metrics and Observability](metrics_and_observability.md) for the Prometheus metrics
exposed by the replication subsystem.

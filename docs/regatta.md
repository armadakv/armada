# Regatta and Armada

## Origin

Armada is the spiritual successor to **Regatta**, a distributed key-value store developed
internally at [JAMF Ltd](https://www.jamf.com/). Regatta was open-sourced and published on GitHub,
but was subsequently taken down following copyright claims. The original engineering story is
documented in the JAMF engineering blog post
[The story of Regatta](https://medium.com/jamf-engineering/the-story-of-regatta-4652f71a350f).

Armada is a **hard fork** of the Regatta codebase, maintained and developed as a fully
independent open-source project under [armadakv](https://github.com/armadakv).

## Compatibility with Regatta

Armada and Regatta are **not** drop-in binary replacements of each other, but migration from
Regatta to Armada is straightforward. The table below summarises what is and is not compatible:

| Area | Compatible? | Notes |
|------|:-----------:|-------|
| **KV wire API** | ✅ Yes | Armada implements the same `regatta.v1.KV` gRPC service, so existing Regatta clients (CLI, SDKs) work against an Armada cluster without code changes. |
| **Backup format** | ✅ Yes | Backups taken from a Regatta cluster can be restored onto an Armada cluster and vice-versa. This makes backup-based migration the recommended path. |
| **Data storage format** | ❌ No | Armada uses a different on-disk layout and MVCC scheme. Armada nodes cannot read Regatta data files directly. |
| **In-cluster replication** | ❌ No | Armada's Raft transport (QUIC-based dragonboat fork) is incompatible with Regatta's Raft transport. A mixed Regatta/Armada cluster is not supported. |
| **Cross-cluster replication** | ❌ No | The inter-cluster replication protocol (log and snapshot streaming RPCs) has changed. A Regatta leader cannot replicate to an Armada follower, and vice versa. |

## Migrating from Regatta

The recommended migration path is:

1. Take a backup of each table from the live Regatta cluster using `arctl backup` (or the
   equivalent Regatta CLI — the backup format is identical).
2. Deploy a fresh Armada cluster.
3. Restore each table backup onto the new Armada cluster using `arctl restore`.

Because the KV wire API is fully compatible, clients can be pointed at the new Armada cluster
with no code changes needed.

## Legacy Naming

Armada retains many `regatta`-prefixed identifiers as a historical artifact of its origin:

* **gRPC service names** — `regatta.v1.KV`, `regatta.v1.Tables`, `regatta.v1.Cluster`, etc.
* **Prometheus metric prefixes** — `regatta_*`
* **Helm chart name** — published as `regatta/regatta` (the chart will be renamed in a future release)
* **Docker image path** — still published under the `regatta` org on GHCR

These identifiers are intentionally preserved to maintain wire compatibility with Regatta clients
and tooling. They will be gradually updated in future releases as breaking changes are scheduled.

---
title: "Introduction"
subtitle: "A distributed, eventually consistent key-value store built for Kubernetes"
description: "Overview of ArmadaKV — a distributed key-value store designed for high read throughput and global data distribution."
section: "overview"
section_label: "Overview"
section_order: 1
order: 1
---

# Armada

**Armada** is a distributed, eventually consistent key-value store built for Kubernetes.
It distributes data globally using a
[*hub-and-spoke model*](https://en.wikipedia.org/wiki/Spoke%E2%80%93hub_distribution_paradigm),
with an emphasis on *high read throughput*, *fault-tolerance*, and *low operational overhead*.

```
                         ┌──────────────────┐
                         │   Leader Cluster  │
                         │   (core / hub)    │
                         └────────┬─────────┘
                 pull replication │ (async)
           ┌─────────────────┬────┘────────────────┐
           ▼                 ▼                      ▼
  ┌──────────────┐  ┌──────────────┐      ┌──────────────┐
  │   Follower   │  │   Follower   │  ... │   Follower   │
  │  (edge DC 1) │  │  (edge DC 2) │      │  (edge DC N) │
  └──────────────┘  └──────────────┘      └──────────────┘
```

A single **leader cluster** accepts all writes and replicates them asynchronously to any number
of **follower clusters**. Followers serve low-latency reads locally without requiring round-trips
to the leader. See [Architecture](architecture.md) for an in-depth explanation.

> **Note:** Armada has not yet reached the 1.0 milestone. The API may change before 1.0 is
> released. Backward-incompatible changes will always be flagged in the release notes.

---

## Get Started Quickly

| Goal | Where to go |
|------|-------------|
| Run a local single-node cluster | [Quickstart](quickstart.md) |
| Deploy a leader + followers on Kubernetes | [Deploying to Kubernetes](operations_guide/deploying_to_kubernetes.md) |
| Read and write data | [User Guide](user_guide/index.md) |
| Understand the architecture | [Architecture](architecture.md) |
| Secure your deployment | [Security](operations_guide/security.md) |
| Monitor and operate | [Metrics & Observability](operations_guide/metrics_and_observability.md) |

---

## Feature Highlights

### 🌍 Global data distribution

Armada replicates data from one core cluster to many edge clusters worldwide using
asynchronous, pull-based replication. Adding a new edge cluster requires no changes to the
leader — follower clusters bootstrap themselves and replicate automatically.

### ⚡ High read throughput

Reads are served entirely from the local follower's memory-mapped storage.
This delivers **sub-millisecond read latency** and unlimited horizontal read scaling simply
by adding more followers.

### 🛡️ Fault-tolerant by design

Every cluster — both leader and follower — uses the [Raft consensus algorithm](https://raft.github.io)
internally. The system continues to serve reads even if a minority of nodes in a cluster are
unavailable, and tolerates transient network partitions between clusters without data loss.

### 🗄️ Persistent, MVCC storage

Data is stored in [Pebble](https://github.com/cockroachdb/pebble), a high-performance
LSM-tree engine. Every write is versioned with a monotonically increasing **revision**, enabling
multi-version concurrency control (MVCC) across all regions. The same logical write always
carries the same revision on every cluster that has applied it.

### 🔒 Production-grade security

TLS, mutual TLS (mTLS), per-API token authentication, and Unix socket transport are all
supported out of the box. See [Security](operations_guide/security.md) for the full guide.

### ☸️ Kubernetes native

Armada is designed to run on Kubernetes. Use the
[official Armada Helm Chart](https://github.com/armadakv/armada-helm) to deploy and manage
both leader and follower clusters with a single `helm install`. Helm values control cluster
mode, replication targets, TLS certificates, and more.

### 📦 Backups and restores

The Maintenance API provides online backup and point-in-time restore without downtime.
See [Backups](operations_guide/backups.md) for details.

### 🔑 Rich key-value semantics

Beyond basic put/get/delete, Armada supports:

* **Prefix scans** and **range deletes**
* **Compare-and-swap (CAS) transactions** — atomic multi-key operations with preconditions
* **Secondary indexes** — model additional lookup patterns within a single table
* **Tables** — isolated keyspaces, each with independent consistency guarantees

---

## When to Use Armada

| Scenario | Fit |
|----------|-----|
| Low-latency reads from edge locations worldwide | ✅ Excellent |
| Persistent local cache where reads heavily outnumber writes | ✅ Excellent |
| Configuration or feature-flag distribution to many clusters | ✅ Excellent |
| Mixed read/write workloads requiring strong consistency | ⚠️ Use the leader cluster directly |
| Pure write-heavy workloads | ❌ Not the right tool |

---

## Why Armada?

Armada was built because no existing distributed system offered all of the above features
together. The original story is told in the JAMF engineering blog post
[The story of Regatta](https://medium.com/jamf-engineering/the-story-of-regatta-4652f71a350f).

Armada is the spiritual successor to the **Regatta** project. See [Regatta and Armada](regatta.md) for the
full history, the relationship between the two, and guidance on migrating from Regatta to Armada.

Want to contribute? Check the [Contributing](contributing.md) page!

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
It is designed to distribute data globally in a
[*hub-and-spoke model*](https://en.wikipedia.org/wiki/Spoke–hub_distribution_paradigm)
with an emphasis on *high read throughput*.
It is *fault-tolerant* and able to handle network partitions and node outages gracefully.

## Feature Overview

### Built for Kubernetes

Armada is Kubernetes native. You can manage and monitor Armada as any
other Kubernetes deployment. Check out the
[official Armada Helm Chart](https://github.com/armadakv/armada-helm) for more information.

### Distribute data globally in a hub-and-spoke model

Armada is designed to efficiently distribute data from a single core cluster
to multiple edge clusters around the world. See the [Architecture](architecture.md#Topology)
page for more information.

### Emphasis on high read throughput

Armada is built to handle read-heavy workloads and to serve sub-millisecond reads.

### Fault-tolerance and data availability

Thanks to the Raft algorithm and data redundancy, Armada can serve reads even in the event of
network partition or node outage.

### Data persistence

Armada is more than just an in-memory cache -- data persistence is built-in. Armada
can be backed up and restored from backups.

### Dynamic scaling of edge clusters

Armada makes it really easy to add new edge clusters. When adding a new edge cluster,
no other clusters are affected and the data is automatically replicated from the core cluster to the edge cluster.

## What is Armada good for?

### You need a distributed key-value store to allow local and quick access to data in edge locations

Armada will provide a read-only copy of the data in edge locations. Armada will take care of the data replication,
data availability, and resilience in case of failure of the core cluster.

### You need a local, persistent, cache within a data center where reads heavily outnumber writes

Armada writes are expensive in comparison with Redis for example.
Reads are usually served from memory, resulting in sub-millisecond reads.

### You need a pseudo-document store

You can define secondary indexes or additional columns/tables within a single Armada table.
The data consistency is granted within a single table.
There are compare-and-switch and multi-key atomic operations available.

## Why Armada?

Armada was built because we were unable to find a distributed system
offering all the aforementioned features. The original story behind the project can be found in the JAMF blog post
[The story of Regatta](https://medium.com/jamf-engineering/the-story-of-regatta-4652f71a350f).

Armada is the spiritual successor to the **Regatta** project. See [Regatta and Armada](regatta.md) for the
history of the project, the relationship between the two, and guidance on migrating from Regatta to Armada.

> **Note:**
Armada has not yet reached the 1.0 milestone. The API may change before 1.0 is released.
Backward-incompatible changes will always be flagged in the release notes. If you would like to
help, do not hesitate and check the [Contributing](contributing.md) page!

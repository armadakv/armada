---
title: "Operations Guide"
subtitle: "Deploying and operating ArmadaKV"
description: "Guides for deploying, configuring, and operating ArmadaKV in production environments."
section: "operations_guide"
section_label: "Operations Guide"
section_order: 3
order: 1
---

# Operations Guide

This guide covers everything needed to deploy, configure, and operate an Armada cluster.
When deploying to Kubernetes, we highly encourage you to use the
official [Armada Helm Chart](https://github.com/armadakv/armada-helm).
For more information regarding Armada configuration, consult the
[CLI](cli) documentation and [Armada Helm Chart](https://github.com/armadakv/armada-helm).
The Helm repository still publishes the chart under the legacy `regatta` name.

## Contents

* [Deploying to Kubernetes](deploying_to_kubernetes.md) — deploy a leader and follower cluster using Helm
* [Tables](tables.md) — create, manage, and secure tables
* [Cross-Cluster Replication](replication.md) — how replication works and how to tune it
* [Backups](backups.md) — create backups and restore from them
* [Security](security.md) — TLS, mTLS, and token-based authentication
* [Metrics and Observability](metrics_and_observability.md) — Prometheus metrics, alerting, and profiling
* [CLI Reference](cli) — full flag reference for `armada` server commands and `arctl` maintenance commands

---
title: "Backups"
description: "How to create and restore ArmadaKV backups using the Maintenance gRPC API and built-in CLI commands."
section: "operations_guide"
order: 3
---

# Backups

Armada supports creating backups and restoring from them through the [Maintenance gRPC API](../api.md#maintenance-proto)
and the [`arctl`](cli/arctl.md) `backup` and `restore` commands.


> **Important:**
Backing up and restoring can be done only in a leader cluster.

To interact with Armada's Maintenance API, use the `arctl` binary, which can be
downloaded from the [Releases GitHub page](https://github.com/armadakv/armada/releases)
or use the [Docker Image](https://github.com/armadakv/armada/pkgs/container/regatta).

## Create backup

To create backups, Maintenance API must be enabled during Armada startup.
See the [Helm Chart](https://github.com/armadakv/armada-helm/blob/master/charts/regatta/values.yaml)
or the [CLI documentation](cli/armada.md#leader) for reference. The chart path still uses the legacy `regatta` name.

Additionally, a *token* must be provided during Armada startup which
is then provided to the `arctl backup` command:

```bash
arctl backup \
      --address=https://127.0.0.1:8443 \
      --token=$(BACKUP_TOKEN) \
      --ca=ca.crt \
      --dir=/backup \
      --json=true
```

The command then creates binary file for each table and a human-readable JSON manifest
from Armada leader cluster running on `127.0.0.1:8443`.

### Periodically backing up to S3 Bucket

Armada Helm Chart also offers a [CronJob](https://github.com/armadakv/armada-helm/blob/master/charts/regatta/values.yaml#L322)
to periodically create backup and push it to an S3 Bucket.

### Shared-Store Snapshot Exporter

Armada includes a shared-store snapshot exporter that can continuously or periodically push snapshots to an object store (e.g., S3). This capability, along with HTTP live fallback, provides a resilient and resumable path for snapshot queries and recovery without overloading the leader cluster's local disks or RPC network during large restores.

### Incremental Snapshots

In addition to full cluster backups, Armada supports **Incremental Snapshots**. This allows taking backups of only the differences (deltas) since the last full snapshot, significantly reducing backup sizes and the network footprint required to transfer them.

## Restore from backup

> **Warning:**
Restoring from backups is a destructive operation and should be used only as a part of a break-glass procedure.

To restore from backups, Maintenance API must be enabled during Armada startup and a *token* and a directory
containing binary backups and the JSON manifest must be provided to the `arctl restore` command.
All tables present in the manifest are then restored.

```bash
arctl restore \
      --address=https://127.0.0.1:8443 \
      --token=$(BACKUP_TOKEN) \
      --ca=ca.crt \
      --dir=./backup \
      --json=true
```

This command overwrites all the tables specified in the `backup` directory in an Armada leader cluster
running on `127.0.0.1:8443`.

## Resetting a follower cluster

Data in the follower cluster can also be wiped completely, forcing the follower to reload all the data directly from
the leader. See the [Reset method in the Maintenance gRPC API documentation](../api.md#maintenance) for more information.

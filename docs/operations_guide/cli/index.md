---
title: "CLI Reference"
subtitle: "Command-line interface documentation"
description: "Complete reference for the ArmadaKV server, control-plane, and data-plane CLIs."
section: "operations_guide"
subsection: "cli"
subsection_label: "CLI Reference"
subsection_order: 1
order: 1
---

# CLI Reference

Armada ships three CLIs:

* `armada` — the server binary used to start and run ArmadaKV nodes.
* [`arctl`](arctl.md) — the control-plane and maintenance CLI used for backup, restore, and table administration.
* [`arq`](arq.md) — the data-plane CLI used to query and modify key-value data.

## `armada` Commands

| Command | Description |
|---------|-------------|
| [armada](armada.md) | Root command |
| [armada leader](armada.md#leader) | Start Armada in leader mode |
| [armada follower](armada.md#follower) | Start Armada in follower mode |
| [armada version](armada.md#version) | Print current version |

## `arctl` Commands

| Command | Description |
|---------|-------------|
| [arctl](arctl.md) | Control-plane and maintenance CLI |
| [arctl backup](arctl.md#backup) | Backup Armada to local files |
| [arctl restore](arctl.md#restore) | Restore Armada from local files |
| [arctl tables](arctl.md#tables) | Create, list, and delete tables |

## `arq` Commands

| Command | Description |
|---------|-------------|
| [arq](arq.md) | Query and data-plane CLI |
| [arq get](arq.md#get-query) | Get a key or range of keys |
| [arq put](arq.md#put) | Put a key-value pair |
| [arq delete](arq.md#delete-del-rm) | Delete a key or range of keys |
| [arq txn](arq.md#txn) | Execute an atomic transaction |

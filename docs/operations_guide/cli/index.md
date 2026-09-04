---
title: "CLI Reference"
subtitle: "Command-line interface documentation"
description: "Complete reference for the ArmadaKV CLIs — the armada server binary and the arctl control-plane binary."
section: "operations_guide"
subsection: "cli"
subsection_label: "CLI Reference"
subsection_order: 1
order: 1
---

# CLI Reference

Armada ships two CLIs:

* `armada` — the server binary used to start and run ArmadaKV nodes.
* [`arctl`](arctl.md) — the control-plane and maintenance CLI used for backup, restore, and table administration.

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

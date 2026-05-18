---
title: "RFC 004: Split CLI Binaries"
description: "Proposal to split Armada's CLI into dedicated server, control, and query binaries."
section: "proposals"
order: 6
---

Proposal status: Accepted

## Summary

Split Armada's current all-in-one CLI into three binaries with clear responsibilities:

* `armada` for running server processes and server-oriented utilities
* `arctl` for administrative and maintenance operations
* `arq` for KV reads, writes, and transaction-oriented data access

As part of this split, `backup` and `restore` move entirely out of `armada` and into `arctl`.
The `arq txn` command should follow the same user interaction model as `etcdctl txn`, including
`cmp` / `then` / `else` sections and both interactive stdin and file-driven input modes.

## Motivation

The current `armada` binary mixes multiple concerns:

* starting and configuring Armada server processes
* performing operational maintenance such as backup and restore
* serving as a future home for user-facing KV workflows

This coupling makes the CLI harder to understand, harder to document, and harder to evolve.
Operators and application users have different expectations, permissions, and workflows, yet they
currently share a single top-level entrypoint.

Splitting the CLI improves the product in several ways:

* **Clearer responsibilities**: server lifecycle, maintenance, and KV interaction each get a
  dedicated binary with a focused command surface.
* **Safer operational model**: destructive maintenance commands such as restore no longer sit next
  to routine server startup commands.
* **Better UX for data access**: `arq` can optimize for developer and operator ergonomics without
  inheriting server-oriented command conventions.
* **Better compatibility story**: transaction workflows can align with `etcdctl`, which is a known
  model for users familiar with etcd-style compare-and-swap operations.

## Design

### Binary responsibilities

The Armada CLI surface will be split into three binaries:

#### `armada`

`armada` becomes the server binary. It is responsible only for starting Armada processes and
exposing server-adjacent utilities.

Expected command surface:

* `armada leader`
* `armada follower`
* `armada docs`
* `armada version`

`armada` will no longer expose `backup` or `restore`.

#### `arctl`

`arctl` becomes the control-plane and maintenance CLI.

Expected responsibilities include:

* backup and restore workflows
* reset and break-glass operations
* table and cluster administration
* other explicitly operational commands that are not part of routine application KV access

`backup` and `restore` move fully to `arctl`. This is a direct cutover rather than a staged
deprecation. The original `armada backup` and `armada restore` commands are removed instead of
being preserved as aliases or compatibility shims.

#### `arq`

`arq` becomes the query and data-plane CLI.

Expected responsibilities include:

* KV reads
* KV writes
* watches
* transactions
* other user-facing data access commands

### Transaction UX

The `arq txn` experience should follow the same handling model as `etcdctl txn`.

This means the RFC adopts the following baseline expectations:

* the command is organized around `cmp`, `then`, and `else` sections
* the command supports interactive stdin-driven entry
* the command supports file-based input for scripting and repeatable workflows
* compare predicates and request operations are compatible in spirit with `etcdctl` transaction
  grammar and flow

The goal is not necessarily bit-for-bit argument compatibility in every detail, but a familiar and
predictable etcd-style transaction experience for users.

### Backward compatibility

This proposal preserves API and wire compatibility. It intentionally breaks CLI compatibility for
backup and restore:

* `armada backup` is removed
* `armada restore` is removed
* the supported replacement is `arctl backup` and `arctl restore`

This tradeoff keeps the long-term command layout simple and avoids carrying duplicate command trees
indefinitely.

### Documentation implications

Documentation should describe the CLI around the new split:

* `armada` as the server/runtime binary
* `arctl` as the administration and maintenance binary
* `arq` as the KV/query binary

Transaction documentation for `arq` should explicitly call out etcd-style handling so users know
which mental model to bring to the command.

## Alternatives

### Keep a single binary

We could continue using one `armada` binary for all server, maintenance, and query operations.
This avoids new executable names, but keeps the command surface crowded and does not create a clean
separation between operational and application-facing workflows.

### Keep `backup` and `restore` in `armada` as aliases

We could move the canonical implementation to `arctl` while preserving `armada backup` and
`armada restore` as wrappers or aliases. This would reduce migration pain in the short term, but it
would also preserve ambiguity about which binary owns maintenance operations. This proposal prefers
a clean cutover.

### Design a custom transaction UX

We could build a transaction command syntax unique to Armada. This would offer maximum freedom, but
would make the CLI harder to learn and harder to integrate into existing operator habits. Reusing
the `etcdctl txn` model lowers that adoption cost.

## Unresolved Questions

* How closely should `arq txn` match `etcdctl txn` syntax at the parser level beyond the shared
  `cmp` / `then` / `else` interaction model?
* How should shared connection, TLS, and auth configuration be exposed consistently across
  `armada`, `arctl`, and `arq`?
* Should generated CLI reference docs live in one combined section or in per-binary sections once
  the split is implemented?

---
title: "Changelog"
description: "Release notes and version history for ArmadaKV."
section: "overview"
order: 6
---

# Changelog

Release sections are generated from merged pull request titles when a release is tagged, see
[Contributing](contributing.md#release-notes). The same notes become the body of the GitHub release,
where the maintainer adds the highlights and upgrade notes by hand.

## v0.3.0 - 2026-11-24

### Features

- add durable learner-first table recovery ([#154](https://github.com/armadakv/armada/pull/154))
- publish and select recoverable snapshot artifacts ([#155](https://github.com/armadakv/armada/pull/155))
- resume follower recovery from durable downloads ([#156](https://github.com/armadakv/armada/pull/156))
- define key creation revision query contract ([#175](https://github.com/armadakv/armada/pull/175))
- expose creation revision filters in the KV API ([#177](https://github.com/armadakv/armada/pull/177))
- compare creation and modification revisions in transactions ([#180](https://github.com/armadakv/armada/pull/180))
- automated release notes ([#185](https://github.com/armadakv/armada/pull/185))

### Bug fixes

- raft correctness fixes ([#145](https://github.com/armadakv/armada/pull/145))
- fix some subtle replication issues ([#148](https://github.com/armadakv/armada/pull/148))
- improve lease handling of replication routines ([#149](https://github.com/armadakv/armada/pull/149))
- stabilize transport tests and table error codes ([#153](https://github.com/armadakv/armada/pull/153))
- acknowledge completed follower recovery durably ([#174](https://github.com/armadakv/armada/pull/174))
- harden snapshot and transaction validation ([#181](https://github.com/armadakv/armada/pull/181))
- improve CI and test stability ([#183](https://github.com/armadakv/armada/pull/183))
- README badges ([#184](https://github.com/armadakv/armada/pull/184))

### Internal

- add isolated recovery integration harness ([#157](https://github.com/armadakv/armada/pull/157))

<details markdown="1">
<summary>Other commits (12)</summary>

Commits that are part of a merged PR but not named by its title, for example commits of stacked PRs.

- snapshot correctness error fixes ([278601c](https://github.com/armadakv/armada/commit/278601cfb8fe92053a432a448194a2b4f4d284cd))
- decoupling and coalescing engine events ([9d34e52](https://github.com/armadakv/armada/commit/9d34e529cc306dab430681a7e2e18269e9f54159))
- striped locks for snapshots ([2cac8fe](https://github.com/armadakv/armada/commit/2cac8fe6a52d7b51f81293b40beb63e92d6286a8))
- bound max quic frame size ([937086e](https://github.com/armadakv/armada/commit/937086eb729b213f1a918c0a0036dfd86bc2779c))
- sort out backup stream compatibility ([d3bf9d0](https://github.com/armadakv/armada/commit/d3bf9d0f094b1befc76c6e04667e4d73ed948759))
- address recovery harness review feedback ([f4dc0d0](https://github.com/armadakv/armada/commit/f4dc0d09984878a937d31e0b9a507c1c6da861e3))
- document snapshot path validation for gosec ([3b0c8fc](https://github.com/armadakv/armada/commit/3b0c8fc6d71e25b54c8f55c44ba0e66a03106b83))
- persist and query MVCC key creation revisions ([08edf09](https://github.com/armadakv/armada/commit/08edf0940447bd0f5a8385a7110ea67e734670f2))
- support modification revision query filters ([62ca161](https://github.com/armadakv/armada/commit/62ca161134b920bfc10af91510eee8b14592333e))
- defense in-depth for malformed snapshots ([9d2ad44](https://github.com/armadakv/armada/commit/9d2ad44faf109920ba715d92d8e823226b459613))
- optimise fetching values ([195ced3](https://github.com/armadakv/armada/commit/195ced31197fa64a6f75419d01625148844b738a))
- improve txn compare perf ([6a3d801](https://github.com/armadakv/armada/commit/6a3d801d6fa18047bf18dbe5dbff4a63768efa5d))

</details>

<details markdown="1">
<summary>Dependency updates (5)</summary>

- **deps:** bump github/codeql-action from 4.37.4 to 4.37.9 ([#147](https://github.com/armadakv/armada/pull/147))
- **deps:** bump the gomod group with 5 updates ([#146](https://github.com/armadakv/armada/pull/146))
- **deps:** bump github.com/go-jose/go-jose/v4 from 4.1.4 to 4.1.6 ([#169](https://github.com/armadakv/armada/pull/169))
- **deps:** bump the gomod group across 1 directory with 21 updates ([#171](https://github.com/armadakv/armada/pull/171))
- **deps:** bump the gomod group with 5 updates ([#182](https://github.com/armadakv/armada/pull/182))

</details>

## Earlier releases

Releases `v0.1.0` to `v0.2.1` are documented on the
[GitHub Releases](https://github.com/armadakv/armada/releases) page.

## Regatta history (pre-Armada)

Armada continues the Regatta project. The entries below are kept as they were written and are not generated.
Their version numbers belong to Regatta, not to Armada.

## v0.5.2 (unreleased)

### Bugfixes
* Fix `regatta.v1.KV/IterateRange` without `range_end` returning 0 results.
---
## v0.5.1

### Improvements
* API server will now by default use fixed number of pre-created worker go routines for stream handling. The behaviour could be overridden by setting `api.stream-workers` config value.
* regatta.v1.KV/IterateRange now uses coroutine instead of full goroutine.

### Security
* API server max number of concurrent connections can be set by `api.max-concurrent-connections` config value.
* API server max number of concurrent streams can be set by `api.max-concurrent-streams` config value.

### Bugfixes
* Fix storage compaction metrics.
---
## v0.5.0

### Highlights
#### Follower to leader replication
* The write API (Put, DeleteRange, Txn) is now available on leader cluster nodes as well.
* The API provides read after write guarantee by relying on backpropagation from the leader.

#### Dynamic tables management
* Tables now could be managed dynamically during the runtime of the server using newly provided `regatta.v1.Tables` API.
* Tables API could be secured by an API token using `tables.token` configuration value.

### Security
* API port now supports mTLS
* API port mTLS can verify Hostname or CN in presented certificate.
* Follower to Leader mTLS can now verify Hostname or CN in presented certificate.

### Improvements
* Improve on API allocations when deserializing gRPC messages.
* Support `zstd` API compression.
* Removed obsolete table manager cache that could have caused hard to debug race conditions.
* Bump to Go 1.22.

### Deprecations
* `tables.names` and `tables.delete` configuration values were deprecated and will be removed in future releases.

### Bugfixes
* Proper authentication of `maintenance.v1.Backup/Restore` API endpoint.
* When table is deleted in the leader cluster the followers will gracefully handle the situation by deleting the table locally and stopping the replication.
* Fixed potential Engine deadlock when shutting down the server.
---
## v0.4.1

### Improvements
* Server now reports transient errors for requests that could be potentially retried as `codes.Unavailable`.

### Bugfixes
* Fixed the default timeout of `KV/IterateRange` operation.

---
## v0.4.0

### Breaking changes
* Remove maintenance server port, the API is now available on standard API port. It could still though be secured by a separate API token.
* Replaced `clusterID` with `shard_id` in metric label names.

### Features
* Added new `regatta.v1.KV/IterateRange` API.
* Added option to get the server configuration via the `regatta.v1.Cluster/Status` API.
* Added `memberlist.cluster-name` and `memberlist.node-name` options.
  * Both help identifying cluster and nodes in CLuster API responses.
  * Cluster name setting prevents accidental pairing of multiple clusters running in the same network. (with different cluster name setting)
* Added `api.advertise-address` option to split address advertised to clients from the listen address.
* Removed `maintenance.address` config option.
* Removed `maintenance.cert-filename` config option.
* Removed `maintenance.key-filename` config option.

### Bugfixes
* Fix `KV/DeleteRange` `Count` erroneously returning `1` if no key was deleted.
* Fix `KV/DeleteRange` `PrevKV` panicking when previous key did not exist.

---
## v0.3.2
A bugfix release, fixes critical issue in replication server TLS handling.

### Bugfixes
* Fix replication server TLS.

---
## v0.3.1
A bugfix release, with small improvements.

### Improvements
* Improve startup logging and failure reporting.

### Bugfixes
* Fix `rest.address` handling.
* Fix race-conditions in Raft events handling.

---
## v0.3.0
Release brings a brand-new Cluster API as well as ability to listen over plain or unix socket connection.

### Breaking changes
* Semantics of `api.address` `replication.address` and `maintenance.address` changed. The protocol is now mandatory could be one of `http|https|unix|unixs`.
* Removed `api.reflection-api` configuration.

### Features
* Armada could now serve its api on unix sockets.
* Support for serving over plain (non-TLS) gRPC added.
* Added `regatta.v1.Cluster` api.

### Improvements
* Improve `version` command output.
* Follower can connect to leader over plain connection or via unix socket.

### Bugfixes
* Fix cluster member node registration.

---
## v0.2.3
Release with profile guided optimization.

### Improvements
* Removal of few obsolete dependencies.
* Set toolchain in go.mod file.
* Enable profile guided optimizations.

---
## v0.2.2
Minor release with just updated dependencies.

---
## v0.2.1
Release focuses on stability and performance of the leader-follower replication.

### Breaking changes
* Leader `replication.logCacheSize` now defaults to 0.

### Features
* Add `replication.keepalive-time` config option for follower. Sets how often the keepalive should be sent.
* Add `replication.keepalive-timeout` config option for follower. Sets how long to wait for an ack of the keepalive message.

### Improvements
* Improve follower replication throughput and latency.
* Snapshot recovery type could be switched on a running cluster safely.
* Bump to Go 1.21.
* Keepalive for replication connection to tackle misbehaving LBs.

### Bugfixes
* Fix release image cosign.
* Fix off by one replication error.

---
## v0.2.0
Release is mostly focused on tidying up the docs and code base and on resource consumption improvements.
- New way of producing table snapshots is introduced that should lead to quicker and more efficient process of recovering shards in a Raft cluster.
- Experimental Tan logdb feature was promoted to a regular and made a default choice.
- Gossip based cluster registry was added, the necessary migration is handled automatically by the new version of the `armadakv/armada-helm` helm chart. If you want to know more refer to the documentation of `memberlist` flags.

### Breaking changes
* Removal of `--experimental.tanlogdb` flag.
* Addition of gossip based cluster registry.
* `regatta.v1.KV/Range` now returns empty response instead of `NOT_FOUND` error if result of the query is an empty range. Implements proposal #001.

### Features
* New `--raft.logdb` flag (defaults to Tan).
* Added `--memberlist.address`, `--memberlist.advertise-address` and  `--memberlist.members` flags to configure cluster registry.
* Added a raft cluster snapshot mode option `--raft.snapshot-recovery-type` which defaults to new `checkpoint` mode.
* Added a flag for configuring shared table cache size `--storage.table-cache-size` which defaults to `1024`.
* Added support for `gzip`, and `snappy` compression for API calls.

### Improvements
* Compressed Log replication messages to lower API bandwidth usage.
* Bump to Go 1.20.
* Pebble instances table cache is now shared.
* Single key lookups now utilise bloom filters optimizing the "key not present" case.
* Added compaction metrics of pebble instances.

### Bugfixes
* Removing kafka package leftovers.
* Improving logreader tests.
* Fixed raft header returned from non-leader cluster nodes.
* Fixed value of `Deleted` field of the `DeleteRange` response.
* Fixed potential DOS by limiting the storage iterator with max RPC size.

---
## v0.1.0

Initial Armada v0.1.0 Release

This release was made possible thanks to the contributions of @jizi @martinvizvary @jojand @mrackoa and @juldou :rocket::tada:

Special shout-out to @coufalja for his efforts!

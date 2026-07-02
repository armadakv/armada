# Copilot instructions for Armada

## Build, test, and lint

- `make armada` builds only the `armada` binary.
- `make build` regenerates protobuf outputs, generates CLI docs into `docs/operations_guide/cli`, and builds the binary.
- `make proto` regenerates the checked-in protobuf and vtprotobuf outputs in `regattapb/` from `proto/*.proto`.
- `make run` starts a local single-node leader in dev mode.
- `make run-follower` starts a local follower wired to the local leader target; both commands use the certs under `hack/replication/`.
- `make test` runs the full suite locally with coverage and race detection: `go test ./... -cover -race -v`.
- Run a single package with `go test ./path/to/package`.
- Run a single test with `go test ./path/to/package -run '^TestName$'`.
- CI coverage uses `go test -json -cover ./... -coverprofile coverage.out -coverpkg ./log/...,./pebble/...,./raft/...,./armadaserver/...,./replication/...,./storage/...,./security/...,./util/...`.
- `make check` is the local lint entrypoint; it runs `make proto` first and then `golangci-lint run`.

## High-level architecture

- `main.go` is only a Cobra entrypoint. Runtime setup lives in `cmd/`.
- The deployment model is hub-and-spoke: there is one leader cluster that accepts writes and any number of follower clusters that replicate from it asynchronously.
- `cmd/leader.go` and `cmd/follower.go` share startup helpers from `cmd/common.go`, so engine/config/startup changes usually need to work in both modes.
- `storage.Engine` is the core runtime. It wraps the Dragonboat `raft.NodeHost`, the gossip cluster view in `storage/cluster`, a raft-backed metadata store (`storage/kv.RaftStore`, cluster ID `1000`), and the `storage/table.Manager`.
- Each table is its own raft-backed state machine managed by `storage/table`. Table CRUD is metadata-driven, but KV operations execute against the per-table raft state machine.
- **Replication within one cluster** goes through Dragonboat `raft.NodeHost`. `storage/table.Manager.startTable` starts one on-disk Raft replica per table shard with `StartOnDiskReplica`, `tickWorkerMain` drives Raft ticks, the transport layer batches `raftpb.Message` traffic over QUIC, and the execution engine fans work into step/commit/apply workers. Incoming `MessageBatch` traffic is routed through `messageHandler` into per-node queues, then step/apply workers persist entries, advance Raft, and apply committed commands to the table FSM.
- `storage/table/fsm` is where Raft entries become MVCC state. It persists both the local Raft apply index (`sysLocalIndex`) and, when present, the source leader index (`sysLeaderIndex`) inside the Pebble-backed FSM.
- Raft is only for replication within a single cluster. Cross-location replication is pull-based over the replication gRPC APIs, not cross-cluster Raft.
- Leader mode serves local KV, cluster, tables, maintenance, metrics, and replication APIs. The replication listener exposes metadata, snapshot, log, and KV services for followers.
- Follower mode still boots a local `storage.Engine`, but write APIs are not handled locally. `armadaserver.ForwardingKVServer` forwards writes to the leader and waits until the follower's local replication queue has applied the returned revision before replying.
- **Replication between clusters** is handled by `replication.Manager` and one `replication.worker` per leased table. The worker polls the leader `Log.Replicate` RPC using the follower table's stored leader index, receives leader Raft entries as `regattapb.Command` values, batches them into `Command_SEQUENCE`, and re-proposes them into the follower's own table shard. This means inter-cluster replication replays logical commands into a different Raft group rather than shipping raw Dragonboat log/state across regions.
- `armadaserver.LogServer` serves cross-cluster log replication by reading the leader table's persistent Raft log through `storage/logreader`, converting `raftpb.Entry` values back into `regattapb.Command`, and streaming them with the current applied leader index. If the requested index has already fallen behind compaction or GC, it tells the follower to recover from snapshot instead.
- Snapshot recovery is the fallback for inter-cluster catch-up. `SnapshotServer` streams a full or incremental snapshot and appends a final dummy command carrying the latest leader index; follower restore creates a recovery shard, replays the streamed commands into it, then atomically flips the table metadata to the recovered shard ID.
- gRPC server implementations live in `armadaserver/`; protobuf definitions are in `proto/*.proto`; generated code is checked in under `regattapb/`.

## Key conventions

- Configuration is Viper-backed and uses dot-separated keys (`raft.address`, `replication.leader-address`, etc.). `initConfig` reads `config` files from `/etc/armada/`, `/config`, `$HOME/.armada`, and the working directory, then overlays environment variables and bound Cobra flags.
- The project started as **Regatta** inside JAMF and was later forked into the open-source **Armada** project (`armadakv.io`). Many protobufs, packages, and generated types still use the old `regatta` naming (`regattapb`, `armadaserver`, `regatta.v1.*`), but that naming is legacy and transitional rather than the desired end state.
- Address strings are URL-like, not raw `host:port` values. `resolveURL` uses the scheme to choose transport and TLS behavior (`http`/`https` and `unix`/`unixs`).
- Preserve the server error-mapping pattern in `armadaserver/*`: validate request fields first, translate `storage/errors.ErrTableNotFound` and other storage errors to gRPC status codes, and use `storage/errors.IsSafeToRetry` for retryable `Unavailable` responses.
- Do not remove the follower-side propagation wait after forwarded writes. `Put`, `DeleteRange`, and non-readonly `Txn` must wait on `storage.IndexNotificationQueue` so follower API calls only return after the local replica has applied the leader's revision.
- Keep the distinction between **local Raft index** and **source leader index** intact. In follower clusters, `storage/table/fsm/updateContext.seqno()` must use the replicated leader index when present so all regions stamp the same MVCC version for the same logical write.
- Cross-cluster replication should continue to operate on logical commands, not copied Dragonboat internals. `LogServer` converts Raft entries to `regattapb.Command`, and followers re-propose those commands into their own shard; changes in this path must preserve idempotence and leader-index tracking.
- Compaction and GC interact with replication. When leader history is no longer available in the Raft log or falls at/below the table GC horizon, the leader intentionally forces followers to use snapshot recovery rather than serving an incomplete delta.
- Table replication work is lease-based. Only the node that currently holds the table lease should run the follower replication worker for that table.
- Tokens for maintenance and table APIs are intentionally redacted by `viperConfigReader`; if new sensitive config is surfaced through cluster config responses, add it to `secretConfigs`.
- Follower APIs are intentionally more limited than leader APIs: table create/delete is unimplemented on followers, and maintenance behavior differs (`BackupServer` on leader, `ResetServer` on follower).
- CLI docs under `docs/operations_guide/cli` are generated from Cobra commands via `armada docs`; do not hand-edit generated output when command flags or help text change.
- Tests primarily use `testify/require` or `assert`, while `vfs/` also uses CockroachDB `datadriven` tests with `testdata/` fixtures.

# Tables

Armada organises all key-value data into **tables**. Each table is an independent keyspace with
its own Raft consensus group, its own storage, and its own replication stream. All API consistency
guarantees (e.g. compare-and-swap, linearisable reads) are scoped to a single table — there is
no cross-table atomicity.

## Managing Tables

Tables are managed through the `regatta.v1.Tables` gRPC API. You can interact with it using
[grpcurl](https://github.com/fullstorydev/grpcurl) or the official client libraries.

### Create a Table

```bash
grpcurl -plaintext \
  -d '{"name": "my-table"}' \
  127.0.0.1:8443 regatta.v1.Tables/Create
```

### List Tables

```bash
grpcurl -plaintext 127.0.0.1:8443 regatta.v1.Tables/List
```

### Delete a Table

```bash
grpcurl -plaintext \
  -d '{"name": "my-table"}' \
  127.0.0.1:8443 regatta.v1.Tables/Delete
```

> **Warning:**
> Deleting a table is permanent. All data stored in the table will be lost.
> In a leader–follower deployment, deleting a table on the leader will also cause followers
> to gracefully delete their local copy.

## Securing the Tables API

The Tables API can be protected with a static token. Set `--tables.token` when starting Armada:

```bash
armada leader --tables.token=my-secret-token ...
```

Clients must then include the token as a gRPC metadata header:

```bash
grpcurl -plaintext \
  -H 'authorization: Bearer my-secret-token' \
  -d '{"name": "my-table"}' \
  127.0.0.1:8443 regatta.v1.Tables/Create
```

If `--tables.token` is not set, no authentication is required for the Tables API.

The Tables API can be disabled entirely with `--tables.enabled=false`.

## How Tables Map to Raft Groups

Each table is backed by its own Raft consensus group. This means:

* Writes to one table do not block writes to another table.
* A Raft leader election in one table does not affect other tables.
* Snapshots and log compaction happen independently per table.
* In a multi-node cluster, different tables may have different Raft leaders.

The metadata table (cluster ID `1000`) is a special internal Raft group that tracks the set of
known tables and is not directly accessible through the KV API.

## Consistency Guarantees

* **Within a single table:** reads and writes are linearisable. Compare-and-swap (`Txn`) and
  multi-key atomic operations are supported within a single table.
* **Across tables:** there are no cross-table consistency guarantees. Two writes to different
  tables may be observed in any order.

## Read-Only Tables on Followers

In a follower cluster, tables are read-only. Write requests (`Put`, `DeleteRange`, `Txn`)
received by a follower are forwarded to the leader and the follower waits until its local
replica has applied the returned revision before replying to the client. This provides a
**read-after-write guarantee** even when the client sends requests to a follower.

## Static Cluster Membership

Armada currently requires a static set of cluster members. The number of nodes in a cluster
must be specified at cluster creation time and cannot be changed while the cluster is running.
See the [Helm Chart](https://github.com/armadakv/armada-helm) for how to set the replica count
before deploying.

---
title: "Watching Changes"
description: "How to stream changes of keys in ArmadaKV with the Watch API."
section: "user_guide"
order: 6
---

# Watching Changes

See [Watch Request API](../api.md#watchrequest) and [Watch Response API](../api.md#watchresponse)
for the complete gRPC API documentation of the Watch API.

The Watch API streams the changes of a key or key range of one table. Each change is an
event: a `PUT` carries the new value, a `DELETE` the deleted key. Every event carries the
key's `create_revision` and the `mod_revision` of the change.

`Watch` is a bidirectional stream. The client sends requests to create and cancel watchers,
and the server sends the events of every watcher on the stream. Responses carry the
`watch_id` of their watcher.

## Ordering and revisions

* Events arrive in revision order, and all events of one revision arrive before any event
  of a later revision.
* There is at most one event per key per revision. A transaction that changes a key twice
  produces one event with the final state.
* Revisions are sparse: not every revision produces an event.
* Revisions are the same on every node and cluster. A watch is served by the node it was
  created on and is never forwarded, so a client that loses its connection can resume on any
  node, including one in a follower cluster, from the revision after the last event it saw.

## Watch a key

```bash
grpcurl -insecure -d @ 127.0.0.1:8443 regatta.v1.Watch/Watch <<EOF
{"create_request": {
    "table": "$(echo -n "armada-test" | base64)",
    "key": "$(echo -n "key_1" | base64)"}}
EOF
```

The first response has `created` set and carries the `watch_id`. Further responses carry
events until the stream is closed or the watcher is canceled.

With the `arq` CLI:

```bash
arq --table armada-test watch key_1
```

## Watch a range or prefix

`key` and `range_end` select keys the same way as in [Range](get.md): the right-open
interval `[key, range_end)`, `range_end = key + 1` for a prefix, and `range_end` set to `\0`
for every key greater than or equal to `key`.

```bash
arq --table armada-test watch --prefix 1626783802
arq --table armada-test watch --all
```

## Replay from a revision

Set `start_revision` to receive the history from that revision on, followed by live
events. Without it, only the changes made after the watcher was created are sent.

```bash
grpcurl -insecure -d @ 127.0.0.1:8443 regatta.v1.Watch/Watch <<EOF
{"create_request": {
    "table": "$(echo -n "armada-test" | base64)",
    "key": "$(echo -n "key_1" | base64)",
    "start_revision": 100}}
EOF
```

```bash
arq --table armada-test watch --rev 100 key_1
```

History is available as far back as the table's history floor, which follows the
compaction of the Raft log. When a cross-cluster recovery rebuilds a table from a snapshot,
the floor moves to the snapshot's revision, because a snapshot holds only the latest version
of each key. A watcher that needs a revision below the floor is canceled with
`compact_revision` set to the first available revision. The client should read the current
state with Range and start a new watch from a recent revision.

## Previous values

Set `prev_kv` to receive the previous key-value pair with each event. It is always present
for live events. For replayed history it is best-effort: it is left out when garbage
collection has already removed the previous version.

## Filters

`filters` drops events before they are sent: `NOPUT` drops `PUT` events and `NODELETE`
drops `DELETE` events.

## Progress

A response without events is a progress notification: the watcher has received every
event up to `header.revision`, so a client can resume from `header.revision + 1`.

* Set `progress_notify` on a watcher to get a notification periodically while it receives
  no events. The interval is configured with `--watch.progress-interval`.
* Send a `progress_request` to get one notification per watcher on the stream as soon as
  each has caught up.

## Large revisions

Responses are kept below `--watch.max-response-size-bytes`, splitting between revisions.
A single revision that is larger can only be sent in parts: set `fragment` on the watcher,
and the server marks every part but the last with `fragment`. Without it, the watcher is
canceled when such a revision occurs.

## Cancellation

The server cancels a watcher, setting `canceled` and `cancel_reason`, when:

* the client sends a `cancel_request`;
* the table is deleted, recreated, or replaced by a recovery;
* a recovery rewrites the table's history, in which case `compact_revision` is set;
* the node stops serving the table.

A slow client does not lose events: while it falls behind, its watchers read the missed
events back from the store, and continue live once they catch up.

## Access

Watching a table needs the same access as reading it with Range. Each watcher watches
one table; a client that needs several tables creates one watcher per table, on the same
stream if it likes.

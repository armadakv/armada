# User Guide

Throughout this documentation, we are using [gRPCurl](https://github.com/fullstorydev/grpcurl)
to interact with Armada's gRPC API.

To follow this user guide, it is enough to spin up a single Armada leader cluster
with one instance locally as described in [Quickstart](../quickstart).

See [API documentation](../api.md#regatta-proto) for the complete user-facing gRPC API reference.

## Topics

* [Retrieving Records](get.md) — single-key lookup, prefix search, range scans, and the streaming `IterateRange` API
* [Updating Records](put.md) — inserting and updating key-value pairs
* [Deleting Records](delete.md) — deleting single keys, prefix ranges, and bulk deletes
* [Transactions](transactions.md) — atomic if/then/else operations with compare predicates

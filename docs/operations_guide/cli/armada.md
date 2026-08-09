---
title: "armada"
section: "operations_guide"
subsection: "cli"
order: 1
---
## armada

Armada is a read-optimized distributed key-value store.

### Synopsis

Armada can be run in two modes -- leader and follower. Write API is enabled in the leader mode
and the node (or cluster of leader nodes) acts as a source of truth for the follower nodes/clusters.
Write API is disabled in the follower mode and the follower node or cluster of follower nodes replicate the writes
done to the leader cluster to which the follower is connected to.

### Options

```
  -h, --help   help for armada
```

### SEE ALSO

* [armada follower](armada_follower.md)	 - Start Armada in follower mode.
* [armada leader](armada_leader.md)	 - Start Armada in leader mode.
* [armada version](armada_version.md)	 - Print current version.


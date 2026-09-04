---
title: "armada"
section: "operations_guide"
subsection: "cli"
order: 1
---
# NAME

armada - Armada is a read-optimized distributed key-value store.

# SYNOPSIS

armada

```
[--help|-h]
```

# DESCRIPTION

Armada can be run in two modes -- leader and follower. Write API is enabled in the leader mode
and the node (or cluster of leader nodes) acts as a source of truth for the follower nodes/clusters.
Write API is disabled in the follower mode and the follower node or cluster of follower nodes replicate the writes
done to the leader cluster to which the follower is connected to.

**Usage**:

```
armada [GLOBAL OPTIONS] [command [COMMAND OPTIONS]] [ARGUMENTS...]
```

# GLOBAL OPTIONS

**--help, -h**: show help


# COMMANDS

## leader

Start Armada in leader mode.

**--api.address**="": API server address. The address the server listens on. (default: "http://0.0.0.0:8443")

**--api.advertise-address**="": Advertise API server address, used for NAT traversal. (default: "http://127.0.0.1:8443")

**--api.allowed-cn**="": AllowedCN is a CN which must be provided by a client.

**--api.allowed-hostname**="": AllowedHostname is an IP address or hostname that must match the TLS certificate provided by a client.

**--api.ca-filename**="": Path to the API server client auth CA file.

**--api.cert-filename**="": Path to the API server certificate.

**--api.client-cert-auth**: API server client certificate auth enabled. If set to true the api.ca-filename should be provided as well.

**--api.key-filename**="": Path to the API server private key file.

**--api.max-concurrent-connections**="": Maximum number of allowed concurrent client connections. Default of 0 means no limit. (default: 0)

**--api.max-concurrent-streams**="": Maximum number of concurrent streams open. Default of 0 means no limit. (default: 0)

**--api.stream-workers**="": Number of workers to use to process incoming streams. These workers are pre-started and should reduce an overhead of stack allocation as well as prevent potential overload of a storage layer. Default of 0 means number of CPUs + 1, any negative number will result in unlimited workers. (default: 0)

**--config**="": Path to a configuration file (.yaml, .yml, .json or .toml). Values are overridden by environment variables and command-line flags.

**--dev-mode**: Development mode enabled (verbose logging, human-friendly log format).

**--help, -h**: show help

**--log-level**="": Log level: DEBUG/INFO/WARN/ERROR. (default: "INFO")

**--maintenance.enabled**: Whether maintenance API is enabled.

**--maintenance.token**="": Token to check for maintenance API access, if left empty (default) no token is checked.

**--memberlist.advertise-address**="": AdvertiseAddress is the address to advertise to other Armada instances used for NAT traversal.
Gossip services running on remote Armada instances will use AdvertiseAddress to exchange gossip service related messages. AdvertiseAddress is in the format of IP:Port, Hostname:Port or DNS Name:Port.
When not set, the raft.address value is used (gossip shares the raft UDP port).

**--memberlist.cluster-name**="": Cluster name, propagated in Memberlist API responses as well as used as used as a label when forming the gossip cluster.
All nodes of the cluster MUST set this to the same value. If changing it is advisable to turn off all the nodes and then startup with the new value. (default: "default")

**--memberlist.members**="": Seed is a list of addresses of remote Armada instances to bootstrap the gossip service.
Each address should be the raft address (IP:Port) of a peer — gossip shares the same UDP port as raft via ALPN multiplexing.
When not set, the raft.initial-members addresses are used automatically as gossip seeds.

**--memberlist.node-name**="": Node name override, MUST be unique in a cluster, if not specified random stable UUID will be used instead.

**--memberlist.tls-ca-file**="": Path to the CA certificate file used to verify gossip peer certificates.

**--memberlist.tls-cert-file**="": Path to the TLS certificate file for mutual TLS between gossip peers.

**--memberlist.tls-key-file**="": Path to the TLS private key file for mutual TLS between gossip peers.

**--raft.address**="": RaftAddress is a hostname:port or IP:port address used by the Raft RPC module for exchanging Raft messages and snapshots.
This is also the identifier for a Storage instance. RaftAddress should be set to the public address that can be accessed from remote Storage instances.

**--raft.compaction-overhead**="": CompactionOverhead defines the number of most recent entries to keep after each Raft log compaction.
Raft log compaction is performed automatically every time when a snapshot is created. (default: 5000)

**--raft.election-rtt**="": ElectionRTT is the minimum number of message RTT between elections. Message RTT is defined by NodeHostConfig.RTTMillisecond.
The Raft paper suggests it to be a magnitude greater than HeartbeatRTT, which is the interval between two heartbeats. In Raft, the actual interval between elections is randomized to be between ElectionRTT and 2 * ElectionRTT.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the election interval to be 1 second, then ElectionRTT should be set to 10.
When CheckQuorum is enabled, ElectionRTT also defines the interval for checking leader quorum. (default: 20)

**--raft.heartbeat-rtt**="": HeartbeatRTT is the number of message RTT between heartbeats. Message RTT is defined by NodeHostConfig.RTTMillisecond. The Raft paper suggest the heartbeat interval to be close to the average RTT between nodes.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the heartbeat interval to be every 200 milliseconds, then HeartbeatRTT should be set to 2. (default: 1)

**--raft.initial-members**="": Raft cluster initial members is an ordered list of raft addresses for the initial cluster nodes.
The position in the list (1-based) determines the replica ID. Each node derives its own replica ID
by finding its own raft.address in this list. All nodes must specify the same list in the same order.
Example for a 3-node cluster: "--raft.initial-members=127.0.0.1:5012,127.0.0.1:5013,127.0.0.1:5014".

**--raft.listen-address**="": ListenAddress is a hostname:port or IP:port address used by the Raft RPC module to listen on for Raft message and snapshots.
When the ListenAddress field is not set, The Raft RPC module listens on RaftAddress. If 0.0.0.0 is specified as the IP of the ListenAddress, Armada listens to the specified port on all interfaces.
When hostname or domain name is specified, it is locally resolved to IP addresses first and Armada listens to all resolved IP addresses.

**--raft.max-in-mem-log-size**="": MaxInMemLogSize is the target size in bytes allowed for storing in memory Raft logs on each Raft node.
In memory Raft logs are the ones that have not been applied yet. (default: 6291456)

**--raft.max-recv-queue-size**="": MaxReceiveQueueSize is the maximum size in bytes of each receive queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the queue size is unlimited. (default: 0)

**--raft.max-send-queue-size**="": MaxSendQueueSize is the maximum size in bytes of each send queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the send queue size is unlimited. (default: 0)

**--raft.node-host-dir**="": NodeHostDir raft internal storage (default: "/tmp/armada/raft")

**--raft.quic-udp-buffer-size**="": QUICUDPBufferSize is the UDP socket receive/send buffer size in bytes requested for the QUIC transport.
When set to a positive value the QUIC library's buffer requests are capped at this value, preventing log warnings on systems
where the kernel UDP buffer limit is lower than the library default (7 MiB). A value of 0 uses the library default. (default: 0)

**--raft.rtt**="": RTTMillisecond defines the average Round Trip Time (RTT) between two NodeHost instances.
Such a RTT interval is internally used as a logical clock tick, Raft heartbeat and election intervals are both defined in term of how many such RTT intervals.
Note that RTTMillisecond is the combined delays between two NodeHost instances including all delays caused by network transmission, delays caused by NodeHost queuing and processing. (default: 50ms)

**--raft.snapshot-entries**="": SnapshotEntries defines how often the state machine should be snapshot automatically.
It is defined in terms of the number of applied Raft log entries.
SnapshotEntries can be set to 0 to disable such automatic snapshotting. (default: 10000)

**--raft.snapshot-recovery-type**="": Specifies the way how the snapshots should be shared between nodes within the cluster. Options: snapshot, checkpoint, default: checkpoint for non Windows systems.
Type 'snapshot' uses in-memory snapshot of DB to send over wire to the peer. Type 'checkpoint'' uses hardlinks on FS a sends DB in tarball over wire. Checkpoint is thus much more memory and compute efficient at the potential expense of disk space, it is not advisable to use on OS/FS which does not support hardlinks.

**--raft.state-machine-dir**="": StateMachineDir persistent storage for the state machine. (default: "/tmp/armada/state-machine")

**--raft.tls-ca-file**="": Path to the CA certificate file used to verify raft peer certificates.

**--raft.tls-cert-file**="": Path to the TLS certificate file for mutual TLS between raft peers. Must be set together with raft.tls-key-file and raft.tls-ca-file.

**--raft.tls-key-file**="": Path to the TLS private key file for mutual TLS between raft peers.

**--raft.wal-dir**="": WALDir is the directory used for storing the WAL of Raft entries.
It is recommended to use low latency storage such as NVME SSD with power loss protection to store such WAL data.
Leave WALDir to have zero value will have everything stored in NodeHostDir.

**--replication.address**="": Replication API server address. The address the server listens on. (default: "http://0.0.0.0:8444")

**--replication.ca-filename**="": Path to the API server CA cert file.

**--replication.cert-filename**="": Path to the API server certificate.

**--replication.client-cert-auth**: Replication server client certificate auth enabled. If set to true the `replication.ca-filename` should be provided as well.

**--replication.enabled**: Whether replication API is enabled.

**--replication.key-filename**="": Path to the API server private key file.

**--replication.max-send-message-size-bytes**="": The target maximum size of single replication message allowed to send. (default: 4194304)

**--replication.snapshot-timeout**="": Timeout for a single incremental snapshot export triggered by log compaction. (default: 10m0s)

**--rest.address**="": REST API server address. (default: "http://127.0.0.1:8079")

**--rest.read-timeout**="": Maximum duration for reading the entire request. (default: 5s)

**--shared-store.azure.account**="": Account name to use for the Azure shared-store backend.

**--shared-store.azure.container**="": Container name to use for the Azure shared-store backend.

**--shared-store.azure.key**="": Key to use for the Azure shared-store backend.

**--shared-store.backend**="": Blob store backend. Supported values: none (disabled), filesystem, s3, gcs, azblob. (default: "none")

**--shared-store.filesystem.directory**="": Directory path to use for the filesystem backend.

**--shared-store.gc-interval**="": How often the GC worker runs to delete expired artefacts from the shared store. (default: 1h0m0s)

**--shared-store.gcs.bucket**="": Bucket name to use for the GCS shared-store backend.

**--shared-store.retention**="": Maximum age of artefacts in the shared store. Older artefacts are eligible for GC. (default: 48h0m0s)

**--shared-store.s3.bucket**="": Bucket name to use for the S3 shared-store backend.

**--storage.block-cache-size**="": Shared block cache size in bytes, the cache is used to hold uncompressed blocks of data in memory. (default: 16777216)

**--storage.table-cache-size**="": Shared table cache size, the cache is used to hold handles to open SSTs. (default: 1024)

**--tables.delete**="": Delete Armada tables with given names.

**--tables.enabled**: Whether tables API is enabled.

**--tables.names**="": Create Armada tables with given names.

**--tables.token**="": Token to check for tables API access, if left empty (default) no token is checked.

### help, h

Shows a list of commands or help for one command

## follower

Start Armada in follower mode.

**--api.address**="": API server address. The address the server listens on. (default: "http://0.0.0.0:8443")

**--api.advertise-address**="": Advertise API server address, used for NAT traversal. (default: "http://127.0.0.1:8443")

**--api.allowed-cn**="": AllowedCN is a CN which must be provided by a client.

**--api.allowed-hostname**="": AllowedHostname is an IP address or hostname that must match the TLS certificate provided by a client.

**--api.ca-filename**="": Path to the API server client auth CA file.

**--api.cert-filename**="": Path to the API server certificate.

**--api.client-cert-auth**: API server client certificate auth enabled. If set to true the api.ca-filename should be provided as well.

**--api.key-filename**="": Path to the API server private key file.

**--api.max-concurrent-connections**="": Maximum number of allowed concurrent client connections. Default of 0 means no limit. (default: 0)

**--api.max-concurrent-streams**="": Maximum number of concurrent streams open. Default of 0 means no limit. (default: 0)

**--api.stream-workers**="": Number of workers to use to process incoming streams. These workers are pre-started and should reduce an overhead of stack allocation as well as prevent potential overload of a storage layer. Default of 0 means number of CPUs + 1, any negative number will result in unlimited workers. (default: 0)

**--config**="": Path to a configuration file (.yaml, .yml, .json or .toml). Values are overridden by environment variables and command-line flags.

**--dev-mode**: Development mode enabled (verbose logging, human-friendly log format).

**--help, -h**: show help

**--log-level**="": Log level: DEBUG/INFO/WARN/ERROR. (default: "INFO")

**--maintenance.enabled**: Whether maintenance API is enabled.

**--maintenance.token**="": Token to check for maintenance API access, if left empty (default) no token is checked.

**--memberlist.advertise-address**="": AdvertiseAddress is the address to advertise to other Armada instances used for NAT traversal.
Gossip services running on remote Armada instances will use AdvertiseAddress to exchange gossip service related messages. AdvertiseAddress is in the format of IP:Port, Hostname:Port or DNS Name:Port.
When not set, the raft.address value is used (gossip shares the raft UDP port).

**--memberlist.cluster-name**="": Cluster name, propagated in Memberlist API responses as well as used as used as a label when forming the gossip cluster.
All nodes of the cluster MUST set this to the same value. If changing it is advisable to turn off all the nodes and then startup with the new value. (default: "default")

**--memberlist.members**="": Seed is a list of addresses of remote Armada instances to bootstrap the gossip service.
Each address should be the raft address (IP:Port) of a peer — gossip shares the same UDP port as raft via ALPN multiplexing.
When not set, the raft.initial-members addresses are used automatically as gossip seeds.

**--memberlist.node-name**="": Node name override, MUST be unique in a cluster, if not specified random stable UUID will be used instead.

**--memberlist.tls-ca-file**="": Path to the CA certificate file used to verify gossip peer certificates.

**--memberlist.tls-cert-file**="": Path to the TLS certificate file for mutual TLS between gossip peers.

**--memberlist.tls-key-file**="": Path to the TLS private key file for mutual TLS between gossip peers.

**--raft.address**="": RaftAddress is a hostname:port or IP:port address used by the Raft RPC module for exchanging Raft messages and snapshots.
This is also the identifier for a Storage instance. RaftAddress should be set to the public address that can be accessed from remote Storage instances.

**--raft.compaction-overhead**="": CompactionOverhead defines the number of most recent entries to keep after each Raft log compaction.
Raft log compaction is performed automatically every time when a snapshot is created. (default: 5000)

**--raft.election-rtt**="": ElectionRTT is the minimum number of message RTT between elections. Message RTT is defined by NodeHostConfig.RTTMillisecond.
The Raft paper suggests it to be a magnitude greater than HeartbeatRTT, which is the interval between two heartbeats. In Raft, the actual interval between elections is randomized to be between ElectionRTT and 2 * ElectionRTT.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the election interval to be 1 second, then ElectionRTT should be set to 10.
When CheckQuorum is enabled, ElectionRTT also defines the interval for checking leader quorum. (default: 20)

**--raft.heartbeat-rtt**="": HeartbeatRTT is the number of message RTT between heartbeats. Message RTT is defined by NodeHostConfig.RTTMillisecond. The Raft paper suggest the heartbeat interval to be close to the average RTT between nodes.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the heartbeat interval to be every 200 milliseconds, then HeartbeatRTT should be set to 2. (default: 1)

**--raft.initial-members**="": Raft cluster initial members is an ordered list of raft addresses for the initial cluster nodes.
The position in the list (1-based) determines the replica ID. Each node derives its own replica ID
by finding its own raft.address in this list. All nodes must specify the same list in the same order.
Example for a 3-node cluster: "--raft.initial-members=127.0.0.1:5012,127.0.0.1:5013,127.0.0.1:5014".

**--raft.listen-address**="": ListenAddress is a hostname:port or IP:port address used by the Raft RPC module to listen on for Raft message and snapshots.
When the ListenAddress field is not set, The Raft RPC module listens on RaftAddress. If 0.0.0.0 is specified as the IP of the ListenAddress, Armada listens to the specified port on all interfaces.
When hostname or domain name is specified, it is locally resolved to IP addresses first and Armada listens to all resolved IP addresses.

**--raft.max-in-mem-log-size**="": MaxInMemLogSize is the target size in bytes allowed for storing in memory Raft logs on each Raft node.
In memory Raft logs are the ones that have not been applied yet. (default: 6291456)

**--raft.max-recv-queue-size**="": MaxReceiveQueueSize is the maximum size in bytes of each receive queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the queue size is unlimited. (default: 0)

**--raft.max-send-queue-size**="": MaxSendQueueSize is the maximum size in bytes of each send queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the send queue size is unlimited. (default: 0)

**--raft.node-host-dir**="": NodeHostDir raft internal storage (default: "/tmp/armada/raft")

**--raft.quic-udp-buffer-size**="": QUICUDPBufferSize is the UDP socket receive/send buffer size in bytes requested for the QUIC transport.
When set to a positive value the QUIC library's buffer requests are capped at this value, preventing log warnings on systems
where the kernel UDP buffer limit is lower than the library default (7 MiB). A value of 0 uses the library default. (default: 0)

**--raft.rtt**="": RTTMillisecond defines the average Round Trip Time (RTT) between two NodeHost instances.
Such a RTT interval is internally used as a logical clock tick, Raft heartbeat and election intervals are both defined in term of how many such RTT intervals.
Note that RTTMillisecond is the combined delays between two NodeHost instances including all delays caused by network transmission, delays caused by NodeHost queuing and processing. (default: 50ms)

**--raft.snapshot-entries**="": SnapshotEntries defines how often the state machine should be snapshot automatically.
It is defined in terms of the number of applied Raft log entries.
SnapshotEntries can be set to 0 to disable such automatic snapshotting. (default: 10000)

**--raft.snapshot-recovery-type**="": Specifies the way how the snapshots should be shared between nodes within the cluster. Options: snapshot, checkpoint, default: checkpoint for non Windows systems.
Type 'snapshot' uses in-memory snapshot of DB to send over wire to the peer. Type 'checkpoint'' uses hardlinks on FS a sends DB in tarball over wire. Checkpoint is thus much more memory and compute efficient at the potential expense of disk space, it is not advisable to use on OS/FS which does not support hardlinks.

**--raft.state-machine-dir**="": StateMachineDir persistent storage for the state machine. (default: "/tmp/armada/state-machine")

**--raft.tls-ca-file**="": Path to the CA certificate file used to verify raft peer certificates.

**--raft.tls-cert-file**="": Path to the TLS certificate file for mutual TLS between raft peers. Must be set together with raft.tls-key-file and raft.tls-ca-file.

**--raft.tls-key-file**="": Path to the TLS private key file for mutual TLS between raft peers.

**--raft.wal-dir**="": WALDir is the directory used for storing the WAL of Raft entries.
It is recommended to use low latency storage such as NVME SSD with power loss protection to store such WAL data.
Leave WALDir to have zero value will have everything stored in NodeHostDir.

**--replication.ca-filename**="": Path to the client CA cert file. The CA file is used to verify server authority. (default: "hack/replication/ca.crt")

**--replication.cert-filename**="": Path to the client certificate. (default: "hack/replication/client.crt")

**--replication.insecure-skip-verify**: InsecureSkipVerify controls whether a client verifies the server's certificate chain and host name. If InsecureSkipVerify is true, crypto/tls accepts any certificate presented by the server and any host name in that certificate.

**--replication.keepalive-time**="": After a duration of this time if the replication client doesn't see any activity it pings the server to see if the transport is still alive. If set below 10s, a minimum value of 10s will be used instead. (default: 1m0s)

**--replication.keepalive-timeout**="": After having pinged for keepalive check, the replication client waits for a duration of Timeout and if no activity is seen even after that the connection is closed. (default: 10s)

**--replication.key-filename**="": Path to the client private key file. (default: "hack/replication/client.key")

**--replication.leader-address**="": Address of the leader replication API to connect to. (default: "localhost:8444")

**--replication.lease-interval**="": Interval in which the workers re-new their table leases. (default: 15s)

**--replication.log-rpc-timeout**="": The log RPC timeout. (default: 1m0s)

**--replication.max-recovery-in-flight**="": The maximum number of recovery goroutines allowed to run in this instance. (default: 1)

**--replication.max-recv-message-size-bytes**="": The maximum size of single replication message allowed to receive. (default: 8388608)

**--replication.poll-interval**="": Replication interval in seconds, the leader poll time. (default: 1s)

**--replication.reconcile-interval**="": Replication interval of tables reconciliation (workers startup/shutdown). (default: 30s)

**--replication.server-name**="": ServerName ensures the cert matches the given host in case of discovery/virtual hosting.

**--replication.snapshot-rpc-timeout**="": The snapshot RPC timeout. (default: 1h0m0s)

**--replication.snapshot-source**="": Snapshot object source: auto, direct, or proxy. direct uses shared-store backend from follower config; proxy uses leader HTTP endpoint. (default: "auto")

**--replication.snapshot-timeout**="": Timeout for a single incremental snapshot export triggered by log compaction. (default: 10m0s)

**--rest.address**="": REST API server address. (default: "http://127.0.0.1:8079")

**--rest.read-timeout**="": Maximum duration for reading the entire request. (default: 5s)

**--shared-store.azure.account**="": Account name to use for the Azure shared-store backend.

**--shared-store.azure.container**="": Container name to use for the Azure shared-store backend.

**--shared-store.azure.key**="": Key to use for the Azure shared-store backend.

**--shared-store.backend**="": Blob store backend. Supported values: none (disabled), filesystem, s3, gcs, azblob. (default: "none")

**--shared-store.filesystem.directory**="": Directory path to use for the filesystem backend.

**--shared-store.gc-interval**="": How often the GC worker runs to delete expired artefacts from the shared store. (default: 1h0m0s)

**--shared-store.gcs.bucket**="": Bucket name to use for the GCS shared-store backend.

**--shared-store.retention**="": Maximum age of artefacts in the shared store. Older artefacts are eligible for GC. (default: 48h0m0s)

**--shared-store.s3.bucket**="": Bucket name to use for the S3 shared-store backend.

**--storage.block-cache-size**="": Shared block cache size in bytes, the cache is used to hold uncompressed blocks of data in memory. (default: 16777216)

**--storage.table-cache-size**="": Shared table cache size, the cache is used to hold handles to open SSTs. (default: 1024)

**--tables.enabled**: Whether tables API is enabled.

**--tables.token**="": Token to check for tables API access, if left empty (default) no token is checked.

### help, h

Shows a list of commands or help for one command

## version

Print current version.

**--help, -h**: show help

### help, h

Shows a list of commands or help for one command

## help, h

Shows a list of commands or help for one command

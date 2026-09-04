// Copyright Armada Contributors

package main

import (
	"time"

	"github.com/urfave/cli/v3"
)

var (
	rootFlags = []cli.Flag{
		&cli.StringFlag{Name: "config", Value: "", Usage: "Path to a configuration file (.yaml, .yml, .json or .toml). Values are overridden by environment variables and command-line flags."},
		&cli.BoolFlag{Name: "dev-mode", Value: false, Usage: "Development mode enabled (verbose logging, human-friendly log format)."},
		&cli.StringFlag{Name: "log-level", Value: "INFO", Usage: "Log level: DEBUG/INFO/WARN/ERROR."},
	}

	apiFlags = []cli.Flag{
		&cli.StringFlag{Name: "api.address", Value: "http://0.0.0.0:8443", Usage: "API server address. The address the server listens on."},
		&cli.StringFlag{Name: "api.advertise-address", Value: "http://127.0.0.1:8443", Usage: "Advertise API server address, used for NAT traversal."},
		&cli.StringFlag{Name: "api.cert-filename", Value: "", Usage: "Path to the API server certificate."},
		&cli.StringFlag{Name: "api.key-filename", Value: "", Usage: "Path to the API server private key file."},
		&cli.StringFlag{Name: "api.ca-filename", Value: "", Usage: "Path to the API server client auth CA file."},
		&cli.BoolFlag{Name: "api.client-cert-auth", Value: false, Usage: "API server client certificate auth enabled. If set to true the api.ca-filename should be provided as well."},
		&cli.StringFlag{Name: "api.allowed-cn", Value: "", Usage: "AllowedCN is a CN which must be provided by a client."},
		&cli.StringFlag{Name: "api.allowed-hostname", Value: "", Usage: "AllowedHostname is an IP address or hostname that must match the TLS certificate provided by a client."},
		&cli.UintFlag{Name: "api.max-concurrent-connections", Value: 0, Usage: "Maximum number of allowed concurrent client connections. Default of 0 means no limit."},
		&cli.UintFlag{Name: "api.max-concurrent-streams", Value: 0, Usage: "Maximum number of concurrent streams open. Default of 0 means no limit."},
		&cli.IntFlag{Name: "api.stream-workers", Value: 0, Usage: "Number of workers to use to process incoming streams. These workers are pre-started and should reduce an overhead of stack allocation as well as prevent potential overload of a storage layer. Default of 0 means number of CPUs + 1, any negative number will result in unlimited workers."},
	}

	restFlags = []cli.Flag{
		&cli.StringFlag{Name: "rest.address", Value: "http://127.0.0.1:8079", Usage: "REST API server address."},
		&cli.DurationFlag{Name: "rest.read-timeout", Value: time.Second * 5, Usage: "Maximum duration for reading the entire request."},
	}

	raftFlags = []cli.Flag{
		&cli.DurationFlag{Name: "raft.rtt", Value: 50 * time.Millisecond, Usage: `RTTMillisecond defines the average Round Trip Time (RTT) between two NodeHost instances.
Such a RTT interval is internally used as a logical clock tick, Raft heartbeat and election intervals are both defined in term of how many such RTT intervals.
Note that RTTMillisecond is the combined delays between two NodeHost instances including all delays caused by network transmission, delays caused by NodeHost queuing and processing.`},
		&cli.IntFlag{Name: "raft.heartbeat-rtt", Value: 1, Usage: `HeartbeatRTT is the number of message RTT between heartbeats. Message RTT is defined by NodeHostConfig.RTTMillisecond. The Raft paper suggest the heartbeat interval to be close to the average RTT between nodes.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the heartbeat interval to be every 200 milliseconds, then HeartbeatRTT should be set to 2.`},
		&cli.IntFlag{Name: "raft.election-rtt", Value: 20, Usage: `ElectionRTT is the minimum number of message RTT between elections. Message RTT is defined by NodeHostConfig.RTTMillisecond.
The Raft paper suggests it to be a magnitude greater than HeartbeatRTT, which is the interval between two heartbeats. In Raft, the actual interval between elections is randomized to be between ElectionRTT and 2 * ElectionRTT.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the election interval to be 1 second, then ElectionRTT should be set to 10.
When CheckQuorum is enabled, ElectionRTT also defines the interval for checking leader quorum.`},
		&cli.StringFlag{Name: "raft.wal-dir", Value: "", Usage: `WALDir is the directory used for storing the WAL of Raft entries.
It is recommended to use low latency storage such as NVME SSD with power loss protection to store such WAL data.
Leave WALDir to have zero value will have everything stored in NodeHostDir.`},
		&cli.StringFlag{Name: "raft.node-host-dir", Value: "/tmp/armada/raft", Usage: "NodeHostDir raft internal storage"},
		&cli.StringFlag{Name: "raft.state-machine-dir", Value: "/tmp/armada/state-machine", Usage: "StateMachineDir persistent storage for the state machine."},
		&cli.StringFlag{Name: "raft.snapshot-recovery-type", Value: "", Usage: `Specifies the way how the snapshots should be shared between nodes within the cluster. Options: snapshot, checkpoint, default: checkpoint for non Windows systems.
Type 'snapshot' uses in-memory snapshot of DB to send over wire to the peer. Type 'checkpoint'' uses hardlinks on FS a sends DB in tarball over wire. Checkpoint is thus much more memory and compute efficient at the potential expense of disk space, it is not advisable to use on OS/FS which does not support hardlinks.`},
		&cli.StringFlag{Name: "raft.address", Value: "", Usage: `RaftAddress is a hostname:port or IP:port address used by the Raft RPC module for exchanging Raft messages and snapshots.
This is also the identifier for a Storage instance. RaftAddress should be set to the public address that can be accessed from remote Storage instances.`},
		&cli.StringFlag{Name: "raft.listen-address", Value: "", Usage: `ListenAddress is a hostname:port or IP:port address used by the Raft RPC module to listen on for Raft message and snapshots.
When the ListenAddress field is not set, The Raft RPC module listens on RaftAddress. If 0.0.0.0 is specified as the IP of the ListenAddress, Armada listens to the specified port on all interfaces.
When hostname or domain name is specified, it is locally resolved to IP addresses first and Armada listens to all resolved IP addresses.`},
		&cli.StringSliceFlag{Name: "raft.initial-members", Value: []string{}, Usage: `Raft cluster initial members is an ordered list of raft addresses for the initial cluster nodes.
The position in the list (1-based) determines the replica ID. Each node derives its own replica ID
by finding its own raft.address in this list. All nodes must specify the same list in the same order.
Example for a 3-node cluster: "--raft.initial-members=127.0.0.1:5012,127.0.0.1:5013,127.0.0.1:5014".`},
		&cli.Uint64Flag{Name: "raft.snapshot-entries", Value: 10000, Usage: `SnapshotEntries defines how often the state machine should be snapshot automatically.
It is defined in terms of the number of applied Raft log entries.
SnapshotEntries can be set to 0 to disable such automatic snapshotting.`},
		&cli.Uint64Flag{Name: "raft.compaction-overhead", Value: 5000, Usage: `CompactionOverhead defines the number of most recent entries to keep after each Raft log compaction.
Raft log compaction is performed automatically every time when a snapshot is created.`},
		&cli.Uint64Flag{Name: "raft.max-in-mem-log-size", Value: 6 * 1024 * 1024, Usage: `MaxInMemLogSize is the target size in bytes allowed for storing in memory Raft logs on each Raft node.
In memory Raft logs are the ones that have not been applied yet.`},
		&cli.Uint64Flag{Name: "raft.max-recv-queue-size", Value: 0, Usage: `MaxReceiveQueueSize is the maximum size in bytes of each receive queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the queue size is unlimited.`},
		&cli.Uint64Flag{Name: "raft.max-send-queue-size", Value: 0, Usage: `MaxSendQueueSize is the maximum size in bytes of each send queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the send queue size is unlimited.`},
		&cli.IntFlag{Name: "raft.quic-udp-buffer-size", Value: 0, Usage: `QUICUDPBufferSize is the UDP socket receive/send buffer size in bytes requested for the QUIC transport.
When set to a positive value the QUIC library's buffer requests are capped at this value, preventing log warnings on systems
where the kernel UDP buffer limit is lower than the library default (7 MiB). A value of 0 uses the library default.`},
		&cli.StringFlag{Name: "raft.tls-cert-file", Value: "", Usage: "Path to the TLS certificate file for mutual TLS between raft peers. Must be set together with raft.tls-key-file and raft.tls-ca-file."},
		&cli.StringFlag{Name: "raft.tls-key-file", Value: "", Usage: "Path to the TLS private key file for mutual TLS between raft peers."},
		&cli.StringFlag{Name: "raft.tls-ca-file", Value: "", Usage: "Path to the CA certificate file used to verify raft peer certificates."},
	}

	memberlistFlags = []cli.Flag{
		&cli.StringFlag{Name: "memberlist.advertise-address", Value: "", Usage: `AdvertiseAddress is the address to advertise to other Armada instances used for NAT traversal.
Gossip services running on remote Armada instances will use AdvertiseAddress to exchange gossip service related messages. AdvertiseAddress is in the format of IP:Port, Hostname:Port or DNS Name:Port.
When not set, the raft.address value is used (gossip shares the raft UDP port).`},
		&cli.StringSliceFlag{Name: "memberlist.members", Value: []string{}, Usage: `Seed is a list of addresses of remote Armada instances to bootstrap the gossip service.
Each address should be the raft address (IP:Port) of a peer — gossip shares the same UDP port as raft via ALPN multiplexing.
When not set, the raft.initial-members addresses are used automatically as gossip seeds.`},
		&cli.StringFlag{Name: "memberlist.cluster-name", Value: "default", Usage: `Cluster name, propagated in Memberlist API responses as well as used as used as a label when forming the gossip cluster.
All nodes of the cluster MUST set this to the same value. If changing it is advisable to turn off all the nodes and then startup with the new value.`},
		&cli.StringFlag{Name: "memberlist.node-name", Value: "", Usage: "Node name override, MUST be unique in a cluster, if not specified random stable UUID will be used instead."},
		&cli.StringFlag{Name: "memberlist.tls-cert-file", Value: "", Usage: "Path to the TLS certificate file for mutual TLS between gossip peers."},
		&cli.StringFlag{Name: "memberlist.tls-key-file", Value: "", Usage: "Path to the TLS private key file for mutual TLS between gossip peers."},
		&cli.StringFlag{Name: "memberlist.tls-ca-file", Value: "", Usage: "Path to the CA certificate file used to verify gossip peer certificates."},
	}

	storageFlags = []cli.Flag{
		&cli.Int64Flag{Name: "storage.block-cache-size", Value: 16 * 1024 * 1024, Usage: "Shared block cache size in bytes, the cache is used to hold uncompressed blocks of data in memory."},
		&cli.IntFlag{Name: "storage.table-cache-size", Value: 1024, Usage: "Shared table cache size, the cache is used to hold handles to open SSTs."},
	}

	maintenanceFlags = []cli.Flag{
		&cli.BoolFlag{Name: "maintenance.enabled", Value: true, Usage: "Whether maintenance API is enabled."},
		&cli.StringFlag{Name: "maintenance.token", Value: "", Usage: "Token to check for maintenance API access, if left empty (default) no token is checked."},
	}

	tablesFlags = []cli.Flag{
		&cli.BoolFlag{Name: "tables.enabled", Value: true, Usage: "Whether tables API is enabled."},
		&cli.StringFlag{Name: "tables.token", Value: "", Usage: "Token to check for tables API access, if left empty (default) no token is checked."},
	}

	experimentalFlags = []cli.Flag{}

	sharedStoreFlags = []cli.Flag{
		&cli.StringFlag{Name: "shared-store.backend", Value: "none", Usage: `Blob store backend. Supported values: none (disabled), filesystem, s3, gcs, azblob.`},
		&cli.StringFlag{Name: "shared-store.filesystem.directory", Value: "", Usage: "Directory path to use for the filesystem backend."},
		&cli.StringFlag{Name: "shared-store.s3.bucket", Value: "", Usage: "Bucket name to use for the S3 shared-store backend."},
		&cli.StringFlag{Name: "shared-store.gcs.bucket", Value: "", Usage: "Bucket name to use for the GCS shared-store backend."},
		&cli.StringFlag{Name: "shared-store.azure.container", Value: "", Usage: "Container name to use for the Azure shared-store backend."},
		&cli.StringFlag{Name: "shared-store.azure.account", Value: "", Usage: "Account name to use for the Azure shared-store backend."},
		&cli.StringFlag{Name: "shared-store.azure.key", Value: "", Usage: "Key to use for the Azure shared-store backend."},
		&cli.DurationFlag{Name: "shared-store.retention", Value: 48 * time.Hour, Usage: "Maximum age of artefacts in the shared store. Older artefacts are eligible for GC."},
		&cli.DurationFlag{Name: "shared-store.gc-interval", Value: time.Hour, Usage: "How often the GC worker runs to delete expired artefacts from the shared store."},
		&cli.DurationFlag{Name: "replication.snapshot-timeout", Value: 10 * time.Minute, Usage: "Timeout for a single incremental snapshot export triggered by log compaction."},
	}
)

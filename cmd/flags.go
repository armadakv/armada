// Copyright JAMF Software, LLC

package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	rootFlagSet         = pflag.NewFlagSet("root", pflag.ContinueOnError)
	apiFlagSet          = pflag.NewFlagSet("api", pflag.ContinueOnError)
	restFlagSet         = pflag.NewFlagSet("rest", pflag.ContinueOnError)
	raftFlagSet         = pflag.NewFlagSet("raft", pflag.ContinueOnError)
	memberlistFlagSet   = pflag.NewFlagSet("memberlist", pflag.ContinueOnError)
	storageFlagSet      = pflag.NewFlagSet("storage", pflag.ContinueOnError)
	maintenanceFlagSet  = pflag.NewFlagSet("maintenance", pflag.ContinueOnError)
	tablesFlagSet       = pflag.NewFlagSet("tables", pflag.ContinueOnError)
	experimentalFlagSet = pflag.NewFlagSet("experimental", pflag.ContinueOnError)
)

func init() {
	// Root flags
	rootFlagSet.Bool("dev-mode", false, "Development mode enabled (verbose logging, human-friendly log format).")
	rootFlagSet.String("log-level", "INFO", "Log level: DEBUG/INFO/WARN/ERROR.")

	// API flags
	apiFlagSet.String("api.address", "http://0.0.0.0:8443", "API server address. The address the server listens on.")
	apiFlagSet.String("api.advertise-address", "http://127.0.0.1:8443", "Advertise API server address, used for NAT traversal.")
	apiFlagSet.String("api.cert-filename", "", "Path to the API server certificate.")
	apiFlagSet.String("api.key-filename", "", "Path to the API server private key file.")
	apiFlagSet.String("api.ca-filename", "", "Path to the API server client auth CA file.")
	apiFlagSet.Bool("api.client-cert-auth", false, "API server client certificate auth enabled. If set to true the api.ca-filename should be provided as well.")
	apiFlagSet.String("api.allowed-cn", "", "AllowedCN is a CN which must be provided by a client.")
	apiFlagSet.String("api.allowed-hostname", "", "AllowedHostname is an IP address or hostname that must match the TLS certificate provided by a client.")
	apiFlagSet.Uint32("api.max-concurrent-connections", 0, "Maximum number of allowed concurrent client connections. Default of 0 means no limit.")
	apiFlagSet.Uint32("api.max-concurrent-streams", 0, "Maximum number of concurrent streams open. Default of 0 means no limit.")
	apiFlagSet.Int("api.stream-workers", 0, "Number of workers to use to process incoming streams. These workers are pre-started and should reduce an overhead of stack allocation as well as prevent potential overload of a storage layer. Default of 0 means number of CPUs + 1, any negative number will result in unlimited workers.")

	// REST API flags
	restFlagSet.String("rest.address", "http://127.0.0.1:8079", "REST API server address.")
	restFlagSet.Duration("rest.read-timeout", time.Second*5, "Maximum duration for reading the entire request.")

	// Raft flags
	raftFlagSet.Duration("raft.rtt", 50*time.Millisecond,
		`RTTMillisecond defines the average Round Trip Time (RTT) between two NodeHost instances.
Such a RTT interval is internally used as a logical clock tick, Raft heartbeat and election intervals are both defined in term of how many such RTT intervals.
Note that RTTMillisecond is the combined delays between two NodeHost instances including all delays caused by network transmission, delays caused by NodeHost queuing and processing.`)
	raftFlagSet.Int("raft.heartbeat-rtt", 1,
		`HeartbeatRTT is the number of message RTT between heartbeats. Message RTT is defined by NodeHostConfig.RTTMillisecond. The Raft paper suggest the heartbeat interval to be close to the average RTT between nodes.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the heartbeat interval to be every 200 milliseconds, then HeartbeatRTT should be set to 2.`)
	raftFlagSet.Int("raft.election-rtt", 20,
		`ElectionRTT is the minimum number of message RTT between elections. Message RTT is defined by NodeHostConfig.RTTMillisecond. 
The Raft paper suggests it to be a magnitude greater than HeartbeatRTT, which is the interval between two heartbeats. In Raft, the actual interval between elections is randomized to be between ElectionRTT and 2 * ElectionRTT.
As an example, assuming NodeHostConfig.RTTMillisecond is 100 millisecond, to set the election interval to be 1 second, then ElectionRTT should be set to 10.
When CheckQuorum is enabled, ElectionRTT also defines the interval for checking leader quorum.`)
	raftFlagSet.String("raft.wal-dir", "",
		`WALDir is the directory used for storing the WAL of Raft entries. 
It is recommended to use low latency storage such as NVME SSD with power loss protection to store such WAL data. 
Leave WALDir to have zero value will have everything stored in NodeHostDir.`)
	raftFlagSet.String("raft.node-host-dir", "/tmp/armada/raft", "NodeHostDir raft internal storage")
	raftFlagSet.String("raft.state-machine-dir", "/tmp/armada/state-machine",
		"StateMachineDir persistent storage for the state machine.")
	raftFlagSet.String("raft.snapshot-recovery-type", "",
		`Specifies the way how the snapshots should be shared between nodes within the cluster. Options: snapshot, checkpoint, default: checkpoint for non Windows systems. 
Type 'snapshot' uses in-memory snapshot of DB to send over wire to the peer. Type 'checkpoint'' uses hardlinks on FS a sends DB in tarball over wire. Checkpoint is thus much more memory and compute efficient at the potential expense of disk space, it is not advisable to use on OS/FS which does not support hardlinks.`)
	raftFlagSet.String("raft.address", "",
		`RaftAddress is a hostname:port or IP:port address used by the Raft RPC module for exchanging Raft messages and snapshots.
This is also the identifier for a Storage instance. RaftAddress should be set to the public address that can be accessed from remote Storage instances.`)
	raftFlagSet.String("raft.listen-address", "",
		`ListenAddress is a hostname:port or IP:port address used by the Raft RPC module to listen on for Raft message and snapshots.
When the ListenAddress field is not set, The Raft RPC module listens on RaftAddress. If 0.0.0.0 is specified as the IP of the ListenAddress, Armada listens to the specified port on all interfaces.
When hostname or domain name is specified, it is locally resolved to IP addresses first and Armada listens to all resolved IP addresses.`)
	raftFlagSet.Uint64("raft.node-id", 1, "Raft Node ID is a non-zero value used to identify a node within a Raft cluster.")
	raftFlagSet.StringToString("raft.initial-members", map[string]string{}, `Raft cluster initial members defines a mapping of node IDs to their respective raft address.
The node ID must be must be Integer >= 1. Example for the initial 3 node cluster setup on the localhost: "--raft.initial-members=1=127.0.0.1:5012,2=127.0.0.1:5013,3=127.0.0.1:5014".`)
	raftFlagSet.Uint64("raft.snapshot-entries", 10000,
		`SnapshotEntries defines how often the state machine should be snapshot automatically.
It is defined in terms of the number of applied Raft log entries.
SnapshotEntries can be set to 0 to disable such automatic snapshotting.`)
	raftFlagSet.Uint64("raft.compaction-overhead", 5000,
		`CompactionOverhead defines the number of most recent entries to keep after each Raft log compaction.
Raft log compaction is performed automatically every time when a snapshot is created.`)
	raftFlagSet.Uint64("raft.max-in-mem-log-size", 6*1024*1024,
		`MaxInMemLogSize is the target size in bytes allowed for storing in memory Raft logs on each Raft node.
In memory Raft logs are the ones that have not been applied yet.`)
	raftFlagSet.Uint64("raft.max-recv-queue-size", 0,
		`MaxReceiveQueueSize is the maximum size in bytes of each receive queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the queue size is unlimited.`)
	raftFlagSet.Uint64("raft.max-send-queue-size", 0,
		`MaxSendQueueSize is the maximum size in bytes of each send queue. Once the maximum size is reached, further replication messages will be
dropped to restrict memory usage. When set to 0, it means the send queue size is unlimited.`)
	memberlistFlagSet.String("memberlist.address", "0.0.0.0:7432", `Address is the address for the gossip service to bind to and listen on. Both UDP and TCP ports are used by the gossip service.
The local gossip service should be able to receive gossip service related messages by binding to and listening on this address. BindAddress is usually in the format of IP:Port, Hostname:Port or DNS Name:Port.`)
	memberlistFlagSet.String("memberlist.advertise-address", "", `AdvertiseAddress is the address to advertise to other Armada instances used for NAT traversal.
Gossip services running on remote Armada instances will use AdvertiseAddress to exchange gossip service related messages. AdvertiseAddress is in the format of IP:Port, Hostname:Port or DNS Name:Port.`)
	memberlistFlagSet.StringSlice("memberlist.members", []string{""}, `Seed is a list of AdvertiseAddress of remote Armada instances. Local Armada instance will try to contact all of them to bootstrap the gossip service. 
At least one reachable Armada instance is required to successfully bootstrap the gossip service. Each seed address is in the format of IP:Port, Hostname:Port or DNS Name:Port.`)
	memberlistFlagSet.String("memberlist.cluster-name", "default", `Cluster name, propagated in Memberlist API responses as well as used as used as a label when forming the gossip cluster.
All nodes of the cluster MUST set this to the same value. If changing it is advisable to turn off all the nodes and then startup with the new value.`)
	memberlistFlagSet.String("memberlist.node-name", "", "Node name override, MUST be unique in a cluster, if not specified random stable UUID will be used instead.")

	// Storage flags
	storageFlagSet.Int64("storage.block-cache-size", 16*1024*1024, "Shared block cache size in bytes, the cache is used to hold uncompressed blocks of data in memory.")
	storageFlagSet.Int("storage.table-cache-size", 1024, "Shared table cache size, the cache is used to hold handles to open SSTs.")

	// Maintenance flags
	maintenanceFlagSet.Bool("maintenance.enabled", true, "Whether maintenance API is enabled.")
	maintenanceFlagSet.String("maintenance.token", "", "Token to check for maintenance API access, if left empty (default) no token is checked.")

	// Tables flags
	tablesFlagSet.Bool("tables.enabled", true, "Whether tables API is enabled.")
	tablesFlagSet.String("tables.token", "", "Token to check for tables API access, if left empty (default) no token is checked.")
}

func initConfig(set *pflag.FlagSet) {
	viper.SetConfigName("config")
	viper.AddConfigPath("/etc/armada/")
	viper.AddConfigPath("/config")
	viper.AddConfigPath("$HOME/.armada")
	viper.AddConfigPath(".")
	viper.AutomaticEnv()

	err := viper.BindPFlags(set)
	if err != nil {
		panic(fmt.Errorf("error binding pflags %v", err))
	}

	err = viper.ReadInConfig()
	if err != nil && !errors.As(err, &viper.ConfigFileNotFoundError{}) {
		panic(fmt.Errorf("error reading config %v", err))
	}
}

// Copyright JAMF Software, LLC

package cluster

import (
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/util"
	"github.com/stretchr/testify/require"
)

func TestSingleNodeCluster(t *testing.T) {
	address := getTestBindAddress()
	cluster, err := New(address, "", "", nil, nil, nil, func() Info { return Info{} }, func() NodeMeta { return NodeMeta{} })
	require.NoError(t, err)
	t.Cleanup(func() { _ = cluster.Close() })
	cluster.Start([]string{address})
	require.Len(t, cluster.Nodes(), 1)
}

func TestMultiNodeCluster(t *testing.T) {
	clusters := make(map[string]*Cluster)
	t.Log("start 3 node cluster")
	for i := range 3 {
		address := getTestBindAddress()
		nodeHostID := util.RandString(64)
		nodeID := uint64(i)
		raftAddr := fmt.Sprintf("127.0.0.%d:5762", i)
		cluster, err := New(address, "", strconv.Itoa(i), nil, nil, nil, func() Info {
			return Info{
				NodeHostID:  nodeHostID,
				NodeID:      nodeID,
				RaftAddress: raftAddr,
				ShardInfoList: []raft.ShardInfo{
					{
						Replicas:          map[uint64]string{1: "127.0.0.1:5762", 2: "127.0.0.2:5762", 3: "127.0.0.3:5762"},
						ShardID:           1,
						ReplicaID:         1,
						ConfigChangeIndex: 1,
						LeaderID:          1,
						Term:              5,
					},
					{
						Replicas:          map[uint64]string{1: "127.0.0.1:5762", 2: "127.0.0.2:5762", 3: "127.0.0.3:5762"},
						ShardID:           2,
						ReplicaID:         1,
						ConfigChangeIndex: 1,
						LeaderID:          1,
						Term:              5,
					},
				},
			}
		}, func() NodeMeta {
			return NodeMeta{
				ID:          nodeHostID,
				NodeID:      nodeID,
				RaftAddress: raftAddr,
			}
		})
		require.NoError(t, err)
		clusters[address] = cluster
		cluster.Start(keys(clusters))
		require.Eventually(t, func() bool {
			return len(cluster.Nodes()) == i+1
		}, 30*time.Second, 50*time.Millisecond)
	}
	t.Cleanup(func() {
		for _, cluster := range clusters {
			_ = cluster.Close()
		}
	})

	t.Log("all members see the others and has the same view of the world")
	require.Eventually(t, func() bool {
		for _, cluster1 := range clusters {
			nodes := cluster1.Nodes()
			if len(nodes) != len(clusters) {
				return false
			}
			for _, cluster2 := range clusters {
				if len(cluster2.Nodes()) != len(nodes) {
					return false
				}
			}
			if cluster1.ShardInfo(1).Term != 5 {
				return false
			}
		}
		return true
	}, 30*time.Second, 50*time.Millisecond)
	for _, cluster1 := range clusters {
		nodes := cluster1.Nodes()
		for _, cluster2 := range clusters {
			require.ElementsMatch(t, nodes, cluster2.Nodes())
		}
		require.Equal(t, raft.ShardView{
			ShardID:           1,
			Replicas:          map[uint64]string{1: "127.0.0.1:5762", 2: "127.0.0.2:5762", 3: "127.0.0.3:5762"},
			ConfigChangeIndex: 1,
			LeaderID:          1,
			Term:              5,
		}, cluster1.ShardInfo(1))
	}
	c1 := values(clusters)[0]
	c2 := values(clusters)[1]

	t.Log("test prefix watch")
	prefixChan := make(chan Message, 16)
	c2.WatchPrefix("test-", func(message Message) {
		prefixChan <- message
	})
	require.NoError(t, c1.SendTo(c2.LocalNode(), Message{Key: "test-foo", Payload: nil}))
	select {
	case m := <-prefixChan:
		require.True(t, strings.HasPrefix(m.Key, "test-"))
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for prefix message")
	}

	t.Log("test key watch")
	keyChan := make(chan Message, 16)
	c2.WatchKey("specific-key", func(message Message) {
		keyChan <- message
	})
	require.NoError(t, c1.SendTo(c2.LocalNode(), Message{Key: "specific-key", Payload: nil}))
	select {
	case m := <-keyChan:
		require.Equal(t, "specific-key", m.Key)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for key message")
	}

	t.Log("test broadcast")
	// count distinct receivers: gossip retransmits and never echoes to the sender
	var received sync.Map
	for addr, cluster := range clusters {
		if cluster == c1 {
			continue
		}
		cluster.WatchKey("broadcast", func(message Message) {
			received.Store(addr, struct{}{})
		})
	}
	// re-send on every poll: delivery is not guaranteed before gossip converges
	require.Eventually(t, func() bool {
		c1.Broadcast(Message{Key: "broadcast"})
		n := 0
		received.Range(func(_, _ any) bool { n++; return true })
		return n >= len(clusters)-1
	}, 60*time.Second, 250*time.Millisecond)

	t.Log("shutdown all members")
	for addr, cluster := range clusters {
		require.NoError(t, cluster.Close())
		delete(clusters, addr)
		// otherwise the last member may leave while still seeing the departed one as alive, leaving no gossip target for its leave message
		require.Eventually(t, func() bool {
			for _, remaining := range clusters {
				if len(remaining.Nodes()) != len(clusters) {
					return false
				}
			}
			return true
		}, 30*time.Second, 10*time.Millisecond)
	}
}

func getTestBindAddress() string {
	return fmt.Sprintf("127.0.0.1:%d", getTestPort())
}

var (
	reservedPortsMu sync.Mutex
	reservedPorts   = map[int]struct{}{}
)

// getTestPort returns a port free on TCP and UDP, below the OS ephemeral range and never handed out twice in this process.
func getTestPort() int {
	reservedPortsMu.Lock()
	defer reservedPortsMu.Unlock()
	for range 1000 {
		port := 10000 + rand.IntN(16000) // avoid raft test ports 26001-26003
		if _, ok := reservedPorts[port]; ok {
			continue
		}
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		tcp, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		udp, err := net.ListenPacket("udp", addr)
		_ = tcp.Close()
		if err != nil {
			continue
		}
		_ = udp.Close()
		reservedPorts[port] = struct{}{}
		return port
	}
	panic("unable to find a free test port")
}

func keys(m map[string]*Cluster) []string {
	var ret []string
	for key := range m {
		ret = append(ret, key)
	}
	return ret
}

func values(m map[string]*Cluster) []*Cluster {
	var ret []*Cluster
	for _, val := range m {
		ret = append(ret, val)
	}
	return ret
}

func TestCluster_INodeRegistry(t *testing.T) {
	address := getTestBindAddress()
	c, err := New(address, "", "", nil, nil, nil, func() Info { return Info{} }, func() NodeMeta { return NodeMeta{} })
	require.NoError(t, err)

	t.Run("Resolve unknown returns error", func(t *testing.T) {
		_, _, err := c.Resolve(1, 1)
		require.ErrorIs(t, err, ErrUnknownTarget)
	})

	t.Run("Add then Resolve local cache", func(t *testing.T) {
		c.Add(1, 1, "127.0.0.1:5000")
		addr, key, err := c.Resolve(1, 1)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:5000", addr)
		require.NotEmpty(t, key)
	})

	t.Run("Remove clears local cache entry", func(t *testing.T) {
		c.Add(2, 1, "127.0.0.1:5001")
		c.Remove(2, 1)
		_, _, err := c.Resolve(2, 1)
		require.ErrorIs(t, err, ErrUnknownTarget)
	})

	t.Run("RemoveShard clears all entries for shard", func(t *testing.T) {
		c.Add(3, 1, "127.0.0.1:5002")
		c.Add(3, 2, "127.0.0.1:5003")
		c.Add(3, 3, "127.0.0.1:5004")
		c.RemoveShard(3)
		for _, rid := range []uint64{1, 2, 3} {
			_, _, err := c.Resolve(3, rid)
			require.ErrorIs(t, err, ErrUnknownTarget)
		}
	})

	t.Run("Resolve falls back to gossip shard view", func(t *testing.T) {
		c.shardView.update([]raft.ShardView{{
			ShardID:           10,
			Replicas:          map[uint64]string{5: "127.0.0.1:5010"},
			ConfigChangeIndex: 1,
		}})
		addr, key, err := c.Resolve(10, 5)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:5010", addr)
		require.NotEmpty(t, key)
	})

	t.Run("Local cache takes precedence over gossip view", func(t *testing.T) {
		c.shardView.update([]raft.ShardView{{
			ShardID:           20,
			Replicas:          map[uint64]string{1: "gossip-addr:6000"},
			ConfigChangeIndex: 1,
		}})
		c.Add(20, 1, "local-addr:7000")
		addr, _, err := c.Resolve(20, 1)
		require.NoError(t, err)
		require.Equal(t, "local-addr:7000", addr)
	})
}

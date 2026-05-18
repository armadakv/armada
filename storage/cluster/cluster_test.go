// Copyright JAMF Software, LLC

package cluster

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/util"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

func TestSingleNodeCluster(t *testing.T) {
	address := getTestBindAddress()
	cluster, err := New(address, "", "", nil, nil, nil, func() Info { return Info{} }, func() NodeMeta { return NodeMeta{} })
	require.NoError(t, err)
	cluster.Start([]string{address})
	require.Len(t, cluster.Nodes(), 1)
}

func TestMultiNodeCluster(t *testing.T) {
	clusters := make(map[string]*Cluster)
	t.Log("start 3 node cluster")
	for i := 0; i < 3; i++ {
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
		require.Len(t, cluster.Nodes(), i+1)
	}

	t.Log("all members see the others and has the same view of the world")
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
	recvChan := make(chan Message)
	c2.WatchPrefix("test-", func(message Message) {
		recvChan <- message
	})
	require.NoError(t, c1.SendTo(c2.LocalNode(), Message{Key: "test-foo", Payload: nil}))
	require.Eventually(t, func() bool {
		m := <-recvChan
		return strings.HasPrefix(m.Key, "test-")
	}, 5*time.Second, 100*time.Millisecond)

	t.Log("test key watch")
	recvChan = make(chan Message)
	c2.WatchKey("specific-key", func(message Message) {
		recvChan <- message
	})
	require.NoError(t, c1.SendTo(c2.LocalNode(), Message{Key: "specific-key", Payload: nil}))
	require.Eventually(t, func() bool {
		m := <-recvChan
		return m.Key == "specific-key"
	}, 5*time.Second, 100*time.Millisecond)

	t.Log("test broadcast")
	count := atomic.NewUint32(0)
	for _, cluster := range clusters {
		cluster.WatchKey("broadcast", func(message Message) {
			count.Add(1)
		})
	}
	// Wait for cluster stabilisation before broadcasting.
	time.Sleep(5 * time.Second)
	c1.Broadcast(Message{Key: "broadcast"})
	require.Eventually(t, func() bool {
		return int(count.Load()) >= len(clusters)
	}, 10*time.Second, 100*time.Millisecond)

	t.Log("shutdown all members")
	for _, cluster := range clusters {
		require.NoError(t, cluster.Close())
	}
}

func getTestBindAddress() string {
	conn, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer conn.Close()
	return conn.LocalAddr().String()
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

// Copyright JAMF Software, LLC

package armadaserver

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/storage"
	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/armada/vfs"
	pvfs "github.com/cockroachdb/pebble/v2/vfs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

type MockTableService struct {
	tables []table.Table
	error  error
}

func (t MockTableService) CreateTable(name string) error {
	return nil
}

func (t MockTableService) DeleteTable(name string) error {
	return nil
}

func (t MockTableService) GetTables() ([]table.Table, error) {
	return t.tables, t.error
}

func (t MockTableService) GetTable(name string) (table.ActiveTable, error) {
	return t.tables[0].AsActive(nil), t.error
}

func (t MockTableService) Restore(ctx context.Context, name string, reader io.Reader) error {
	return t.error
}

func (t MockTableService) RestoreLegacy(ctx context.Context, name string, reader io.Reader) error {
	return t.error
}

// testAddr reserves a loopback address for the engine's shared QUIC socket.
//
// The probe has to be UDP. The engine binds its transport with ListenPacket, so
// a port being free for TCP says nothing about the same port being free for
// UDP, and the old tcp4 probe happily handed back ports that were already taken
// for UDP.
func testAddr(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer c.Close()
	return c.LocalAddr().String()
}

func newInMemTestEngine(t *testing.T, tables ...string) *storage.Engine {
	// Closing the probe socket before the engine binds leaves a window in which
	// another test can take the port, so retry with a fresh one instead of
	// failing. Only bind conflicts are retried; anything else fails outright.
	var e *storage.Engine
	for attempt := 1; ; attempt++ {
		raftAddr := testAddr(t)
		var err error
		e, err = storage.New(storage.Config{
			ClientAddress:     raftAddr,
			NodeID:            1,
			InitialMembers:    map[uint64]string{1: raftAddr},
			NodeHostDir:       "/nh",
			RTTMillisecond:    10,
			RaftAddress:       raftAddr,
			EnableMetrics:     false,
			QUICUDPBufferSize: 4 * 1024 * 1024, // 4 MiB — fits within most CI kernel limits
			Gossip: storage.GossipConfig{
				ClusterName:    uuid.New().String(),
				InitialMembers: []string{raftAddr},
			},
			Table: storage.TableConfig{
				ElectionRTT:        10,
				HeartbeatRTT:       1,
				SnapshotEntries:    10,
				CompactionOverhead: 5,
				MaxInMemLogSize:    1024,
				FS:                 pvfs.NewMem(),
				DataDir:            "/data",
				BlockCacheSize:     1024,
				TableCacheSize:     64,
				RecoveryType:       table.RecoveryTypeCheckpoint,
			},
			Meta: storage.MetaConfig{
				ElectionRTT:        10,
				HeartbeatRTT:       1,
				SnapshotEntries:    10,
				CompactionOverhead: 5,
				MaxInMemLogSize:    1024,
			},
			FS:  vfs.NewMem(),
			Log: zaptest.NewLogger(t).Sugar(),
		})
		if err == nil {
			break
		}
		if attempt == 5 || !strings.Contains(err.Error(), "address already in use") {
			require.NoError(t, err)
		}
		t.Logf("engine bind on %s raced another listener, retrying: %v", raftAddr, err)
	}
	require.NoError(t, e.Start())
	require.NoError(t, e.WaitUntilReady(context.Background()))
	for _, tableName := range tables {
		at, err := e.CreateTable(tableName)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			_, _, ok, _ := e.GetLeaderID(at.ClusterID)
			return ok
		}, 10*time.Second, 5*time.Millisecond, "table did not start in time")
	}
	t.Cleanup(func() {
		_ = e.Close()
	})
	return e
}

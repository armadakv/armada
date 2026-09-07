// Copyright JAMF Software, LLC

package backup

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/armadaserver"
	gvfs "github.com/armadakv/armada/pebble"
	"github.com/armadakv/armada/storage"
	lvfs "github.com/armadakv/armada/vfs"
	"github.com/benbjohnson/clock"
	pvfs "github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestBackup_Backup(t *testing.T) {
	type fields struct {
		Timeout time.Duration
	}
	tests := []struct {
		name      string
		fields    fields
		tableData map[string][]*armadapb.PutRequest
		want      Manifest
		wantErr   error
	}{
		{
			name: "No tables backup",
			want: Manifest{
				Format:   backupFormatV2,
				Started:  time.Unix(0, 0),
				Finished: time.Unix(0, 0),
				Tables:   nil,
			},
		},
		{
			name: "Single empty table backup",
			tableData: map[string][]*armadapb.PutRequest{
				"regatta-test": nil,
			},
			want: Manifest{
				Format:   backupFormatV2,
				Started:  time.Unix(0, 0),
				Finished: time.Unix(0, 0),
				Tables: []ManifestTable{
					{
						Name:     "regatta-test",
						FileName: "regatta-test.bak",
					},
				},
			},
		},
		{
			name: "Multiple empty table backup",
			tableData: map[string][]*armadapb.PutRequest{
				"regatta-test":  nil,
				"regatta-test2": nil,
			},
			want: Manifest{
				Format:   backupFormatV2,
				Started:  time.Unix(0, 0),
				Finished: time.Unix(0, 0),
				Tables: []ManifestTable{
					{
						Name:     "regatta-test",
						FileName: "regatta-test.bak",
					},
					{
						Name:     "regatta-test2",
						FileName: "regatta-test2.bak",
					},
				},
			},
		},
		{
			name: "Multiple table backup",
			tableData: map[string][]*armadapb.PutRequest{
				"regatta-test": {
					&armadapb.PutRequest{
						Key:   []byte("foo"),
						Value: []byte("bar"),
					},
				},
				"regatta-test2": {
					&armadapb.PutRequest{
						Key:   []byte("foo2"),
						Value: []byte("bar2"),
					},
				},
			},
			want: Manifest{
				Format:   backupFormatV2,
				Started:  time.Unix(0, 0),
				Finished: time.Unix(0, 0),
				Tables: []ManifestTable{
					{
						Name:     "regatta-test",
						FileName: "regatta-test.bak",
					},
					{
						Name:     "regatta-test2",
						FileName: "regatta-test2.bak",
					},
				},
			},
		},
		{
			name: "Multiple table backup timeout",
			fields: fields{
				Timeout: 1 * time.Microsecond,
			},
			tableData: map[string][]*armadapb.PutRequest{
				"regatta-test": {
					&armadapb.PutRequest{
						Key:   []byte("foo"),
						Value: []byte("bar"),
					},
				},
				"regatta-test2": {
					&armadapb.PutRequest{
						Key:   []byte("foo2"),
						Value: []byte("bar2"),
					},
				},
			},
			wantErr: status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error()),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)

			e := newTestEngine(t)
			defer e.Close()

			for name, data := range tt.tableData {
				_, err := e.CreateTable(name)
				r.NoError(err)
				time.Sleep(1 * time.Second)
				for _, req := range data {
					tbl, err := e.GetTable(name)
					r.NoError(err)
					func() {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						_, err := tbl.Put(ctx, req)
						r.NoError(err)
					}()
				}
			}

			srv := startBackupServer(e)
			conn, err := grpc.NewClient(srv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			r.NoError(err)

			path := filepath.Join(t.TempDir(), strings.ReplaceAll(tt.name, " ", "_"))
			r.NoError(os.MkdirAll(path, 0o777))
			b := &Backup{
				Conn:    conn,
				Dir:     path,
				Timeout: tt.fields.Timeout,
				clock:   clock.NewMock(),
			}
			got, err := b.Backup()
			if tt.wantErr != nil {
				r.ErrorIs(err, tt.wantErr)
				return
			}
			r.NoError(err)
			assertManifest(t, path, tt.want, got)
		})
	}
}

func assertManifest(t *testing.T, dir string, want, got Manifest) {
	t.Helper()
	r := require.New(t)

	r.Equal(want.Format, got.Format)
	r.Equal(want.Started, got.Started)
	r.Equal(want.Finished, got.Finished)
	r.Len(got.Tables, len(want.Tables))

	for i, gotTable := range got.Tables {
		wantTable := want.Tables[i]
		r.Equal(wantTable.Name, gotTable.Name)
		r.Equal(wantTable.FileName, gotTable.FileName)

		contents, err := os.ReadFile(filepath.Join(dir, gotTable.FileName))
		r.NoError(err)
		sum := md5.Sum(contents)
		r.Equal(hex.EncodeToString(sum[:]), gotTable.MD5)
	}
}

func TestBackup_Restore(t *testing.T) {
	type fields struct {
		Timeout time.Duration
		Dir     string
	}
	tests := []struct {
		name       string
		fields     fields
		wantErrStr string
	}{
		{
			name: "Restore backup",
			fields: fields{
				Dir: "testdata/backup",
			},
		},
		{
			name: "Restore empty",
			fields: fields{
				Dir: "testdata/backup-empty",
			},
		},
		{
			name: "Restore corrupted",
			fields: fields{
				Dir: "testdata/backup-corrupted",
			},
			wantErrStr: "checksum mismatch",
		},
		{
			name: "Restore missing file",
			fields: fields{
				Dir: "testdata/backup-missing-file",
			},
			wantErrStr: "no such file or directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)

			e := newTestEngine(t)
			defer e.Close()

			srv := startBackupServer(e)
			conn, err := grpc.NewClient(srv.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			r.NoError(err)

			b := &Backup{
				Conn:    conn,
				Dir:     tt.fields.Dir,
				Timeout: tt.fields.Timeout,
				clock:   clock.NewMock(),
			}
			err = b.Restore()
			if tt.wantErrStr != "" {
				r.Contains(err.Error(), tt.wantErrStr)
				return
			}
			r.NoError(err)
		})
	}
}

func TestBackup_ensureDefaults(t *testing.T) {
	type fields struct {
		Conn    *grpc.ClientConn
		Log     Logger
		Timeout time.Duration
		Dir     string
	}
	tests := []struct {
		name   string
		fields fields
	}{
		{
			name: "Log defaults to nilLogger",
			fields: fields{
				Log:     nil,
				Timeout: 1 * time.Second,
			},
		},
		{
			name: "Timeout defaults to 1hr",
			fields: fields{
				Log:     nilLogger{},
				Timeout: 0,
			},
		},
		{
			name:   "Default all values",
			fields: fields{},
		},
		{
			name: "No defaulting",
			fields: fields{
				Log:     nilLogger{},
				Timeout: 1 * time.Second,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			b := &Backup{
				Conn:    tt.fields.Conn,
				Log:     tt.fields.Log,
				Timeout: tt.fields.Timeout,
				Dir:     tt.fields.Dir,
			}
			b.ensureDefaults()
			if tt.fields.Log == nil {
				r.Equal(nilLogger{}, b.Log)
			}
			if tt.fields.Timeout == 0 {
				r.Equal(1*time.Hour, b.Timeout)
			}
		})
	}
}

func newTestConfig(t *testing.T) storage.Config {
	fs := lvfs.NewMem()
	raftPort := getTestPort()
	return storage.Config{
		Log:               zaptest.NewLogger(t).Sugar(),
		NodeID:            1,
		InitialMembers:    map[uint64]string{1: fmt.Sprintf("127.0.0.1:%d", raftPort)},
		WALDir:            "/wal",
		NodeHostDir:       "/nh",
		RTTMillisecond:    5,
		RaftAddress:       fmt.Sprintf("127.0.0.1:%d", raftPort),
		QUICUDPBufferSize: 4 * 1024 * 1024, // 4 MiB — fits within most CI kernel limits
		Gossip:            storage.GossipConfig{InitialMembers: []string{fmt.Sprintf("127.0.0.1:%d", raftPort)}},
		Table:             storage.TableConfig{FS: wrapFS(fs), TableCacheSize: 1024, ElectionRTT: 10, HeartbeatRTT: 1},
		Meta:              storage.MetaConfig{ElectionRTT: 10, HeartbeatRTT: 1},
		FS:                fs,
	}
}

func newTestEngine(t *testing.T) *storage.Engine {
	e, err := storage.New(newTestConfig(t))
	require.NoError(t, err)
	require.NoError(t, e.Start())
	require.NoError(t, e.WaitUntilReady(context.Background()))
	t.Cleanup(func() {
		defer func() { recover() }()
		require.NoError(t, e.Close())
	})
	return e
}

func startBackupServer(manager *storage.Engine) *armadaserver.Server {
	testNodeAddress := fmt.Sprintf("127.0.0.1:%d", getTestPort())
	l, _ := net.Listen("tcp", testNodeAddress)
	server := armadaserver.NewServer(l, zap.NewNop().Sugar())
	armadapb.RegisterClusterServer(server, &armadaserver.ClusterServer{Cluster: manager})
	armadapb.RegisterMaintenanceServer(server, &armadaserver.BackupServer{AuthFunc: func(ctx context.Context) (context.Context, error) {
		return ctx, nil
	}, Tables: manager})
	go func() {
		err := server.Serve()
		if err != nil {
			panic(err)
		}
	}()
	// Let the server start.
	time.Sleep(100 * time.Millisecond)
	return server
}

func getTestPort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer func() {
		_ = l.Close()
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// wrapFS creates a new pebble/vfs.FS instance.
func wrapFS(fs lvfs.FS) pvfs.FS {
	return gvfs.NewPebbleFS(fs)
}

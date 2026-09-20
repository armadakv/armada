// Copyright JAMF Software, LLC

package armadaserver

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// restoreStream feeds a canned Backup stream into BackupServer.Restore.
type restoreStream struct {
	grpc.ServerStream
	ctx      context.Context
	messages []*armadapb.RestoreMessage
	pos      int
	response *armadapb.RestoreResponse
}

func (s *restoreStream) Context() context.Context { return s.ctx }

func (s *restoreStream) Recv() (*armadapb.RestoreMessage, error) {
	if s.pos >= len(s.messages) {
		return nil, io.EOF
	}
	m := s.messages[s.pos]
	s.pos++
	return m, nil
}

func (s *restoreStream) SendAndClose(r *armadapb.RestoreResponse) error {
	s.response = r
	return nil
}

// backupCollector captures a Backup stream so it can be replayed into Restore.
type backupCollector struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*armadapb.SnapshotChunk
}

func (c *backupCollector) Context() context.Context { return c.ctx }

func (c *backupCollector) Send(chunk *armadapb.SnapshotChunk) error {
	c.chunks = append(c.chunks, &armadapb.SnapshotChunk{
		Data: append([]byte(nil), chunk.Data...),
		Len:  chunk.Len,
	})
	return nil
}

// TestBackupServer_RestoreRoundTrip covers the operator-facing restore path,
// which now routes through the learner-first recovery coordinator: the backup
// must still load and the shard must still be swapped in.
func TestBackupServer_RestoreRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name   string
		format string
	}{
		{name: "armada-command-v2", format: "armada-command-v2"},
		{name: "legacy unterminated format", format: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			engine := newInMemTestEngine(t, "orders")
			srv := &BackupServer{Tables: engine, AuthFunc: func(c context.Context) (context.Context, error) { return c, nil }}

			tab, err := engine.GetTable("orders")
			r.NoError(err)
			originalShard := tab.ClusterID
			for i := range 4 {
				_, err := tab.Put(ctx, &armadapb.PutRequest{
					Table: []byte("orders"),
					Key:   []byte{byte(i)},
					Value: []byte{byte(i), byte(i)},
				})
				r.NoError(err)
			}

			collector := &backupCollector{ctx: ctx}
			r.NoError(srv.Backup(&armadapb.BackupRequest{Table: []byte("orders")}, collector))
			r.NotEmpty(collector.chunks)

			messages := []*armadapb.RestoreMessage{{
				Data: &armadapb.RestoreMessage_Info{Info: &armadapb.RestoreInfo{
					Table:  []byte("orders"),
					Format: tt.format,
				}},
			}}
			for _, c := range collector.chunks {
				messages = append(messages, &armadapb.RestoreMessage{
					Data: &armadapb.RestoreMessage_Chunk{Chunk: c},
				})
			}

			stream := &restoreStream{ctx: ctx, messages: messages}
			r.NoError(srv.Restore(stream))
			r.NotNil(stream.response)

			restored, err := engine.GetTable("orders")
			r.NoError(err)
			r.Greater(restored.ClusterID, originalShard, "the restored shard should have been swapped in")

			_, ok, err := engine.RecoveryStatus("orders")
			r.NoError(err)
			r.False(ok, "the journal record should be gone once the restore completes")

			resp, err := restored.Range(ctx, &armadapb.RangeRequest{
				Key:          []byte{0},
				RangeEnd:     []byte{0},
				Linearizable: true,
			})
			r.NoError(err)
			r.Len(resp.Kvs, 4)
		})
	}
}

// TestBackupServer_RestoreRejectsUnknownFormat guards the format check that
// keeps an unrecognised stream from being fed to the loader.
func TestBackupServer_RestoreRejectsUnknownFormat(t *testing.T) {
	engine := newInMemTestEngine(t, "orders")
	srv := &BackupServer{Tables: engine}

	stream := &restoreStream{
		ctx: context.Background(),
		messages: []*armadapb.RestoreMessage{{
			Data: &armadapb.RestoreMessage_Info{Info: &armadapb.RestoreInfo{
				Table:  []byte("orders"),
				Format: "armada-command-v99",
			}},
		}},
	}
	require.ErrorContains(t, srv.Restore(stream), "unsupported backup format")
}

// TestRestoreSpoolExposesItsPath is a compile-and-behaviour check that the file
// the maintenance handler spools the upload into exposes a path. The recovery
// coordinator records that path in the journal, which is what lets a crash
// mid-load resume instead of making the operator upload the backup again.
func TestRestoreSpoolExposesItsPath(t *testing.T) {
	sf, err := snapshot.NewTemp()
	require.NoError(t, err)
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	var pather interface{ Path() string } = sf
	require.NotEmpty(t, pather.Path())
}

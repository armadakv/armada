// Copyright JAMF Software, LLC

package regattaserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/armadakv/armada/regattapb"
	"github.com/armadakv/armada/replication/snapshot"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ResetServer implements some Maintenance service methods from proto/regatta.proto.
type ResetServer struct {
	regattapb.UnimplementedMaintenanceServer
	Tables   TableService
	AuthFunc func(ctx context.Context) (context.Context, error)
}

func (m *ResetServer) Reset(ctx context.Context, req *regattapb.ResetRequest) (*regattapb.ResetResponse, error) {
	reset := func(name string) error {
		t, err := m.Tables.GetTable(name)
		if err != nil {
			return err
		}
		dctx := ctx
		if _, ok := dctx.Deadline(); !ok {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			dctx = ctx
		}
		return t.Reset(dctx)
	}
	if req.ResetAll {
		tables, err := m.Tables.GetTables()
		if err != nil {
			return nil, err
		}
		for _, table := range tables {
			err := reset(table.Name)
			if err != nil {
				return nil, err
			}
		}
		return &regattapb.ResetResponse{}, nil
	}
	if len(req.Table) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "table must be set")
	}
	if err := reset(string(req.Table)); err != nil {
		return nil, err
	}
	return &regattapb.ResetResponse{}, nil
}

func (m *ResetServer) AuthFuncOverride(ctx context.Context, _ string) (context.Context, error) {
	return m.AuthFunc(ctx)
}

// BackupServer implements some Maintenance service methods from proto/regatta.proto.
type BackupServer struct {
	regattapb.UnimplementedMaintenanceServer
	Tables   TableService
	AuthFunc func(ctx context.Context) (context.Context, error)
}

func (m *BackupServer) Backup(req *regattapb.BackupRequest, srv regattapb.Maintenance_BackupServer) error {
	ctx := srv.Context()
	table, err := m.Tables.GetTable(string(req.Table))
	if err != nil {
		return err
	}

	if _, ok := ctx.Deadline(); !ok {
		dctx, cancel := context.WithTimeout(srv.Context(), 1*time.Hour)
		defer cancel()
		ctx = dctx
	}

	sf, err := snapshot.NewTemp()
	if err != nil {
		return err
	}
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	_, err = table.Snapshot(ctx, sf)
	if err != nil {
		return err
	}
	err = sf.Sync()
	if err != nil {
		return err
	}
	_, err = sf.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	_, err = io.Copy(&snapshot.Writer{Sender: srv}, bufio.NewReaderSize(sf.File, snapshot.DefaultSnapshotChunkSize))
	return err
}

func (m *BackupServer) Restore(srv regattapb.Maintenance_RestoreServer) error {
	msg, err := srv.Recv()
	if err != nil {
		return err
	}
	info := msg.GetInfo()
	if info == nil {
		return fmt.Errorf("first message should contain info")
	}
	sf, err := snapshot.NewTemp()
	if err != nil {
		return err
	}
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()
	_, err = io.Copy(sf.File, backupReader{stream: srv})
	if err != nil {
		return err
	}
	err = sf.Sync()
	if err != nil {
		return err
	}
	_, err = sf.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	err = m.Tables.Restore(string(info.Table), sf)
	if err != nil {
		return err
	}
	return srv.SendAndClose(&regattapb.RestoreResponse{})
}

func (m *BackupServer) AuthFuncOverride(ctx context.Context, _ string) (context.Context, error) {
	return m.AuthFunc(ctx)
}

type backupReader struct {
	stream regattapb.Maintenance_RestoreServer
}

func (s backupReader) Read(p []byte) (int, error) {
	m, err := s.stream.Recv()
	if err != nil {
		return 0, err
	}
	chunk := m.GetChunk()
	if chunk == nil {
		return 0, errors.New("chunk expected")
	}
	if len(p) < int(chunk.Len) {
		return 0, io.ErrShortBuffer
	}
	return copy(p, chunk.Data), nil
}

func (s backupReader) WriteTo(w io.Writer) (int64, error) {
	n := int64(0)
	for {
		m, err := s.stream.Recv()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		chunk := m.GetChunk()
		if chunk == nil {
			return 0, errors.New("chunk expected")
		}
		w, err := w.Write(chunk.Data)
		if err != nil {
			return n, err
		}
		n += int64(w)
	}
}

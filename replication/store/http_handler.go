// Copyright Armada Contributors

package store

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/replication/snapshot"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/objfs"
	"go.uber.org/zap"
)

// LiveSnapshotTableService provides access to live table snapshots.
type LiveSnapshotTableService interface {
	GetTable(name string) (table.ActiveTable, error)
}

// SnapshotHTTPHandler handles HTTP requests for snapshot artifacts from shared
// storage and for on-demand live snapshots.
type SnapshotHTTPHandler struct {
	bucket objfs.Bucket
	tables LiveSnapshotTableService
	log    *zap.SugaredLogger
	router *http.ServeMux
}

// NewSnapshotHTTPHandler creates a new HTTP handler for serving snapshot
// artefacts from shared storage and on-demand live snapshots from local tables.
func NewSnapshotHTTPHandler(b objfs.Bucket, tables LiveSnapshotTableService, log *zap.SugaredLogger) *SnapshotHTTPHandler {
	h := &SnapshotHTTPHandler{
		bucket: b,
		tables: tables,
		log:    log,
	}
	h.router = http.NewServeMux()
	h.router.HandleFunc("GET /snapshots/{object...}", h.serveSharedSnapshot)
	h.router.HandleFunc("HEAD /snapshots/{object...}", h.serveSharedSnapshot)
	h.router.HandleFunc("GET /snapshots-live/{table}", h.serveLiveSnapshot)
	return h
}

func (h *SnapshotHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

func (h *SnapshotHTTPHandler) serveSharedSnapshot(w http.ResponseWriter, r *http.Request) {
	if h.bucket == nil {
		http.Error(w, "Shared snapshot store is not configured", http.StatusServiceUnavailable)
		return
	}

	objectKey := path.Clean(path.Join("snapshots", r.PathValue("object")))
	if objectKey == "snapshots" || !strings.HasPrefix(objectKey, "snapshots/") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	attrs, err := h.bucket.Stat(r.Context(), objectKey)
	if err != nil {
		if errors.Is(err, objfs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		h.log.Errorf("failed to get attributes for %s: %v", objectKey, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodGet {
		http.ServeFileFS(w, r, h.bucket, objectKey)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(attrs.Size, 10))
}

func (h *SnapshotHTTPHandler) serveLiveSnapshot(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if tableName == "" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	sf, err := snapshot.NewTemp()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	ctx := r.Context()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Hour)
		defer cancel()
	}

	tbl, err := h.tables.GetTable(tableName)
	if err != nil {
		if errors.Is(err, serrors.ErrTableNotFound) {
			http.Error(w, "Table not found", http.StatusNotFound)
			return
		}
		h.log.Errorf("failed to get table %s for live snapshot: %v", tableName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	resp, err := tbl.Snapshot(ctx, sf)
	if err != nil {
		h.log.Errorf("failed creating live snapshot for table %s: %v", tableName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	index := resp.Index

	final, err := (&armadapb.Command{
		Table:       []byte(tableName),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &index,
	}).MarshalVT()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if _, err := sf.Write(final); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if err := sf.Sync(); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if _, err := sf.Seek(0, 0); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = sf.WriteTo(w)
}

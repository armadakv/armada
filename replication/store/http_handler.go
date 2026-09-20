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

// presignTTL bounds how long a redirect handed to a follower stays usable. It
// must comfortably exceed a large snapshot's transfer time.
const presignTTL = time.Hour

// SnapshotLeaseHolderHeader carries the stable follower identity for lease
// operations. The follower uses its raft address so acquire, refresh, and
// release address the same shared-store key across separate HTTP connections.
const SnapshotLeaseHolderHeader = "X-Armada-Snapshot-Lease-Holder"

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
	h.router.HandleFunc("PUT /snapshot-leases/{table}", h.serveSnapshotLease)
	h.router.HandleFunc("DELETE /snapshot-leases/{table}", h.serveSnapshotLease)
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
		// When the backend can mint a time-limited URL, hand the follower
		// straight to the blob store instead of streaming every byte through
		// the leader. The follower's HTTP getter follows redirects and carries
		// its Range header across, so resumption is unaffected.
		if url, err := objfs.PresignedGet(r.Context(), h.bucket, objectKey, presignTTL); err == nil && url != "" {
			// #nosec G710 -- the URL is minted by the operator-configured
			// blob backend, not derived from the request. objectKey is
			// already constrained to the snapshots/ prefix above.
			http.Redirect(w, r, url, http.StatusTemporaryRedirect)
			return
		} else if err != nil && !errors.Is(err, objfs.ErrUnsupported) {
			h.log.Warnf("presign failed for %s, streaming instead: %v", objectKey, err)
		}
		// http.ServeFileFS honours Range, which is what makes a follower's
		// download resumable without any extra server-side support.
		http.ServeFileFS(w, r, h.bucket, objectKey)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(attrs.Size, 10))
}

// serveSnapshotLease manages a follower download lease without exposing bucket
// credentials. This endpoint deliberately has no redirect path: a snapshot GET
// may be redirected to an object store, but lease renewal must keep reaching the
// leader while that transfer is in progress.
func (h *SnapshotHTTPHandler) serveSnapshotLease(w http.ResponseWriter, r *http.Request) {
	if h.bucket == nil {
		http.Error(w, "Shared snapshot store is not configured", http.StatusServiceUnavailable)
		return
	}

	tableName := r.PathValue("table")
	if !validLeaseTable(tableName) {
		http.Error(w, "Invalid table", http.StatusBadRequest)
		return
	}
	holder := r.Header.Get(SnapshotLeaseHolderHeader)
	if !validLeaseHolder(holder) {
		http.Error(w, "Invalid lease holder", http.StatusBadRequest)
		return
	}

	var err error
	switch r.Method {
	case http.MethodPut:
		err = WriteLease(r.Context(), h.bucket, tableName, holder)
	case http.MethodDelete:
		err = ReleaseLease(r.Context(), h.bucket, tableName, holder)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		h.log.Errorf("failed managing snapshot lease for table %s: %v", tableName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validLeaseTable permits exactly one safe object-key path segment. Snapshot
// table names are used in object paths, so accepting a path separator or dot
// segment here could let a lease request escape its intended table prefix.
func validLeaseTable(table string) bool {
	return table != "" && table != "." && table != ".." &&
		!strings.ContainsAny(table, "/\\")
}

// validLeaseHolder admits the stable raft-address forms used by followers while
// rejecting path separators and dot segments before the value reaches LeaseKey.
func validLeaseHolder(holder string) bool {
	if holder == "" || holder == "." || holder == ".." || len(holder) > 255 {
		return false
	}
	for _, r := range holder {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune(".-_:[]", r) {
			continue
		}
		return false
	}
	return true
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

package store

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/armadakv/objfs"
	"go.uber.org/zap"
)

// SnapshotHTTPHandler handles HTTP GET requests for snapshot artefacts.
type SnapshotHTTPHandler struct {
	bucket objfs.Bucket
	log    *zap.SugaredLogger
}

// NewSnapshotHTTPHandler creates a new HTTP handler for serving snapshots.
func NewSnapshotHTTPHandler(b objfs.Bucket, log *zap.SugaredLogger) *SnapshotHTTPHandler {
	return &SnapshotHTTPHandler{
		bucket: b,
		log:    log,
	}
}

func (h *SnapshotHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	objectKey := strings.TrimPrefix(r.URL.Path, "/")
	if !strings.HasPrefix(objectKey, "snapshots/") {
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

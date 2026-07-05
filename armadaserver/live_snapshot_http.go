package armadaserver

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/replication/snapshot"
	"go.uber.org/zap"
)

// LiveSnapshotHTTPHandler serves on-demand full snapshots from local tables.
// It is used by follower proxy fallback when shared store is unavailable.
type LiveSnapshotHTTPHandler struct {
	tables TableService
	log    *zap.SugaredLogger
}

func NewLiveSnapshotHTTPHandler(tables TableService, log *zap.SugaredLogger) *LiveSnapshotHTTPHandler {
	return &LiveSnapshotHTTPHandler{
		tables: tables,
		log:    log,
	}
}

func (h *LiveSnapshotHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tableName := strings.TrimPrefix(r.URL.Path, "/snapshots-live/")
	if tableName == "" || strings.Contains(tableName, "/") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	table, err := h.tables.GetTable(tableName)
	if err != nil {
		http.Error(w, "Table not found", http.StatusNotFound)
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

	resp, err := table.Snapshot(ctx, sf)
	if err != nil {
		h.log.Errorf("failed creating live snapshot for table %s: %v", tableName, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	final, err := (&armadapb.Command{
		Table:       []byte(tableName),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &resp.Index,
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
	_, _ = sf.File.WriteTo(w)
}

package armadaserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/armadakv/armada/armadapb"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestLiveSnapshotHTTPHandler(t *testing.T) {
	engine := newInMemTestEngine(t, "orders")
	_, err := engine.Put(context.Background(), &armadapb.PutRequest{
		Table: []byte("orders"),
		Key:   []byte("k1"),
		Value: []byte("v1"),
	})
	require.NoError(t, err)

	h := NewLiveSnapshotHTTPHandler(engine, zaptest.NewLogger(t).Sugar())

	t.Run("returns snapshot stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/snapshots-live/orders", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotEmpty(t, rec.Body.Bytes())
	})

	t.Run("returns not found for missing table", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/snapshots-live/missing", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// Copyright Armada Contributors

package store

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestSnapshotHTTPHandler(t *testing.T) {
	log := zaptest.NewLogger(t).Sugar()
	bucket := NewLocalBucket(t)

	// Setup some data
	err := bucket.Upload(context.Background(), "snapshots/t/full/1.snap", bytes.NewReader([]byte("testdata")))
	require.NoError(t, err)

	handler := NewSnapshotHTTPHandler(bucket, testLiveTableService{}, log)

	tests := []struct {
		name           string
		method         string
		path           string
		expectedStatus int
		expectedBody   string
		expectedLength string
	}{
		{
			name:           "method not allowed",
			method:         http.MethodPost,
			path:           "/snapshots/t/full/1.snap",
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "invalid path prefix",
			method:         http.MethodGet,
			path:           "/other/path/file",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "not found",
			method:         http.MethodGet,
			path:           "/snapshots/t/full/2.snap",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "get success",
			method:         http.MethodGet,
			path:           "/snapshots/t/full/1.snap",
			expectedStatus: http.StatusOK,
			expectedBody:   "testdata",
			expectedLength: "8",
		},
		{
			name:           "head success",
			method:         http.MethodHead,
			path:           "/snapshots/t/full/1.snap",
			expectedStatus: http.StatusOK,
			expectedLength: "8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			resp := w.Result()
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			assert.Equal(t, tt.expectedStatus, resp.StatusCode)

			if tt.expectedStatus == http.StatusOK {
				switch tt.method {
				case http.MethodGet:
					assert.Equal(t, tt.expectedBody, string(body))
				case http.MethodHead:
					assert.Empty(t, string(body))
				}
				assert.Equal(t, tt.expectedLength, resp.Header.Get("Content-Length"))
			}
		})
	}
}

func TestSnapshotHTTPHandlerContinuation(t *testing.T) {
	log := zaptest.NewLogger(t).Sugar()
	bucket := NewLocalBucket(t)

	// Setup some data
	testData := []byte("testdata")
	err := bucket.Upload(context.Background(), "snapshots/t/full/1.snap", bytes.NewReader(testData))
	require.NoError(t, err)

	handler := NewSnapshotHTTPHandler(bucket, testLiveTableService{}, log)

	req := httptest.NewRequest(http.MethodGet, "/snapshots/t/full/1.snap", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	read := make([]byte, 1)
	_, err = resp.Body.Read(read)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, testData[:1], read, "one byte read")

	req = httptest.NewRequest(http.MethodGet, "/snapshots/t/full/1.snap", nil)
	req.Header.Set("Range", "bytes=1-")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req)

	resp = w2.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, string(testData[1:]), string(body), "rest read")
}

func TestSnapshotHTTPHandler_LiveSnapshot(t *testing.T) {
	log := zaptest.NewLogger(t).Sugar()
	handler := NewSnapshotHTTPHandler(nil, testLiveTableService{
		getTable: func(name string) (table.ActiveTable, error) {
			return table.ActiveTable{}, serrors.ErrTableNotFound
		},
	}, log)

	req := httptest.NewRequest(http.MethodGet, "/snapshots-live/orders", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	_, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSnapshotHTTPHandler_NoSharedStore(t *testing.T) {
	log := zaptest.NewLogger(t).Sugar()
	handler := NewSnapshotHTTPHandler(nil, testLiveTableService{}, log)

	req := httptest.NewRequest(http.MethodGet, "/snapshots/t/full/1.snap", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

type testLiveTableService struct {
	getTable func(name string) (table.ActiveTable, error)
}

func (s testLiveTableService) GetTable(name string) (table.ActiveTable, error) {
	return s.getTable(name)
}

func TestSnapshotHTTPHandler_LiveTableNotFound(t *testing.T) {
	log := zaptest.NewLogger(t).Sugar()
	handler := NewSnapshotHTTPHandler(nil, testLiveTableService{
		getTable: func(name string) (table.ActiveTable, error) {
			return table.ActiveTable{}, serrors.ErrTableNotFound
		},
	}, log)

	req := httptest.NewRequest(http.MethodGet, "/snapshots-live/missing", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

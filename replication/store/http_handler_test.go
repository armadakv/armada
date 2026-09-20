// Copyright Armada Contributors

package store

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/objfs"
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

type presigningBucket struct {
	objfs.Bucket
	url string
}

func (b presigningBucket) PresignedURL(_ context.Context, _ string, op objfs.Operation, expiry time.Duration) (string, error) {
	if op != objfs.OpGet || expiry != presignTTL {
		return "", objfs.ErrUnsupported
	}
	return b.url, nil
}

func TestSnapshotHTTPHandler_LeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	handler := NewSnapshotHTTPHandler(bucket, testLiveTableService{}, zaptest.NewLogger(t).Sugar())

	put := httptest.NewRequest(http.MethodPut, "/snapshot-leases/orders", nil)
	put.Header.Set(SnapshotLeaseHolderHeader, "follower.example:5012")
	putResult := httptest.NewRecorder()
	handler.ServeHTTP(putResult, put)
	require.Equal(t, http.StatusNoContent, putResult.Code)

	var found int
	require.NoError(t, bucket.List(ctx, "snapshots/orders/.lease/", func(objfs.Attributes) error {
		found++
		return nil
	}))
	require.Equal(t, 1, found, "the HTTP lease must be written where GC looks for it")

	deleteReq := httptest.NewRequest(http.MethodDelete, "/snapshot-leases/orders", nil)
	deleteReq.Header.Set(SnapshotLeaseHolderHeader, "follower.example:5012")
	deleteResult := httptest.NewRecorder()
	handler.ServeHTTP(deleteResult, deleteReq)
	require.Equal(t, http.StatusNoContent, deleteResult.Code)

	found = 0
	require.NoError(t, bucket.List(ctx, "snapshots/orders/.lease/", func(objfs.Attributes) error {
		found++
		return nil
	}))
	require.Zero(t, found)
}

func TestSnapshotHTTPHandler_LeaseValidation(t *testing.T) {
	handler := NewSnapshotHTTPHandler(NewLocalBucket(t), testLiveTableService{}, zaptest.NewLogger(t).Sugar())

	t.Run("requires a stable holder identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/snapshot-leases/orders", nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		require.Equal(t, http.StatusBadRequest, res.Code)
	})

	t.Run("rejects table path traversal", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/snapshot-leases/orders", nil)
		req.SetPathValue("table", "../other")
		req.Header.Set(SnapshotLeaseHolderHeader, "follower.example:5012")
		res := httptest.NewRecorder()
		handler.serveSnapshotLease(res, req)
		require.Equal(t, http.StatusBadRequest, res.Code)
	})

	t.Run("rejects unsafe holder paths", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/snapshot-leases/orders", nil)
		req.Header.Set(SnapshotLeaseHolderHeader, "../other")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		require.Equal(t, http.StatusBadRequest, res.Code)
	})
}

func TestSnapshotHTTPHandler_PresignedGetRedirect(t *testing.T) {
	base := NewLocalBucket(t)
	require.NoError(t, base.Upload(context.Background(), "snapshots/t/full/1.snap", bytes.NewReader([]byte("testdata"))))
	handler := NewSnapshotHTTPHandler(presigningBucket{Bucket: base, url: "https://object.example/signed"}, testLiveTableService{}, zaptest.NewLogger(t).Sugar())

	req := httptest.NewRequest(http.MethodGet, "/snapshots/t/full/1.snap", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.Equal(t, "https://object.example/signed", resp.Header.Get("Location"))
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

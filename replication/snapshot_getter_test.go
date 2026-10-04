// Copyright Armada Contributors

package replication

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/objfs"
	"github.com/stretchr/testify/require"
)

func TestHTTPSnapshotGetter_GetFrom(t *testing.T) {
	payload := []byte("0123456789abcdef")

	t.Run("a range request resumes from the offset", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "snap", time.Unix(0, 0), bytes.NewReader(payload))
		}))
		defer srv.Close()

		g := NewHTTPSnapshotObjectGetter(srv.Client(), srv.URL)
		body, ranged, err := g.GetFrom(context.Background(), "snapshots/t/full/1.snap", 10)
		require.NoError(t, err)
		defer body.Close()
		require.True(t, ranged, "the server honoured the range, so the caller must keep what it staged")

		got, err := io.ReadAll(body)
		require.NoError(t, err)
		require.Equal(t, payload[10:], got)
	})

	t.Run("a mismatched Content-Range is rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-5/16")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:6])
		}))
		defer srv.Close()

		g := NewHTTPSnapshotObjectGetter(srv.Client(), srv.URL)
		_, ranged, err := g.GetFrom(context.Background(), "snapshots/t/full/1.snap", 10)
		require.ErrorContains(t, err, "invalid Content-Range")
		require.False(t, ranged)
	})

	t.Run("a server that ignores the range reports the whole object", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload)
		}))
		defer srv.Close()

		g := NewHTTPSnapshotObjectGetter(srv.Client(), srv.URL)
		body, ranged, err := g.GetFrom(context.Background(), "snapshots/t/full/1.snap", 10)
		require.NoError(t, err)
		defer body.Close()
		require.False(t, ranged, "without this the caller would append a full copy onto its partial file")

		got, err := io.ReadAll(body)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("a redirect is followed", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload)
		}))
		defer origin.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, origin.URL, http.StatusTemporaryRedirect)
		}))
		defer srv.Close()

		g := NewHTTPSnapshotObjectGetter(srv.Client(), srv.URL)
		body, _, err := g.GetFrom(context.Background(), "snapshots/t/full/1.snap", 0)
		require.NoError(t, err)
		defer body.Close()
		got, err := io.ReadAll(body)
		require.NoError(t, err)
		require.Equal(t, payload, got)
	})

	t.Run("a non-OK status is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		}))
		defer srv.Close()

		g := NewHTTPSnapshotObjectGetter(srv.Client(), srv.URL)
		_, _, err := g.GetFrom(context.Background(), "snapshots/t/full/1.snap", 0)
		require.ErrorContains(t, err, "404")
	})
}

func TestBucketSnapshotObjectGetter_GetFrom(t *testing.T) {
	ctx := context.Background()
	bucket, err := objfs.NewLocal(t.TempDir())
	require.NoError(t, err)
	payload := []byte("0123456789abcdef")
	require.NoError(t, bucket.Upload(ctx, "snapshots/t/full/1.snap", bytes.NewReader(payload)))

	g := NewBucketSnapshotObjectGetter(bucket)

	body, ranged, err := g.GetFrom(ctx, "snapshots/t/full/1.snap", 0)
	require.NoError(t, err)
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.False(t, ranged)
	require.Equal(t, payload, got)

	body, ranged, err = g.GetFrom(ctx, "snapshots/t/full/1.snap", 10)
	require.NoError(t, err)
	got, err = io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.True(t, ranged)
	require.Equal(t, payload[10:], got)

	_, _, err = g.GetFrom(ctx, "snapshots/t/full/missing.snap", 0)
	require.Error(t, err)
}

func TestHTTPLeaseKeeper(t *testing.T) {
	var requests []struct {
		method string
		path   string
		holder string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, struct {
			method string
			path   string
			holder string
		}{r.Method, r.URL.Path, r.Header.Get(store.SnapshotLeaseHolderHeader)})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	keeper := NewHTTPLeaseKeeper(srv.Client(), srv.URL, "follower.example:5012")
	require.NoError(t, keeper.AcquireLease(context.Background(), "orders"))
	require.NoError(t, keeper.RefreshLease(context.Background(), "orders"))
	require.NoError(t, keeper.ReleaseLease(context.Background(), "orders"))

	require.Equal(t, []struct {
		method string
		path   string
		holder string
	}{
		{http.MethodPut, "/snapshot-leases/orders", "follower.example:5012"},
		{http.MethodPut, "/snapshot-leases/orders", "follower.example:5012"},
		{http.MethodDelete, "/snapshot-leases/orders", "follower.example:5012"},
	}, requests)
}

func TestBucketLeaseKeeper(t *testing.T) {
	ctx := context.Background()
	bucket, err := objfs.NewLocal(t.TempDir())
	require.NoError(t, err)

	k := NewBucketLeaseKeeper(bucket, "10.0.0.1:5012")
	require.NoError(t, k.AcquireLease(ctx, "orders"))
	require.NoError(t, k.RefreshLease(ctx, "orders"))

	var found int
	require.NoError(t, bucket.List(ctx, "snapshots/orders/.lease/", func(objfs.Attributes) error {
		found++
		return nil
	}))
	require.Equal(t, 1, found, "the lease must be written where GC looks for it")

	require.NoError(t, k.ReleaseLease(ctx, "orders"))
	found = 0
	require.NoError(t, bucket.List(ctx, "snapshots/orders/.lease/", func(objfs.Attributes) error {
		found++
		return nil
	}))
	require.Zero(t, found)
}

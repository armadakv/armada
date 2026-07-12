// Copyright Armada Contributors

package snapshot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestDownloadHTTP(t *testing.T) {
	content := []byte("hello world this is a test payload")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic range request support for testing
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var offset int
			fmt.Sscanf(rangeHeader, "bytes=%d-", &offset)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[offset:])
			return
		}
		w.Write(content)
	}))
	defer ts.Close()

	client := ts.Client()

	sf, err := NewTemp()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sf.Path())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Download first half
	err = DownloadHTTP(ctx, client, ts.URL, sf)
	if err != nil {
		t.Fatal(err)
	}

	// Read content
	sf.Seek(0, io.SeekStart)
	got, _ := io.ReadAll(sf.File)
	if string(got) != string(content) {
		t.Fatalf("expected %q, got %q", content, got)
	}
}

// Copyright Armada Contributors

package snapshot

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// DownloadHTTP downloads a snapshot from the given URL into the provided TempFile.
// It uses Range headers if the file already contains data to resume the download.
func DownloadHTTP(ctx context.Context, client *http.Client, url string, sf *snapshotFile) error {
	fi, err := sf.Stat()
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}

	offset := fi.Size()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status code: %d %s", resp.StatusCode, resp.Status)
	}

	// If the server didn't respect the Range header, we need to overwrite.
	if offset > 0 && resp.StatusCode == http.StatusOK {
		offset = 0
		if err := sf.Truncate(0); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
	}

	if _, err := sf.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	_, err = io.Copy(sf.File, resp.Body)
	if err != nil {
		return fmt.Errorf("copy body: %w", err)
	}

	return nil
}

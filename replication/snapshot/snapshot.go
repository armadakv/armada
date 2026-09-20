// Copyright JAMF Software, LLC

package snapshot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/armadakv/armada/armadapb"
	"github.com/klauspost/compress/snappy"
	"golang.org/x/time/rate"
)

// DefaultSnapshotChunkSize default chunk size of gRPC snapshot stream.
const DefaultSnapshotChunkSize = 1024 * 1024

const snapshotFilenamePattern = "snapshot-*.bin"

type Writer struct {
	Sender armadapb.Snapshot_StreamServer
}

func (g *Writer) ReadFrom(r io.Reader) (int64, error) {
	count := int64(0)
	chunk := make([]byte, DefaultSnapshotChunkSize)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			count += int64(n)
			if err := g.Sender.Send(&armadapb.SnapshotChunk{
				Data: chunk[:n],
				Len:  uint64(n),
			}); err != nil {
				return count, err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return count, err
		}
	}
	return count, nil
}

func (g *Writer) Write(p []byte) (int, error) {
	ln := len(p)
	if err := g.Sender.Send(&armadapb.SnapshotChunk{
		Data: p,
		Len:  uint64(ln),
	}); err != nil {
		return 0, err
	}
	return ln, nil
}

type Reader struct {
	Stream  armadapb.Snapshot_StreamClient
	Limiter *rate.Limiter
}

func (s Reader) Read(p []byte) (int, error) {
	chunk := armadapb.SnapshotChunkFromVTPool()
	defer chunk.ReturnToVTPool()
	if err := s.Stream.RecvMsg(chunk); err != nil {
		return 0, err
	}
	if len(p) < int(chunk.Len) {
		return 0, io.ErrShortBuffer
	}
	if s.Limiter != nil {
		s.Limiter.WaitN(s.Stream.Context(), int(chunk.Len))
	}
	return copy(p, chunk.Data), nil
}

func (s Reader) WriteTo(w io.Writer) (int64, error) {
	n := int64(0)
	chunk := armadapb.SnapshotChunkFromVTPool()
	defer chunk.ReturnToVTPool()
	for {
		chunk.ResetVT()
		err := s.Stream.RecvMsg(chunk)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if s.Limiter != nil {
			s.Limiter.WaitN(s.Stream.Context(), int(chunk.Len))
		}
		w, err := w.Write(chunk.Data)
		if err != nil {
			return n, err
		}
		n += int64(w)
	}
}

func OpenFile(path string) (*snapshotFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return newFile(f, path), nil
}

func NewTemp() (*snapshotFile, error) {
	dir := os.TempDir()
	f, err := os.CreateTemp(dir, snapshotFilenamePattern)
	if err != nil {
		return nil, err
	}
	return newFile(f, f.Name()), nil
}

func newFile(file *os.File, path string) *snapshotFile {
	return &snapshotFile{
		File:    file,
		path:    path,
		w:       snappy.NewBufferedWriter(file),
		r:       snappy.NewReader(file),
		lenBuff: make([]byte, 8),
	}
}

type snapshotFile struct {
	*os.File
	r       *snappy.Reader
	w       *snappy.Writer
	lenBuff []byte
	path    string
}

func (s *snapshotFile) Path() string {
	return s.path
}

func (s *snapshotFile) Read(p []byte) (n int, err error) {
	buf := s.lenBuff[:]
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return 0, err
	}
	size := binary.LittleEndian.Uint64(buf)
	if _, err := io.ReadFull(s.r, p[:size]); err != nil {
		return 0, err
	}
	return int(size), nil
}

func (s *snapshotFile) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := s.lenBuff[:]
	binary.LittleEndian.PutUint64(buf, uint64(len(p)))
	_, err := s.w.Write(buf)
	if err != nil {
		return 0, err
	}

	n, err := s.w.Write(p)
	if err != nil {
		return 0, err
	}
	return n, err
}

func (s *snapshotFile) Sync() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	if err := s.File.Sync(); err != nil {
		return err
	}
	return nil
}

func (s *snapshotFile) Close() error {
	if err := s.w.Close(); err != nil {
		return err
	}
	if err := s.File.Close(); err != nil {
		return err
	}
	return nil
}

// OffsetSuffix is appended to a staged artifact's path to name the sidecar file
// that records how many verified bytes have been written so far.
const OffsetSuffix = ".offset"

// StagedDirName is the directory, relative to the table data directory, where
// resumable snapshot downloads are staged. Unlike NewTemp it survives a
// restart, which is what lets a recovery pick a download back up rather than
// starting over.
const StagedDirName = "snapshots-staging"

// Staged is a resumable download target. It is a plain file holding the raw
// (still snappy-compressed) artifact bytes plus a sidecar recording the number
// of bytes already durably written. The raw/framed split is deliberate: bytes
// go in unframed and are only wrapped in the snappy framing reader when the
// completed artifact is handed to the snapshot reader.
type Staged struct {
	*os.File
	path string
}

// NewStaged opens (creating if needed) the staged artifact called name under
// dir, positioned for appending.
func NewStaged(dir, name string) (*Staged, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	p := filepath.Join(dir, name+".part")
	// #nosec G304 -- the path is composed from operator-configured directories.
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &Staged{File: f, path: p}, nil
}

// Path returns the staged artifact path.
func (s *Staged) Path() string { return s.path }

// Offset returns the number of bytes already durably written, and truncates the
// file back to that offset when a crash left a longer, unaccounted-for tail.
func (s *Staged) Offset() (int64, error) {
	var off int64
	data, err := os.ReadFile(s.path + OffsetSuffix)
	if err == nil {
		if v, perr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); perr == nil {
			off = v
		}
	} else if !os.IsNotExist(err) {
		return 0, err
	}

	fi, err := s.Stat()
	if err != nil {
		return 0, err
	}
	if off > fi.Size() {
		// The sidecar cannot be ahead of the data; distrust it.
		off = 0
	}
	if fi.Size() != off {
		if err := s.Truncate(off); err != nil {
			return 0, err
		}
	}
	if _, err := s.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return off, nil
}

// Commit fsyncs the staged data and atomically records off as the verified byte
// count. The temporary sidecar and its containing directory are synced as well,
// so a successful checkpoint survives a process or power failure as a unit.
func (s *Staged) Commit(off int64) error {
	if err := s.Sync(); err != nil {
		return fmt.Errorf("sync staged data: %w", err)
	}
	tmp := s.path + OffsetSuffix + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open checkpoint sidecar: %w", err)
	}
	if _, err := f.WriteString(strconv.FormatInt(off, 10)); err != nil {
		_ = f.Close()
		return fmt.Errorf("write checkpoint sidecar: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync checkpoint sidecar: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close checkpoint sidecar: %w", err)
	}
	if err := os.Rename(tmp, s.path+OffsetSuffix); err != nil {
		return fmt.Errorf("publish checkpoint sidecar: %w", err)
	}
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("sync staging directory: %w", err)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Reset discards everything staged so far and rewinds to offset zero.
func (s *Staged) Reset() error {
	if err := s.Truncate(0); err != nil {
		return err
	}
	if err := s.Sync(); err != nil {
		return err
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := os.Remove(s.path + OffsetSuffix); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(filepath.Dir(s.path))
}

// Discard closes and removes the staged artifact and its sidecar.
func (s *Staged) Discard() {
	_ = s.Close()
	_ = os.Remove(s.path)
	_ = os.Remove(s.path + OffsetSuffix)
}

// SHA256 returns the hex-encoded SHA-256 of the staged bytes.
func (s *Staged) SHA256() (string, error) {
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, s.File); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

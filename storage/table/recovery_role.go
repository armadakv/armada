// Copyright Armada Contributors

package table

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path"
	"strconv"

	"github.com/cockroachdb/pebble/v2/vfs"
)

// Role markers record that a local Raft replica was started as a non-voting
// learner for a given shard. They are node-local (never replicated) because
// they describe this replica's own on-disk Raft state.
//
// The marker exists to close a start-up hazard: config.Config.IsNonVoting must
// be decided before the replica can read its persisted membership, and the two
// error directions are not symmetric. Starting as non-voting when membership
// already says "voter" is recoverable — the Raft layer promotes the replica to
// follower when it restores a snapshot. Starting as a voter while the log still
// holds an unapplied AddNonVoting entry naming this replica panics. Erring
// toward non-voting is therefore always the safe choice, and the marker is what
// makes that choice durable across restarts.
//
// The file layout mirrors the CRC-checked flag files the Raft library keeps for
// its own metadata (raft/internal/fileutil), which is not importable here.
const (
	roleMarkerDirName = "recovery-role"
	roleMarkerMagic   = "armada-nonvoting-v1"
)

var errRoleMarkerCorrupt = errors.New("recovery role marker is corrupt")

func (m *Manager) markerFS() vfs.FS {
	if m.cfg.Table.FS != nil {
		return m.cfg.Table.FS
	}
	return vfs.Default
}

func (m *Manager) roleMarkerDir() string {
	return path.Join(m.cfg.Table.DataDir, roleMarkerDirName)
}

func (m *Manager) roleMarkerPath(shardID uint64) string {
	return path.Join(m.roleMarkerDir(), strconv.FormatUint(shardID, 10))
}

// writeNonVotingMarker durably records that shardID must be started as a
// non-voting replica on this node for the lifetime of its local Raft data.
//
// Promotion alone cannot prove that the original AddNonVoting entry has been
// removed from the persisted log. Keeping the marker is safe: Dragonboat
// restores a promoted member as a voter, while starting that member as a voter
// before replaying AddNonVoting panics. cleanup removes the marker together
// with the replica data, when the shard can no longer be restarted.
func (m *Manager) writeNonVotingMarker(shardID uint64) error {
	fs := m.markerFS()
	dir := m.roleMarkerDir()
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create recovery role dir: %w", err)
	}

	payload := fmt.Appendf(nil, "%s %d %d", roleMarkerMagic, shardID, m.cfg.NodeID)
	buf := make([]byte, 0, len(payload)+4)
	buf = append(buf, payload...)
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(payload))

	f, err := fs.Create(m.roleMarkerPath(shardID), "armada-recovery-role")
	if err != nil {
		return fmt.Errorf("create recovery role marker: %w", err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("write recovery role marker: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync recovery role marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Sync the parent directory so the new entry survives a power loss.
	if d, err := fs.OpenDir(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// hasNonVotingMarker reports whether shardID must be started as a non-voting
// replica. A corrupt or unreadable marker is treated as present: non-voting is
// the safe direction.
func (m *Manager) hasNonVotingMarker(shardID uint64) bool {
	ok, err := m.readNonVotingMarker(shardID)
	if err != nil {
		m.log.Warnf("[%d] unreadable recovery role marker, assuming non-voting: %v", shardID, err)
		return true
	}
	return ok
}

func (m *Manager) readNonVotingMarker(shardID uint64) (bool, error) {
	f, err := m.markerFS().Open(m.roleMarkerPath(shardID))
	if err != nil {
		// Missing marker is the common case. Any other failure must remain
		// visible to the caller; treating it as absent risks a voter start over
		// an unapplied AddNonVoting entry.
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	if len(data) < 4 {
		return false, errRoleMarkerCorrupt
	}
	payload := data[:len(data)-4]
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return false, errRoleMarkerCorrupt
	}
	return true, nil
}

// clearNonVotingMarker removes the marker for shardID. It is a no-op when the
// marker does not exist.
func (m *Manager) clearNonVotingMarker(shardID uint64) error {
	fs := m.markerFS()
	p := m.roleMarkerPath(shardID)
	if _, err := fs.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return fs.Remove(p)
}

// listNonVotingMarkers returns the shard IDs that currently carry a marker.
func (m *Manager) listNonVotingMarkers() []uint64 {
	entries, err := m.markerFS().List(m.roleMarkerDir())
	if err != nil {
		return nil
	}
	var ids []uint64
	for _, e := range entries {
		id, err := strconv.ParseUint(path.Base(e), 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

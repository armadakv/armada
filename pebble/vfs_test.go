// Copyright JAMF Software, LLC

package pebble

import (
	"testing"

	gvfs "github.com/armadakv/armada/vfs"
	pvfs "github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func TestFS_Create(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	file, err := fs.Create("testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	require.NotNil(t, file)
}

func TestFS_Open(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	_, err := fs.Create("testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	file, err := fs.Open("testfile")
	require.NoError(t, err)
	require.NotNil(t, file)
}

func TestFS_Remove(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	_, err := fs.Create("testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	err = fs.Remove("testfile")
	require.NoError(t, err)
	_, err = fs.Open("testfile")
	require.Error(t, err)
}

func TestFS_Stat(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	_, err := fs.Create("testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	info, err := fs.Stat("testfile")
	require.NoError(t, err)
	require.NotNil(t, info)
}

func TestFS_Link(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	_, err := fs.Create("testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	err = fs.Link("testfile", "testfile_link")
	require.NoError(t, err)
	file, err := fs.Open("testfile_link")
	require.NoError(t, err)
	require.NotNil(t, file)
}

func TestFS_Rename(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	_, err := fs.Create("testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	err = fs.Rename("testfile", "renamedfile")
	require.NoError(t, err)
	file, err := fs.Open("renamedfile")
	require.NoError(t, err)
	require.NotNil(t, file)
}

func TestFS_MkdirAll(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	err := fs.MkdirAll("testdir", 0o755)
	require.NoError(t, err)
	info, err := fs.Stat("testdir")
	require.NoError(t, err)
	require.NotNil(t, info)
}

func TestFS_List(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	err := fs.MkdirAll("testdir", 0o755)
	require.NoError(t, err)
	_, err = fs.Create("testdir/testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	files, err := fs.List("testdir")
	require.NoError(t, err)
	require.Contains(t, files, "testfile")
}

func Test_fileCompat_Stat(t *testing.T) {
	fs := NewPebbleFS(gvfs.NewMem())
	_, err := fs.Create("testfile", pvfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	file, err := fs.Open("testfile")
	require.NoError(t, err)
	require.NotNil(t, file)
	info, err := file.Stat()
	require.NoError(t, err)
	require.NotNil(t, info)
	require.NotPanics(t, func() {
		_ = info.DeviceID()
	})
}

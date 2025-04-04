// Copyright JAMF Software, LLC

package pebble

import (
	"io"
	"os"
	"reflect"

	gvfs "github.com/armadakv/armada/vfs"
	pvfs "github.com/cockroachdb/pebble/v2/vfs"
)

type fileCompat struct {
	gvfs.File
}

func (f *fileCompat) Stat() (pvfs.FileInfo, error) {
	s, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return &statCompat{s}, nil
}

type statCompat struct {
	gvfs.FileInfo
}

func (s *statCompat) DeviceID() pvfs.DeviceID {
	id := s.FileInfo.DeviceID()
	pid := pvfs.DeviceID{}
	v := reflect.ValueOf(&pid).Elem()
	*(*uint64)(v.FieldByName("major").Addr().UnsafePointer()) = uint64(id.Major())
	*(*uint64)(v.FieldByName("minor").Addr().UnsafePointer()) = uint64(id.Minor())
	return pid
}

// FS is a wrapper struct that implements the pebble/vfs.FS interface.
type FS struct {
	fs gvfs.FS
}

// NewPebbleFS creates a new pebble/vfs.FS instance.
func NewPebbleFS(fs gvfs.FS) pvfs.FS {
	return &FS{fs}
}

// GetDiskUsage ...
func (p *FS) GetDiskUsage(path string) (pvfs.DiskUsage, error) {
	du, err := p.fs.GetDiskUsage(path)
	if err != nil {
		return pvfs.DiskUsage{}, err
	}
	return pvfs.DiskUsage{
		AvailBytes: du.AvailBytes,
		TotalBytes: du.TotalBytes,
		UsedBytes:  du.UsedBytes,
	}, err
}

// Create ...
func (p *FS) Create(name string, category pvfs.DiskWriteCategory) (pvfs.File, error) {
	file, err := p.fs.Create(name, gvfs.DiskWriteCategory(category))
	return &fileCompat{file}, err
}

// Link ...
func (p *FS) Link(oldname, newname string) error {
	return p.fs.Link(oldname, newname)
}

// Open ...
func (p *FS) Open(name string, opts ...pvfs.OpenOption) (pvfs.File, error) {
	f, err := p.fs.Open(name)
	if err != nil {
		return nil, err
	}
	file := &fileCompat{f}
	for _, opt := range opts {
		opt.Apply(file)
	}
	return file, nil
}

func (p *FS) OpenReadWrite(name string, category pvfs.DiskWriteCategory, opts ...pvfs.OpenOption) (pvfs.File, error) {
	f, err := p.fs.Open(name)
	if err != nil {
		return nil, err
	}
	file := &fileCompat{f}
	for _, opt := range opts {
		opt.Apply(file)
	}
	return file, nil
}

// OpenDir ...
func (p *FS) OpenDir(name string) (pvfs.File, error) {
	dir, err := p.fs.OpenDir(name)
	return &fileCompat{dir}, err
}

// Remove ...
func (p *FS) Remove(name string) error {
	return p.fs.Remove(name)
}

// RemoveAll ...
func (p *FS) RemoveAll(name string) error {
	return p.fs.RemoveAll(name)
}

// Rename ...
func (p *FS) Rename(oldname, newname string) error {
	return p.fs.Rename(oldname, newname)
}

// ReuseForWrite ...
func (p *FS) ReuseForWrite(oldname, newname string, category pvfs.DiskWriteCategory) (pvfs.File, error) {
	file, err := p.fs.ReuseForWrite(oldname, newname, gvfs.DiskWriteCategory(category))
	return &fileCompat{file}, err
}

// MkdirAll ...
func (p *FS) MkdirAll(dir string, perm os.FileMode) error {
	return p.fs.MkdirAll(dir, perm)
}

// Lock ...
func (p *FS) Lock(name string) (io.Closer, error) {
	return p.fs.Lock(name)
}

// List ...
func (p *FS) List(dir string) ([]string, error) {
	return p.fs.List(dir)
}

// Stat ...
func (p *FS) Stat(name string) (pvfs.FileInfo, error) {
	stat, err := p.fs.Stat(name)
	return &statCompat{stat}, err
}

// PathBase ...
func (p *FS) PathBase(path string) string {
	return p.fs.PathBase(path)
}

// PathJoin ...
func (p *FS) PathJoin(elem ...string) string {
	return p.fs.PathJoin(elem...)
}

// PathDir ...
func (p *FS) PathDir(path string) string {
	return p.fs.PathDir(path)
}

func (p *FS) Unwrap() pvfs.FS {
	return nil
}

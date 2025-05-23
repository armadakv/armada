// Copyright 2017-2019 Lei Ni (nilei81@gmail.com) and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/armadakv/armada/vfs"

	"github.com/cockroachdb/errors"

	"github.com/armadakv/armada/raft/config"
	"github.com/armadakv/armada/raft/internal/fileutil"
	"github.com/armadakv/armada/raft/internal/id"
	"github.com/armadakv/armada/raft/raftio"
	"github.com/armadakv/armada/raft/raftpb"
)

const (
	singleNodeHostTestDir = "test_nodehost_dir_safe_to_delete"
	testLogDBName         = "test-name"
	testBinVer            = raftio.LogDBBinVersion
	testAddress           = "localhost:1111"
	testDeploymentID      = 100
)

func getTestNodeHostConfig() config.NodeHostConfig {
	return config.NodeHostConfig{
		WALDir:         singleNodeHostTestDir,
		NodeHostDir:    singleNodeHostTestDir,
		RTTMillisecond: 50,
		RaftAddress:    testAddress,
	}
}

func TestCheckNodeHostDirWorksWhenEverythingMatches(t *testing.T) {
	fs := vfs.NewMem()
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	func() {
		c := getTestNodeHostConfig()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic not expected")
			}
		}()
		env, err := NewEnv(c, fs)
		if err != nil {
			t.Fatalf("failed to new environment %v", err)
		}
		if _, _, err := env.CreateNodeHostDir(testDeploymentID); err != nil {
			t.Fatalf("%v", err)
		}
		dir, _ := env.getDataDirs()
		testName := "test-name"
		cfg := config.NodeHostConfig{
			Expert:       config.GetDefaultExpertConfig(),
			RaftAddress:  testAddress,
			DeploymentID: testDeploymentID,
		}
		status := raftpb.RaftDataStatus{
			Address:      testAddress,
			BinVer:       raftio.LogDBBinVersion,
			LogdbType:    testName,
			Hostname:     env.hostname,
			DeploymentId: testDeploymentID,
		}
		err = fileutil.CreateFlagFile(dir, flagFilename, &status, fs)
		if err != nil {
			t.Errorf("failed to create flag file %v", err)
		}
		if err := env.CheckNodeHostDir(cfg,
			raftio.LogDBBinVersion, testName); err != nil {
			t.Fatalf("check node host dir failed %v", err)
		}
	}()
	reportLeakedFD(fs, t)
}

func testNodeHostDirectoryDetectsMismatches(t *testing.T,
	addr string, hostname string, binVer uint32, name string,
	hardHashMismatch bool, expErr error, fs vfs.FS,
) {
	c := getTestNodeHostConfig()
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	env, err := NewEnv(c, fs)
	if err != nil {
		t.Fatalf("failed to new environment %v", err)
	}
	if _, _, err := env.CreateNodeHostDir(testDeploymentID); err != nil {
		t.Fatalf("%v", err)
	}
	dir, _ := env.getDataDirs()
	cfg := config.NodeHostConfig{
		Expert:       config.GetDefaultExpertConfig(),
		DeploymentID: testDeploymentID,
		RaftAddress:  testAddress,
	}

	status := raftpb.RaftDataStatus{
		Address:             addr,
		BinVer:              binVer,
		LogdbType:           name,
		Hostname:            hostname,
		AddressByNodeHostId: false,
	}
	if hardHashMismatch {
		status.HardHash = 1
	}
	err = fileutil.CreateFlagFile(dir, flagFilename, &status, fs)
	if err != nil {
		t.Errorf("failed to create flag file %v", err)
	}
	err = env.CheckNodeHostDir(cfg, testBinVer, testLogDBName)
	if err != expErr {
		t.Errorf("expect err %v, got %v", expErr, err)
	}
	reportLeakedFD(fs, t)
}

func TestCanDetectMismatchedHostname(t *testing.T) {
	fs := vfs.NewMem()
	testNodeHostDirectoryDetectsMismatches(t,
		testAddress, "incorrect-hostname", raftio.LogDBBinVersion,
		testLogDBName, false, ErrHostnameChanged, fs)
}

func TestCanDetectMismatchedLogDBName(t *testing.T) {
	fs := vfs.NewMem()
	testNodeHostDirectoryDetectsMismatches(t,
		testAddress, "", raftio.LogDBBinVersion,
		"incorrect name", false, ErrLogDBType, fs)
}

func TestCanDetectMismatchedBinVer(t *testing.T) {
	fs := vfs.NewMem()
	testNodeHostDirectoryDetectsMismatches(t,
		testAddress, "", raftio.LogDBBinVersion+1,
		testLogDBName, false, ErrIncompatibleData, fs)
}

func TestCanDetectMismatchedAddress(t *testing.T) {
	fs := vfs.NewMem()
	testNodeHostDirectoryDetectsMismatches(t,
		"invalid:12345", "", raftio.LogDBBinVersion,
		testLogDBName, false, ErrNotOwner, fs)
}

func TestLockFileCanBeLockedAndUnlocked(t *testing.T) {
	fs := vfs.NewMem()
	c := getTestNodeHostConfig()
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	env, err := NewEnv(c, fs)
	if err != nil {
		t.Fatalf("failed to new environment %v", err)
	}
	if _, _, err := env.CreateNodeHostDir(c.DeploymentID); err != nil {
		t.Fatalf("%v", err)
	}
	if err := env.LockNodeHostDir(); err != nil {
		t.Fatalf("failed to lock the directory %v", err)
	}
	if err := env.Close(); err != nil {
		t.Fatalf("failed to stop env %v", err)
	}
	reportLeakedFD(fs, t)
}

func TestNodeHostIDCanBeGenerated(t *testing.T) {
	fs := vfs.NewMem()
	if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
		t.Fatalf("%v", err)
	}
	if err := fs.MkdirAll(singleNodeHostTestDir, 0o755); err != nil {
		t.Fatalf("%v", err)
	}
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	c := getTestNodeHostConfig()
	env, err := NewEnv(c, fs)
	if err != nil {
		t.Fatalf("failed to create env %v", err)
	}
	v, err := env.PrepareNodeHostID("")
	if err != nil {
		t.Fatalf("failed to prepare nodehost id %v", err)
	}
	if len(v.String()) == 0 {
		t.Fatalf("failed to generate UUID")
	}
}

func TestPrepareNodeHostIDWillReportNodeHostIDChange(t *testing.T) {
	fs := vfs.NewMem()
	if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
		t.Fatalf("%v", err)
	}
	if err := fs.MkdirAll(singleNodeHostTestDir, 0o755); err != nil {
		t.Fatalf("%v", err)
	}
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	c := getTestNodeHostConfig()
	env, err := NewEnv(c, fs)
	if err != nil {
		t.Fatalf("failed to create env %v", err)
	}
	v, err := env.PrepareNodeHostID("")
	if err != nil {
		t.Fatalf("failed to prepare nodehost id %v", err)
	}
	// using the same uuid is okay
	v2, err := env.PrepareNodeHostID(v.String())
	if err != nil {
		t.Fatalf("failed to prepare nodehost id %v", err)
	}
	if v2.String() != v.String() {
		t.Fatalf("returned UUID is unexpected")
	}
	// change it is not allowed
	v3 := id.New()
	_, err = env.PrepareNodeHostID(v3.String())
	if !errors.Is(err, ErrNodeHostIDChanged) {
		t.Fatalf("failed to report ErrNodeHostIDChanged")
	}
}

func TestRemoveSavedSnapshots(t *testing.T) {
	fs := vfs.NewMem()
	if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
		t.Fatalf("%v", err)
	}
	if err := fs.MkdirAll(singleNodeHostTestDir, 0o755); err != nil {
		t.Fatalf("%v", err)
	}
	defer func() {
		if err := fs.RemoveAll(singleNodeHostTestDir); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	for i := 0; i < 16; i++ {
		ssdir := fs.PathJoin(singleNodeHostTestDir, fmt.Sprintf("snapshot-%X", i))
		if err := fs.MkdirAll(ssdir, 0o755); err != nil {
			t.Fatalf("failed to mkdir %v", err)
		}
	}
	for i := 1; i <= 2; i++ {
		ssdir := fs.PathJoin(singleNodeHostTestDir, fmt.Sprintf("mydata-%X", i))
		if err := fs.MkdirAll(ssdir, 0o755); err != nil {
			t.Fatalf("failed to mkdir %v", err)
		}
	}
	if err := removeSavedSnapshots(singleNodeHostTestDir, fs); err != nil {
		t.Fatalf("failed to remove saved snapshots %v", err)
	}
	files, err := fs.List(singleNodeHostTestDir)
	if err != nil {
		t.Fatalf("failed to read dir %v", err)
	}
	for _, fn := range files {
		fi, err := fs.Stat(fs.PathJoin(singleNodeHostTestDir, fn))
		if err != nil {
			t.Fatalf("failed to get stat %v", err)
		}
		if !fi.IsDir() {
			t.Errorf("found unexpected file %v", fi)
		}
		if fi.Name() != "mydata-1" && fi.Name() != "mydata-2" {
			t.Errorf("unexpected dir found %s", fi.Name())
		}
	}
	reportLeakedFD(fs, t)
}

func TestWALDirCanBeSet(t *testing.T) {
	walDir := "d2-wal-dir-name"
	nhConfig := config.NodeHostConfig{
		NodeHostDir: "d1",
		WALDir:      walDir,
	}
	fs := vfs.NewMem()
	c, err := NewEnv(nhConfig, fs)
	if err != nil {
		t.Fatalf("failed to get environment %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Fatalf("failed to stop the env %v", err)
		}
	}()
	dir, lldir := c.GetLogDBDirs(12345)
	if dir == lldir {
		t.Errorf("wal dir not considered")
	}
	if !strings.Contains(lldir, walDir) {
		t.Errorf("wal dir not used, %s", lldir)
	}
	if strings.Contains(dir, walDir) {
		t.Errorf("wal dir appeared in node host dir, %s", dir)
	}
}

func TestCompatibleLogDBType(t *testing.T) {
	tests := []struct {
		saved      string
		name       string
		compatible bool
	}{
		{"sharded-rocksdb", "sharded-pebble", true},
		{"sharded-pebble", "sharded-rocksdb", true},
		{"pebble", "rocksdb", true},
		{"rocksdb", "pebble", true},
		{"pebble", "tee", false},
		{"tee", "pebble", false},
		{"rocksdb", "tee", false},
		{"tee", "rocksdb", false},
		{"tee", "tee", true},
		{"", "tee", false},
		{"tee", "", false},
		{"tee", "inmem", false},
	}
	for idx, tt := range tests {
		if r := compatibleLogDBType(tt.saved, tt.name); r != tt.compatible {
			t.Errorf("%d, compatibleLogDBType failed, want %t, got %t", idx, tt.compatible, r)
		}
	}
}

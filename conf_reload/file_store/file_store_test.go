// Copyright(c) 2026 The Rainway AI Gateway (壬远AI网关) Authors.
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

package file_store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rainway-ai-gateway/conf-agent/xfile"
)

func TestNewFileStoreDefaultVersionKeepCount(t *testing.T) {
	fs, err := NewFileStore("/tmp/test", nil, 0)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	if fs.VersionKeepCount != 1 {
		t.Fatalf("expect VersionKeepCount=1, got %d", fs.VersionKeepCount)
	}
}

func TestStoreFile2TmpDirWritesVersionMarker(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	version := "20260101120000"
	files := map[string][]byte{
		"test.json": []byte(`{"Version":"20260101120000"}`),
	}

	if err := fs.StoreFile2TmpDir(context.Background(), version, files); err != nil {
		t.Fatalf("StoreFile2TmpDir fail, err: %v", err)
	}

	markerFile := filepath.Join(fs.tmpDir(version), versionMarkerFile)
	content, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("read marker file fail, err: %v", err)
	}

	if string(content) != version {
		t.Fatalf("expect marker content %s, got %s", version, string(content))
	}
}

// CopyFiles entries that are directories must keep their entry name in the
// versioned config dir (e.g. tls_conf version dir needs client_ca/ and
// client_crl/ subdirs for BFE to load a self-contained config unit).
func TestStoreFile2TmpDirCopyFilesKeepDirEntry(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "tls_conf")
	if err := os.MkdirAll(filepath.Join(confDir, "client_ca"), 0755); err != nil {
		t.Fatalf("mkdir client_ca fail, err: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(confDir, "client_crl"), 0755); err != nil {
		t.Fatalf("mkdir client_crl fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "client_ca", "example_ca.crt"), []byte("ca"), 0644); err != nil {
		t.Fatalf("write ca file fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "tls_rule_conf.data"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write tls rule fail, err: %v", err)
	}

	fs, err := NewFileStore(confDir, []string{"client_ca", "client_crl", "tls_rule_conf.data"}, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	version := "20260101120000"
	if err := fs.StoreFile2TmpDir(context.Background(), version, nil); err != nil {
		t.Fatalf("StoreFile2TmpDir fail, err: %v", err)
	}

	versionDir := fs.tmpDir(version)
	for _, rel := range []string{
		filepath.Join("client_ca", "example_ca.crt"),
		filepath.Join("client_crl"),
		"tls_rule_conf.data",
	} {
		if _, err := os.Stat(filepath.Join(versionDir, rel)); err != nil {
			t.Fatalf("expect %s in version dir, err: %v", rel, err)
		}
	}

	// directory contents must not be flattened into the version dir root
	if _, err := os.Stat(filepath.Join(versionDir, "example_ca.crt")); !os.IsNotExist(err) {
		t.Fatalf("ca file should not exist at version dir root")
	}
}

func TestCleanupOldVersionsKeepRecentN(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	// create old version directories with explicit mod times (older first)
	versions := []string{
		"20260101120001",
		"20260101120002",
		"20260101120003",
		"20260101120004",
	}
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i, v := range versions {
		createVersionDir(t, fs.tmpDir(v), v)
		setDirModTime(t, fs.tmpDir(v), baseTime.Add(time.Duration(i)*time.Second))
	}

	// create current symlink target with the newest mod time
	currentVersion := "20260101120005"
	currentDir := fs.tmpDir(currentVersion)
	createVersionDir(t, currentDir, currentVersion)
	setDirModTime(t, currentDir, baseTime.Add(time.Duration(len(versions))*time.Second))
	if err := xfile.FileLink(currentDir, confDir); err != nil {
		t.Fatalf("create link fail, err: %v", err)
	}

	if err := fs.cleanupOldVersions(context.Background(), fs.VersionKeepCount); err != nil {
		t.Fatalf("cleanupOldVersions fail, err: %v", err)
	}

	// current + one previous should be kept
	assertDirExists(t, currentDir)
	assertDirExists(t, fs.tmpDir("20260101120004"))

	// older versions should be removed
	assertDirNotExists(t, fs.tmpDir("20260101120003"))
	assertDirNotExists(t, fs.tmpDir("20260101120002"))
	assertDirNotExists(t, fs.tmpDir("20260101120001"))
}

func TestCleanupOldVersionsIgnoreDirsWithoutMarker(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	currentVersion := "20260101120002"
	currentDir := fs.tmpDir(currentVersion)
	createVersionDir(t, currentDir, currentVersion)
	if err := xfile.FileLink(currentDir, confDir); err != nil {
		t.Fatalf("create link fail, err: %v", err)
	}

	// directory with marker
	createVersionDir(t, fs.tmpDir("20260101120001"), "20260101120001")

	// non-empty directory without marker (user-managed, must be left alone)
	noMarkerDir := fs.tmpDir("20260101120000") + "_manual"
	if err := os.MkdirAll(noMarkerDir, os.ModePerm); err != nil {
		t.Fatalf("mkdir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(noMarkerDir, "manual.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("write manual file fail, err: %v", err)
	}

	if err := fs.cleanupOldVersions(context.Background(), 1); err != nil {
		t.Fatalf("cleanupOldVersions fail, err: %v", err)
	}

	assertDirNotExists(t, fs.tmpDir("20260101120001"))
	assertDirExists(t, noMarkerDir)
}

func TestUpdateDefaultConfDirCreatesLinkWhenConfDirMissing(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	version := "20260101120000"
	createVersionDir(t, fs.tmpDir(version), version)

	if err := fs.UpdateDefaultConfDir(context.Background(), version); err != nil {
		t.Fatalf("UpdateDefaultConfDir fail, err: %v", err)
	}

	dest, err := filepath.EvalSymlinks(confDir)
	if err != nil {
		t.Fatalf("eval symlink fail, err: %v", err)
	}

	absExpected, _ := filepath.Abs(fs.tmpDir(version))
	absDest, _ := filepath.Abs(dest)
	if absDest != absExpected {
		t.Fatalf("expect symlink target %s, got %s", absExpected, absDest)
	}
}

func TestUpdateDefaultConfDirBacksUpRegularDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	// create a regular directory as ConfDir
	if err := os.MkdirAll(confDir, os.ModePerm); err != nil {
		t.Fatalf("mkdir fail, err: %v", err)
	}
	oldFile := filepath.Join(confDir, "old.json")
	if err := os.WriteFile(oldFile, []byte("old"), 0644); err != nil {
		t.Fatalf("write old file fail, err: %v", err)
	}

	version := "20260101120000"
	createVersionDir(t, fs.tmpDir(version), version)

	if err := fs.UpdateDefaultConfDir(context.Background(), version); err != nil {
		t.Fatalf("UpdateDefaultConfDir fail, err: %v", err)
	}

	// ConfDir should now be a link to tmp dir
	dest, err := filepath.EvalSymlinks(confDir)
	if err != nil {
		t.Fatalf("eval symlink fail, err: %v", err)
	}

	absExpected, _ := filepath.Abs(fs.tmpDir(version))
	absDest, _ := filepath.Abs(dest)
	if absDest != absExpected {
		t.Fatalf("expect symlink target %s, got %s", absExpected, absDest)
	}

	// old directory should be backed up
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read dir fail, err: %v", err)
	}

	foundBackup := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "mod_demo_") && strings.HasSuffix(entry.Name(), ".backup") {
			foundBackup = true
			break
		}
	}

	if !foundBackup {
		t.Fatalf("old conf dir should be backed up")
	}
}

func TestUpdateDefaultConfDirSwitchesLinkTarget(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	oldVersion := "20260101120000"
	newVersion := "20260101120001"
	createVersionDir(t, fs.tmpDir(oldVersion), oldVersion)
	createVersionDir(t, fs.tmpDir(newVersion), newVersion)

	// create initial link
	if err := xfile.FileLink(fs.tmpDir(oldVersion), confDir); err != nil {
		t.Fatalf("create initial link fail, err: %v", err)
	}

	// switch to new version
	if err := fs.UpdateDefaultConfDir(context.Background(), newVersion); err != nil {
		t.Fatalf("UpdateDefaultConfDir fail, err: %v", err)
	}

	dest, err := filepath.EvalSymlinks(confDir)
	if err != nil {
		t.Fatalf("eval symlink fail, err: %v", err)
	}

	absExpected, _ := filepath.Abs(fs.tmpDir(newVersion))
	absDest, _ := filepath.Abs(dest)
	if absDest != absExpected {
		t.Fatalf("expect symlink target %s, got %s", absExpected, absDest)
	}

	// old version should be removed (keep=2, current + no previous because only one old exists)
	// Actually keep=2 means current + 1 previous. But we only have one old version.
	// Since newVersion is current, oldVersion is the previous, so it should be kept.
	assertDirExists(t, fs.tmpDir(oldVersion))
}

func setDirModTime(t *testing.T, dir string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(dir, modTime, modTime); err != nil {
		t.Fatalf("set dir mod time fail, dir: %s, err: %v", dir, err)
	}
}

func createVersionDir(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		t.Fatalf("mkdir fail, dir: %s, err: %v", dir, err)
	}

	markerFile := filepath.Join(dir, versionMarkerFile)
	if err := os.WriteFile(markerFile, []byte(version), 0644); err != nil {
		t.Fatalf("write marker fail, file: %s, err: %v", markerFile, err)
	}

	// touch a config file
	configFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configFile, []byte(`{"Version":"`+version+`"}`), 0644); err != nil {
		t.Fatalf("write config fail, file: %s, err: %v", configFile, err)
	}
}

func assertDirExists(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("expect dir %s to exist, err: %v", dir, err)
	}
}

func assertDirNotExists(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expect dir %s to not exist", dir)
	}
}

func TestRenameDirSameDevice(t *testing.T) {
	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "src")
	dst := filepath.Join(tmpDir, "dst")

	if err := os.MkdirAll(src, os.ModePerm); err != nil {
		t.Fatalf("mkdir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "test.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write file fail, err: %v", err)
	}

	if err := renameDir(src, dst); err != nil {
		t.Fatalf("renameDir fail, err: %v", err)
	}

	assertDirNotExists(t, src)
	assertDirExists(t, dst)

	content, err := os.ReadFile(filepath.Join(dst, "test.txt"))
	if err != nil {
		t.Fatalf("read file fail, err: %v", err)
	}
	if string(content) != "hello" {
		t.Fatalf("expect content 'hello', got '%s'", string(content))
	}
}

func TestRenameDirCrossDeviceFallback(t *testing.T) {
	// Requires Linux + root to mount tmpfs. Skip otherwise.
	if os.Getuid() != 0 {
		t.Skip("requires root to mount tmpfs for cross-device test")
	}

	tmpDir := t.TempDir()
	src := filepath.Join(tmpDir, "src")
	dst := filepath.Join(tmpDir, "dst")

	if err := os.MkdirAll(src, os.ModePerm); err != nil {
		t.Fatalf("mkdir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "test.txt"), []byte("cross-device"), 0644); err != nil {
		t.Fatalf("write file fail, err: %v", err)
	}

	tmpfsDir := filepath.Join(tmpDir, "tmpfs")
	if err := os.MkdirAll(tmpfsDir, os.ModePerm); err != nil {
		t.Fatalf("mkdir tmpfs fail, err: %v", err)
	}

	cmd := exec.Command("mount", "-t", "tmpfs", "-o", "size=10M", "tmpfs", tmpfsDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mount tmpfs fail (need root+linux): %v, output: %s", err, string(out))
	}
	defer exec.Command("umount", tmpfsDir).Run()

	dst = filepath.Join(tmpfsDir, "dst")

	if err := renameDir(src, dst); err != nil {
		t.Fatalf("renameDir cross-device fail, err: %v", err)
	}

	assertDirNotExists(t, src)
	assertDirExists(t, dst)

	content, err := os.ReadFile(filepath.Join(dst, "test.txt"))
	if err != nil {
		t.Fatalf("read file fail, err: %v", err)
	}
	if string(content) != "cross-device" {
		t.Fatalf("expect content 'cross-device', got '%s'", string(content))
	}
}

// TestStoreFile2TmpDirMissingCopyFileSkips verifies that a missing CopyFiles
// source is skipped instead of aborting the store (conf-agent#20: the skip
// branch used to be dead code because the wrapped not-exist error was not
// recognized, which permanently stalled the reload loop).
func TestStoreFile2TmpDirMissingCopyFileSkips(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	if err := os.MkdirAll(confDir, 0755); err != nil {
		t.Fatalf("mkdir conf dir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "tls_rule_conf.data"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write rule file fail, err: %v", err)
	}

	fs, err := NewFileStore(confDir, []string{"no_such.data", "tls_rule_conf.data"}, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	files := map[string][]byte{"test.json": []byte(`{"Version":"v1"}`)}
	if err := fs.StoreFile2TmpDir(context.Background(), "v1", files); err != nil {
		t.Fatalf("StoreFile2TmpDir should skip missing copy file, err: %v", err)
	}

	versionDir := fs.tmpDir("v1")
	if _, err := os.Stat(filepath.Join(versionDir, "tls_rule_conf.data")); err != nil {
		t.Fatalf("existing copy file should be copied, err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(versionDir, "test.json")); err != nil {
		t.Fatalf("fetched file should be written, err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(versionDir, versionMarkerFile)); err != nil {
		t.Fatalf("version marker should be written, err: %v", err)
	}
}

// TestStoreFile2TmpDirActiveDirCollision verifies the version-dir name
// collision guard: when the incoming version equals the active dir name
// (second-precision version stamps can collide across topics or agent
// processes), StoreFile2TmpDir must not delete the active dir, must not
// copy files onto themselves (which would truncate them), and must write
// the fetched content in place (conf-agent#20).
func TestStoreFile2TmpDirActiveDirCollision(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	activeDir := fs_tmpDir(confDir, "v1")
	if err := os.MkdirAll(activeDir, 0755); err != nil {
		t.Fatalf("mkdir active dir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "cluster_table.data"), []byte("old content"), 0644); err != nil {
		t.Fatalf("write cluster table fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "other.data"), []byte("keep me"), 0644); err != nil {
		t.Fatalf("write other file fail, err: %v", err)
	}
	if err := os.Symlink(activeDir, confDir); err != nil {
		t.Fatalf("symlink conf dir fail, err: %v", err)
	}

	fs, err := NewFileStore(confDir, []string{"cluster_table.data", "other.data"}, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	content := []byte(`{"Version":"v1","Config":{}}`)
	files := map[string][]byte{"cluster_table.data": content}
	if err := fs.StoreFile2TmpDir(context.Background(), "v1", files); err != nil {
		t.Fatalf("in-place store should succeed, err: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(activeDir, "cluster_table.data"))
	if err != nil {
		t.Fatalf("read cluster table fail, err: %v", err)
	}
	if string(got) != string(formatJSONWithIndent(content)) {
		t.Fatalf("cluster table = %q, want overwritten content %q (self-copy would truncate it)",
			got, formatJSONWithIndent(content))
	}

	other, err := os.ReadFile(filepath.Join(activeDir, "other.data"))
	if err != nil {
		t.Fatalf("read other file fail, err: %v", err)
	}
	if string(other) != "keep me" {
		t.Fatalf("unrelated file = %q, want %q", other, "keep me")
	}
}

func fs_tmpDir(confDir, version string) string {
	return confDir + "_" + version
}

// TestStoreFile2TmpDirCleansHalfDoneDir verifies that a failed store removes
// its half-written version dir so unmarked dirs do not pile up
// (conf-agent#20).
func TestStoreFile2TmpDirCleansHalfDoneDir(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	// a directory entry in CopyFiles lands in the version dir as a directory
	if err := os.MkdirAll(filepath.Join(confDir, "conflict"), 0755); err != nil {
		t.Fatalf("mkdir conflict dir fail, err: %v", err)
	}

	fs, err := NewFileStore(confDir, []string{"conflict"}, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	// writing a file over the directory copied from CopyFiles must fail
	files := map[string][]byte{"conflict": []byte(`{"Version":"v1"}`)}
	if err := fs.StoreFile2TmpDir(context.Background(), "v1", files); err == nil {
		t.Fatal("StoreFile2TmpDir should fail when target path is a directory")
	}

	if _, err := os.Stat(fs.tmpDir("v1")); !os.IsNotExist(err) {
		t.Fatalf("half-done version dir should be cleaned, err: %v", err)
	}
}

// TestUpdateDefaultConfDirRejectsDirWithoutMarker verifies that the symlink
// is never switched to a dir without the version marker (e.g. a
// half-written one), which would break BFE (conf-agent#20).
func TestUpdateDefaultConfDirRejectsDirWithoutMarker(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")
	// version dir exists but has no marker
	if err := os.MkdirAll(fs_tmpDir(confDir, "v1"), 0755); err != nil {
		t.Fatalf("mkdir version dir fail, err: %v", err)
	}

	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	if err := fs.UpdateDefaultConfDir(context.Background(), "v1"); err == nil {
		t.Fatal("UpdateDefaultConfDir should reject dir without version marker")
	}
	if _, err := os.Lstat(confDir); !os.IsNotExist(err) {
		t.Fatalf("conf dir link should not be created, err: %v", err)
	}
}

// TestCleanupOldVersionsRemovesEmptyUnmarkedDirs verifies the best-effort
// sweep of stale empty dirs without a version marker (half-written dirs from
// interrupted stores), while non-empty unmarked dirs and marked dirs are
// left alone (conf-agent#20).
func TestCleanupOldVersionsRemovesEmptyUnmarkedDirs(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, "mod_demo")

	activeDir := fs_tmpDir(confDir, "v3")
	if err := os.MkdirAll(activeDir, 0755); err != nil {
		t.Fatalf("mkdir active dir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, versionMarkerFile), []byte("v3"), 0644); err != nil {
		t.Fatalf("write marker fail, err: %v", err)
	}
	if err := os.Symlink(activeDir, confDir); err != nil {
		t.Fatalf("symlink conf dir fail, err: %v", err)
	}

	markedOld := fs_tmpDir(confDir, "v2")
	if err := os.MkdirAll(markedOld, 0755); err != nil {
		t.Fatalf("mkdir marked old dir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markedOld, versionMarkerFile), []byte("v2"), 0644); err != nil {
		t.Fatalf("write marker fail, err: %v", err)
	}

	emptyUnmarked := fs_tmpDir(confDir, "v1")
	if err := os.MkdirAll(emptyUnmarked, 0755); err != nil {
		t.Fatalf("mkdir empty unmarked dir fail, err: %v", err)
	}

	nonEmptyUnmarked := fs_tmpDir(confDir, "v0")
	if err := os.MkdirAll(nonEmptyUnmarked, 0755); err != nil {
		t.Fatalf("mkdir non-empty unmarked dir fail, err: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyUnmarked, "manual.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("write manual file fail, err: %v", err)
	}

	fs, err := NewFileStore(confDir, nil, 2)
	if err != nil {
		t.Fatalf("NewFileStore fail, err: %v", err)
	}

	if err := fs.cleanupOldVersions(context.Background(), 2); err != nil {
		t.Fatalf("cleanupOldVersions fail, err: %v", err)
	}

	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("active dir should be kept, err: %v", err)
	}
	if _, err := os.Stat(markedOld); err != nil {
		t.Fatalf("marked old dir should be kept (VersionKeepCount=2), err: %v", err)
	}
	if _, err := os.Stat(emptyUnmarked); !os.IsNotExist(err) {
		t.Fatalf("empty unmarked dir should be removed, err: %v", err)
	}
	if _, err := os.Stat(nonEmptyUnmarked); err != nil {
		t.Fatalf("non-empty unmarked dir should be kept, err: %v", err)
	}
}

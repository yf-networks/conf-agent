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

// Package selfheal_test 集成验证 conf-agent#20 的修复：
// 激活目录被清空 / CopyFiles 源缺失时 reload 自动恢复，不卡在永久失败循环。
package selfheal_test

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rainway-ai-gateway/conf-agent/integration/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const module = "mod_demo"

// TestSelfHeal_EmptiedActiveDirRecovers reproduces the conf-agent#20
// production incident end to end:
//
//  1. normal delivery to v1; ops places a CopyFiles-only file (static.data)
//     into the activated dir;
//  2. version bump delivers v2, copying static.data into the v2 dir;
//  3. the activated v2 dir is wiped (the incident's first empty dir);
//  4. without any manual intervention the next reload cycle must recover:
//     the local version read fails so the server returns the full config,
//     whose version equals the active dir name (version collision) — the
//     store must rebuild in place instead of RemoveAll-ing the active dir,
//     skip the now-missing CopyFiles source, rewrite the config files and
//     marker, and switch the symlink back.
//
// Before the fix this loop was permanently stuck at the keepFile error and
// the symlink stayed on the empty dir.
func TestSelfHeal_EmptiedActiveDirRecovers(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, module)

	confServer := testutil.NewMockConfigServer(t, module)
	bfeServer := testutil.NewMockBFEServer(t, module)

	confURL, _ := url.Parse(confServer.URL())
	bfeURL, _ := url.Parse(bfeServer.URL())

	runner := testutil.StartReloader(t, module, confDir, confURL, bfeURL,
		testutil.WithVersionKeepCount(2),
		testutil.WithCopyFiles([]string{"static.data"}))

	// 1. first delivery
	runner.WaitForReload(t, bfeServer, 1, 5*time.Second)
	target1 := testutil.CurrentTarget(t, confDir)

	// ops places a CopyFiles-only file into the activated dir
	staticContent := []byte("ops managed static conf")
	require.NoError(t, os.WriteFile(filepath.Join(target1, "static.data"), staticContent, 0644))

	// 2. version bump delivers v2, static.data is copied over
	confServer.SetVersion("20260101120001")
	runner.WaitForReload(t, bfeServer, 2, 5*time.Second)
	target2 := testutil.CurrentTarget(t, confDir)
	require.NotEqual(t, target1, target2, "target should change after version update")
	require.FileExists(t, filepath.Join(target2, "static.data"))

	// 3. incident: the activated dir is wiped (config + copy file + marker)
	entries, err := os.ReadDir(target2)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, os.RemoveAll(filepath.Join(target2, e.Name())))
	}
	require.Empty(t, listNames(t, target2), "active dir should be empty after wipe")

	// 4. self heal: next cycle(s) must rebuild the active dir in place and
	// switch the symlink back without manual intervention
	runner.WaitForReload(t, bfeServer, 3, 5*time.Second)

	healed := testutil.CurrentTarget(t, confDir)
	assert.Equal(t, target2, healed, "symlink should stay on the rebuilt dir")

	content, err := os.ReadFile(filepath.Join(healed, module+".json"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "20260101120001", "config file should be rewritten with full content")

	marker, err := os.ReadFile(filepath.Join(healed, ".conf-agent-version"))
	require.NoError(t, err)
	assert.Equal(t, "20260101120001", string(marker), "version marker should be rewritten")

	assert.True(t, strings.Contains(bfeServer.LastReloadPath(), "20260101120001"),
		"last reload should use the recovered version, got %s", bfeServer.LastReloadPath())

	runner.Stop()
}

// TestSelfHeal_MissingCopyFileFromFirstDeploy verifies that a CopyFiles
// source which has never existed does not stall the very first delivery.
// Before the fix, the wrapped not-exist error was not recognized and the
// store aborted every cycle, so the symlink was never created.
func TestSelfHeal_MissingCopyFileFromFirstDeploy(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, module)

	confServer := testutil.NewMockConfigServer(t, module)
	bfeServer := testutil.NewMockBFEServer(t, module)

	confURL, _ := url.Parse(confServer.URL())
	bfeURL, _ := url.Parse(bfeServer.URL())

	runner := testutil.StartReloader(t, module, confDir, confURL, bfeURL,
		testutil.WithCopyFiles([]string{"never_existed.data"}))

	runner.WaitForReload(t, bfeServer, 1, 5*time.Second)

	target := testutil.CurrentTarget(t, confDir)
	assert.FileExists(t, filepath.Join(target, module+".json"),
		"first delivery should succeed even if a CopyFiles source is missing")

	runner.Stop()
}

// TestCleanup_SweepsEmptyUnmarkedDirs verifies the best-effort sweep of
// stale empty dirs without a version marker (half-written dirs from
// interrupted stores, conf-agent#20), while non-empty unmarked dirs are
// left alone.
func TestCleanup_SweepsEmptyUnmarkedDirs(t *testing.T) {
	tmpDir := t.TempDir()
	confDir := filepath.Join(tmpDir, module)

	confServer := testutil.NewMockConfigServer(t, module)
	bfeServer := testutil.NewMockBFEServer(t, module)

	confURL, _ := url.Parse(confServer.URL())
	bfeURL, _ := url.Parse(bfeServer.URL())

	runner := testutil.StartReloader(t, module, confDir, confURL, bfeURL,
		testutil.WithVersionKeepCount(2))

	runner.WaitForReload(t, bfeServer, 1, 5*time.Second)

	// stale empty dir without marker (like the incident's half-written dirs)
	emptyUnmarked := filepath.Join(tmpDir, module+"_20200101000000")
	require.NoError(t, os.MkdirAll(emptyUnmarked, 0755))

	// non-empty dir without marker (user-managed, must be kept)
	nonEmptyUnmarked := filepath.Join(tmpDir, module+"_19990101000000")
	require.NoError(t, os.MkdirAll(nonEmptyUnmarked, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nonEmptyUnmarked, "manual.txt"), []byte("x"), 0644))

	// a successful switch triggers cleanupOldVersions
	confServer.SetVersion("20260101120001")
	runner.WaitForReload(t, bfeServer, 2, 5*time.Second)

	_, err := os.Stat(emptyUnmarked)
	assert.True(t, os.IsNotExist(err), "stale empty unmarked dir should be swept")

	_, err = os.Stat(nonEmptyUnmarked)
	assert.NoError(t, err, "non-empty unmarked dir should be kept")

	runner.Stop()
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

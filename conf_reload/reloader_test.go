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

package conf_reload

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rainway-ai-gateway/conf-agent/config"
)

// newTestReloader builds a Reloader whose prober fetches from confSrv and
// whose trigger calls triggerSrv.
func newTestReloader(t *testing.T, confSrv, triggerSrv *httptest.Server, confDir string) *Reloader {
	t.Helper()

	rc := &config.ReloaderConfig{
		Name:             "mod_test",
		ConfDir:          confDir,
		ReloadInterval:   10 * time.Millisecond,
		VersionKeepCount: 2,
		Trigger: config.TriggerConfig{
			BFEReloadAPI:     triggerSrv.URL + "/reload/mod_test",
			BFEReloadTimeout: time.Second,
			ConfDir:          confDir,
		},
		NormalFileTasks: []*config.NormalFileTaskConfig{
			{
				ConfDir:         confDir,
				ConfAPI:         confSrv.URL + "/conf",
				ConfFileName:    "test.data",
				ConfTaskTimeout: time.Second,
			},
		},
	}

	r, err := NewReloader(rc)
	if err != nil {
		t.Fatalf("NewReloader fail: %v", err)
	}
	return r
}

// TestReload_ConsecutiveTriggerFailures verifies the consecutive trigger
// failure counter: it grows while BFE reload keeps failing, resets after a
// successful trigger, and resets when there is nothing new to reload.
func TestReload_ConsecutiveTriggerFailures(t *testing.T) {
	confSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("version") == "20260904205703" {
			// nothing newer than the activated version
			fmt.Fprint(w, `{"ErrNum":200,"Data":null}`)
			return
		}
		fmt.Fprint(w, `{"ErrNum":200,"Data":{"Version":"20260904205703","Config":{}}}`)
	}))
	defer confSrv.Close()

	var triggerMu sync.Mutex
	triggerFail := true
	triggerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		triggerMu.Lock()
		defer triggerMu.Unlock()
		if triggerFail {
			fmt.Fprint(w, `{"error":"mock reload failure"}`)
			return
		}
		fmt.Fprint(w, `{"error":null}`)
	}))
	defer triggerSrv.Close()

	confDir := filepath.Join(t.TempDir(), "mod_test")
	r := newTestReloader(t, confSrv, triggerSrv, confDir)
	ctx := context.Background()

	// 3 consecutive failures: counter grows, symlink never created
	for i := 1; i <= 3; i++ {
		r.reload(ctx)
		if r.consecutiveFailures != i {
			t.Fatalf("after failure %d, consecutiveFailures = %d, want %d",
				i, r.consecutiveFailures, i)
		}
		if _, err := os.Lstat(confDir); !os.IsNotExist(err) {
			t.Fatalf("conf dir should not be created while trigger fails, err: %v", err)
		}
	}

	// trigger recovers: counter resets and symlink is switched
	triggerMu.Lock()
	triggerFail = false
	triggerMu.Unlock()

	r.reload(ctx)
	if r.consecutiveFailures != 0 {
		t.Fatalf("after recovery, consecutiveFailures = %d, want 0", r.consecutiveFailures)
	}
	info, err := os.Lstat(confDir)
	if err != nil {
		t.Fatalf("conf dir should be created after trigger succ: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("conf dir should be a symlink, mode: %v", info.Mode())
	}

	// nothing newer: without_update path keeps counter at zero
	r.reload(ctx)
	if r.consecutiveFailures != 0 {
		t.Fatalf("after without_update, consecutiveFailures = %d, want 0",
			r.consecutiveFailures)
	}
}

// TestReload_ConsecutiveStoreFailures verifies that failures at the
// StoreFile2TmpDir stage also count towards the consecutive failure counter
// and recover cleanly (conf-agent#20: the stuck state happened at this
// stage, before the trigger, so trigger-only counting stayed silent).
func TestReload_ConsecutiveStoreFailures(t *testing.T) {
	confSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ErrNum":200,"Data":{"Version":"20260904230000","Config":{}}}`)
	}))
	defer confSrv.Close()

	triggerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":null}`)
	}))
	defer triggerSrv.Close()

	parentDir := t.TempDir()
	confDir := filepath.Join(parentDir, "mod_test")
	r := newTestReloader(t, confSrv, triggerSrv, confDir)
	ctx := context.Background()

	// make the parent dir read-only so MkdirAll of the version dir fails
	if err := os.Chmod(parentDir, 0555); err != nil {
		t.Fatalf("chmod parent dir fail: %v", err)
	}
	defer os.Chmod(parentDir, 0755)

	for i := 1; i <= 3; i++ {
		r.reload(ctx)
		if r.consecutiveFailures != i {
			t.Fatalf("after store failure %d, consecutiveFailures = %d, want %d",
				i, r.consecutiveFailures, i)
		}
	}

	// store recovers: counter resets and symlink is switched
	if err := os.Chmod(parentDir, 0755); err != nil {
		t.Fatalf("chmod parent dir back fail: %v", err)
	}

	r.reload(ctx)
	if r.consecutiveFailures != 0 {
		t.Fatalf("after recovery, consecutiveFailures = %d, want 0", r.consecutiveFailures)
	}
	info, err := os.Lstat(confDir)
	if err != nil {
		t.Fatalf("conf dir should be created after recovery: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("conf dir should be a symlink, mode: %v", info.Mode())
	}
}

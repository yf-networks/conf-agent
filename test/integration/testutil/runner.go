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

package testutil

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rainway-ai-gateway/conf-agent/agent"
	"github.com/rainway-ai-gateway/conf-agent/config"
)

// ReloaderRunner 封装集成测试中的 Reloader 生命周期
type ReloaderRunner struct {
	agent   *agent.Agent
	confDir string
}

// ReloaderOption 用于自定义 Reloader 配置
type ReloaderOption func(*config.ReloaderConfig)

// WithVersionKeepCount 设置保留版本数
func WithVersionKeepCount(count int) ReloaderOption {
	return func(rc *config.ReloaderConfig) {
		rc.VersionKeepCount = count
	}
}

// WithCopyFiles 设置 CopyFiles（从激活目录复制到版本目录的文件/目录列表）
func WithCopyFiles(copyFiles []string) ReloaderOption {
	return func(rc *config.ReloaderConfig) {
		rc.CopyFiles = copyFiles
	}
}

// StartReloader 启动一个 Reloader 实例
func StartReloader(t *testing.T, module, confDir string, confServer, bfeServer *url.URL, opts ...ReloaderOption) *ReloaderRunner {
	t.Helper()

	// Note: do NOT create confDir here. UpdateDefaultConfDir will create the
	// symlink/junction at this path. Creating it as a regular directory would
	// prevent junction creation on Windows.
	bfeMonitorPort := "80"
	if bfeServer.Port() != "" {
		bfeMonitorPort = bfeServer.Port()
	}

	port := 80
	fmt.Sscanf(bfeMonitorPort, "%d", &port)

	rc := &config.ReloaderConfig{
		Name:           module,
		ReloadInterval: 200 * time.Millisecond,
		ConfDir:        confDir,
		VersionKeepCount: 2,
		Trigger: config.TriggerConfig{
			BFEReloadAPI:     fmt.Sprintf("%s/reload/%s", bfeServer.String(), module),
			BFEReloadTimeout: 1500 * time.Millisecond,
			ConfDir:          confDir,
		},
		NormalFileTasks: []*config.NormalFileTaskConfig{
			{
				BFECluster:      "default",
				ConfDir:         confDir,
				ConfAPI:         fmt.Sprintf("%s/configs/%s", confServer.String(), module),
				ConfFileName:    fmt.Sprintf("%s.json", module),
				ConfTaskHeaders: map[string]string{},
				ConfTaskTimeout: 2500 * time.Millisecond,
			},
		},
	}

	for _, opt := range opts {
		opt(rc)
	}

	ag, err := agent.New([]*config.ReloaderConfig{rc})
	if err != nil {
		t.Fatalf("agent.New fail: %v", err)
	}

	runner := &ReloaderRunner{
		agent:   ag,
		confDir: confDir,
	}

	go func() {
		ag.Start()
	}()

	t.Cleanup(func() {
		runner.Stop()
	})

	return runner
}

// Stop 停止 Reloader
func (r *ReloaderRunner) Stop() {
	if r.agent == nil {
		return
	}

	r.agent.Stop()
}

// ConfDir 返回配置目录
func (r *ReloaderRunner) ConfDir() string {
	return r.confDir
}

// WaitForReload 等待指定次数的 reload 完成，并确保符号链接已建立
func (r *ReloaderRunner) WaitForReload(t *testing.T, bfe *MockBFEServer, minCount int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if bfe.ReloadCount() >= minCount {
			// BFE reload 完成后，UpdateDefaultConfDir 可能还未执行完，
			// 继续等待符号链接建立
			if _, err := os.Lstat(r.confDir); err == nil {
				if _, err := filepath.EvalSymlinks(r.confDir); err == nil {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("timeout waiting for reload count >= %d and symlink established, got count %d", minCount, bfe.ReloadCount())
}

// ParsePortFromURL 从 URL 中解析端口
func ParsePortFromURL(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 80
	}

	port := u.Port()
	if port == "" {
		if strings.HasPrefix(rawURL, "https") {
			return 443
		}
		return 80
	}

	var p int
	fmt.Sscanf(port, "%d", &p)
	return p
}

// CurrentTarget 返回 ConfDir 符号链接指向的目录
func CurrentTarget(t *testing.T, confDir string) string {
	t.Helper()

	target, err := filepath.EvalSymlinks(confDir)
	if err != nil {
		t.Fatalf("eval symlink fail: %v", err)
	}
	return target
}

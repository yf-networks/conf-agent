// Copyright (c) 2021 The BFE Authors.
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
	"math/rand"
	"sync"
	"time"

	"github.com/rainway-ai-gateway/conf-agent/conf_reload/file_store"
	"github.com/rainway-ai-gateway/conf-agent/conf_reload/prober"
	"github.com/rainway-ai-gateway/conf-agent/conf_reload/trigger"
	"github.com/rainway-ai-gateway/conf-agent/config"
	"github.com/rainway-ai-gateway/conf-agent/xlog"
)

type Reloader struct {
	// Name is the name of reloader
	Name string
	// ReloadInterval is the interval reloader try to reload
	ReloadInterval time.Duration

	prober    *prober.Prober
	trigger   *trigger.Trigger
	fileStore *file_store.FileStore

	// consecutiveFailures counts consecutive reload failures at the store or
	// trigger stage. While it keeps growing, the symlink is never switched
	// and BFE keeps running the old config, so a summary log is emitted
	// periodically to make the stuck state visible.
	consecutiveFailures int

	stop     chan bool
	stopOnce sync.Once
}

func NewReloader(rc *config.ReloaderConfig) (*Reloader, error) {
	prober, err := prober.NewProber(rc.NormalFileTasks, rc.MultiJSONKeyFileTasks, rc.ExtraFileFileTasks)
	if err != nil {
		return nil, err
	}

	trigger, err := trigger.NewTrigger(rc.Trigger)
	if err != nil {
		return nil, err
	}

	fileStore, err := file_store.NewFileStore(rc.ConfDir, rc.CopyFiles, rc.VersionKeepCount)
	if err != nil {
		return nil, err
	}

	return &Reloader{
		Name:           rc.Name,
		ReloadInterval: rc.ReloadInterval,

		prober:    prober,
		trigger:   trigger,
		fileStore: fileStore,

		stop: make(chan bool),
	}, nil
}

func (r *Reloader) Start() {
	// don't request config sever at the same time
	time.Sleep(time.Duration(rand.Int()%int(r.ReloadInterval/time.Millisecond)) * time.Millisecond)

	for {
		select {
		case <-r.stop:
			return
		default:
		}

		r.reload(xlog.NewContext(context.Background(), r.Name))

		select {
		case <-r.stop:
			return
		case <-time.After(r.ReloadInterval):
		}
	}
}

// Stop signals the reloader goroutine to exit. It is safe to call multiple times.
func (r *Reloader) Stop() {
	r.stopOnce.Do(func() {
		close(r.stop)
	})
}

// noteFailure records a failed reload stage: bump the consecutive failure
// counter and, every 10 failures, emit a summary error so a permanently
// stuck reload loop does not stay silent.
func (r *Reloader) noteFailure(ctx context.Context, stage, version string, err error) {
	r.consecutiveFailures++
	xlog.Default.Error(xlog.ErrLogFormat(ctx, stage+" fail", err))
	if r.consecutiveFailures%10 == 0 {
		xlog.Default.Error(xlog.InfoLogFormat(ctx, "reload keeps failing",
			fmt.Sprintf("stage: %s, consecutive: %d, version: %s, symlink not switched, bfe still runs old config",
				stage, r.consecutiveFailures, version)))
	}
}

// noteSuccess resets the failure counter after a successful trigger and
// logs recovery if the previous reloads had been failing.
func (r *Reloader) noteSuccess(ctx context.Context) {
	if r.consecutiveFailures > 0 {
		xlog.Default.Info(xlog.InfoLogFormat(ctx, "reload recovered",
			fmt.Sprintf("after %d consecutive failures", r.consecutiveFailures)))
		r.consecutiveFailures = 0
	}
}

func (r *Reloader) reload(ctx context.Context) {
	xlog.Default.Info(xlog.InfoLogFormat(ctx, "reload begin"))

	// fetch newer data file
	fileList, err := r.prober.Probe(ctx)
	if err != nil {
		xlog.Default.Error(xlog.ErrLogFormat(ctx, "probe", err))
		return
	}
	xlog.Default.Info(xlog.InfoLogFormat(ctx, "probe succ"))

	// no newer data file, exit
	if len(fileList) == 0 {
		r.consecutiveFailures = 0
		xlog.Default.Info(xlog.InfoLogFormat(ctx, "reload succ", "without_update"))
		return
	}

	version := ""
	files := map[string][]byte{}
	for _, one := range fileList {
		files[one.Name] = one.Content
		if one.Version > version {
			version = one.Version
		}
	}

	// store all newer data file
	err = r.fileStore.StoreFile2TmpDir(ctx, version, files)
	if err != nil {
		r.noteFailure(ctx, "StoreFile2TmpDir", version, err)
		return
	}
	xlog.Default.Info(xlog.InfoLogFormat(ctx, "StoreFile2TmpDir succ"))

	// trigger bfe reload
	err = r.trigger.TriggerBFEReload(ctx, version)
	if err != nil {
		r.noteFailure(ctx, "TriggerBFEReload", version, err)
		return
	}
	r.noteSuccess(ctx)
	xlog.Default.Info(xlog.InfoLogFormat(ctx, "TriggerBFEReload succ"))

	// replace old config by newest, if fail, it's ok
	err = r.fileStore.UpdateDefaultConfDir(ctx, version)
	if err != nil {
		xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir fail", err))
		return
	}
	xlog.Default.Info(xlog.InfoLogFormat(ctx, "UpdateDefaultConfDir succ"))

	xlog.Default.Info(xlog.InfoLogFormat(ctx, "reload succ", "update"))
}

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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rainway-ai-gateway/conf-agent/xfile"
	"github.com/rainway-ai-gateway/conf-agent/xlog"
)

const versionMarkerFile = ".conf-agent-version"

type FileStore struct {
	// ConfDir is the root dir of conf file
	ConfDir string
	// CoypFiles is list of files and directories copied from default dir to tmp dir
	CopyFiles []string
	// VersionKeepCount is the number of version directories to keep
	VersionKeepCount int
}

// compose path of tempory directory to store files
func (fileStore *FileStore) tmpDir(version string) string {
	return fileStore.ConfDir + "_" + version
}

func NewFileStore(confDir string, copyFiles []string, versionKeepCount int) (*FileStore, error) {
	if versionKeepCount < 1 {
		versionKeepCount = 1
	}

	return &FileStore{
		ConfDir:          confDir,
		CopyFiles:        copyFiles,
		VersionKeepCount: versionKeepCount,
	}, nil
}

// writeVersionMarker writes a marker file to identify a conf-agent managed version directory.
func (fileStore *FileStore) writeVersionMarker(tmpDir, version string) error {
	markerFile := filepath.Join(tmpDir, versionMarkerFile)
	if err := os.WriteFile(markerFile, []byte(version), 0644); err != nil {
		return fmt.Errorf("write version marker fail, file: %s, err: %v", markerFile, err)
	}
	return nil
}

// cleanupOldVersions removes expired version directories, keeping the current
// target plus the most recent VersionKeepCount-1 previous versions.
func (fileStore *FileStore) cleanupOldVersions(ctx context.Context, keep int) error {
	if keep < 1 {
		keep = 1
	}

	parentDir := filepath.Dir(fileStore.ConfDir)
	baseName := filepath.Base(fileStore.ConfDir)

	currentTarget, err := filepath.EvalSymlinks(fileStore.ConfDir)
	if err != nil {
		// If the link does not exist or is broken, there is nothing to protect.
		currentTarget = ""
	}

	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return fmt.Errorf("read parent dir fail, dir: %s, err: %v", parentDir, err)
	}

	type versionDir struct {
		path    string
		modTime time.Time
	}

	var versions []versionDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, baseName+"_") {
			continue
		}
		// Skip backup directories created from regular directory migration.
		if strings.HasSuffix(name, ".backup") {
			continue
		}

		dirPath := filepath.Join(parentDir, name)
		markerFile := filepath.Join(dirPath, versionMarkerFile)
		if _, err := os.Stat(markerFile); err != nil {
			continue
		}

		// Never remove the currently active target.
		absDirPath, _ := filepath.Abs(dirPath)
		absCurrent, _ := filepath.Abs(currentTarget)
		if absDirPath == absCurrent {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			xlog.Default.Error(xlog.ErrLogFormat(ctx, "cleanupOldVersions.Info", err))
			continue
		}

		versions = append(versions, versionDir{path: dirPath, modTime: info.ModTime()})
	}

	// Keep the newest versions first.
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].modTime.After(versions[j].modTime)
	})

	var removeErrs []string
	for i, v := range versions {
		if i < keep-1 {
			continue
		}

		if err := os.RemoveAll(v.path); err != nil {
			removeErrs = append(removeErrs, fmt.Sprintf("%s: %v", v.path, err))
		}
	}

	// Best-effort sweep of stale EMPTY dirs without the version marker.
	// Half-written dirs from interrupted stores carry no marker and would
	// otherwise pile up forever (conf-agent#20). Only empty dirs are
	// removed; non-empty unmarked dirs are left alone (may be user-managed).
	var staleEmpty []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, baseName+"_") || strings.HasSuffix(name, ".backup") {
			continue
		}

		dirPath := filepath.Join(parentDir, name)
		if abs, _ := filepath.Abs(dirPath); currentTarget != "" && abs == currentTarget {
			continue
		}
		if _, err := os.Stat(filepath.Join(dirPath, versionMarkerFile)); err == nil {
			continue
		}
		if sub, err := os.ReadDir(dirPath); err == nil && len(sub) == 0 {
			staleEmpty = append(staleEmpty, dirPath)
		}
	}

	for _, dirPath := range staleEmpty {
		xlog.Default.Info(xlog.InfoLogFormat(ctx, "cleanupOldVersions.RemoveStaleEmptyDir", dirPath))
		if err := os.RemoveAll(dirPath); err != nil {
			removeErrs = append(removeErrs, fmt.Sprintf("%s: %v", dirPath, err))
		}
	}

	if len(removeErrs) > 0 {
		return fmt.Errorf("cleanupOldVersions remove fail: %s", strings.Join(removeErrs, "; "))
	}

	return nil
}

// renameDir renames a directory from src to dst. When rename fails with EXDEV
// (cross-device link, common on overlay2 where lower-layer directories cannot
// be renamed), it falls back to copy + delete.
func renameDir(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	// EXDEV: src and dst are on different filesystems (e.g. overlay2 lower → upper).
	// Fall back to recursive copy then remove source.
	if err := xfile.FileCopyRecursive(src, dst); err != nil {
		return fmt.Errorf("copy fallback fail, from: %s, to: %s, err: %v", src, dst, err)
	}

	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("remove source after copy fail, dir: %s, err: %v", src, err)
	}

	return nil
}

// UpdateDefaultConfDir updates default config directory with config files in tempory directory.
func (fileStore *FileStore) UpdateDefaultConfDir(ctx context.Context, version string) error {
	// Inspect ConfDir without following symlinks so we can distinguish a symlink
	// from its target and from a regular directory.
	info, err := os.Lstat(fileStore.ConfDir)

	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			// ConfDir is a symlink: remove the link itself. The previous target is kept
			// so cleanupOldVersions can retain up to VersionKeepCount versions.
			if err := os.Remove(fileStore.ConfDir); err != nil {
				err = fmt.Errorf("file: %s, err: %v", fileStore.ConfDir, err)
				xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir.RemoveLink", err))
				return err
			}
		} else if info.IsDir() {
			// ConfDir is a regular directory: back it up instead of deleting it.
			backupDir := fileStore.ConfDir + "_" + strconv.FormatInt(time.Now().Unix(), 10) + ".backup"
			if err := renameDir(fileStore.ConfDir, backupDir); err != nil {
				err = fmt.Errorf("backup dir fail, from: %s, to: %s, err: %v", fileStore.ConfDir, backupDir, err)
				xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir.Backup", err))
				return err
			}
		} else {
			// ConfDir exists as a non-directory, non-symlink entry: remove it.
			if err := os.RemoveAll(fileStore.ConfDir); err != nil {
				err = fmt.Errorf("file: %s, err: %v", fileStore.ConfDir, err)
				xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir.Remove", err))
				return err
			}
		}

	case os.IsNotExist(err):
		// ConfDir does not exist yet: create the new link directly.

	default:
		// Broken symlink or other Lstat error: try to remove the path.
		if err := os.RemoveAll(fileStore.ConfDir); err != nil && !os.IsNotExist(err) {
			err = fmt.Errorf("file: %s, err: %v", fileStore.ConfDir, err)
			xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir.Remove", err))
			return err
		}
	}

	// Refuse to switch to a directory without the version marker: it is not
	// a complete conf-agent version dir (e.g. a half-written one), and
	// pointing ConfDir at it would break BFE.
	if _, err := os.Stat(filepath.Join(fileStore.tmpDir(version), versionMarkerFile)); err != nil {
		err = fmt.Errorf("version dir %s has no marker file: %v", fileStore.tmpDir(version), err)
		xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir.CheckMarker", err))
		return err
	}

	// ln -sf ModDemo_{version} ModDemo
	// NOTICE: if link fail, bfe can't restart automatically !!!
	if err := xfile.FileLink(fileStore.tmpDir(version), fileStore.ConfDir); err != nil {
		xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir.FileLink", err))
		return err
	}

	// Clean up expired version directories after a successful switch.
	if err := fileStore.cleanupOldVersions(ctx, fileStore.VersionKeepCount); err != nil {
		xlog.Default.Error(xlog.ErrLogFormat(ctx, "UpdateDefaultConfDir.cleanupOldVersions", err))
	}

	return nil
}

// StoreFile2TmpDir store all file to tempory directory
// it will create new file or overwrite old file
func (fileStore *FileStore) StoreFile2TmpDir(ctx context.Context, version string, files map[string][]byte) (retErr error) {
	tmpDir := fileStore.tmpDir(version)

	// Never RemoveAll the currently active conf dir. With second-precision
	// version stamps the incoming version may equal the active dir name
	// (cross-topic same-second stamps, or a second agent process), and the
	// active dir may hold the only copy of CopyFiles content.
	activeTarget, evalErr := filepath.EvalSymlinks(fileStore.ConfDir)
	absTmp, absErr := filepath.Abs(tmpDir)
	inPlace := evalErr == nil && absErr == nil && activeTarget == absTmp
	if inPlace {
		xlog.Default.Info(xlog.InfoLogFormat(ctx, "fileStore.tmpDirEqualsActiveDir",
			"tmpDir ", tmpDir, " is the active conf dir, rebuild in place"))
	} else {
		// delete tmp directory if exist
		if err := os.RemoveAll(tmpDir); err != nil && !xfile.IsFileNotExistError(err) {
			err = fmt.Errorf("RemoveAll fail, dir: %s, err: %v", tmpDir, err)
			xlog.Default.Error(xlog.ErrLogFormat(ctx, "fileStore.RemoveAll", err))

			return err
		}

		// create tmp directory
		if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
			err = fmt.Errorf("MkDirAll fail, dir: %s, err: %v", tmpDir, err)
			xlog.Default.Error(xlog.ErrLogFormat(ctx, "fileStore.MkdirAll", err))

			return err
		}
	}

	// Best-effort cleanup of a half-written version dir on failure, so
	// interrupted stores do not pile up unmarked dirs. The active dir
	// (inPlace) must never be removed here.
	defer func() {
		if retErr != nil && !inPlace {
			if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
				xlog.Default.Error(xlog.ErrLogFormat(ctx, "fileStore.RemoveHalfDoneDir",
					fmt.Errorf("remove half-done dir fail, dir: %s, err: %v", tmpDir, rmErr)))
			}
		}
	}()

	// copy config files (listed in fileStore.CopyFiles) from default dir to tmp dir
	for _, copyFile := range fileStore.CopyFiles {
		file := filepath.Join(fileStore.ConfDir, copyFile)
		// xfile.FileCopyRecursive copies a directory's *contents* into the
		// destination, so directories must be copied to tmpDir/<name> to keep
		// their entry name in the versioned config dir.
		target := tmpDir
		if info, err := os.Stat(file); err == nil && info.IsDir() {
			target = filepath.Join(tmpDir, copyFile)
		}
		// When rebuilding in place, sources resolve into tmpDir itself;
		// copying a file onto itself would truncate it.
		if inPlace {
			if srcAbs, err := filepath.Abs(filepath.Join(activeTarget, copyFile)); err == nil &&
				(srcAbs == absTmp || strings.HasPrefix(srcAbs, absTmp+string(filepath.Separator))) {
				continue
			}
		}
		if err := xfile.FileCopyRecursive(file, target); err != nil {
			if xfile.IsFileNotExistError(err) {
				xlog.Default.Info(xlog.ErrLogFormat(ctx, "fileStore.CopyFiles", err))
				continue
			}

			err = fmt.Errorf("keepFile fail, file: %s, err: %v", file, err)
			xlog.Default.Error(xlog.ErrLogFormat(ctx, "fileStore.CopyFiles", err))

			return err
		}
	}

	// write content to file
	for fileName, fileContent := range files {
		formattedContent := formatJSONWithIndent(fileContent)
		if err := xfile.FileOverwrite(filepath.Join(tmpDir, fileName), formattedContent); err != nil {
			xlog.Default.Error(xlog.ErrLogFormat(ctx, "fileStore.FileOverwrite", err))
			return err
		}

		// xlog.Default.Debug(xlog.InfoLogFormat(ctx, "fileStore.FileOverwrite", "fileName: ", fileName,
		// 	" fileContent: ", string(fileContent)))
	}

	// write version marker so the directory can be identified as conf-agent managed
	if err := fileStore.writeVersionMarker(tmpDir, version); err != nil {
		xlog.Default.Error(xlog.ErrLogFormat(ctx, "fileStore.writeVersionMarker", err))
		return err
	}

	return nil
}

func formatJSONWithIndent(content []byte) []byte {
	var jsonData interface{}
	if err := json.Unmarshal(content, &jsonData); err != nil {
		return content
	}

	formatted, err := json.MarshalIndent(jsonData, "", "    ")
	if err != nil {
		return content
	}

	return formatted
}

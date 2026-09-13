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

package xfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// IsFileNotExistError reports whether err indicates a missing file or
// directory. It unwraps the error chain, so wrapped errors (e.g. from
// fmt.Errorf("...: %w", err)) are recognized as well.
func IsFileNotExistError(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

func FileOverwrite(fileName string, content []byte) error {
	dir := filepath.Dir(fileName)
	_, err := os.Stat(dir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return fmt.Errorf("create dir fail, dir: %s, err: %v", dir, err)
		}
	}

	if err := ioutil.WriteFile(fileName, content, os.ModePerm); err != nil {
		return fmt.Errorf("overwrite file fail, file: %s, err: %v", fileName, err)
	}

	return nil
}

// FileCopyRecursive copies a file or directory recursively from `from` to `to`.
// When `from` is a file and `to` is a directory, the file is copied into `to`.
// Errors are wrapped with %w so callers can inspect them with errors.Is
// (e.g. xfile.IsFileNotExistError).
func FileCopyRecursive(from, to string) error {
	fromInfo, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("FileCopyRecursive fail, from: %s, to: %s, err: %w", from, to, err)
	}

	if fromInfo.IsDir() {
		return copyDir(from, to)
	}

	// from is a file
	target := to
	toInfo, err := os.Stat(to)
	if err == nil && toInfo.IsDir() {
		target = filepath.Join(to, filepath.Base(from))
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("FileCopyRecursive fail, from: %s, to: %s, err: %w", from, to, err)
	}

	return copyFile(from, target)
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	if err := os.MkdirAll(filepath.Dir(dst), os.ModePerm); err != nil {
		return err
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	return dstFile.Sync()
}

func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	// create dst up front so empty directories are copied too
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := ioutil.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}

		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}

	// preserve source directory permissions
	return os.Chmod(dst, srcInfo.Mode())
}

// RenameFileIfNotLinkFile rename oldPath to newPath then link oldPath to newPath if oldPath is not a link file
// if file is link, do nothing
// else rename it by newPath then link it by oldPath
func RenameFileIfNotLinkFile(oldPath, newPath string) error {
	originPath, err := filepath.EvalSymlinks(oldPath)
	if err != nil {
		return err
	}

	// link file, do nothing
	if originPath != oldPath {
		return nil
	}

	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("rename fail, oldPath: %s, newPath: %s, err: %v", oldPath, newPath, err)
	}

	return FileLink(newPath, oldPath)
}

func FileLink(target, linkName string) error {
	// Normalize paths; Windows mklink requires backslashes.
	target = filepath.FromSlash(target)
	linkName = filepath.FromSlash(linkName)

	// Ensure the target exists so we can decide between a directory junction
	// (Windows, no special privilege required) and a symbolic link.
	targetInfo, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("link target not exist, target: %s, err: %v", target, err)
	}

	// remove existing link name so the new link won't fail
	_ = os.Remove(linkName)

	// On Windows, directory junctions are preferred over symlinks because they
	// do not require developer mode or administrative privileges.
	if runtime.GOOS == "windows" && targetInfo.IsDir() {
		// mklink /J works reliably with absolute paths; relative paths may be
		// interpreted against the current directory and create broken junctions.
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return fmt.Errorf("get abs target fail, target: %s, err: %v", target, err)
		}
		absLinkName, err := filepath.Abs(linkName)
		if err != nil {
			return fmt.Errorf("get abs link name fail, linkName: %s, err: %v", linkName, err)
		}
		cmd := exec.Command("cmd", "/c", "mklink", "/J", absLinkName, absTarget)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("create junction %s -> %s fail, err: %v, output: %s", absLinkName, absTarget, err, string(out))
		}
		return nil
	}

	// For Unix symlinks (and Windows file symlinks), store the target as a path
	// relative to the link's directory. This matches the behavior of ln -sf and
	// keeps symlinks valid regardless of the current working directory.
	relTarget, err := filepath.Rel(filepath.Dir(linkName), target)
	if err != nil {
		// Targets on different filesystem roots; fall back to the original target.
		relTarget = target
	}

	if err := os.Symlink(relTarget, linkName); err != nil {
		return fmt.Errorf("symlink %s, %s fail, err: %v", relTarget, linkName, err)
	}

	return nil
}

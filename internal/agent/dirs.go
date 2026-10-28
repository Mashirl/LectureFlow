// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

// Where the agent keeps its files (plan §4): one data directory (agent
// binaries, Python runtime, venvs, worker scripts, models, kits) and one
// job directory, both 0700 and created with umask 077, without root. The
// agent touches nothing else: every path it opens is built from these two
// and checked with Lstat (no symbolic link is followed, no directory owned
// by another user is used).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Dirs are the agent's two directories.
type Dirs struct {
	Root string // data: agent/, python/, venv/, worker/, models/, kits/, downloads/
	Jobs string // one directory per job
}

// Subdirectories of Root.
const (
	dirAgent     = "agent"
	dirPython    = "python"
	dirVenv      = "venv"
	dirWorker    = "worker"
	dirModels    = "models"
	dirKits      = "kits"
	dirDownloads = "downloads"
)

// ResolveDirs computes the directories from the standard environment
// (HOME, XDG_DATA_HOME / XDG_CACHE_HOME on Linux, LOCALAPPDATA on Windows).
func ResolveDirs(goos string, getenv func(string) string) (Dirs, error) {
	abs := func(p string) bool { return p != "" && filepath.IsAbs(p) }
	switch goos {
	case "windows":
		base := getenv("LOCALAPPDATA")
		if !abs(base) {
			return Dirs{}, errors.New("LOCALAPPDATA is not set")
		}
		root := filepath.Join(base, "LectureFlowAgent")
		return Dirs{Root: root, Jobs: filepath.Join(root, "jobs")}, nil
	case "darwin":
		home := getenv("HOME")
		if !abs(home) {
			return Dirs{}, errors.New("HOME is not set")
		}
		return Dirs{
			Root: filepath.Join(home, "Library", "Application Support", "LectureFlow Agent"),
			Jobs: filepath.Join(home, "Library", "Caches", "LectureFlow Agent", "jobs"),
		}, nil
	default:
		home := getenv("HOME")
		data, cache := getenv("XDG_DATA_HOME"), getenv("XDG_CACHE_HOME")
		if !abs(data) {
			if !abs(home) {
				return Dirs{}, errors.New("HOME is not set")
			}
			data = filepath.Join(home, ".local", "share")
		}
		if !abs(cache) {
			if !abs(home) {
				return Dirs{}, errors.New("HOME is not set")
			}
			cache = filepath.Join(home, ".cache")
		}
		return Dirs{Root: filepath.Join(data, "lectureflow-agent"), Jobs: filepath.Join(cache, "lectureflow-agent", "jobs")}, nil
	}
}

// DefaultDirs are the directories of this process's environment.
func DefaultDirs() (Dirs, error) { return ResolveDirs(runtime.GOOS, os.Getenv) }

// errUnsafe: a path is a symbolic link, not a directory where one is
// expected, or owned by another user.
var errUnsafe = errors.New("unsafe path")

// ensurePrivateDir creates dir (and missing parents, each 0700) and checks
// that dir itself is a real directory of this user; a group- or
// world-accessible dir is made private.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkPrivateDir(dir)
}

// checkPrivateDir checks an existing directory (Lstat: no symbolic link).
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%w: %s", errUnsafe, dir)
	}
	if !ownedByMe(fi) {
		return fmt.Errorf("%w: %s is owned by another user", errUnsafe, dir)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// inside joins base and the clean relative elems and checks that the
// result stays below base, that every existing component below base is a
// real directory (Lstat) and the leaf is not a symbolic link.
func inside(base string, elems ...string) (string, error) {
	for _, e := range elems {
		if e == "" || e == "." || e == ".." || strings.ContainsAny(e, `/\`) || strings.ContainsRune(e, 0) {
			return "", fmt.Errorf("%w: bad name", errUnsafe)
		}
	}
	p := filepath.Join(append([]string{base}, elems...)...)
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: outside", errUnsafe)
	}
	cur := base
	for i, e := range elems {
		cur = filepath.Join(cur, e)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: symbolic link", errUnsafe)
		}
		if i < len(elems)-1 && !fi.IsDir() {
			return "", fmt.Errorf("%w: not a directory", errUnsafe)
		}
	}
	return p, nil
}

// regularFile reports whether p is a regular file (Lstat) and its size.
func regularFile(p string) (int64, bool) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	return fi.Size(), true
}

// removeAllInside removes p (below base) without following links.
func removeAllInside(base, p string) error {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: refusing to remove %s", errUnsafe, p)
	}
	// os.RemoveAll does not follow symbolic links (it removes the link).
	return os.RemoveAll(p)
}

// interpreter is inside() for a Python interpreter: the leaf may be a
// symbolic link (python-build-standalone's bin/python3 → python3.12, a
// venv's bin/python → the runtime's), as long as what it resolves to is a
// regular file below base.
func interpreter(base string, elems ...string) (string, error) {
	if len(elems) == 0 {
		return "", fmt.Errorf("%w: no interpreter", errUnsafe)
	}
	dir, err := inside(base, elems[:len(elems)-1]...)
	if err != nil {
		return "", err
	}
	leaf := elems[len(elems)-1]
	if leaf == "" || leaf == "." || leaf == ".." || strings.ContainsAny(leaf, `/\`) {
		return "", fmt.Errorf("%w: bad name", errUnsafe)
	}
	p := filepath.Join(dir, leaf)
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realBase, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: the interpreter resolves outside the agent's directory", errUnsafe)
	}
	if fi, err := os.Stat(real); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: the interpreter is not a file", errUnsafe)
	}
	return p, nil
}

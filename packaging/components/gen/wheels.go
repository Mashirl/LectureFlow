// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"
	"strconv"
	"strings"
)

// lockedPkg is one requirement of a uv lock file.
type lockedPkg struct {
	Name    string
	Version string
	Hashes  []string
}

// parseLock reads "name==version \" lines followed by "--hash=sha256:..."
// lines (uv pip compile --generate-hashes).
func parseLock(data string) ([]lockedPkg, error) {
	var out []lockedPkg
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimSuffix(line, `\`))
		if strings.HasPrefix(line, "--hash=sha256:") {
			if len(out) == 0 {
				return nil, fmt.Errorf("line %d: hash before any requirement", i+1)
			}
			out[len(out)-1].Hashes = append(out[len(out)-1].Hashes, strings.TrimPrefix(line, "--hash=sha256:"))
			continue
		}
		if strings.HasPrefix(line, "-") {
			return nil, fmt.Errorf("line %d: unexpected option %q", i+1, line)
		}
		if j := strings.IndexByte(line, ';'); j >= 0 {
			return nil, fmt.Errorf("line %d: markers are not expected in a platform lock: %q", i+1, line)
		}
		name, ver, ok := strings.Cut(line, "==")
		if !ok {
			return nil, fmt.Errorf("line %d: not pinned: %q", i+1, line)
		}
		out = append(out, lockedPkg{Name: strings.TrimSpace(name), Version: strings.TrimSpace(ver)})
	}
	for _, p := range out {
		if len(p.Hashes) == 0 {
			return nil, fmt.Errorf("%s has no hash", p.Name)
		}
	}
	return out, nil
}

// wheelTarget describes the interpreter a lock is for.
type wheelTarget struct {
	pyMinor  int    // 12 for CPython 3.12
	platform string // windows-x64 | darwin-arm64
	macMajor int    // darwin: the deployment target (uv's default 13.0)
	macMinor int
}

// wheelScore returns how well a wheel file name fits the target (higher is
// better, uv's preference order); 0 = incompatible.
func wheelScore(filename string, t wheelTarget) int {
	if !strings.HasSuffix(filename, ".whl") {
		return 0
	}
	parts := strings.Split(strings.TrimSuffix(filename, ".whl"), "-")
	if len(parts) < 5 {
		return 0
	}
	pyTags, abiTags, platTags := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
	best := 0
	for _, py := range strings.Split(pyTags, ".") {
		for _, abi := range strings.Split(abiTags, ".") {
			for _, plat := range strings.Split(platTags, ".") {
				s := tagScore(py, abi, plat, t)
				if s > best {
					best = s
				}
			}
		}
	}
	return best
}

func tagScore(py, abi, plat string, t wheelTarget) int {
	cp := fmt.Sprintf("cp3%d", t.pyMinor)
	pyOK, abiScore := false, 0
	switch {
	case py == cp && abi == cp:
		pyOK, abiScore = true, 3
	case strings.HasPrefix(py, "cp3") && abi == "abi3":
		if n, err := strconv.Atoi(strings.TrimPrefix(py, "cp3")); err == nil && n <= t.pyMinor {
			pyOK, abiScore = true, 2
		}
	case (py == cp || py == "py3" || py == fmt.Sprintf("py3%d", t.pyMinor) || py == "py2.py3") && abi == "none":
		pyOK, abiScore = true, 1
	}
	if !pyOK {
		return 0
	}
	platScore := 0
	switch {
	case plat == "any":
		platScore = 1
	case t.platform == "windows-x64" && plat == "win_amd64":
		platScore = 2
	case t.platform == "darwin-arm64" && strings.HasPrefix(plat, "macosx_"):
		f := strings.Split(strings.TrimPrefix(plat, "macosx_"), "_")
		if len(f) < 3 {
			return 0
		}
		major, e1 := strconv.Atoi(f[0])
		minor, e2 := strconv.Atoi(f[1])
		arch := strings.Join(f[2:], "_")
		if e1 != nil || e2 != nil || (arch != "arm64" && arch != "universal2") {
			return 0
		}
		if major > t.macMajor || (major == t.macMajor && minor > t.macMinor) {
			return 0
		}
		platScore = 2
	case t.platform == "linux-x64":
		// ssh-mvp: the SSH servers' target; glibc 2.28 like the locks
		// (refresh-locks.ps1: x86_64-manylinux_2_28).
		if glibc, ok := manylinuxGlibc(plat); ok && glibc <= 28 {
			platScore = 2
		}
	}
	if platScore == 0 {
		return 0
	}
	return platScore*10 + abiScore
}

// manylinuxGlibc is the glibc minor version an x86_64 manylinux tag needs
// (manylinux_2_28_x86_64 → 28; the legacy aliases manylinux1 = 2.5,
// manylinux2010 = 2.12, manylinux2014 = 2.17).
func manylinuxGlibc(plat string) (int, bool) {
	switch plat {
	case "manylinux1_x86_64":
		return 5, true
	case "manylinux2010_x86_64":
		return 12, true
	case "manylinux2014_x86_64":
		return 17, true
	}
	rest, ok := strings.CutPrefix(plat, "manylinux_2_")
	if !ok {
		return 0, false
	}
	minor, arch, ok := strings.Cut(rest, "_")
	if !ok || arch != "x86_64" {
		return 0, false
	}
	n, err := strconv.Atoi(minor)
	return n, err == nil
}

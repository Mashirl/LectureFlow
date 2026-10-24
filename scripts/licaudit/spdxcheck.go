// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// SPDX header check (SPEC-v3 V7): every tracked source file must carry
// "SPDX-License-Identifier: AGPL-3.0-or-later" in its first six lines.

const (
	spdxTag       = "SPDX-License-Identifier: AGPL-3.0-or-later"
	spdxHeadLines = 6
)

var spdxExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true, ".css": true,
	".html": true, ".sql": true, ".ps1": true, ".sh": true,
}

// spdxExcluded: third-party code, fixtures and documentation.
func spdxExcluded(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, p := range []string{"web/vendor/", "docs/", "desktop/node_modules/"} {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "testdata" || seg == "node_modules" {
			return true
		}
	}
	return false
}

// gitTrackedFiles lists the files tracked by git (relative, slash form).
func gitTrackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

// CheckSPDX returns the source files (relative paths) without the header.
func CheckSPDX(root string, files []string) ([]string, error) {
	var missing []string
	for _, rel := range files {
		if !spdxExts[strings.ToLower(filepath.Ext(rel))] || spdxExcluded(rel) {
			continue
		}
		ok, err := hasSPDX(filepath.Join(root, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue // deleted in the working tree
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, filepath.ToSlash(rel))
		}
	}
	sort.Strings(missing)
	return missing, nil
}

func hasSPDX(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, 16*1024)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false, err
	}
	lines := strings.SplitN(string(head[:n]), "\n", spdxHeadLines+1)
	if len(lines) > spdxHeadLines {
		lines = lines[:spdxHeadLines]
	}
	for _, l := range lines {
		if strings.Contains(l, spdxTag) {
			return true, nil
		}
	}
	return false, nil
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Go modules linked into the program. `go list -m all` also contains
// modules that are never compiled (tools, other modules' tests, some not
// even downloadable), so the audit uses the package graph of the main
// module instead, without tests, for every release platform, and takes the
// union (build-tag dependent imports differ per GOOS).

var goTargets = [][2]string{{"windows", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}}

const goListFormat = `{{with .Module}}{{if not .Main}}{{.Path}}	{{if .Replace}}{{.Replace.Version}}	{{.Replace.Dir}}{{else}}{{.Version}}	{{.Dir}}{{end}}{{end}}{{end}}`

// listGoModules runs `go list -deps` in root for each target platform.
func listGoModules(root string) ([]GoModule, error) {
	seen := map[string]GoModule{}
	for _, t := range goTargets {
		cmd := exec.Command("go", "list", "-deps", "-f", goListFormat, "./...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+t[0], "GOARCH="+t[1], "CGO_ENABLED=0", "GOFLAGS=-mod=readonly")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list (%s/%s): %v: %s", t[0], t[1], err, stderr.String())
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Split(strings.TrimRight(line, "\r"), "\t")
			if len(f) != 3 || f[0] == "" {
				continue
			}
			seen[f[0]+"@"+f[1]] = GoModule{Path: f[0], Version: f[1], Dir: f[2]}
		}
	}
	mods := make([]GoModule, 0, len(seen))
	for _, m := range seen {
		mods = append(mods, m)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Path < mods[j].Path })
	return mods, nil
}

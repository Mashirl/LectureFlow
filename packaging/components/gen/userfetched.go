// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Offline archives and user-fetched packages (SPEC-v3 V7; v3 Gate round 1,
// item 6). A Python package that packaging/licenses/allowlist.json accepts
// only with scope "user-fetched" (the NVIDIA libraries, av, ctranslate2,
// regex, ...) is acceptable because LectureFlow never distributes it: the
// component manager downloads it from PyPI on the user's computer. An
// offline archive carries the wheels of its component's lock file, so it
// would distribute those packages: gen refuses to build one for a component
// whose lock file (on any platform) contains such a package, and drops an
// offline entry an earlier run left in components.json for one.

// userFetched maps normalized package names to the versions the allowlist
// accepts as user-fetched ("" = any version).
type userFetched map[string][]string

var pep503 = regexp.MustCompile(`[-_.]+`)

// normName is the PEP 503 normalized form of a package name.
func normName(n string) string {
	return pep503.ReplaceAllString(strings.ToLower(strings.TrimSpace(n)), "-")
}

// loadUserFetched reads the user-fetched Python packages of the allowlist.
func loadUserFetched(path string) (userFetched, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("allowlist: %w", err)
	}
	var al struct {
		Entries []struct {
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			Scope   string `json:"scope"`
			Version string `json:"version"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &al); err != nil {
		return nil, fmt.Errorf("allowlist %s: %w", path, err)
	}
	uf := userFetched{}
	for _, e := range al.Entries {
		if e.Kind == "python" && e.Scope == "user-fetched" && e.Name != "" {
			n := normName(e.Name)
			uf[n] = append(uf[n], e.Version)
		}
	}
	return uf, nil
}

func (uf userFetched) has(p lockedPkg) bool {
	for _, v := range uf[normName(p.Name)] {
		if v == "" || v == p.Version {
			return true
		}
	}
	return false
}

// in returns the user-fetched packages of a lock file ("name==version").
func (uf userFetched) in(lock []lockedPkg) []string {
	var out []string
	for _, p := range lock {
		if uf.has(p) {
			out = append(out, p.Name+"=="+p.Version)
		}
	}
	sort.Strings(out)
	return out
}

// lockFiles returns the lock files of a component of the manifest source
// (locks/{id}-{platform}.txt for each platform its gen section locks).
func lockFiles(src *srcManifest, dir, id string) ([]string, error) {
	for _, raw := range src.Components {
		var c struct {
			ID  string `json:"id"`
			Gen *struct {
				Locks []string `json:"locks"`
			} `json:"gen"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		if c.ID != id || c.Gen == nil {
			continue
		}
		var out []string
		for _, plat := range c.Gen.Locks {
			out = append(out, filepath.Join(dir, "locks", id+"-"+plat+".txt"))
		}
		return out, nil
	}
	return nil, nil // a model or binary: no lock
}

// offlineRefusal explains why id must not get an offline archive ("" = it
// may): its lock files contain user-fetched packages, or cannot be read.
func offlineRefusal(src *srcManifest, dir, id string, uf userFetched) string {
	locks, err := lockFiles(src, dir, id)
	if err != nil {
		return fmt.Sprintf("cannot read the manifest source: %v", err)
	}
	for _, p := range locks {
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Sprintf("cannot check %s: %v", filepath.Base(p), err)
		}
		pkgs, err := parseLock(string(data))
		if err != nil {
			return fmt.Sprintf("cannot check %s: %v", filepath.Base(p), err)
		}
		if bad := uf.in(pkgs); len(bad) > 0 {
			return fmt.Sprintf("%s contains %s, which LectureFlow must not distribute "+
				"(packaging/licenses/allowlist.json, scope \"user-fetched\": the component manager downloads them "+
				"on the user's computer); an offline archive would carry their wheels",
				filepath.Base(p), strings.Join(bad, ", "))
		}
	}
	return ""
}

// checkOffline refuses the whole -offline request when any id may not get
// an archive (before anything is downloaded).
func checkOffline(src *srcManifest, dir string, ids []string, uf userFetched) error {
	var errs []string
	for _, id := range ids {
		if why := offlineRefusal(src, dir, id, uf); why != "" {
			errs = append(errs, fmt.Sprintf("offline %s refused: %s", id, why))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Offline component archives and user-fetched packages (v3 Gate round 1,
// item 6). A Python package accepted by an allowlist entry with scope
// "user-fetched" is acceptable only because LectureFlow never distributes
// it: the component manager downloads it on the user's computer. An offline
// archive of a component (the "offline" entry of a build in the component
// manifest, built by `gen -offline`) carries the wheels of the component's
// lock file, so such an archive would distribute those packages. Every
// component whose lock file contains a user-fetched package therefore must
// not have an offline entry in any manifest. This check cannot be
// allowlisted: an exception would remove the premise of the scope itself.

// auditOffline runs after auditPython (it needs the user-fetched packages).
func auditOffline(opt Options, r *Report, _ *allowlist) error {
	userFetched := map[string]bool{}
	for _, c := range r.Components {
		if c.Kind == KindPython && c.UserFetched {
			userFetched[c.Name] = true
		}
	}
	locked, _, err := readPythonLocks(opt.Root)
	if err != nil {
		return err
	}
	byComponent := map[string]map[string]bool{} // component id -> packages of its lock files
	for _, l := range locked {
		if byComponent[l.component] == nil {
			byComponent[l.component] = map[string]bool{}
		}
		byComponent[l.component][l.name] = true
	}
	manifests, err := componentManifests(opt.Root)
	if err != nil {
		return err
	}
	for _, mf := range manifests {
		for _, comp := range mf.components {
			id, _ := comp["id"].(string)
			hits := offlineEntries(comp)
			if len(hits) == 0 {
				continue
			}
			for _, h := range hits {
				pkgs, lockDesc, why := offlinePackages(opt.Root, mf, comp, id, h, byComponent)
				if why != "" {
					r.fail("offline", id, "", "", fmt.Sprintf("%s: %s has an offline archive, but %s; cannot confirm that the archive contains no user-fetched package", mf.rel, h, why))
					continue
				}
				var bad []string
				for p := range pkgs {
					if userFetched[p] {
						bad = append(bad, p)
					}
				}
				if len(bad) == 0 {
					continue
				}
				sort.Strings(bad)
				r.fail("offline", id, "", "", fmt.Sprintf("%s: %s is an offline archive, but %s contains %s, accepted only as user-fetched (allowlist scope %q: not distributed by LectureFlow); an offline archive would distribute them. Remove the offline entry", mf.rel, h, lockDesc, strings.Join(bad, ", "), ScopeUserFetched))
			}
		}
	}
	return nil
}

type componentManifest struct {
	rel        string // repository-relative path
	dir        string // absolute directory (lock paths are relative to it)
	components []map[string]any
}

// componentManifests reads every JSON file under packaging/components with a
// top-level "components" array (components.json, manifest.src.json).
func componentManifests(root string) ([]componentManifest, error) {
	base := filepath.Join(root, componentsDir)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil, nil
	}
	var out []componentManifest
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var top struct {
			Components []map[string]any `json:"components"`
		}
		if json.Unmarshal(b, &top) != nil || top.Components == nil {
			return nil // not a manifest
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, componentManifest{rel: filepath.ToSlash(rel), dir: filepath.Dir(p), components: top.Components})
		return nil
	})
	return out, err
}

// present reports a value that declares something: not null, false, "",
// 0, an empty object or an empty array.
func present(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return strings.TrimSpace(t) != ""
	case float64:
		return t != 0
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return true
}

// offlineEntries returns the JSON paths (inside the component) of every
// "offline" key with a value, at any depth, sorted.
func offlineEntries(comp map[string]any) []string {
	var out []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				p := k
				if path != "" {
					p = path + "." + k
				}
				if k == "offline" && present(x) {
					out = append(out, p)
					continue
				}
				walk(x, p)
			}
		case []any:
			for i, x := range t {
				walk(x, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(comp, "")
	sort.Strings(out)
	return out
}

// offlinePackages returns the packages an offline entry may contain: the lock
// file of its build when the entry sits in platforms.{platform} and that
// build names one, else every lock file of the component (its lock files
// under packaging/components and the lock paths of all its builds).
func offlinePackages(root string, mf componentManifest, comp map[string]any, id, hit string, byComponent map[string]map[string]bool) (map[string]bool, string, string) {
	builds, _ := comp["platforms"].(map[string]any)
	lockOf := func(plat string) string {
		b, _ := builds[plat].(map[string]any)
		l, _ := b["lock"].(map[string]any)
		s, _ := l["path"].(string)
		return strings.TrimSpace(s)
	}
	if rest, ok := strings.CutPrefix(hit, "platforms."); ok {
		plat := strings.SplitN(rest, ".", 2)[0]
		if lp := lockOf(plat); lp != "" {
			pkgs, err := readLockFile(filepath.Join(mf.dir, filepath.FromSlash(lp)))
			if err != nil {
				return nil, "", fmt.Sprintf("its lock file %s cannot be read (%v)", lp, err)
			}
			return pkgs, "lock file " + lp, ""
		}
	}
	pkgs := map[string]bool{}
	for p := range byComponent[id] {
		pkgs[p] = true
	}
	plats := make([]string, 0, len(builds))
	for p := range builds {
		plats = append(plats, p)
	}
	sort.Strings(plats)
	for _, plat := range plats {
		lp := lockOf(plat)
		if lp == "" {
			continue
		}
		ps, err := readLockFile(filepath.Join(mf.dir, filepath.FromSlash(lp)))
		if err != nil {
			return nil, "", fmt.Sprintf("its lock file %s cannot be read (%v)", lp, err)
		}
		for p := range ps {
			pkgs[p] = true
		}
	}
	return pkgs, "a lock file of component " + id, ""
}

func readLockFile(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	n := strings.ToLower(filepath.Base(path))
	var nv [][2]string
	if strings.HasSuffix(n, ".toml") || n == "uv.lock" {
		nv = parseTomlPackages(b)
	} else {
		nv = parseRequirements(b)
	}
	out := map[string]bool{}
	for _, x := range nv {
		out[pyName(x[0])] = true
	}
	return out, nil
}

// --- bundled binaries: corresponding source and statically linked libraries ---------

// BundledLib is a third-party library statically linked into a bundled
// binary (binaries.json "bundled").
type BundledLib struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	License string `json:"license"`
	URL     string `json:"url"`
	Note    string `json:"note,omitempty"`
}

// checkBinaryDetails validates the optional source_code and bundled fields of
// a binary. It returns the reasons that fail the binary itself; each bundled
// library with an incompatible license fails on its own ("{binary} / {lib}",
// which an allowlist entry of kind binary and scope distributed may accept).
func checkBinaryDetails(r *Report, al *allowlist, x BinaryEntry) []string {
	var extra []string
	for _, u := range x.SourceCode {
		if !isURL(strings.TrimSpace(u)) {
			extra = append(extra, fmt.Sprintf("source_code %q is not an http(s) URL", u))
		}
	}
	for i, l := range x.Bundled {
		name := x.Name + " / " + l.Name
		var why []string
		if strings.TrimSpace(l.Name) == "" {
			name = fmt.Sprintf("%s / bundled[%d]", x.Name, i)
			why = append(why, "bundled library name missing")
		}
		if strings.TrimSpace(l.URL) == "" {
			why = append(why, "bundled library url missing")
		} else if !isURL(strings.TrimSpace(l.URL)) {
			why = append(why, fmt.Sprintf("bundled library url %q is not an http(s) URL", l.URL))
		}
		if strings.TrimSpace(l.Version) == "" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s: %s has no version", binariesFile, name))
		}
		if ok, rs := EvaluateWith(l.License, extraTableFor(KindBinary)); !ok {
			if e := al.match(KindBinary, name, l.Version, ""); e != nil && e.Scope == ScopeDistributed {
				rs = nil
			}
			why = append(why, rs...)
		}
		for _, w := range why {
			r.fail(KindBinary, name, l.Version, l.License, w)
		}
	}
	if len(x.SourceCode) == 0 && strings.Contains(strings.ToUpper(x.License), "GPL") {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s: %s is under %s but lists no source_code (where its corresponding source can be obtained)", binariesFile, x.Name, x.License))
	}
	return extra
}

// renderBinaryDetails writes the corresponding source and the statically
// linked libraries of the bundled binaries that declare them.
func renderBinaryDetails(w func(string, ...any), cs []Component) {
	for _, c := range cs {
		x := c.Bin
		if x == nil || (len(x.SourceCode) == 0 && len(x.Bundled) == 0) {
			continue
		}
		w("### %s\n\n", mdEscape(c.Name))
		if len(x.SourceCode) > 0 {
			w("Corresponding source code / 对应源码:\n\n")
			for _, u := range x.SourceCode {
				w("- %s\n", mdEscape(u))
			}
			w("\n")
		}
		if len(x.Bundled) > 0 {
			w("Statically linked libraries / 静态链入的库:\n\n| Library | Version | License | Source | Note |\n|---|---|---|---|---|\n")
			for _, l := range x.Bundled {
				w("| %s | %s | %s | %s | %s |\n", mdEscape(l.Name), mdEscape(l.Version), mdEscape(l.License), mdEscape(l.URL), mdEscape(l.Note))
			}
			w("\n")
		}
	}
}

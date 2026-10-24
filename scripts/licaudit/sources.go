// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	reReadmeVersion = regexp.MustCompile(`(?i)\bversion\b\W{0,6}v?([0-9]+(?:\.[0-9]+)+[0-9A-Za-z.\-]*)`)
	reGitHubURL     = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+`)
)

// --- npm (desktop/package-lock.json, lockfile v2/v3) ---------------------------

// shippedDevPackages are devDependencies that end up in the installer anyway
// (electron-builder requires Electron itself to be a devDependency).
var shippedDevPackages = map[string]bool{"electron": true}

func npmLicense(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct{ Type string }
	if json.Unmarshal(raw, &o) == nil && o.Type != "" {
		return o.Type
	}
	var arr []struct{ Type string }
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		var ids []string
		for _, a := range arr {
			ids = append(ids, a.Type)
		}
		return strings.Join(ids, " OR ")
	}
	return ""
}

func repoURL(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct{ URL string }
	if json.Unmarshal(raw, &o) == nil {
		return o.URL
	}
	return ""
}

func auditNPM(opt Options, r *Report, al *allowlist) error {
	b, err := os.ReadFile(filepath.Join(opt.Root, npmLockFile))
	if os.IsNotExist(err) {
		r.Skipped = append(r.Skipped, npmLockFile+" (not present yet; Electron dependencies not audited)")
		return nil
	}
	if err != nil {
		return err
	}
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Version     string          `json:"version"`
			License     json.RawMessage `json:"license"`
			Resolved    string          `json:"resolved"`
			Dev         bool            `json:"dev"`
			DevOptional bool            `json:"devOptional"`
			Link        bool            `json:"link"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return fmt.Errorf("%s: %w", npmLockFile, err)
	}
	if lock.LockfileVersion < 2 || lock.Packages == nil {
		r.fail(KindNPM, npmLockFile, "", "", fmt.Sprintf("lockfileVersion %d has no \"packages\" map (need v2 or v3)", lock.LockfileVersion))
		return nil
	}
	keys := make([]string, 0, len(lock.Packages))
	for k := range lock.Packages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dev := 0
	for _, k := range keys {
		p := lock.Packages[k]
		i := strings.LastIndex(k, "node_modules/")
		if k == "" || i < 0 || p.Link {
			continue
		}
		name := k[i+len("node_modules/"):]
		if (p.Dev || p.DevOptional) && !shippedDevPackages[name] {
			dev++
			continue
		}
		c := Component{Kind: KindNPM, Name: name, Version: p.Version, License: npmLicense(p.License), Source: p.Resolved}
		if c.Source == "" {
			c.Source = "https://www.npmjs.com/package/" + name
		}
		if shippedDevPackages[name] {
			c.Note = "bundles Chromium; its notices ship with the app (LICENSES.chromium.html)"
		}
		dir := filepath.Join(opt.Root, "desktop", filepath.FromSlash(k))
		if primary, notices, err := licenseFiles(dir); err == nil {
			if primary != nil {
				c.Texts = append(c.Texts, *primary)
			}
			c.Texts = append(c.Texts, notices...)
		}
		judge(r, al, c)
	}
	if dev > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%d npm devDependencies not audited (build tools, not shipped)", dev))
	}
	return nil
}

// --- Python ----------------------------------------------------------------------

// PyEntry is one package of packaging/licenses/python.json.
type PyEntry struct {
	License    string   `json:"license"`
	Versions   []string `json:"versions,omitempty"`
	Components []string `json:"components,omitempty"`
	Source     string   `json:"source,omitempty"`
	Evidence   string   `json:"evidence,omitempty"`
	Manual     bool     `json:"manual,omitempty"` // refresh keeps license/note
	Note       string   `json:"note,omitempty"`
	// LicenseText is a repository copy of the license file
	// (packaging/licenses/texts/...) for a wheel that carries none; it is
	// verified and reproduced in Appendix A.
	LicenseText       string `json:"license_text,omitempty"`
	LicenseTextSHA256 string `json:"license_text_sha256,omitempty"`
	LicenseTextSource string `json:"license_text_source,omitempty"`
}

// PyFile is packaging/licenses/python.json.
type PyFile struct {
	Doc      string             `json:"_doc,omitempty"`
	Packages map[string]PyEntry `json:"packages"`
}

// forbiddenPython are packages that must not appear in any component
// (SPEC-v3 V6: PyMuPDF was replaced by pypdf).
var forbiddenPython = map[string]string{
	"pymupdf":        "PyMuPDF was removed (SPEC-v3 V6)",
	"pymupdfb":       "PyMuPDF was removed (SPEC-v3 V6)",
	"pymupdf4llm":    "PyMuPDF was removed (SPEC-v3 V6)",
	"pymupdf-layout": "PyMuPDF was removed (SPEC-v3 V6)",
	"fitz":           "PyMuPDF was removed (SPEC-v3 V6)",
}

var (
	rePyNorm         = regexp.MustCompile(`[-_.]+`)
	rePlatformSuffix = regexp.MustCompile(`(?i)-(windows|win|darwin|macos|linux)-(x64|x86_64|amd64|arm64|aarch64)$`)
)

// pyName is the PEP 503 normalised package name.
func pyName(s string) string {
	return rePyNorm.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
}

func loadPython(root string) (*PyFile, error) {
	pf := &PyFile{Packages: map[string]PyEntry{}}
	b, err := os.ReadFile(filepath.Join(root, pythonFile))
	if os.IsNotExist(err) {
		return pf, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, pf); err != nil {
		return nil, fmt.Errorf("%s: %w", pythonFile, err)
	}
	if pf.Packages == nil {
		pf.Packages = map[string]PyEntry{}
	}
	norm := map[string]PyEntry{}
	for k, v := range pf.Packages {
		norm[pyName(k)] = v
	}
	pf.Packages = norm
	return pf, nil
}

type pyLocked struct{ name, version, component, file string }

// readPythonLocks finds the lock files of the components:
// packaging/components/locks/{component}-{os}-{arch}.txt (the layout of the
// component manager, uv pip compile output), and also
// packaging/components/**/{requirements*.txt,*.lock.txt,uv.lock,pylock*.toml}.
// In locks/ the component is the file name without the platform suffix;
// elsewhere it is the first directory under packaging/components, or the
// file name without "requirements-"/".lock" when the file sits at the top.
func readPythonLocks(root string) ([]pyLocked, bool, error) {
	base := filepath.Join(root, componentsDir)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil, false, nil
	}
	var out []pyLocked
	found := false
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n := strings.ToLower(d.Name())
		inLocks := strings.EqualFold(filepath.Base(filepath.Dir(p)), "locks")
		isReq := strings.HasSuffix(n, ".txt") && (inLocks || strings.HasPrefix(n, "requirements") || strings.HasSuffix(n, ".lock.txt"))
		isToml := n == "uv.lock" || (strings.HasPrefix(n, "pylock") && strings.HasSuffix(n, ".toml"))
		if !isReq && !isToml {
			return nil
		}
		found = true
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		comp := strings.SplitN(rel, "/", 2)[0]
		if !strings.Contains(rel, "/") || inLocks {
			comp = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(d.Name(), "requirements-"), ".txt"), ".lock"), ".toml")
		}
		if inLocks {
			comp = rePlatformSuffix.ReplaceAllString(comp, "")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var pkgs [][2]string
		if isReq {
			pkgs = parseRequirements(b)
		} else {
			pkgs = parseTomlPackages(b)
		}
		for _, nv := range pkgs {
			out = append(out, pyLocked{pyName(nv[0]), nv[1], comp, componentsDir + "/" + rel})
		}
		return nil
	})
	return out, found, err
}

var reReq = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._\-]*)(?:\[[^\]]*\])?\s*===?\s*([^\s;\\#]+)`)

// parseRequirements reads pinned "name==version" lines (pip-compile / uv
// export output); hashes, comments, markers and options are ignored.
func parseRequirements(b []byte) [][2]string {
	var out [][2]string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := reReq.FindStringSubmatch(line); m != nil {
			out = append(out, [2]string{m[1], m[2]})
		}
	}
	return out
}

var (
	reTomlTable = regexp.MustCompile(`^\[\[(package|packages)\]\]\s*$`)
	reTomlKV    = regexp.MustCompile(`^(name|version)\s*=\s*"([^"]*)"`)
)

// parseTomlPackages reads the [[package]] (uv.lock) or [[packages]]
// (pylock.toml, PEP 751) tables: their name and version keys.
func parseTomlPackages(b []byte) [][2]string {
	var out [][2]string
	var cur *[2]string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if reTomlTable.MatchString(line) {
			if cur != nil && cur[0] != "" {
				out = append(out, *cur)
			}
			cur = &[2]string{}
			continue
		}
		if strings.HasPrefix(line, "[") {
			if cur != nil && cur[0] != "" {
				out = append(out, *cur)
			}
			cur = nil
			continue
		}
		if cur == nil {
			continue
		}
		if m := reTomlKV.FindStringSubmatch(line); m != nil {
			if m[1] == "name" && cur[0] == "" {
				cur[0] = m[2]
			} else if m[1] == "version" && cur[1] == "" {
				cur[1] = m[2]
			}
		}
	}
	if cur != nil && cur[0] != "" {
		out = append(out, *cur)
	}
	return out
}

func auditPython(opt Options, r *Report, al *allowlist) error {
	pf, err := loadPython(opt.Root)
	if err != nil {
		return err
	}
	locked, haveLocks, err := readPythonLocks(opt.Root)
	if err != nil {
		return err
	}
	// Every entry of python.json is judged, so the table itself stays clean.
	names := make([]string, 0, len(pf.Packages))
	for n := range pf.Packages {
		names = append(names, n)
	}
	sort.Strings(names)
	byName := map[string]*Component{}
	var comps []Component
	for _, n := range names {
		e := pf.Packages[n]
		c := Component{Kind: KindPython, Name: n, Version: strings.Join(e.Versions, ", "), License: e.License, Source: e.Source, Note: e.Note}
		if c.Source == "" {
			c.Source = "https://pypi.org/project/" + n + "/"
		}
		if !haveLocks {
			c.Group = strings.Join(e.Components, ", ")
		}
		comps = append(comps, c)
	}
	for i := range comps {
		byName[comps[i].Name] = &comps[i]
	}
	if !haveLocks {
		r.Skipped = append(r.Skipped, componentsDir+" lock files (not present yet; python.json judged as the approximate dependency set)")
	}
	groups := map[string]map[string]bool{}
	lockedVersions := map[string]map[string]bool{}
	for _, l := range locked {
		if why, bad := forbiddenPython[l.name]; bad {
			r.fail(KindPython, l.name, l.version, "", why+" (found in "+l.file+")")
			continue
		}
		c := byName[l.name]
		if c == nil {
			r.fail(KindPython, l.name, l.version, "", "no entry in "+pythonFile+" (locked in "+l.file+")")
			continue
		}
		if groups[l.name] == nil {
			groups[l.name] = map[string]bool{}
		}
		groups[l.name][l.component] = true
		if lockedVersions[l.name] == nil {
			lockedVersions[l.name] = map[string]bool{}
		}
		if l.version != "" {
			lockedVersions[l.name][l.version] = true
		}
	}
	for i := range comps {
		c := comps[i]
		if why, bad := forbiddenPython[c.Name]; bad {
			r.fail(KindPython, c.Name, c.Version, c.License, why+" (entry in "+pythonFile+")")
		}
		if haveLocks {
			if len(groups[c.Name]) == 0 {
				continue // not used by any component: not shipped
			}
			var g []string
			for k := range groups[c.Name] {
				g = append(g, k)
			}
			sort.Strings(g)
			c.Group = strings.Join(g, ", ")
			c.Version = strings.Join(sortedKeys(lockedVersions[c.Name]), ", ")
			c.Locked = true
		}
		var extra []string
		if e := pf.Packages[c.Name]; e.LicenseText != "" {
			lt, why := repoLicenseText(opt.Root, e.LicenseText, e.LicenseTextSHA256, e.License)
			if why != "" {
				extra = append(extra, why)
			} else {
				c.Texts = append(c.Texts, lt)
			}
		}
		judge(r, al, c, extra...)
	}
	return nil
}

// --- models ------------------------------------------------------------------------

// ModelEntry is one model of packaging/licenses/models.json.
type ModelEntry struct {
	Backend  string `json:"backend"`
	Name     string `json:"name"`
	License  string `json:"license"`
	Source   string `json:"source"`
	Upstream string `json:"upstream,omitempty"`
	Note     string `json:"note,omitempty"`
}

// modelRef is one model object of a component manifest.
type modelRef struct {
	id, license, note, source, upstream, revision, file string
}

// auditModels judges the speech recognition models. The component manifests
// (packaging/components/*.json) are authoritative: every object whose "id"
// is model:{backend}:{name}:{precision} is one model of the notices, with
// its own "license", "license_note", "source" and "revision". models.json is
// only a fallback for a manifest model without a "license" field (keyed by
// backend + name, any precision). A models.json entry that contradicts a
// manifest license fails; one that repeats it, or that no manifest model
// uses, gives a warning. Without any manifest, models.json is listed as is.
func auditModels(opt Options, r *Report, al *allowlist) error {
	var mf struct {
		Models []ModelEntry `json:"models"`
	}
	b, err := os.ReadFile(filepath.Join(opt.Root, modelsFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(b, &mf); err != nil {
			return fmt.Errorf("%s: %w", modelsFile, err)
		}
	}
	known := map[string]ModelEntry{}
	var keys []string
	for _, m := range mf.Models {
		k := m.Backend + ":" + m.Name
		if _, dup := known[k]; !dup {
			keys = append(keys, k)
		}
		known[k] = m
	}

	refs, err := manifestModels(opt.Root)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		for _, k := range keys {
			m := known[k]
			note := m.Note
			if m.Upstream != "" {
				note = strings.TrimSpace("from " + m.Upstream + ". " + note)
			}
			judge(r, al, Component{Kind: KindModel, Name: k, License: m.License, Source: m.Source, Note: note})
		}
		return nil
	}

	used := map[string]bool{}
	seen := map[string]modelRef{}
	var ids []string
	for _, m := range refs {
		if prev, ok := seen[m.id]; ok {
			if prev.license != m.license {
				r.fail(KindModel, m.id, "", m.license, fmt.Sprintf("license %q in %s differs from %q in %s", m.license, m.file, prev.license, prev.file))
			}
			continue
		}
		seen[m.id] = m
		ids = append(ids, m.id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := seen[id]
		parts := strings.Split(id, ":")
		if len(parts) < 3 {
			r.fail(KindModel, id, "", m.license, "malformed model id in "+m.file)
			continue
		}
		key := parts[1] + ":" + parts[2]
		fb, hasFB := known[key]
		if hasFB {
			used[key] = true
		}
		c := Component{Kind: KindModel, Name: id, License: m.license, Source: m.source, Note: m.note}
		if len(m.revision) >= 12 {
			c.Version = m.revision[:12]
		} else {
			c.Version = m.revision
		}
		if m.upstream != "" && !strings.Contains(c.Note, m.upstream) {
			c.Note = strings.TrimSpace("from " + m.upstream + ". " + c.Note)
		}
		var extra []string
		switch {
		case m.license != "" && hasFB && fb.License != m.license:
			extra = append(extra, fmt.Sprintf("%s says %q for %s but %s says %q; the manifest is authoritative: fix or remove the %s entry", modelsFile, fb.License, key, m.file, m.license, modelsFile))
		case m.license != "" && hasFB:
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s entry %s repeats the license of %s (the manifest is authoritative; the entry can be removed)", modelsFile, key, m.file))
		case m.license == "" && hasFB:
			c.License = fb.License
			c.Source = firstNonEmpty(c.Source, fb.Source)
			c.Note = strings.TrimSpace(c.Note + " License from " + modelsFile + ".")
		case m.license == "":
			extra = append(extra, "no license field in "+m.file+" and no entry in "+modelsFile)
		}
		if c.Source == "" {
			extra = append(extra, "source missing in "+m.file)
		}
		judge(r, al, c, extra...)
	}
	for _, k := range keys {
		if used[k] {
			continue
		}
		m := known[k]
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s entry %s matches no model of the component manifests (not listed)", modelsFile, k))
		// The table itself stays clean: an incompatible entry fails even unused.
		if ok, rs := Evaluate(m.License); !ok && al.match(KindModel, k, "", "") == nil {
			for _, s := range rs {
				r.fail(KindModel, k, "", m.License, s+" (in "+modelsFile+")")
			}
		}
	}
	return nil
}

// manifestModels reads the model objects of every JSON file under
// packaging/components, in a stable order.
func manifestModels(root string) ([]modelRef, error) {
	base := filepath.Join(root, componentsDir)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil, nil
	}
	var out []modelRef
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var v any
		if json.Unmarshal(b, &v) != nil {
			return nil // not a manifest
		}
		rel, _ := filepath.Rel(root, p)
		walkModels(v, func(o map[string]any) {
			s := func(k string) string { x, _ := o[k].(string); return strings.TrimSpace(x) }
			out = append(out, modelRef{id: s("id"), license: s("license"), note: s("license_note"), source: s("source"),
				upstream: s("upstream"), revision: s("revision"), file: filepath.ToSlash(rel)})
		})
		return nil
	})
	return out, err
}

func walkModels(v any, f func(obj map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		if id, ok := t["id"].(string); ok && strings.HasPrefix(id, "model:") {
			f(t)
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkModels(t[k], f)
		}
	case []any:
		for _, x := range t {
			walkModels(x, f)
		}
	}
}

// --- bundled binaries ---------------------------------------------------------------

// BinaryEntry is one binary of packaging/licenses/binaries.json.
type BinaryEntry struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	License   string   `json:"license"`
	Source    string   `json:"source"`
	Component string   `json:"component,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
	Note      string   `json:"note,omitempty"`
	// SourceCode (optional): where the corresponding source of this exact
	// build can be obtained (http(s) URLs).
	SourceCode []string `json:"source_code,omitempty"`
	// Bundled (optional): third-party libraries statically linked into the
	// binary; each license is judged like a component's.
	Bundled []BundledLib `json:"bundled,omitempty"`
}

func auditBinaries(opt Options, r *Report, al *allowlist) error {
	b, err := os.ReadFile(filepath.Join(opt.Root, binariesFile))
	if os.IsNotExist(err) {
		r.Skipped = append(r.Skipped, binariesFile+" (not present)")
		return nil
	}
	if err != nil {
		return err
	}
	var bf struct {
		Binaries []BinaryEntry `json:"binaries"`
	}
	if err := json.Unmarshal(b, &bf); err != nil {
		return fmt.Errorf("%s: %w", binariesFile, err)
	}
	for _, x := range bf.Binaries {
		var extra []string
		if x.Version == "" {
			extra = append(extra, "version missing")
		}
		if x.Source == "" {
			extra = append(extra, "source missing")
		}
		extra = append(extra, checkBinaryDetails(r, al, x)...)
		bin := x
		judge(r, al, Component{Kind: KindBinary, Name: x.Name, Version: x.Version, License: x.License, Source: x.Source,
			Group: strings.Join(append([]string{x.Component}, x.Platforms...), " "), Note: x.Note, Bin: &bin}, extra...)
	}
	return nil
}

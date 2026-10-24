// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kinds of third-party components (sections of THIRD_PARTY_NOTICES.md).
const (
	KindGo     = "go"
	KindVendor = "web-vendor"
	KindNPM    = "npm"
	KindPython = "python"
	KindModel  = "model"
	KindBinary = "binary"
	KindMedia  = "media"
	// KindReference: an upstream whose protocol was reimplemented without
	// copying code (SPEC-v3 V14.6); listed for its notices.
	KindReference = "reference"
)

// Component is one third-party item in the notices.
type Component struct {
	Kind    string
	Name    string
	Version string
	License string // SPDX expression
	Source  string
	Group   string // python: components using it; npm: "runtime"
	Note    string
	Texts   []LicenseText // reproduced in the appendix
	Allowed string        // allowlist reason when accepted by the allowlist
	// UserFetched: accepted by a "user-fetched" allowlist entry; not
	// distributed, downloaded on the user's computer (own notices section).
	UserFetched bool
	// Locked: a Python package pinned by a component lock file.
	Locked bool
	// Media and Ref carry the details of the media and reference sections.
	Media *MediaEntry
	Ref   *ReferenceEntry
	// Bin: the binaries.json entry (source code, statically linked libraries).
	Bin *BinaryEntry
}

// LicenseText is one license or notice file of a component.
type LicenseText struct {
	File string // path relative to the component (for the appendix heading)
	Text string
}

// Violation is one audit failure.
type Violation struct {
	Kind, Name, Version, License, Reason string
}

func (v Violation) String() string {
	s := v.Kind + " " + v.Name
	if v.Version != "" {
		s += "@" + v.Version
	}
	if v.License != "" {
		s += " [" + v.License + "]"
	}
	return s + ": " + v.Reason
}

// Report is the result of an audit.
type Report struct {
	Components []Component
	Violations []Violation
	Skipped    []string // sources that do not exist yet
	Warnings   []string
}

func (r *Report) fail(kind, name, version, license, reason string) {
	r.Violations = append(r.Violations, Violation{kind, name, version, license, reason})
}

// GoModule is one module linked into the program.
type GoModule struct {
	Path, Version, Dir string
}

// Options drive one audit run. Tests replace GoModules with fixtures.
type Options struct {
	Root      string
	GoModules func(root string) ([]GoModule, error) // nil: skip Go
}

// Paths inside the repository.
const (
	licDir        = "packaging/licenses"
	allowlistFile = licDir + "/allowlist.json"
	pythonFile    = licDir + "/python.json"
	modelsFile    = licDir + "/models.json"
	binariesFile  = licDir + "/binaries.json"
	mediaFile     = licDir + "/media.json"
	refsFile      = licDir + "/references.json"
	assetsDir     = "assets"
	componentsDir = "packaging/components"
	vendorDir     = "web/vendor"
	npmLockFile   = "desktop/package-lock.json"
	npmModulesDir = "desktop/node_modules"
)

// AllowEntry accepts one component (or one unrecognised notice file of a
// component) despite the policy. Reason is mandatory.
type AllowEntry struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"` // empty: any version
	File    string `json:"file,omitempty"`    // only this license/notice file
	License string `json:"license,omitempty"` // license to show in the notices
	// Scope is mandatory: "distributed" (shipped by LectureFlow) or
	// "user-fetched" (only for Python packages of the component lock files:
	// the component manager downloads them on the user's computer from PyPI
	// or a mirror; LectureFlow never distributes them).
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// Allowlist scopes.
const (
	ScopeDistributed = "distributed"
	ScopeUserFetched = "user-fetched"
)

type allowlist struct {
	Entries []AllowEntry `json:"entries"`
	used    []bool
}

func loadAllowlist(root string, r *Report) (*allowlist, error) {
	al := &allowlist{}
	b, err := os.ReadFile(filepath.Join(root, allowlistFile))
	if os.IsNotExist(err) {
		return al, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, al); err != nil {
		return nil, fmt.Errorf("%s: %w", allowlistFile, err)
	}
	al.used = make([]bool, len(al.Entries))
	for i, e := range al.Entries {
		if strings.TrimSpace(e.Reason) == "" {
			r.fail("allowlist", e.Kind+" "+e.Name, e.Version, "", fmt.Sprintf("%s entry %d has no reason", allowlistFile, i+1))
		}
		if e.Kind == "" || e.Name == "" {
			r.fail("allowlist", e.Kind+" "+e.Name, e.Version, "", fmt.Sprintf("%s entry %d needs kind and name", allowlistFile, i+1))
		}
		switch e.Scope {
		case ScopeDistributed:
		case ScopeUserFetched:
			if e.Kind != KindPython || e.File != "" {
				r.fail("allowlist", e.Kind+" "+e.Name, e.Version, "", fmt.Sprintf("%s entry %d: scope user-fetched is only for whole Python packages of the component lock files, not for kind %q", allowlistFile, i+1, e.Kind))
			}
		default:
			r.fail("allowlist", e.Kind+" "+e.Name, e.Version, "", fmt.Sprintf("%s entry %d: scope must be %q or %q, got %q", allowlistFile, i+1, ScopeDistributed, ScopeUserFetched, e.Scope))
		}
	}
	return al, nil
}

// valid reports an entry that may accept something: it has a reason and a
// scope that is allowed for its kind (invalid entries are violations).
func (e *AllowEntry) valid() bool {
	if strings.TrimSpace(e.Reason) == "" {
		return false
	}
	return e.Scope == ScopeDistributed || (e.Scope == ScopeUserFetched && e.Kind == KindPython && e.File == "")
}

// match returns the entry accepting the component (file == "" for the whole
// component). Entries without a reason never accept anything.
func (al *allowlist) match(kind, name, version, file string) *AllowEntry {
	for i := range al.Entries {
		e := &al.Entries[i]
		if !e.valid() || e.Kind != kind || !strings.EqualFold(e.Name, name) || e.File != file {
			continue
		}
		if e.Version != "" && e.Version != version {
			continue
		}
		al.used[i] = true
		return e
	}
	return nil
}

// judge records the component and its violation (unless allowlisted).
func judge(r *Report, al *allowlist, c Component, extra ...string) {
	reasons := extra
	// With no license and extra reasons, those already say why.
	if c.License != "" || len(extra) == 0 {
		if ok, rs := EvaluateWith(c.License, extraTableFor(c.Kind)); !ok {
			reasons = append(reasons, rs...)
		}
	}
	if e := al.match(c.Kind, c.Name, c.Version, ""); e != nil {
		c.Allowed = e.Reason
		if e.License != "" {
			c.License = e.License
		}
		reasons = nil
		if e.Scope == ScopeUserFetched {
			if c.Kind == KindPython && c.Locked {
				c.UserFetched = true
			} else {
				reasons = append(reasons, "allowlisted as user-fetched, but it is not a Python package of a component lock file")
			}
		}
	}
	for _, rs := range reasons {
		r.fail(c.Kind, c.Name, c.Version, c.License, rs)
	}
	r.Components = append(r.Components, c)
}

// Run performs the dependency audit (not the SPDX header check).
func Run(opt Options) (*Report, error) {
	r := &Report{}
	al, err := loadAllowlist(opt.Root, r)
	if err != nil {
		return nil, err
	}
	steps := []func(Options, *Report, *allowlist) error{auditGo, auditVendor, auditNPM, auditPython, auditOffline, auditException, auditModels, auditBinaries, auditMedia, auditReferences}
	for _, s := range steps {
		if err := s(opt, r, al); err != nil {
			return nil, err
		}
	}
	for i, used := range al.used {
		if !used && al.Entries[i].valid() {
			e := al.Entries[i]
			r.Warnings = append(r.Warnings, fmt.Sprintf("allowlist entry %s %s %s matches nothing (stale?)", e.Kind, e.Name, e.File))
		}
	}
	sort.SliceStable(r.Components, func(i, j int) bool {
		a, b := r.Components[i], r.Components[j]
		if a.Kind != b.Kind {
			return kindOrder(a.Kind) < kindOrder(b.Kind)
		}
		if !strings.EqualFold(a.Name, b.Name) {
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		return a.Version < b.Version
	})
	return r, nil
}

func kindOrder(k string) int {
	for i, x := range []string{KindGo, KindVendor, KindNPM, KindPython, KindModel, KindBinary, KindMedia, KindReference} {
		if x == k {
			return i
		}
	}
	return 99
}

// --- license files ---------------------------------------------------------

// isPrimaryLicenseName reports a top-level LICENSE / COPYING / LICENCE file
// (with an optional extension), the component's own license.
func isPrimaryLicenseName(name string) bool {
	u := strings.ToUpper(name)
	base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(u, ".TXT"), ".MD"), ".RST")
	return base == "LICENSE" || base == "LICENCE" || base == "COPYING"
}

// isNoticeName reports additional license or notice files: LICENSE-*,
// LICENSE.*, COPYING.*, NOTICE*, THIRD_PARTY*, third-party*.
func isNoticeName(name string) bool {
	u := strings.ToUpper(name)
	if isPrimaryLicenseName(name) {
		return false
	}
	for _, p := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "THIRD_PARTY", "THIRD-PARTY", "THIRDPARTY"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// licenseFiles collects the license files of a component directory: the
// top-level primary file, further top-level notice files, and every file in
// a top-level LICENSES/ directory (REUSE layout).
func licenseFiles(dir string) (primary *LicenseText, notices []LicenseText, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			if strings.EqualFold(name, "LICENSES") {
				sub, err := os.ReadDir(filepath.Join(dir, name))
				if err != nil {
					return nil, nil, err
				}
				for _, s := range sub {
					if s.IsDir() {
						continue
					}
					b, err := os.ReadFile(filepath.Join(dir, name, s.Name()))
					if err != nil {
						return nil, nil, err
					}
					notices = append(notices, LicenseText{File: name + "/" + s.Name(), Text: string(b)})
				}
			}
			continue
		}
		if !isPrimaryLicenseName(name) && !isNoticeName(name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		lt := LicenseText{File: name, Text: string(b)}
		if isPrimaryLicenseName(name) && primary == nil {
			primary = &lt
		} else {
			notices = append(notices, lt)
		}
	}
	return primary, notices, nil
}

// classifyDir determines the license of a component from its files. The
// primary file must be recognised; each notice file must contain at least
// one recognised license (or be allowlisted by file name). All licenses
// found are combined with AND.
func classifyDir(r *Report, al *allowlist, kind, name, version, dir, declared string) (license string, texts []LicenseText, reasons []string, err error) {
	primary, notices, err := licenseFiles(dir)
	if err != nil {
		return "", nil, nil, err
	}
	var ids []string
	switch {
	case primary != nil:
		texts = append(texts, *primary)
		id := Identify(primary.Text)
		if id == "" {
			if e := al.match(kind, name, version, primary.File); e != nil && e.License != "" {
				id = e.License
			} else if declared != "" {
				reasons = append(reasons, fmt.Sprintf("%s not recognised (declared %q, not trusted without the text)", primary.File, declared))
			} else {
				reasons = append(reasons, primary.File+" not recognised")
			}
		}
		if id != "" {
			ids = append(ids, id)
		}
	case declared != "":
		ids = append(ids, declared)
	default:
		reasons = append(reasons, "no license file and no declared license")
	}
	for _, n := range notices {
		texts = append(texts, n)
		found := IdentifyAll(n.Text)
		if len(found) == 0 {
			if e := al.match(kind, name, version, n.File); e != nil {
				if e.License != "" {
					ids = append(ids, e.License)
				}
				continue
			}
			reasons = append(reasons, n.File+": no recognised license (allowlist the file with a reason if it is harmless)")
			continue
		}
		ids = append(ids, found...)
	}
	return joinAnd(ids...), texts, reasons, nil
}

// --- Go ----------------------------------------------------------------------

func auditGo(opt Options, r *Report, al *allowlist) error {
	if opt.GoModules == nil {
		r.Skipped = append(r.Skipped, "Go modules (not requested)")
		return nil
	}
	mods, err := opt.GoModules(opt.Root)
	if err != nil {
		return fmt.Errorf("listing Go modules: %w", err)
	}
	for _, m := range mods {
		c := Component{Kind: KindGo, Name: m.Path, Version: m.Version, Source: "https://pkg.go.dev/" + m.Path}
		if m.Dir == "" {
			r.fail(KindGo, m.Path, m.Version, "", "module not in the module cache (run go mod download)")
			r.Components = append(r.Components, c)
			continue
		}
		lic, texts, reasons, err := classifyDir(r, al, KindGo, m.Path, m.Version, m.Dir, "")
		if err != nil {
			return err
		}
		c.License, c.Texts = lic, texts
		judge(r, al, c, reasons...)
	}
	return nil
}

// --- web/vendor --------------------------------------------------------------

func auditVendor(opt Options, r *Report, al *allowlist) error {
	base := filepath.Join(opt.Root, vendorDir)
	ents, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		r.Skipped = append(r.Skipped, vendorDir+" (not present)")
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		name, version, declared, source := e.Name(), "", "", ""
		if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
			var pj struct {
				Name, Version, Homepage string
				License                 json.RawMessage
				Repository              json.RawMessage
			}
			if err := json.Unmarshal(b, &pj); err != nil {
				return fmt.Errorf("%s/%s/package.json: %w", vendorDir, e.Name(), err)
			}
			if pj.Name != "" {
				name = pj.Name
			}
			version, declared, source = pj.Version, npmLicense(pj.License), firstNonEmpty(repoURL(pj.Repository), pj.Homepage)
		}
		if version == "" || source == "" {
			v, s := readmeFacts(dir)
			version, source = firstNonEmpty(version, v), firstNonEmpty(source, s)
		}
		if source == "" {
			source = vendorDir + "/" + e.Name()
		}
		lic, texts, reasons, err := classifyDir(r, al, KindVendor, name, version, dir, declared)
		if err != nil {
			return err
		}
		if version == "" {
			reasons = append(reasons, "version unknown (add package.json or a Version line to the README)")
		}
		judge(r, al, Component{Kind: KindVendor, Name: name, Version: version, License: lic, Source: source, Texts: texts, Note: vendorDir + "/" + e.Name()}, reasons...)
	}
	return nil
}

func readmeFacts(dir string) (version, source string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasPrefix(strings.ToUpper(e.Name()), "README") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if m := reReadmeVersion.FindStringSubmatch(string(b)); m != nil {
			version = m[1]
		}
		if m := reGitHubURL.FindString(string(b)); m != "" {
			source = m
		}
	}
	return version, source
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

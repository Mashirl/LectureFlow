// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// All tests are offline: fixture repositories are built in temporary
// directories, Go modules and git file lists are injected.

type repo struct {
	t    *testing.T
	root string
}

func newRepo(t *testing.T) *repo { return &repo{t: t, root: t.TempDir()} }

func (r *repo) write(rel, content string) string {
	r.t.Helper()
	p := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *repo) writeJSON(rel string, v any) {
	r.t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		r.t.Fatal(err)
	}
	r.write(rel, string(b))
}

// python writes python.json with the given name → license entries and a
// lock file of component "asr-test" that pins all of them.
func (r *repo) python(pkgs map[string]string) {
	r.t.Helper()
	tbl := PyFile{Packages: map[string]PyEntry{}}
	var lock strings.Builder
	for n, l := range pkgs {
		tbl.Packages[n] = PyEntry{License: l, Versions: []string{"1.0"}}
		lock.WriteString(n + "==1.0 \\\n    --hash=sha256:00\n")
	}
	r.writeJSON(pythonFile, tbl)
	r.write(componentsDir+"/asr-test/requirements.txt", lock.String())
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "licenses", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func audit(t *testing.T, root string, mods []GoModule) *Report {
	t.Helper()
	opt := Options{Root: root}
	if mods != nil {
		opt.GoModules = func(string) ([]GoModule, error) { return mods, nil }
	}
	rep, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func violationsOf(rep *Report, name string) []Violation {
	var out []Violation
	for _, v := range rep.Violations {
		if v.Name == name {
			out = append(out, v)
		}
	}
	return out
}

// --- policy -------------------------------------------------------------------

func TestEvaluate(t *testing.T) {
	cases := []struct {
		expr string
		ok   bool
	}{
		{"MIT", true},
		{"Apache-2.0", true},
		{"GPL-3.0-only", true},
		{"GPL-3.0-or-later", true},
		{"GPL-3.0", true}, // deprecated id
		{"AGPL-3.0-or-later", true},
		{"LGPL-2.1-or-later", true},
		{"LGPL-3.0-only", true},
		{"MPL-2.0", true},
		{"BSD-2-Clause", true},
		{"BSD-3-Clause", true},
		{"ISC", true},
		{"Zlib", true},
		{"Python-2.0", true},
		{"PSF-2.0", true},
		{"CC0-1.0", true},
		{"Unlicense", true},
		{"GPL-2.0-or-later", true},
		{"GPL-2.0+", true},
		{"mit", true}, // case-insensitive
		{"GPL-2.0-only", false},
		{"GPL-2.0", false}, // deprecated bare id means "only"
		{"SSPL-1.0", false},
		{"CC-BY-NC-4.0", false},
		{"CC-BY-NC-SA-4.0", false},
		{"BUSL-1.1", false},
		{"", false},
		{"NOASSERTION", false},
		{"UNKNOWN", false},
		{"LicenseRef-NVIDIA-Proprietary", false},
		{"Some Custom License", false},
		{"MIT OR GPL-2.0-only", true},
		{"MIT AND GPL-2.0-only", false},
		{"(MIT OR Apache-2.0) AND BSD-3-Clause", true},
		{"(MIT OR Apache-2.0) AND CC-BY-NC-4.0", false},
		{"Apache-2.0 WITH LLVM-exception", true},
		{"GPL-3.0-or-later WITH GCC-exception-3.1", true},
		{"GPL-2.0-only WITH Classpath-exception-2.0", false},
		{"MIT WITH Unknown-exception", false},
		{"Apache-2.0 AND Apache-2.0 WITH LLVM-exception AND BSD-2-Clause AND BSL-1.0 AND MIT", true},
		{"MIT AND (", false},
		// Permissive ids found in the bundled FFmpeg libraries (gate-fix-license-2).
		{"MIT-Modern-Variant", true},
		{"HPND-sell-variant", true},
		{"libpng-2.0", true},
		{"BSD-2-Clause-Patent", true},
		{"WTFPL", true},
		{"ZPL-2.1", true},
		{"zpl-2.1", true},
		{"ZPL-2.1 AND MIT AND BSD-3-Clause", true},
		{"BSD-3-Clause WITH PCRE2-exception", true},
		{"GPL-3.0-or-later WITH Bison-exception-2.2", true},
		{"BSD-3-Clause AND BSD-2-Clause AND MIT AND Apache-2.0 AND GPL-3.0-or-later WITH Bison-exception-2.2", true},
		// An exception never rescues an incompatible base license.
		{"GPL-2.0-only WITH Bison-exception-2.2", false},
		{"LicenseRef-X WITH PCRE2-exception", false},
		// Neighbouring ids that are not in the table stay rejected.
		{"ZPL-1.1", false},
		{"ZPL-2.0", false},
		{"LGPL-2.0-or-later AND GPL-2.0-only AND MIT", false}, // zvbi
	}
	for _, c := range cases {
		ok, reasons := Evaluate(c.expr)
		if ok != c.ok {
			t.Errorf("Evaluate(%q) = %v %v, want %v", c.expr, ok, reasons, c.ok)
		}
		if !ok && len(reasons) == 0 {
			t.Errorf("Evaluate(%q) failed without a reason", c.expr)
		}
	}
	if _, rs := Evaluate("CC-BY-NC-4.0"); !strings.Contains(rs[0], "non-commercial") {
		t.Errorf("non-commercial reason = %v", rs)
	}
	if _, rs := Evaluate("GPL-2.0-only"); !strings.Contains(rs[0], "GPL-2.0 only") {
		t.Errorf("GPL-2.0-only reason = %v", rs)
	}
}

func TestIdentify(t *testing.T) {
	cases := map[string]string{
		"mit.txt":           "MIT",
		"apache-2.0.txt":    "Apache-2.0",
		"gpl-3.0.txt":       "GPL-3.0-only",
		"gpl-2.0.txt":       "GPL-2.0-only",
		"lgpl-2.1.txt":      "LGPL-2.1-only",
		"cc-by-nc-4.0.txt":  "CC-BY-NC-4.0",
		"bsd-3.txt":         "BSD-3-Clause",
		"bsd-then-gpl3.txt": "BSD-3-Clause", // own license first, bundled runtime after
	}
	for f, want := range cases {
		if got := Identify(fixture(t, f)); got != want {
			t.Errorf("Identify(%s) = %q, want %q", f, got, want)
		}
	}
	if got := Identify("All rights reserved. Do not copy."); got != "" {
		t.Errorf("unknown text identified as %q", got)
	}
	agpl, err := os.ReadFile(filepath.Join("..", "..", "LICENSE"))
	if err == nil {
		if got := Identify(string(agpl)); got != "AGPL-3.0-only" {
			t.Errorf("Identify(LICENSE) = %q", got)
		}
	}
	all := IdentifyAll(fixture(t, "bsd-then-gpl3.txt"))
	if !reflect.DeepEqual(all, []string{"GPL-3.0-only", "BSD-3-Clause"}) {
		t.Errorf("IdentifyAll = %v", all)
	}
}

// --- V12.1: Python packages of a component -------------------------------------

func TestNonCommercialPackageFails(t *testing.T) {
	r := newRepo(t)
	r.python(map[string]string{"good-mit": "MIT", "nc-weights": "CC-BY-NC-4.0"})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 1 || rep.Violations[0].Name != "nc-weights" || !strings.Contains(rep.Violations[0].Reason, "non-commercial") {
		t.Fatalf("violations = %v, want only nc-weights (non-commercial)", rep.Violations)
	}
	code := auditMain([]string{"-root", r.root, "-check", "-only", "deps"}, &bytes.Buffer{}, &bytes.Buffer{}, nil, nil)
	if code != 1 {
		t.Fatalf("auditMain exit = %d, want 1", code)
	}
}

func TestCompatiblePackagesPass(t *testing.T) {
	r := newRepo(t)
	r.python(map[string]string{
		"pkg-mit":    "MIT",
		"pkg-apache": "Apache-2.0",
		"pkg-gpl3":   "GPL-3.0-only",
		"pkg-gpl3p":  "GPL-3.0-or-later",
		"pkg-lgpl":   "LGPL-2.1-or-later",
		"pkg-dual":   "Apache-2.0 OR BSD-2-Clause",
	})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	var names []string
	for _, c := range rep.Components {
		if c.Kind == KindPython {
			names = append(names, c.Name)
			if c.Group != "asr-test" {
				t.Errorf("%s group = %q", c.Name, c.Group)
			}
		}
	}
	if len(names) != 6 {
		t.Fatalf("python components = %v", names)
	}
	var out bytes.Buffer
	if code := auditMain([]string{"-root", r.root, "-check", "-only", "deps"}, &out, &bytes.Buffer{}, nil, nil); code != 0 {
		t.Fatalf("auditMain exit = %d: %s", code, out.String())
	}
}

func TestGPL2OnlyFails(t *testing.T) {
	r := newRepo(t)
	r.python(map[string]string{"old-gpl2": "GPL-2.0-only", "later-gpl2": "GPL-2.0-or-later"})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 1 || rep.Violations[0].Name != "old-gpl2" {
		t.Fatalf("violations = %v, want only old-gpl2", rep.Violations)
	}
}

func TestLockedPackageNeedsTableEntry(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(pythonFile, PyFile{Packages: map[string]PyEntry{"Known_Pkg": {License: "MIT"}}})
	r.write(componentsDir+"/asr-x/requirements.txt", "# pinned\nknown-pkg==2.0\nunknown-pkg==1.0 ; sys_platform == 'win32'\nPyMuPDF==1.24\n")
	rep := audit(t, r.root, nil)
	if len(violationsOf(rep, "unknown-pkg")) != 1 {
		t.Errorf("unknown-pkg not reported: %v", rep.Violations)
	}
	if v := violationsOf(rep, "pymupdf"); len(v) != 1 || !strings.Contains(v[0].Reason, "V6") {
		t.Errorf("pymupdf not reported: %v", rep.Violations)
	}
	if len(violationsOf(rep, "known-pkg")) != 0 {
		t.Errorf("known-pkg (normalised name) reported: %v", rep.Violations)
	}
}

func TestPythonTableJudgedWithoutLocks(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(pythonFile, PyFile{Packages: map[string]PyEntry{"a": {License: "MIT", Components: []string{"asr-cuda"}}, "b": {License: "LicenseRef-Proprietary"}}})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 1 || rep.Violations[0].Name != "b" {
		t.Fatalf("violations = %v", rep.Violations)
	}
	if !strings.Contains(strings.Join(rep.Skipped, "\n"), componentsDir) {
		t.Errorf("missing lock files not reported as skipped: %v", rep.Skipped)
	}
}

func TestLockParsers(t *testing.T) {
	req := parseRequirements([]byte("a==1.0\nB_c[extra]==2.0.post1 ; python_version>'3'\n# c==3\n-e .\nd>=1\nfoo == 4 \\\n  --hash=sha256:x\n"))
	want := [][2]string{{"a", "1.0"}, {"B_c", "2.0.post1"}, {"foo", "4"}}
	if !reflect.DeepEqual(req, want) {
		t.Errorf("parseRequirements = %v", req)
	}
	toml := parseTomlPackages([]byte("version = 1\n[[package]]\nname = \"x\"\nversion = \"1.2\"\n[package.metadata]\nversion = \"9\"\n[[package]]\nname = \"y\"\nversion = \"3\"\n[[packages]]\nname = \"z\"\nversion = \"4\"\n"))
	wantT := [][2]string{{"x", "1.2"}, {"y", "3"}, {"z", "4"}}
	if !reflect.DeepEqual(toml, wantT) {
		t.Errorf("parseTomlPackages = %v", toml)
	}
}

// --- Go modules (license text from the module directory) -----------------------

func TestGoModulesByLicenseText(t *testing.T) {
	r := newRepo(t)
	mod := func(name, file, text string) GoModule {
		dir := filepath.Join(r.root, "modcache", name)
		r.write("modcache/"+name+"/go.mod", "module "+name+"\n")
		if file != "" {
			r.write("modcache/"+name+"/"+file, text)
		}
		return GoModule{Path: "example.com/" + name, Version: "v1.0.0", Dir: dir}
	}
	pass := []GoModule{
		mod("mit", "LICENSE", fixture(t, "mit.txt")),
		mod("apache", "LICENSE.txt", fixture(t, "apache-2.0.txt")),
		mod("gpl3", "COPYING", fixture(t, "gpl-3.0.txt")),
	}
	rep := audit(t, r.root, pass)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	got := map[string]string{}
	for _, c := range rep.Components {
		got[c.Name] = c.License
	}
	if got["example.com/mit"] != "MIT" || got["example.com/apache"] != "Apache-2.0" || got["example.com/gpl3"] != "GPL-3.0-only" {
		t.Errorf("licenses = %v", got)
	}

	bad := append(pass,
		mod("gpl2", "LICENSE", fixture(t, "gpl-2.0.txt")),
		mod("nc", "LICENSE.md", fixture(t, "cc-by-nc-4.0.txt")),
		mod("none", "", ""),
		mod("odd", "LICENSE", "Proprietary. All rights reserved."),
		GoModule{Path: "example.com/missing", Version: "v1.0.0"},
	)
	rep = audit(t, r.root, bad)
	for _, n := range []string{"gpl2", "nc", "none", "odd", "missing"} {
		if len(violationsOf(rep, "example.com/"+n)) == 0 {
			t.Errorf("%s not reported: %v", n, rep.Violations)
		}
	}
	if len(rep.Violations) != 5 {
		t.Errorf("violations = %v", rep.Violations)
	}
}

func TestNoticeFilesAndAllowlistByFile(t *testing.T) {
	r := newRepo(t)
	dir := filepath.Join(r.root, "mod")
	r.write("mod/LICENSE", fixture(t, "bsd-3.txt"))
	r.write("mod/LICENSE-3RD-PARTY.md", "# Third party\n\n"+fixture(t, "mit.txt"))
	r.write("mod/LICENSE-LOGO", "https://example.org/logo.png\n")
	mods := []GoModule{{Path: "example.com/mod", Version: "v1.2.3", Dir: dir}}

	rep := audit(t, r.root, mods)
	if v := violationsOf(rep, "example.com/mod"); len(v) != 1 || !strings.Contains(v[0].Reason, "LICENSE-LOGO") {
		t.Fatalf("violations = %v", rep.Violations)
	}
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{{Kind: KindGo, Name: "example.com/mod", File: "LICENSE-LOGO", Scope: ScopeDistributed, Reason: "logo attribution only"}}})
	rep = audit(t, r.root, mods)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	if c := rep.Components[0]; c.License != "BSD-3-Clause AND MIT" || len(c.Texts) != 3 {
		t.Errorf("component = %s, %d texts", c.License, len(c.Texts))
	}
}

// --- allowlist -------------------------------------------------------------------

func TestAllowlistNeedsReason(t *testing.T) {
	r := newRepo(t)
	r.python(map[string]string{"weird": "LicenseRef-Weird"})
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{{Kind: KindPython, Name: "weird", Scope: ScopeDistributed, Reason: "  "}}})
	rep := audit(t, r.root, nil)
	var allow, pkg bool
	for _, v := range rep.Violations {
		allow = allow || (v.Kind == "allowlist" && strings.Contains(v.Reason, "no reason"))
		pkg = pkg || v.Name == "weird"
	}
	if !allow || !pkg {
		t.Fatalf("want an allowlist violation and the package still failing, got %v", rep.Violations)
	}

	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{{Kind: KindPython, Name: "weird", Scope: ScopeDistributed, Reason: "reviewed: permissive in-house license of a test fixture", License: "LicenseRef-Weird"}}})
	rep = audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	if c := rep.Components[0]; c.Allowed == "" {
		t.Errorf("allowlisted component not marked: %+v", c)
	}
	if !strings.Contains(RenderNotices(rep), "Accepted by allowlist: reviewed") {
		t.Errorf("notices do not show the allowlist reason")
	}
}

func TestStaleAllowlistWarns(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{{Kind: KindNPM, Name: "gone", Scope: ScopeDistributed, Reason: "was needed once"}}})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 || len(rep.Warnings) == 0 {
		t.Fatalf("violations %v warnings %v", rep.Violations, rep.Warnings)
	}
}

// --- npm, vendor, models, binaries -------------------------------------------------

func TestNPMLockfile(t *testing.T) {
	r := newRepo(t)
	if rep := audit(t, r.root, nil); !strings.Contains(strings.Join(rep.Skipped, "\n"), npmLockFile) {
		t.Fatalf("missing lockfile not reported as skipped: %v", rep.Skipped)
	}
	r.writeJSON(npmLockFile, map[string]any{
		"lockfileVersion": 3,
		"packages": map[string]map[string]any{
			"":                                {"name": "lectureflow-desktop", "license": "AGPL-3.0-or-later"},
			"node_modules/electron":           {"version": "44.9.3", "license": "MIT", "dev": true},
			"node_modules/electron-builder":   {"version": "26.17.0", "license": "SSPL-1.0", "dev": true},
			"node_modules/runtime-ok":         {"version": "1.0.0", "license": "ISC"},
			"node_modules/@scope/obj-license": {"version": "2.0.0", "license": map[string]string{"type": "MIT"}},
			"node_modules/a/node_modules/nc":  {"version": "0.1.0", "license": "CC-BY-NC-4.0"},
			"node_modules/nolicense":          {"version": "0.0.1"},
		},
	})
	rep := audit(t, r.root, nil)
	bad := map[string]bool{}
	for _, v := range rep.Violations {
		bad[v.Name] = true
	}
	if !reflect.DeepEqual(bad, map[string]bool{"nc": true, "nolicense": true}) {
		t.Fatalf("violations = %v", rep.Violations)
	}
	seen := map[string]bool{}
	for _, c := range rep.Components {
		seen[c.Name] = true
	}
	if !seen["electron"] || !seen["@scope/obj-license"] || seen["electron-builder"] {
		t.Errorf("components = %v", seen)
	}
}

func TestVendorDirectory(t *testing.T) {
	r := newRepo(t)
	r.write("web/vendor/lib-a/LICENSE", fixture(t, "mit.txt"))
	r.write("web/vendor/lib-a/LICENSES/Apache-2.0.txt", fixture(t, "apache-2.0.txt"))
	r.write("web/vendor/lib-a/README.md", "# lib-a\n\n- **Version**: 1.4.2, from npm\n- [lib-a](https://github.com/example/lib-a)\n")
	r.write("web/vendor/lib-b/package.json", `{"name":"lib-b","version":"0.3.0","license":"CC-BY-NC-4.0","repository":{"url":"https://example.org/b"}}`)
	r.write("web/vendor/lib-b/LICENSE", fixture(t, "cc-by-nc-4.0.txt"))
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 1 || rep.Violations[0].Name != "lib-b" {
		t.Fatalf("violations = %v", rep.Violations)
	}
	a := rep.Components[0]
	if a.Name != "lib-a" || a.Version != "1.4.2" || a.License != "MIT AND Apache-2.0" || a.Source != "https://github.com/example/lib-a" {
		t.Errorf("lib-a = %+v", a)
	}
}

func TestModels(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(modelsFile, map[string]any{"models": []ModelEntry{
		{Backend: "faster-whisper", Name: "large-v3", License: "MIT", Source: "https://example.org/fw"},
		{Backend: "test", Name: "nc", License: "CC-BY-NC-4.0", Source: "https://example.org/nc"},
	}})
	r.writeJSON(componentsDir+"/manifest.json", map[string]any{"components": []map[string]any{
		{"id": "model:faster-whisper:large-v3:float16"},
		{"id": "model:openvino:medium:int8"},
		{"id": "model:mlx:tiny:fp16", "license": "CC-BY-NC-SA-4.0"},
		{"id": "asr-cuda"},
	}})
	rep := audit(t, r.root, nil)
	bad := map[string]bool{}
	for _, v := range rep.Violations {
		bad[v.Name] = true
	}
	want := map[string]bool{"test:nc": true, "model:openvino:medium:int8": true, "model:mlx:tiny:fp16": true}
	if !reflect.DeepEqual(bad, want) {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

func TestBinaries(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(binariesFile, map[string]any{"binaries": []BinaryEntry{
		{Name: "ffmpeg", Version: "7.1", License: "LGPL-2.1-or-later", Source: "https://example.org/ffmpeg"},
		{Name: "ffmpeg-gpl", Version: "7.1", License: "GPL-2.0-only", Source: "https://example.org/ffmpeg"},
		{Name: "uv", License: "MIT OR Apache-2.0", Source: "https://example.org/uv"},
	}})
	rep := audit(t, r.root, nil)
	bad := map[string]bool{}
	for _, v := range rep.Violations {
		bad[v.Name] = true
	}
	if !reflect.DeepEqual(bad, map[string]bool{"ffmpeg-gpl": true, "uv": true}) { // uv: version missing
		t.Fatalf("violations = %v", rep.Violations)
	}
}

// --- SPDX headers ---------------------------------------------------------------------

func TestSPDXCheck(t *testing.T) {
	r := newRepo(t)
	hdr := "// " + spdxTag + "\n// Copyright (C) 2026 LectureFlow contributors.\n"
	files := map[string]string{
		"cmd/ok.go":                  hdr + "\npackage main\n",
		"cmd/missing.go":             "package main\n",
		"worker/ok.py":               "#!/usr/bin/env python3\n# " + spdxTag + "\n",
		"worker/late.py":             "\n\n\n\n\n\n# " + spdxTag + "\n", // line 7
		"worker/line6.py":            "\n\n\n\n\n# " + spdxTag + "\n",   // line 6
		"web/index.html":             "<!doctype html>\n<!-- " + spdxTag + " -->\n",
		"web/app.mjs":                "export {}\n",
		"web/style.css":              "/* " + spdxTag + " */\n",
		"internal/db/m.sql":          "-- nothing here\n",
		"make.ps1":                   "# " + spdxTag + "\n",
		"run.sh":                     "#!/bin/sh\necho hi\n",
		"web/vendor/lib/x.js":        "third party\n",
		"internal/x/testdata/a.go":   "package a\n",
		"docs/demo.html":             "<p>doc</p>\n",
		"desktop/node_modules/m.cjs": "module.exports = 1\n",
		"README.md":                  "no header needed\n",
		"config.toml":                "x = 1\n",
	}
	var list []string
	for f, c := range files {
		r.write(f, c)
		list = append(list, f)
	}
	list = append(list, "deleted/gone.go") // tracked, deleted in the work tree
	missing, err := CheckSPDX(r.root, list)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd/missing.go", "internal/db/m.sql", "run.sh", "web/app.mjs", "worker/late.py"}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}

	var out bytes.Buffer
	code := auditMain([]string{"-root", r.root, "-check", "-only", "spdx"}, &out, &bytes.Buffer{}, nil, func(string) ([]string, error) { return list, nil })
	if code != 1 || !strings.Contains(out.String(), "FAIL cmd/missing.go") {
		t.Fatalf("exit %d, output %s", code, out.String())
	}
}

// --- command line and notices ------------------------------------------------------------

func TestCheckDoesNotWriteAndWriteMode(t *testing.T) {
	r := newRepo(t)
	r.python(map[string]string{"pkg": "MIT"})
	r.write("mod/LICENSE", fixture(t, "mit.txt"))
	mods := func(string) ([]GoModule, error) {
		return []GoModule{{Path: "example.com/mod", Version: "v1.0.0", Dir: filepath.Join(r.root, "mod")}}, nil
	}
	tracked := func(string) ([]string, error) { return nil, nil }
	notices := filepath.Join(r.root, "THIRD_PARTY_NOTICES.md")

	if code := auditMain([]string{"-root", r.root, "-check"}, &bytes.Buffer{}, &bytes.Buffer{}, mods, tracked); code != 0 {
		t.Fatalf("check exit = %d", code)
	}
	if _, err := os.Stat(notices); !os.IsNotExist(err) {
		t.Fatalf("-check wrote the notices file")
	}
	if code := auditMain([]string{"-root", r.root}, &bytes.Buffer{}, &bytes.Buffer{}, mods, tracked); code != 0 {
		t.Fatalf("write exit = %d", code)
	}
	b, err := os.ReadFile(notices)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"| example.com/mod | v1.0.0 | MIT |", "| pkg | 1.0 | MIT | asr-test |", "Used by: example.com/mod v1.0.0 (LICENSE)", "Copyright (c) 2026 Example Authors"} {
		if !strings.Contains(s, want) {
			t.Errorf("notices lack %q", want)
		}
	}
	if strings.Contains(s, "Unresolved audit findings") {
		t.Errorf("clean audit lists findings")
	}
	if code := auditMain([]string{"-root", r.root}, &bytes.Buffer{}, &bytes.Buffer{}, mods, tracked); code != 0 {
		t.Fatal("second run failed")
	}
	if b2, _ := os.ReadFile(notices); !bytes.Equal(b, b2) {
		t.Errorf("notices are not deterministic")
	}
	if code := auditMain([]string{"-only", "nope"}, &bytes.Buffer{}, &bytes.Buffer{}, mods, tracked); code != 2 {
		t.Errorf("bad -only exit = %d", code)
	}
}

func TestNoticesListFindingsAndShareTexts(t *testing.T) {
	r := newRepo(t)
	r.write("a/LICENSE", fixture(t, "mit.txt"))
	r.write("b/LICENSE", fixture(t, "mit.txt"))
	r.write("c/LICENSE", fixture(t, "gpl-2.0.txt"))
	rep := audit(t, r.root, []GoModule{
		{Path: "x/a", Version: "v1", Dir: filepath.Join(r.root, "a")},
		{Path: "x/b", Version: "v1", Dir: filepath.Join(r.root, "b")},
		{Path: "x/c", Version: "v1", Dir: filepath.Join(r.root, "c")},
	})
	s := RenderNotices(rep)
	if !strings.Contains(s, "Unresolved audit findings") || !strings.Contains(s, "x/c@v1") {
		t.Errorf("findings missing from notices")
	}
	if strings.Count(s, "Permission is hereby granted") != 1 || !strings.Contains(s, "Used by: x/a v1 (LICENSE); x/b v1 (LICENSE)") {
		t.Errorf("identical license texts are not shared")
	}
}

func TestComponentLocksLayout(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(pythonFile, PyFile{Packages: map[string]PyEntry{
		"pkg-a":  {License: "MIT"},
		"pkg-b":  {License: "BSD-3-Clause"},
		"unused": {License: "LicenseRef-Unknown"}, // not locked: not shipped, not judged
	}})
	r.write(componentsDir+"/locks/asr-cuda-windows-x64.txt", "# uv pip compile\npkg-a==1.0 \\n    --hash=sha256:00\n    # via x\n")
	r.write(componentsDir+"/locks/asr-cuda-darwin-arm64.txt", "pkg-a==1.1\n")
	r.write(componentsDir+"/locks/python-windows-x64.txt", "pkg-a==1.0\npkg_b==2.0\n")
	r.write(componentsDir+"/requirements/asr-cuda.in", "nothing==9\n")
	r.write(componentsDir+"/requirements/asr-cuda.constraints.txt", "notlocked==9\n")
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	got := map[string]Component{}
	for _, c := range rep.Components {
		got[c.Name] = c
	}
	if _, ok := got["unused"]; ok {
		t.Errorf("unlocked package listed")
	}
	if a := got["pkg-a"]; a.Group != "asr-cuda, python" || a.Version != "1.0, 1.1" {
		t.Errorf("pkg-a = group %q version %q", a.Group, a.Version)
	}
	if b := got["pkg-b"]; b.Group != "python" || b.Version != "2.0" {
		t.Errorf("pkg-b = group %q version %q", b.Group, b.Version)
	}
	r.write(componentsDir+"/locks/asr-x-linux-x64.txt", "nc-model-lib==1.0\n")
	rep = audit(t, r.root, nil)
	if v := violationsOf(rep, "nc-model-lib"); len(v) != 1 || !strings.Contains(v[0].Reason, "no entry") {
		t.Errorf("violations = %v", rep.Violations)
	}
}

func TestAllowlistScope(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(pythonFile, PyFile{Packages: map[string]PyEntry{
		"cuda-blob":  {License: "LicenseRef-NVIDIA-Proprietary"},
		"loose-blob": {License: "LicenseRef-NVIDIA-Proprietary"}, // in python.json, not locked
		"ok":         {License: "MIT"},
	}})
	r.write(componentsDir+"/locks/asr-cuda-windows-x64.txt", "cuda-blob==12.9\nok==1.0\n")
	r.write("mod/LICENSE", fixture(t, "gpl-2.0.txt"))
	r.writeJSON(npmLockFile, map[string]any{"lockfileVersion": 3, "packages": map[string]map[string]any{
		"node_modules/nc": {"version": "1.0.0", "license": "CC-BY-NC-4.0"},
	}})
	r.writeJSON(binariesFile, map[string]any{"binaries": []BinaryEntry{{Name: "x264", Version: "1", License: "GPL-2.0-only", Source: "https://example.org"}}})
	mods := []GoModule{{Path: "example.com/mod", Version: "v1", Dir: filepath.Join(r.root, "mod")}}
	why := "not distributed: downloaded from PyPI on the user's computer"
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{
		{Kind: KindPython, Name: "cuda-blob", Scope: ScopeUserFetched, Reason: why},
		{Kind: KindGo, Name: "example.com/mod", Scope: ScopeUserFetched, Reason: why},
		{Kind: KindNPM, Name: "nc", Scope: ScopeUserFetched, Reason: why},
		{Kind: KindBinary, Name: "x264", Scope: ScopeUserFetched, Reason: why},
		{Kind: KindPython, Name: "ok", Reason: "no scope"},
	}})
	r.exception("cuda-blob")
	rep := audit(t, r.root, mods)
	bad := map[string]int{}
	for _, v := range rep.Violations {
		bad[v.Kind+" "+v.Name]++
	}
	want := map[string]int{
		"allowlist go example.com/mod": 1, "go example.com/mod": 1, // entry invalid, so the module still fails
		"allowlist npm nc": 1, "npm nc": 1,
		"allowlist binary x264": 1, "binary x264": 1,
		"allowlist python ok": 1, // scope missing
	}
	if !reflect.DeepEqual(bad, want) {
		t.Fatalf("violations = %v", rep.Violations)
	}
	var cuda Component
	for _, c := range rep.Components {
		if c.Name == "cuda-blob" {
			cuda = c
		}
	}
	if !cuda.UserFetched || !cuda.Locked {
		t.Fatalf("cuda-blob = %+v", cuda)
	}
	s := RenderNotices(rep)
	i7 := strings.Index(s, "## 7. Downloaded on your computer")
	i4 := strings.Index(s, "## 4. Python packages")
	row := strings.Index(s, "| cuda-blob | 12.9 | LicenseRef-NVIDIA-Proprietary | asr-cuda |")
	if i7 < 0 || i4 < 0 || row < i7 || strings.Contains(s[i4:i7], "cuda-blob") || !strings.Contains(s[i7:], "Not distributed by LectureFlow: "+why) {
		t.Errorf("user-fetched section wrong:\n%s", s[i4:])
	}

	// user-fetched only covers packages of the component lock files.
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{
		{Kind: KindPython, Name: "loose-blob", Scope: ScopeUserFetched, Reason: why},
	}})
	r.write(componentsDir+"/locks/asr-cuda-windows-x64.txt", "ok==1.0\n")
	os.Remove(filepath.Join(r.root, componentsDir, "locks", "asr-cuda-windows-x64.txt"))
	rep = audit(t, r.root, nil)
	if v := violationsOf(rep, "loose-blob"); len(v) != 1 || !strings.Contains(v[0].Reason, "not a Python package of a component lock file") {
		t.Errorf("violations = %v", rep.Violations)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The browser extension's vendored files (extension.go), on fixture
// repositories shaped like extension/src/vendor.

const (
	polyfillJS  = "/* webextension-polyfill - v0.12.0 */\n/* This Source Code Form is subject to the terms of the Mozilla Public\n * License, v. 2.0. */\nglobalThis.browser = globalThis.chrome;\n"
	mplText     = "Mozilla Public License Version 2.0\n==================================\n\n1. Definitions\n--------------\n"
	polyIntegr  = "sha512-AAAAtestintegrityforthepolyfillAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	iconIntegr  = "sha512-BBBBtestintegrityfortheiconsBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=="
	polySource  = "https://registry.npmjs.org/webextension-polyfill/-/webextension-polyfill-0.12.0.tgz"
	iconsSource = "https://registry.npmjs.org/@tabler/icons/-/icons-3.49.0.tgz"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func tablerSVG(name string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" class="icon icon-tabler icons-tabler-outline icon-tabler-` + name + `"><path d="M5 12l5 5l10 -10" /></svg>`
}

func extNotice(polyVersion, polySum string) string {
	return "Third-party files in this directory. They are copied unchanged from the\nnpm packages below; do not edit them.\n\n" +
		"browser-polyfill.js\n" +
		"  Package:   webextension-polyfill " + polyVersion + " (dist/browser-polyfill.js)\n" +
		"  Source:    " + polySource + "\n" +
		"  Integrity: " + polyIntegr + "\n" +
		"  SHA-256 of the file: " + polySum + "\n" +
		"  License:   MPL-2.0 (LICENSE.webextension-polyfill). The source form is this\n" +
		"             file itself.\n\n" +
		"tabler/*.svg\n" +
		"  Package:   @tabler/icons 3.49.0 (icons/outline/*.svg)\n" +
		"  Source:    " + iconsSource + "\n" +
		"  Integrity: " + iconIntegr + "\n" +
		"  License:   MIT (tabler/LICENSE). scripts/build.mjs turns the path data\n" +
		"             into ui/icons.js.\n"
}

func extPackages(polySum string) []ExtPackage {
	return []ExtPackage{
		{Name: "webextension-polyfill", Version: "0.12.0", License: "MPL-2.0", Source: polySource,
			Repository: "https://github.com/mozilla/webextension-polyfill", Integrity: polyIntegr,
			Files:       []ExtFile{{Path: "browser-polyfill.js", SHA256: polySum}},
			LicenseFile: "LICENSE.webextension-polyfill", ShippedAs: "vendor/browser-polyfill.js"},
		{Name: "@tabler/icons", Version: "3.49.0", License: "MIT", Source: iconsSource,
			Repository: "https://github.com/tabler/tabler-icons", Integrity: iconIntegr,
			Files:       []ExtFile{{Glob: "tabler/*.svg", Contains: "icons-tabler-outline icon-tabler-{stem}"}},
			LicenseFile: "tabler/LICENSE", ShippedAs: "ui/icons.js"},
	}
}

// extRepo writes a clean extension: vendored files, NOTICE, registry,
// profiles licence and a lock file with only a devDependency.
func extRepo(t *testing.T) *repo {
	t.Helper()
	r := newRepo(t)
	v := extVendorDir + "/"
	r.write(v+"browser-polyfill.js", polyfillJS)
	r.write(v+"LICENSE.webextension-polyfill", mplText)
	r.write(v+"tabler/LICENSE", fixture(t, "mit.txt"))
	r.write(v+"tabler/check.svg", tablerSVG("check"))
	r.write(v+"tabler/lock.svg", tablerSVG("lock"))
	r.write(v+"NOTICE", extNotice("0.12.0", sum(polyfillJS)))
	r.writeJSON(extensionFile, map[string]any{"packages": extPackages(sum(polyfillJS))})
	r.write(extProfilesLicense, "The platform profiles are dedicated to the public domain.\n\nCreative Commons Legal Code\n\nCC0 1.0 Universal\n")
	r.writeJSON(extLockFile, map[string]any{
		"lockfileVersion": 3,
		"packages": map[string]map[string]any{
			"":                        {"name": "lectureflow-extension", "license": "AGPL-3.0-or-later"},
			"node_modules/playwright": {"version": "1.64.1", "license": "Apache-2.0", "dev": true},
		},
	})
	return r
}

func extViolations(rep *Report) []string {
	var out []string
	for _, v := range rep.Violations {
		if v.Kind == KindExtension {
			out = append(out, v.String())
		}
	}
	sort.Strings(out)
	return out
}

func TestExtensionVendorClean(t *testing.T) {
	r := extRepo(t)
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	var got []string
	for _, c := range componentsOf(rep, KindExtension) {
		got = append(got, c.Name+"@"+c.Version+" "+c.License)
	}
	if want := []string{"@tabler/icons@3.49.0 MIT", "webextension-polyfill@0.12.0 MPL-2.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("components = %v, want %v", got, want)
	}
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "1 npm devDependencies of extension/") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	s := RenderNotices(rep)
	for _, want := range []string{
		"## 11. Browser extension",
		"| webextension-polyfill | 0.12.0 | MPL-2.0 | vendor/browser-polyfill.js | " + polySource + " | Upstream: https://github.com/mozilla/webextension-polyfill. |",
		"| @tabler/icons | 3.49.0 | MIT | ui/icons.js | " + iconsSource,
		"Used by: webextension-polyfill 0.12.0 (vendor/LICENSE.webextension-polyfill)",
		"Mozilla Public License Version 2.0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("notices lack %q", want)
		}
	}
}

func TestExtensionAbsentIsSkipped(t *testing.T) {
	rep := audit(t, newRepo(t).root, nil)
	if len(rep.Violations) != 0 || !strings.Contains(strings.Join(rep.Skipped, "\n"), extVendorDir) {
		t.Fatalf("violations %v skipped %v", rep.Violations, rep.Skipped)
	}
	if s := RenderNotices(rep); !strings.Contains(s, "_Not audited: "+extVendorDir+" (not present)._") {
		t.Errorf("section 11 does not say it was not audited")
	}
}

func TestExtensionVendorFailures(t *testing.T) {
	v := extVendorDir + "/"
	cases := []struct {
		name   string
		change func(r *repo)
		want   string // substring of one extension violation
	}{
		{"unregistered file", func(r *repo) { r.write(v+"helper.js", "export {}\n") }, "extension/src/vendor/helper.js: not registered in packaging/licenses/extension.json"},
		{"edited polyfill", func(r *repo) { r.write(v+"browser-polyfill.js", polyfillJS+"// patched\n") }, "browser-polyfill.js: sha256 mismatch"},
		{"missing pinned file", func(r *repo) { removeFile(t, r, v+"browser-polyfill.js") }, "browser-polyfill.js not found"},
		{"foreign icon", func(r *repo) { r.write(v+"tabler/logo.svg", `<svg class="icon other-set"></svg>`) }, `tabler/logo.svg does not contain "icons-tabler-outline icon-tabler-logo"`},
		{"license text of another license", func(r *repo) { r.write(v+"LICENSE.webextension-polyfill", fixture(t, "mit.txt")) }, `license file LICENSE.webextension-polyfill is MIT, but the declared license is "MPL-2.0"`},
		{"license text not recognised", func(r *repo) { r.write(v+"tabler/LICENSE", "All rights reserved.\n") }, "license file tabler/LICENSE not recognised"},
		{"missing license file", func(r *repo) { removeFile(t, r, v+"tabler/LICENSE") }, "license file tabler/LICENSE not found"},
		{"notice version", func(r *repo) { r.write(v+"NOTICE", extNotice("0.11.0", sum(polyfillJS))) }, "NOTICE says version 0.11.0"},
		{"notice sum", func(r *repo) { r.write(v+"NOTICE", extNotice("0.12.0", sum("old"))) }, "NOTICE gives another SHA-256 for browser-polyfill.js"},
		{"notice without the icons", func(r *repo) {
			n := extNotice("0.12.0", sum(polyfillJS))
			r.write(v+"NOTICE", n[:strings.Index(n, "tabler/*.svg")])
		}, "@tabler/icons@3.49.0 [MIT]: not named in extension/src/vendor/NOTICE"},
		{"notice names an unregistered package", func(r *repo) {
			r.write(v+"NOTICE", extNotice("0.12.0", sum(polyfillJS))+"\nfont.woff2\n  Package:   some-font 1.0.0\n  License:   OFL-1.1 (font/LICENSE)\n")
		}, "some-font@1.0.0 [OFL-1.1]: named in extension/src/vendor/NOTICE but not registered"},
		{"missing notice", func(r *repo) { removeFile(t, r, v+"NOTICE") }, "extension/src/vendor/NOTICE: missing"},
		{"missing registry", func(r *repo) { removeFile(t, r, extensionFile) }, "extension.json is missing"},
		{"incompatible license", func(r *repo) {
			p := extPackages(sum(polyfillJS))
			p[1].License = "CC-BY-NC-4.0"
			r.writeJSON(extensionFile, map[string]any{"packages": p})
			r.write(v+"tabler/LICENSE", fixture(t, "cc-by-nc-4.0.txt"))
		}, "@tabler/icons@3.49.0 [CC-BY-NC-4.0]"},
		{"glob without contains", func(r *repo) {
			p := extPackages(sum(polyfillJS))
			p[1].Files[0].Contains = ""
			r.writeJSON(extensionFile, map[string]any{"packages": p})
		}, `glob "tabler/*.svg" needs "contains"`},
		{"pinned file without sum", func(r *repo) {
			p := extPackages(sum(polyfillJS))
			p[0].Files[0].SHA256 = ""
			r.writeJSON(extensionFile, map[string]any{"packages": p})
		}, "browser-polyfill.js: sha256 missing (actual " + sum(polyfillJS) + ")"},
		{"runtime npm dependency", func(r *repo) {
			r.writeJSON(extLockFile, map[string]any{"lockfileVersion": 3, "packages": map[string]map[string]any{
				"node_modules/server-side": {"version": "2.0.0", "license": "SSPL-1.0"},
			}})
		}, "server-side@2.0.0 [SSPL-1.0]"},
		{"profiles licence missing", func(r *repo) { removeFile(t, r, extProfilesLicense); r.write(extProfilesDir+"/x.json", "{}") }, "extension/profiles/LICENSE: missing"},
		{"profiles licence not CC0", func(r *repo) { r.write(extProfilesLicense, fixture(t, "mit.txt")) }, "extension/profiles/LICENSE: not recognised as CC0-1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := extRepo(t)
			c.change(r)
			got := extViolations(audit(t, r.root, nil))
			for _, g := range got {
				if strings.Contains(g, c.want) {
					return
				}
			}
			t.Fatalf("no violation containing %q in %v", c.want, got)
		})
	}
}

func TestParseExtNotice(t *testing.T) {
	got := parseExtNotice(strings.ReplaceAll(extNotice("0.12.0", sum("x")), "\n", "\r\n"))
	want := []noticeBlock{
		{header: "browser-polyfill.js", name: "webextension-polyfill", version: "0.12.0", source: polySource, integrity: polyIntegr, sha256: sum("x"), license: "MPL-2.0", licenseFile: "LICENSE.webextension-polyfill"},
		{header: "tabler/*.svg", name: "@tabler/icons", version: "3.49.0", source: iconsSource, integrity: iconIntegr, license: "MIT", licenseFile: "tabler/LICENSE"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parse =\n%s\nwant\n%s", fmt.Sprint(got), fmt.Sprint(want))
	}
}

func removeFile(t *testing.T, r *repo, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(r.root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

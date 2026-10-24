// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"lectureflow/web"
)

// The embedded front end must contain the files SPEC §1.1 names, and every
// local module/style it references must be embedded too (Go cannot run the
// JS, so a broken import would otherwise only show up in the browser).
func TestWebFSHasAssets(t *testing.T) {
	for _, name := range []string{"index.html", "app.js", "api.js", "style.css"} {
		b, err := fs.ReadFile(web.FS, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(b) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

var (
	importRe = regexp.MustCompile(`(?m)^\s*import\s[^;]*?from\s+'\./([^']+)'`)
	refRe    = regexp.MustCompile(`(?:src|href)="([^"#]+)"`)
)

func TestWebFSReferencesResolve(t *testing.T) {
	entries, err := fs.ReadDir(web.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() { // i18n/: JSON catalogs (SPEC-v2 P14.2), no references
			continue
		}
		name := e.Name()
		b, err := fs.ReadFile(web.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		var refs []string
		switch {
		case strings.HasSuffix(name, ".js"):
			for _, m := range importRe.FindAllStringSubmatch(src, -1) {
				refs = append(refs, m[1])
			}
		case strings.HasSuffix(name, ".html"):
			for _, m := range refRe.FindAllStringSubmatch(src, -1) {
				refs = append(refs, m[1])
			}
		}
		for _, ref := range refs {
			// No CDN or other external resources (SPEC §1, §17.6).
			if strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") {
				t.Errorf("%s references external resource %q", name, ref)
				continue
			}
			if _, err := fs.Stat(web.FS, strings.TrimPrefix(ref, "./")); err != nil {
				t.Errorf("%s references %q, which is not embedded: %v", name, ref, err)
			}
		}
	}
}

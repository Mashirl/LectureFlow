// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"strings"
	"testing"
)

// Tests of the offline-archive rule for user-fetched packages and of the
// optional source_code / bundled fields of binaries.json (v3 gate-fix-license).

// offlineRepo has a component asr-cuda whose lock file holds the
// user-fetched package cuda-blob, and a component python without one.
func offlineRepo(t *testing.T) *repo {
	r := newRepo(t)
	r.writeJSON(pythonFile, PyFile{Packages: map[string]PyEntry{
		"cuda-blob": {License: "LicenseRef-NVIDIA-Proprietary"},
		"ok":        {License: "MIT"},
	}})
	r.write(componentsDir+"/locks/asr-cuda-windows-x64.txt", "cuda-blob==12.9\nok==1.0\n")
	r.write(componentsDir+"/locks/python-windows-x64.txt", "ok==1.0\n")
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{
		{Kind: KindPython, Name: "cuda-blob", Scope: ScopeUserFetched, Reason: "downloaded from PyPI on the user's computer"},
	}})
	r.exception("cuda-blob")
	return r
}

func build(lock string, offline any) map[string]any {
	b := map[string]any{"lock": map[string]any{"path": lock, "sha256": strings.Repeat("a", 64)}}
	if offline != nil {
		b["offline"] = offline
	}
	return b
}

func (r *repo) manifest(rel string, comps ...map[string]any) {
	r.writeJSON(componentsDir+"/"+rel, map[string]any{"schema": 1, "components": comps})
}

func offlineViolations(rep *Report) []Violation {
	var out []Violation
	for _, v := range rep.Violations {
		if v.Kind == "offline" {
			out = append(out, v)
		}
	}
	return out
}

var anOffline = map[string]any{"name": "x.zip", "sha256": strings.Repeat("b", 64), "size": 1}

func TestOfflineArchiveWithUserFetchedFails(t *testing.T) {
	r := offlineRepo(t)
	r.manifest("components.json",
		map[string]any{"id": "asr-cuda", "platforms": map[string]any{"windows-x64": build("locks/asr-cuda-windows-x64.txt", anOffline)}},
		map[string]any{"id": "python", "platforms": map[string]any{"windows-x64": build("locks/python-windows-x64.txt", anOffline)}},
	)
	rep := audit(t, r.root, nil)
	vs := offlineViolations(rep)
	if len(vs) != 1 || vs[0].Name != "asr-cuda" || !strings.Contains(vs[0].Reason, "cuda-blob") ||
		!strings.Contains(vs[0].Reason, "platforms.windows-x64.offline") || !strings.Contains(vs[0].Reason, "components.json") {
		t.Fatalf("violations = %v", rep.Violations)
	}
	if len(rep.Violations) != 1 {
		t.Fatalf("only the offline rule should fail: %v", rep.Violations)
	}
}

func TestOfflineArchiveWithoutUserFetchedPasses(t *testing.T) {
	r := offlineRepo(t)
	r.manifest("components.json",
		map[string]any{"id": "asr-cuda", "platforms": map[string]any{"windows-x64": build("locks/asr-cuda-windows-x64.txt", nil)}},
		map[string]any{"id": "python", "platforms": map[string]any{"windows-x64": build("locks/python-windows-x64.txt", anOffline)}},
		map[string]any{"id": "model:faster-whisper:small:int8", "license": "MIT", "source": "https://example.org/m",
			"platforms": map[string]any{"windows-x64": map[string]any{"offline": anOffline}}},
	)
	// Empty or null offline values declare nothing.
	r.manifest("manifest.src.json",
		map[string]any{"id": "asr-cuda", "offline": nil, "gen": map[string]any{"offline": false, "locks": []any{"windows-x64"}}},
	)
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

func TestOfflineInSourceManifestFails(t *testing.T) {
	// manifest.src.json has no platforms (gen fills them in): an offline
	// request anywhere in the component is judged against all its locks.
	r := offlineRepo(t)
	r.manifest("manifest.src.json",
		map[string]any{"id": "asr-cuda", "gen": map[string]any{"offline": []any{"windows-x64"}}},
	)
	rep := audit(t, r.root, nil)
	vs := offlineViolations(rep)
	if len(vs) != 1 || !strings.Contains(vs[0].Reason, "manifest.src.json") || !strings.Contains(vs[0].Reason, "gen.offline") || !strings.Contains(vs[0].Reason, "cuda-blob") {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

func TestOfflineUnreadableLockFails(t *testing.T) {
	r := offlineRepo(t)
	r.manifest("components.json",
		map[string]any{"id": "asr-x", "platforms": map[string]any{"windows-x64": build("locks/gone.txt", anOffline)}},
	)
	rep := audit(t, r.root, nil)
	if vs := offlineViolations(rep); len(vs) != 1 || !strings.Contains(vs[0].Reason, "cannot confirm") {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

func TestOfflineRuleIsNotAllowlistable(t *testing.T) {
	r := offlineRepo(t)
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{
		{Kind: KindPython, Name: "cuda-blob", Scope: ScopeUserFetched, Reason: "downloaded from PyPI on the user's computer"},
		{Kind: "offline", Name: "asr-cuda", Scope: ScopeDistributed, Reason: "trying to exempt the archive"},
	}})
	r.manifest("components.json",
		map[string]any{"id": "asr-cuda", "platforms": map[string]any{"windows-x64": build("locks/asr-cuda-windows-x64.txt", anOffline)}},
	)
	rep := audit(t, r.root, nil)
	if len(offlineViolations(rep)) != 1 {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

// --- binaries.json: source_code and bundled ------------------------------------------

func TestBinarySourceCodeAndBundledLibraries(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(binariesFile, map[string]any{"binaries": []BinaryEntry{{
		Name: "ffmpeg / ffprobe (Windows build)", Version: "9.0.2", License: "LGPL-3.0-or-later", Source: "https://example.org/ffmpeg.zip",
		SourceCode: []string{"https://example.org/ffmpeg-9.0.2.tar.xz", "https://example.org/build-scripts/commit/abc"},
		Bundled: []BundledLib{
			{Name: "dav1d", Version: "1.5.1", License: "BSD-2-Clause", URL: "https://example.org/dav1d"},
			{Name: "libmp3lame", Version: "3.100", License: "LGPL-2.0-or-later", URL: "https://example.org/lame"},
		},
	}}})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	s := RenderNotices(rep)
	i6 := strings.Index(s, "## 6. Bundled binaries")
	i7 := strings.Index(s, "## 7. ")
	sec := s[i6:i7]
	for _, want := range []string{
		"### ffmpeg / ffprobe (Windows build)",
		"- https://example.org/ffmpeg-9.0.2.tar.xz",
		"- https://example.org/build-scripts/commit/abc",
		"| dav1d | 1.5.1 | BSD-2-Clause | https://example.org/dav1d |",
		"| libmp3lame | 3.100 | LGPL-2.0-or-later | https://example.org/lame |",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("section 6 lacks %q:\n%s", want, sec)
		}
	}
}

func TestBinaryWithoutOptionalFields(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(binariesFile, map[string]any{"binaries": []BinaryEntry{
		{Name: "ffmpeg", Version: "9", License: "LGPL-2.1-or-later", Source: "https://example.org/ffmpeg"},
		{Name: "uv", Version: "1", License: "MIT OR Apache-2.0", Source: "https://example.org/uv"},
	}})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	if strings.Contains(RenderNotices(rep), "### ffmpeg") {
		t.Errorf("no detail block without source_code / bundled")
	}
	warned := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "ffmpeg is under LGPL-2.1-or-later but lists no source_code") {
			warned = true
		}
		if strings.Contains(w, "uv ") {
			t.Errorf("unexpected warning %q", w)
		}
	}
	if !warned {
		t.Errorf("warnings = %v", rep.Warnings)
	}
}

func TestBinaryBundledFailures(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(binariesFile, map[string]any{"binaries": []BinaryEntry{{
		Name: "ffmpeg", Version: "9", License: "LGPL-3.0-or-later", Source: "https://example.org/ffmpeg",
		SourceCode: []string{"ffmpeg.org"},
		Bundled: []BundledLib{
			{Name: "libx264", Version: "1", License: "GPL-2.0-only", URL: "https://example.org/x264"},
			{Name: "libfdk-aac", Version: "2", License: "LicenseRef-FDK-AAC", URL: "https://example.org/fdk"},
			{Name: "nourl", Version: "1", License: "MIT"},
			{Name: "badurl", Version: "1", License: "MIT", URL: "example.org"},
			{Name: "nolicense", Version: "1", URL: "https://example.org/n"},
			{Version: "1", License: "MIT", URL: "https://example.org/a"},
		},
	}}})
	rep := audit(t, r.root, nil)
	want := map[string]string{
		"ffmpeg":              "source_code \"ffmpeg.org\" is not an http(s) URL",
		"ffmpeg / libx264":    "GPL-2.0-only",
		"ffmpeg / libfdk-aac": "LicenseRef-FDK-AAC",
		"ffmpeg / nourl":      "url missing",
		"ffmpeg / badurl":     "not an http(s) URL",
		"ffmpeg / nolicense":  "no license",
		"ffmpeg / bundled[5]": "name missing",
	}
	got := map[string]string{}
	for _, v := range rep.Violations {
		got[v.Name] += v.Reason + " [" + v.License + "]; "
	}
	if len(got) != len(want) {
		t.Fatalf("violations = %v", rep.Violations)
	}
	for n, w := range want {
		if !strings.Contains(got[n], w) {
			t.Errorf("%s: %q lacks %q", n, got[n], w)
		}
	}

	// A distributed allowlist entry for "{binary} / {lib}" accepts one library.
	r.writeJSON(binariesFile, map[string]any{"binaries": []BinaryEntry{{
		Name: "ffmpeg", Version: "9", License: "LGPL-3.0-or-later", Source: "https://example.org/ffmpeg", SourceCode: []string{"https://example.org/src"},
		Bundled: []BundledLib{{Name: "libx", Version: "1", License: "LicenseRef-X", URL: "https://example.org/x"}},
	}}})
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{
		{Kind: KindBinary, Name: "ffmpeg / libx", Scope: ScopeDistributed, Reason: "fixture reason"},
	}})
	if rep = audit(t, r.root, nil); len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

// The permissive licenses of the bundled FFmpeg libraries pass, while a
// GPL-2.0-only part (zvbi's packet 8/30 decoder) still fails the library.
func TestBinaryBundledPermissiveVariantsPassAndGPL2OnlyFails(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(binariesFile, map[string]any{"binaries": []BinaryEntry{{
		Name: "ffmpeg", Version: "9", License: "LGPL-3.0-or-later", Source: "https://example.org/ffmpeg",
		SourceCode: []string{"https://example.org/src"},
		Bundled: []BundledLib{
			{Name: "harfbuzz", Version: "1", License: "MIT-Modern-Variant", URL: "https://example.org/a"},
			{Name: "fontconfig", Version: "1", License: "HPND-sell-variant", URL: "https://example.org/b"},
			{Name: "libpng", Version: "1", License: "libpng-2.0", URL: "https://example.org/c"},
			{Name: "libvmaf", Version: "1", License: "BSD-2-Clause-Patent", URL: "https://example.org/d"},
			{Name: "zimg", Version: "1", License: "WTFPL", URL: "https://example.org/e"},
			{Name: "pcre2", Version: "1", License: "BSD-3-Clause WITH PCRE2-exception", URL: "https://example.org/f"},
			{Name: "glslang", Version: "1", License: "BSD-3-Clause AND GPL-3.0-or-later WITH Bison-exception-2.2", URL: "https://example.org/g"},
			{Name: "mingw-w64", Version: "1", License: "ZPL-2.1 AND MIT AND BSD-3-Clause", URL: "https://example.org/h"},
			{Name: "zvbi", Version: "1", License: "LGPL-2.0-or-later AND GPL-2.0-only AND MIT", URL: "https://example.org/i"},
		},
	}}})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 1 || rep.Violations[0].Name != "ffmpeg / zvbi" || !strings.Contains(rep.Violations[0].Reason, "GPL-2.0 only") {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

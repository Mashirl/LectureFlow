// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Tests of media.json, references.json, repository license texts and the
// manifest-authoritative model table (v7-license-2). Offline fixtures only.

func (r *repo) sum(rel string) string {
	r.t.Helper()
	s, err := fileSHA256(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// mediaRepo writes one sample file under assets/ and a valid media entry for it.
func mediaRepo(t *testing.T) (*repo, MediaEntry) {
	r := newRepo(t)
	r.write("assets/sample/clip.flac", "fLaC\x00\x00binary")
	return r, MediaEntry{
		Path: "assets/sample/clip.flac", Title: "Lecture I (excerpt)", Creator: "An Author; read by A Reader",
		License: "LicenseRef-Public-Domain", LicenseBasis: "Recording released into the public domain by the publisher.",
		LicenseURL: "https://creativecommons.org/publicdomain/mark/1.0/",
		Sources:    []string{"https://example.org/recording"}, SHA256: r.sum("assets/sample/clip.flac"),
	}
}

func (r *repo) media(ms []MediaEntry, own []OwnFile) {
	r.writeJSON(mediaFile, map[string]any{"media": ms, "own": own})
}

func TestMediaPublicDomainPasses(t *testing.T) {
	r, m := mediaRepo(t)
	r.write("assets/sample/LICENSE", "public domain statement")
	r.media([]MediaEntry{m}, []OwnFile{{Path: "assets/sample/LICENSE", License: "CC0-1.0"}})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	s := RenderNotices(rep)
	for _, want := range []string{"## 8. Bundled media", "| assets/sample/clip.flac | Lecture I (excerpt). An Author; read by A Reader | LicenseRef-Public-Domain | https://example.org/recording |", "publicdomain/mark/1.0"} {
		if !strings.Contains(s, want) {
			t.Errorf("notices lack %q", want)
		}
	}
	if strings.Contains(s, "assets/sample/LICENSE |") {
		t.Errorf("own files must not be listed as third-party media")
	}
}

func TestMediaFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*MediaEntry)
		want   string
	}{
		{"no source", func(m *MediaEntry) { m.Sources = nil }, "source missing"},
		{"blank source", func(m *MediaEntry) { m.Sources = []string{"  "} }, "source missing"},
		{"source not a URL", func(m *MediaEntry) { m.Sources = []string{"LibriVox"} }, "not an http(s) URL"},
		{"no license", func(m *MediaEntry) { m.License = "" }, "license missing"},
		{"no basis", func(m *MediaEntry) { m.LicenseBasis = "" }, "license_basis missing"},
		{"non-commercial", func(m *MediaEntry) { m.License = "CC-BY-NC-4.0" }, "non-commercial"},
		{"share-alike", func(m *MediaEntry) { m.License = "CC-BY-SA-4.0" }, "not confirmed"},
		{"unknown ref", func(m *MediaEntry) { m.License = "LicenseRef-Free-For-Education" }, "custom license"},
		{"no hash", func(m *MediaEntry) { m.SHA256 = "" }, "sha256 missing"},
		{"hash mismatch", func(m *MediaEntry) { m.SHA256 = strings.Repeat("0", 64) }, "sha256 mismatch"},
		{"file missing", func(m *MediaEntry) { m.Path = "assets/sample/gone.flac" }, "file not found"},
		{"no creator", func(m *MediaEntry) { m.Creator = "" }, "title and creator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, m := mediaRepo(t)
			tc.mutate(&m)
			ms := []MediaEntry{m}
			if m.Path != "assets/sample/clip.flac" { // keep the real file listed
				ms = append(ms, MediaEntry{Path: "assets/sample/clip.flac", Title: "t", Creator: "c", License: "CC0-1.0",
					LicenseBasis: "b", Sources: []string{"https://example.org/x"}, SHA256: r.sum("assets/sample/clip.flac")})
			}
			r.media(ms, nil)
			rep := audit(t, r.root, nil)
			vs := violationsOf(rep, m.Path)
			found := false
			for _, v := range vs {
				if strings.Contains(v.Reason, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want a violation containing %q for %s, got %v", tc.want, m.Path, rep.Violations)
			}
		})
	}
}

func TestMediaUnlistedAssetFails(t *testing.T) {
	r, m := mediaRepo(t)
	r.write("assets/sample/extra.wav", "RIFF\x00")
	r.media([]MediaEntry{m}, nil)
	rep := audit(t, r.root, nil)
	if vs := violationsOf(rep, "assets/sample/extra.wav"); len(vs) != 1 || !strings.Contains(vs[0].Reason, "not listed") {
		t.Fatalf("violations = %v", rep.Violations)
	}
	// No media.json at all: every asset fails.
	r2, _ := mediaRepo(t)
	rep = audit(t, r2.root, nil)
	if vs := violationsOf(rep, "assets/sample/clip.flac"); len(vs) != 1 {
		t.Fatalf("violations without media.json = %v", rep.Violations)
	}
}

func TestMediaOwnFileLicenseJudged(t *testing.T) {
	r, m := mediaRepo(t)
	r.write("assets/sample/NOTES.md", "notes")
	r.media([]MediaEntry{m}, []OwnFile{{Path: "assets/sample/NOTES.md", License: "LicenseRef-Public-Domain"}})
	rep := audit(t, r.root, nil)
	if vs := violationsOf(rep, "assets/sample/NOTES.md"); len(vs) != 1 {
		t.Fatalf("own file with a media-only license must fail, violations = %v", rep.Violations)
	}
}

// LicenseRef-Public-Domain is accepted only for media, never for code.
func TestPublicDomainOnlyForMedia(t *testing.T) {
	if ok, _ := Evaluate("LicenseRef-Public-Domain"); ok {
		t.Fatal("Evaluate accepts LicenseRef-Public-Domain for code")
	}
	if ok, _ := EvaluateWith("LicenseRef-Public-Domain", mediaCompatible); !ok {
		t.Fatal("media table rejects LicenseRef-Public-Domain")
	}
	if ok, _ := EvaluateWith("CC0-1.0 OR LicenseRef-Public-Domain", mediaCompatible); !ok {
		t.Fatal("media OR expression rejected")
	}
	if ok, _ := EvaluateWith("LicenseRef-Public-Domain AND CC-BY-NC-4.0", mediaCompatible); ok {
		t.Fatal("media AND with a non-commercial part accepted")
	}
	r := newRepo(t)
	r.python(map[string]string{"pd-pkg": "LicenseRef-Public-Domain"})
	rep := audit(t, r.root, nil)
	if len(violationsOf(rep, "pd-pkg")) == 0 {
		t.Fatal("a Python package under LicenseRef-Public-Domain passed")
	}
}

func refEntry(r *repo) ReferenceEntry {
	r.write("packaging/licenses/texts/up-1.0.LICENSE", fixture(r.t, "apache-2.0.txt"))
	r.write("internal/thing/UPSTREAM.md", "Follows the protocol of **upstream-cli 1.0** (Apache-2.0).\n")
	return ReferenceEntry{
		Name: "upstream-cli", Version: "1.0", License: "Apache-2.0", Copyright: []string{"Copyright 2026 Example Corp"},
		Source: "https://example.org/upstream-cli", Used: "the wire protocol", UsedBy: "internal/thing (Go)",
		Changes: "Reimplemented in Go; no code copied.", Details: "internal/thing/UPSTREAM.md",
		LicenseText: "packaging/licenses/texts/up-1.0.LICENSE", LicenseTextSHA256: r.sum("packaging/licenses/texts/up-1.0.LICENSE"),
		LicenseTextSource: "LICENSE in the wheel",
	}
}

func TestReferencesRenderedIntoNotices(t *testing.T) {
	r := newRepo(t)
	e := refEntry(r)
	r.writeJSON(refsFile, map[string]any{"references": []ReferenceEntry{e}})
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	s := RenderNotices(rep)
	for _, want := range []string{
		"## 9. Upstreams reimplemented from their protocol, no code copied / 参照其协议重新实现、未复制代码的上游",
		"### upstream-cli 1.0", "- License: Apache-2.0", "- Copyright 2026 Example Corp",
		"- Referenced for: the wire protocol", "- Changes: Reimplemented in Go; no code copied.",
		"- Details: `internal/thing/UPSTREAM.md`",
		"Used by: upstream-cli 1.0 (LICENSE, copy in packaging/licenses/texts/up-1.0.LICENSE)",
		"Apache License",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("notices lack %q", want)
		}
	}
	// The reference section comes before the appendix.
	if strings.Index(s, "## 9.") > strings.Index(s, "## Appendix A") {
		t.Errorf("section 9 after Appendix A")
	}
}

func TestReferenceFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*repo, *ReferenceEntry)
		want   string
	}{
		{"no copyright", func(_ *repo, e *ReferenceEntry) { e.Copyright = []string{" "} }, "copyright missing"},
		{"no changes", func(_ *repo, e *ReferenceEntry) { e.Changes = "" }, "changes missing"},
		{"no license text", func(_ *repo, e *ReferenceEntry) { e.LicenseText = "" }, "license_text missing"},
		{"text hash", func(_ *repo, e *ReferenceEntry) { e.LicenseTextSHA256 = strings.Repeat("a", 64) }, "sha256 mismatch"},
		{"text is another license", func(r *repo, e *ReferenceEntry) {
			r.write("packaging/licenses/texts/bsd.LICENSE", fixture(r.t, "bsd-3.txt"))
			e.LicenseText, e.LicenseTextSHA256 = "packaging/licenses/texts/bsd.LICENSE", r.sum("packaging/licenses/texts/bsd.LICENSE")
		}, "declared license"},
		{"version out of step", func(_ *repo, e *ReferenceEntry) { e.Version = "1.1" }, "does not mention"},
		{"incompatible", func(r *repo, e *ReferenceEntry) {
			r.write("packaging/licenses/texts/gpl2.LICENSE", fixture(r.t, "gpl-2.0.txt"))
			e.License = "GPL-2.0-only"
			e.LicenseText, e.LicenseTextSHA256 = "packaging/licenses/texts/gpl2.LICENSE", r.sum("packaging/licenses/texts/gpl2.LICENSE")
		}, "GPL-2.0 only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			e := refEntry(r)
			tc.mutate(r, &e)
			r.writeJSON(refsFile, map[string]any{"references": []ReferenceEntry{e}})
			rep := audit(t, r.root, nil)
			found := false
			for _, v := range violationsOf(rep, "upstream-cli") {
				if strings.Contains(v.Reason, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want %q, violations = %v", tc.want, rep.Violations)
			}
		})
	}
}

func TestPythonLicenseTextCopy(t *testing.T) {
	r := newRepo(t)
	r.write("packaging/licenses/texts/mit.LICENSE", fixture(t, "mit.txt"))
	sum := r.sum("packaging/licenses/texts/mit.LICENSE")
	write := func(want string) {
		r.writeJSON(pythonFile, PyFile{Packages: map[string]PyEntry{
			"nolic": {License: "MIT", LicenseText: "packaging/licenses/texts/mit.LICENSE", LicenseTextSHA256: want},
		}})
		r.write(componentsDir+"/asr-test/requirements.txt", "nolic==1.0\n")
	}
	write(sum)
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations = %v", rep.Violations)
	}
	s := RenderNotices(rep)
	if !strings.Contains(s, "Used by: nolic 1.0 (LICENSE, copy in packaging/licenses/texts/mit.LICENSE)") || !strings.Contains(s, "Permission is hereby granted") {
		t.Errorf("license text copy not in Appendix A")
	}
	write(strings.Repeat("f", 64))
	rep = audit(t, r.root, nil)
	if vs := violationsOf(rep, "nolic"); len(vs) != 1 || !strings.Contains(vs[0].Reason, "sha256 mismatch") {
		t.Fatalf("violations = %v", rep.Violations)
	}
}

func TestModelsManifestAuthoritative(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(modelsFile, map[string]any{"models": []ModelEntry{
		{Backend: "faster-whisper", Name: "small", License: "Apache-2.0", Source: "https://example.org/a"}, // contradicts
		{Backend: "faster-whisper", Name: "medium", License: "MIT", Source: "https://example.org/m"},       // repeats
		{Backend: "faster-whisper", Name: "tiny", License: "MIT", Source: "https://example.org/t"},         // unused
	}})
	r.writeJSON(componentsDir+"/components.json", map[string]any{"components": []map[string]any{
		{"id": "model:faster-whisper:small:float16", "license": "MIT", "source": "https://example.org/s", "revision": "0123456789abcdef", "license_note": "conversion"},
		{"id": "model:faster-whisper:medium:float16", "license": "MIT", "source": "https://example.org/m"},
		{"id": "model:faster-whisper:medium:float16", "license": "Apache-2.0", "source": "https://example.org/m"}, // same id, other license
		{"id": "model:mlx:small:fp16", "license": "Apache-2.0"},                                                   // no source
	}})
	rep := audit(t, r.root, nil)
	bad := map[string]bool{}
	for _, v := range rep.Violations {
		bad[v.Name] = true
	}
	want := map[string]bool{"model:faster-whisper:small:float16": true, "model:faster-whisper:medium:float16": true, "model:mlx:small:fp16": true}
	if !reflect.DeepEqual(bad, want) {
		t.Fatalf("violations = %v", rep.Violations)
	}
	warn := strings.Join(rep.Warnings, "\n")
	if !strings.Contains(warn, "faster-whisper:medium repeats") || !strings.Contains(warn, "faster-whisper:tiny matches no model") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	s := RenderNotices(rep)
	if !strings.Contains(s, "| model:faster-whisper:small:float16 | 0123456789ab | MIT | https://example.org/s | conversion |") {
		t.Errorf("model row from the manifest missing:\n%s", s)
	}
	if strings.Contains(s, "faster-whisper:tiny") {
		t.Errorf("unused models.json entry listed")
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifestValid(t *testing.T) {
	rel := makeRelease(t, nil, newTestKey(t), "3.2.0", ChannelStable, func(m *Manifest) {
		m.MinVersion = "3.0.0"
		m.Mirrors = []MirrorRule{{Prefix: "https://example.invalid/", Replace: "https://mirror.invalid/gh/"}}
	})
	m, err := ParseManifest(rel.raw)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := m.Asset(PlatformWindowsX64, KindPortable)
	if !ok || a.Name != "LectureFlow-3.2.0-x64-portable.exe" {
		t.Fatalf("portable asset %+v %v", a, ok)
	}
	if got := m.mirrorURLs(a.URL); len(got) != 1 || !strings.HasPrefix(got[0], "https://mirror.invalid/gh/") {
		t.Fatalf("mirror URLs %v", got)
	}
	// unknown fields, platforms and kinds are for later versions
	var raw map[string]any
	json.Unmarshal(rel.raw, &raw)
	raw["future_field"] = 1
	raw["platforms"].(map[string]any)["linux-x64"] = []any{map[string]any{"kind": "appimage", "name": "x.AppImage",
		"url": "https://example.invalid/x", "size": 1, "sha256": strings.Repeat("a", 64)}}
	b, _ := json.Marshal(raw)
	if _, err := ParseManifest(b); err != nil {
		t.Fatalf("forward-compatible fields: %v", err)
	}
}

func TestParseManifestInvalid(t *testing.T) {
	k := newTestKey(t)
	cases := map[string]struct {
		mod   func(m *Manifest)
		field string
	}{
		"schema 2":           {func(m *Manifest) { m.Schema = 2 }, "schema"},
		"other app":          {func(m *Manifest) { m.App = "Other" }, "app"},
		"v prefix":           {func(m *Manifest) { m.Version = "v3.2.0" }, "version"},
		"two parts":          {func(m *Manifest) { m.Version = "3.2" }, "version"},
		"stable pre-release": {func(m *Manifest) { m.Version = "3.2.0-beta.1" }, "channel"},
		"unknown channel":    {func(m *Manifest) { m.Channel = "nightly" }, "channel"},
		"released_at":        {func(m *Manifest) { m.ReleasedAt = "yesterday" }, "released_at"},
		"min above version":  {func(m *Manifest) { m.MinVersion = "3.3.0" }, "min_version"},
		"notes over http":    {func(m *Manifest) { m.NotesURL = "http://example.invalid" }, "notes_url"},
		"no platforms":       {func(m *Manifest) { m.Platforms = nil }, "platforms"},
		"path in name":       {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].Name = "../evil.exe" }, "platforms.windows-x64[0].name"},
		"dot name":           {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].Name = ".hidden" }, "platforms.windows-x64[0].name"},
		"backslash":          {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].Name = `a\b.exe` }, "platforms.windows-x64[0].name"},
		"file URL":           {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].URL = "file:///C:/x.exe" }, "platforms.windows-x64[0].url"},
		"user info in URL":   {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].URL = "https://u:p@example.invalid/x" }, "platforms.windows-x64[0].url"},
		"size 0":             {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].Size = 0 }, "platforms.windows-x64[0].size"},
		"upper-case sha":     {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].SHA256 = strings.Repeat("A", 64) }, "platforms.windows-x64[0].sha256"},
		"short sha":          {func(m *Manifest) { m.Platforms[PlatformWindowsX64][0].SHA256 = "abc" }, "platforms.windows-x64[0].sha256"},
		"kind twice":         {func(m *Manifest) { m.Platforms[PlatformWindowsX64][1].Kind = m.Platforms[PlatformWindowsX64][0].Kind }, "platforms.windows-x64[1].kind"},
		"mirror without slash": {func(m *Manifest) {
			m.Mirrors = []MirrorRule{{Prefix: "https://a.invalid", Replace: "https://b.invalid/"}}
		}, "mirrors[0]"},
	}
	for name, c := range cases {
		rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, func(m *Manifest) {
			// a fixed order so that [0] and [1] exist
			ws := m.Platforms[PlatformWindowsX64]
			if ws[0].Kind != KindInstaller {
				ws[0], ws[1] = ws[1], ws[0]
			}
			c.mod(m)
		})
		_, err := ParseManifest(rel.raw)
		wantCode(t, err, CodeManifestInvalid)
		if got := InfoOf(err).Params["field"]; got != c.field {
			t.Errorf("%s: field %v, want %s", name, got, c.field)
		}
	}
	for name, raw := range map[string]string{
		"not JSON":   "hello",
		"two values": `{"schema":1} {}`,
		"array":      `[]`,
		"too large":  `{"x":"` + strings.Repeat("a", maxManifestBytes) + `"}`,
	} {
		if _, err := ParseManifest([]byte(raw)); CodeOf(err) != CodeManifestInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Changing any single byte of update.json makes the signature fail.
func TestManifestTamperedAnyByte(t *testing.T) {
	k := newTestKey(t)
	rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
	if _, err := verifyManifest(k.key, rel.raw, rel.sig); err != nil {
		t.Fatal(err)
	}
	for i := range rel.raw {
		bad := bytes.Clone(rel.raw)
		bad[i] ^= 0x01
		m, err := verifyManifest(k.key, bad, rel.sig)
		if err == nil {
			t.Fatalf("byte %d changed (%q) and update.json still verified: %+v", i, rel.raw[i], m)
		}
		wantCode(t, err, CodeSignatureInvalid)
	}
	// legacy (non-prehashed) signatures too
	legacy := k.sign(rel.raw, TrustedComment("3.2.0"), false)
	if _, err := verifyManifest(k.key, rel.raw, legacy); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, len(rel.raw) / 2, len(rel.raw) - 1} {
		bad := bytes.Clone(rel.raw)
		bad[i] ^= 0x01
		_, err := verifyManifest(k.key, bad, legacy)
		wantCode(t, err, CodeSignatureInvalid)
	}
}

func TestManifestOtherKeyOrVersion(t *testing.T) {
	k, other := newTestKey(t), newTestKey(t)
	rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
	_, err := verifyManifest(other.key, rel.raw, rel.sig)
	wantCode(t, err, CodeSignatureInvalid)

	for tc, signed := range map[string]string{
		TrustedComment("3.2.1"):                 "3.2.1",
		TrustedComment("3.1.0"):                 "3.1.0", // an old signature's comment on a new manifest
		"timestamp:1\tfile:update.json\thashed": "",
	} {
		_, err := verifyManifest(k.key, rel.raw, k.sign(rel.raw, tc, true))
		wantCode(t, err, CodeVersionMismatch)
		if p := InfoOf(err).Params; p["signed"] != signed || p["manifest"] != "3.2.0" {
			t.Errorf("%q: params %v", tc, p)
		}
	}
	// a huge signature file is refused before parsing
	_, err = verifyManifest(k.key, rel.raw, bytes.Repeat([]byte("a"), maxSignatureBytes+1))
	wantCode(t, err, CodeSignatureInvalid)
}

// The README's example is a valid manifest (the frozen format).
func TestReadmeExampleParses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("README.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	start := strings.Index(s, "```json\n{\n  \"schema\": 1,")
	if start < 0 {
		t.Fatal("README.md has no update.json example")
	}
	s = s[start+len("```json\n"):]
	end := strings.Index(s, "```")
	if _, err := ParseManifest([]byte(s[:end])); err != nil {
		t.Fatalf("README example: %v", err)
	}
}

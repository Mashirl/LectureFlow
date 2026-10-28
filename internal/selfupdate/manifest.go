// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// update.json (schema 1; README.md "update.json" is the frozen format).

// SchemaVersion is the update.json schema this version reads.
const SchemaVersion = 1

// AppName is update.json's "app".
const AppName = "LectureFlow"

// Channels.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

// Platforms.
const (
	PlatformWindowsX64  = "windows-x64"
	PlatformDarwinArm64 = "darwin-arm64"
)

// Package kinds.
const (
	KindInstaller = "installer" // Windows NSIS installer (per user)
	KindPortable  = "portable"  // Windows portable executable
	KindDMG       = "dmg"       // macOS disk image (manual installs, offline update)
	KindZip       = "zip"       // macOS: the .app in a zip (downloaded updates)
)

// Size limits of the two small files.
const (
	maxManifestBytes  = 1 << 20
	maxSignatureBytes = 16 << 10
	maxPackageBytes   = 4 << 30
)

// Manifest is update.json.
type Manifest struct {
	Schema     int                `json:"schema"`
	App        string             `json:"app"`
	Version    string             `json:"version"`
	Channel    string             `json:"channel"`
	ReleasedAt string             `json:"released_at"`
	MinVersion string             `json:"min_version,omitempty"`
	NotesURL   string             `json:"notes_url,omitempty"`
	Platforms  map[string][]Asset `json:"platforms"`
	Mirrors    []MirrorRule       `json:"mirrors,omitempty"`
}

// Asset is one downloadable package.
type Asset struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// MirrorRule offers another URL for every asset URL starting with Prefix:
// Replace + the rest of the URL.
type MirrorRule struct {
	Prefix  string `json:"prefix"`
	Replace string `json:"replace"`
}

var (
	kindRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9:_-]{0,63}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// a package file name: a plain base name, no path, no leading dot
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)
)

// ParseManifest decodes and validates update.json. Unknown fields, unknown
// platforms and unknown kinds are allowed (later versions may add them);
// every listed asset must still be well-formed.
func ParseManifest(data []byte) (*Manifest, error) {
	bad := func(field string, err error) error {
		return cerr(CodeManifestInvalid, map[string]any{"field": field}, err)
	}
	if len(data) > maxManifestBytes {
		return nil, bad("size", fmt.Errorf("update.json has %d bytes (at most %d)", len(data), maxManifestBytes))
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&m); err != nil {
		return nil, bad("json", err)
	}
	if dec.More() {
		return nil, bad("json", fmt.Errorf("data after the JSON object"))
	}
	if m.Schema != SchemaVersion {
		return nil, bad("schema", fmt.Errorf("schema %d (this version reads %d)", m.Schema, SchemaVersion))
	}
	if m.App != AppName {
		return nil, bad("app", fmt.Errorf("app %q", m.App))
	}
	v, err := parseVersion(m.Version)
	if err != nil {
		return nil, bad("version", fmt.Errorf("version %q", m.Version))
	}
	switch m.Channel {
	case ChannelStable:
		if len(v.Pre) > 0 {
			return nil, bad("channel", fmt.Errorf("a stable release with a pre-release version %q", m.Version))
		}
	case ChannelBeta:
	default:
		return nil, bad("channel", fmt.Errorf("channel %q", m.Channel))
	}
	if _, err := time.Parse(time.RFC3339, m.ReleasedAt); err != nil {
		return nil, bad("released_at", err)
	}
	if m.MinVersion != "" {
		mv, err := parseVersion(m.MinVersion)
		if err != nil || mv.Compare(v) > 0 {
			return nil, bad("min_version", fmt.Errorf("min_version %q", m.MinVersion))
		}
	}
	if m.NotesURL != "" && !isHTTPURL(m.NotesURL, true) {
		return nil, bad("notes_url", fmt.Errorf("notes_url %q", m.NotesURL))
	}
	if len(m.Platforms) == 0 {
		return nil, bad("platforms", fmt.Errorf("no platform"))
	}
	for p, assets := range m.Platforms {
		if !kindRe.MatchString(p) {
			return nil, bad("platforms", fmt.Errorf("platform %q", p))
		}
		kinds := map[string]bool{}
		for i, a := range assets {
			field := fmt.Sprintf("platforms.%s[%d]", p, i)
			switch {
			case !kindRe.MatchString(a.Kind):
				return nil, bad(field+".kind", fmt.Errorf("kind %q", a.Kind))
			case kinds[a.Kind]:
				return nil, bad(field+".kind", fmt.Errorf("kind %q listed twice", a.Kind))
			case !nameRe.MatchString(a.Name) || strings.Contains(a.Name, "..") || isDeviceName(a.Name):
				return nil, bad(field+".name", fmt.Errorf("name %q", a.Name))
			case !isHTTPURL(a.URL, false):
				return nil, bad(field+".url", fmt.Errorf("url %q", a.URL))
			case a.Size <= 0 || a.Size > maxPackageBytes:
				return nil, bad(field+".size", fmt.Errorf("size %d", a.Size))
			case !sha256Re.MatchString(a.SHA256):
				return nil, bad(field+".sha256", fmt.Errorf("sha256 %q", a.SHA256))
			}
			kinds[a.Kind] = true
		}
	}
	for i, r := range m.Mirrors {
		if !isHTTPURL(r.Prefix, false) || !strings.HasSuffix(r.Prefix, "/") || !isHTTPURL(r.Replace, false) {
			return nil, bad(fmt.Sprintf("mirrors[%d]", i), fmt.Errorf("mirror %q → %q", r.Prefix, r.Replace))
		}
	}
	return &m, nil
}

// Asset returns the asset of a platform and kind.
func (m *Manifest) Asset(platform, kind string) (Asset, bool) {
	for _, a := range m.Platforms[platform] {
		if a.Kind == kind {
			return a, true
		}
	}
	return Asset{}, false
}

// mirrorURLs are the manifest's mirror forms of an asset URL.
func (m *Manifest) mirrorURLs(u string) []string {
	var out []string
	for _, r := range m.Mirrors {
		if strings.HasPrefix(u, r.Prefix) {
			out = append(out, r.Replace+u[len(r.Prefix):])
		}
	}
	return out
}

// isDeviceName reports a name Windows takes for a device whatever its
// directory: CON, PRN, AUX, NUL, COM0–9, LPT0–9 (also with ¹ ² ³),
// CONIN$, CONOUT$, in any case, also before an extension or trailing
// spaces ("NUL.exe", "con .zip").
func isDeviceName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if r := []rune(stem); len(r) == 4 && (string(r[:3]) == "COM" || string(r[:3]) == "LPT") {
		return r[3] >= '0' && r[3] <= '9' || r[3] == '¹' || r[3] == '²' || r[3] == '³'
	}
	return false
}

// isHTTPURL reports an absolute http(s) URL with a host and no user info
// (httpsOnly: https only).
func isHTTPURL(s string, httpsOnly bool) bool {
	if s == "" || len(s) > 2048 || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if httpsOnly {
		return u.Scheme == "https"
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// Verified is an update.json whose signature checked out.
type Verified struct {
	Manifest  *Manifest
	Raw       []byte // update.json as signed
	Signature []byte // update.json.minisig
	Source    string // where it came from ("" = a file)
	source    *source
}

// verifyManifest checks the signature of update.json with pk, that the
// trusted comment names the version update.json declares, and parses it.
// The signature is checked before update.json is parsed.
func verifyManifest(pk Key, raw, sigData []byte) (*Manifest, error) {
	if len(sigData) > maxSignatureBytes {
		return nil, cerr(CodeSignatureInvalid, nil, fmt.Errorf("the signature file has %d bytes", len(sigData)))
	}
	sig, err := ParseSignature(sigData)
	if err != nil {
		return nil, cerr(CodeSignatureInvalid, nil, err)
	}
	if err := pk.Verify(raw, sig); err != nil {
		return nil, cerr(CodeSignatureInvalid, nil, err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	tv, ok := trustedVersion(sig.TrustedComment)
	if !ok || tv != m.Version {
		return nil, cerr(CodeVersionMismatch, map[string]any{"signed": tv, "manifest": m.Version},
			fmt.Errorf("trusted comment %q, update.json version %q", sig.TrustedComment, m.Version))
	}
	return m, nil
}

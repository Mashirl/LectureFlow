// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"lectureflow/internal/selfupdate/selfupdatetest"
)

// Native fuzz tests (go test -fuzz=FuzzXxx ./internal/selfupdate). Under a
// plain go test they run their seeds only.

// fuzzKey is a fixed test key pair, so that the fuzz corpus stays valid
// from one run to the next (no real key is ever used).
func fuzzKey(tb testing.TB) (*selfupdatetest.Key, Key) {
	seed := bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	k := &selfupdatetest.Key{Priv: priv, Pub: priv.Public().(ed25519.PublicKey), ID: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}}
	pk, err := ParsePublicKey(k.PublicKeyFile())
	if err != nil {
		tb.Fatal(err)
	}
	return k, pk
}

// fuzzManifest is a valid update.json of version.
func fuzzManifest(version, channel string) []byte {
	m := Manifest{Schema: 1, App: AppName, Version: version, Channel: channel, ReleasedAt: "2026-11-02T10:00:00Z",
		Platforms: map[string][]Asset{PlatformWindowsX64: {
			{Kind: KindInstaller, Name: "LectureFlow-Setup-" + version + "-x64.exe", URL: "https://example.invalid/s.exe", Size: 10, SHA256: strings.Repeat("a", 64)},
			{Kind: KindPortable, Name: "LectureFlow-" + version + "-x64-portable.exe", URL: "https://example.invalid/p.exe", Size: 11, SHA256: strings.Repeat("b", 64)},
		}},
		Mirrors: []MirrorRule{{Prefix: "https://example.invalid/", Replace: "https://mirror.invalid/gh/"}},
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return b
}

var semverChars = regexp.MustCompile(`^[0-9A-Za-z.+-]+$`)

// isLocalName: a name that stays one entry of its directory on every
// system (Windows reserved device names included, whatever this system is).
func isLocalName(s string) bool {
	if s == "" || strings.ContainsAny(s, `/\:`) || s == "." || s == ".." {
		return false
	}
	return !isDeviceName(s) && filepath.IsLocal(s)
}

// checkManifest: what ParseManifest accepted is safe to use as names and
// URLs.
func checkManifest(t *testing.T, m *Manifest) {
	t.Helper()
	if !semverChars.MatchString(m.Version) || !isLocalName(m.Version) {
		t.Fatalf("version %q accepted", m.Version)
	}
	if _, err := parseVersion(m.Version); err != nil {
		t.Fatalf("version %q: %v", m.Version, err)
	}
	if m.MinVersion != "" && compareVersions(m.MinVersion, m.Version) > 0 {
		t.Fatalf("min_version %q above %q", m.MinVersion, m.Version)
	}
	for p, assets := range m.Platforms {
		for _, a := range assets {
			if !isLocalName(a.Name) || strings.HasPrefix(a.Name, ".") || a.Size <= 0 || len(a.SHA256) != 64 {
				t.Fatalf("asset %+v of %s accepted", a, p)
			}
			if u, err := url.Parse(a.URL); err != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				t.Fatalf("asset URL %q accepted", a.URL)
			}
		}
	}
	for _, r := range m.Mirrors {
		if !strings.HasSuffix(r.Prefix, "/") || !isHTTPURL(r.Replace, false) {
			t.Fatalf("mirror %+v accepted", r)
		}
	}
}

// FuzzParseManifest: no panic; whatever passes is safe to use.
func FuzzParseManifest(f *testing.F) {
	f.Add(fuzzManifest("3.2.0", ChannelStable))
	f.Add(fuzzManifest("3.2.0-beta.1", ChannelBeta))
	f.Add(fuzzManifest("1.0.0+x/../../../evil", ChannelStable))
	f.Add(fuzzManifest("1.0.0-a\\b", ChannelBeta))
	f.Add(bytes.Replace(fuzzManifest("3.2.0", ChannelStable), []byte(`"LectureFlow-3.2.0-x64-portable.exe"`), []byte(`"CON.exe"`), 1))
	f.Add([]byte(`{"schema":1,"app":"LectureFlow","VERSION":"1.0.0"} {}`))
	f.Add([]byte(`{"schema":1e400}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseManifest(data)
		if err != nil {
			return
		}
		checkManifest(t, m)
	})
}

// FuzzVerifyManifest: only the exact bytes the key signed verify, and then
// with the version the trusted comment names.
func FuzzVerifyManifest(f *testing.F) {
	k, pk := fuzzKey(f)
	signed := map[string]bool{}
	for _, c := range []struct {
		version, channel string
		prehashed        bool
	}{{"3.2.0", ChannelStable, true}, {"3.2.0", ChannelStable, false}, {"3.3.0-beta.1", ChannelBeta, true}} {
		raw := fuzzManifest(c.version, c.channel)
		signed[string(raw)] = true
		f.Add(raw, k.Sign(raw, TrustedComment(c.version), c.prehashed))
		f.Add(raw, k.Sign(raw, TrustedComment("9.9.9"), c.prehashed))
		f.Add(raw, k.Sign(raw, TrustedComment(c.version)+" timestamp:1 file:update.json", c.prehashed))
	}
	f.Fuzz(func(t *testing.T, raw, sigData []byte) {
		m, err := verifyManifest(pk, raw, sigData)
		if err != nil {
			return
		}
		if !signed[string(raw)] {
			t.Fatalf("update.json verified without its signature:\n%s\n%s", raw, sigData)
		}
		sig, err := ParseSignature(sigData)
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := trustedVersion(sig.TrustedComment); !ok || v != m.Version {
			t.Fatalf("trusted comment %q for %s", sig.TrustedComment, m.Version)
		}
		checkManifest(t, m)
	})
}

// FuzzParseSignature: no panic; a parsed signature has its fixed sizes and
// never verifies against bytes the key did not sign.
func FuzzParseSignature(f *testing.F) {
	k, pk := fuzzKey(f)
	msg := []byte("hello")
	f.Add(k.Sign(msg, "lectureflow update version:1.0.0", true))
	f.Add(k.Sign(msg, "x", false))
	f.Add([]byte("untrusted comment: x\r\nRUQ=\r\ntrusted comment: \r\nAA==\r\n"))
	f.Add([]byte("untrusted comment: x\n\n\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		sig, err := ParseSignature(data)
		if err != nil {
			return
		}
		if len(sig.Sig) != ed25519.SignatureSize || len(sig.GlobalSignature) != ed25519.SignatureSize ||
			(sig.Algorithm != algLegacy && sig.Algorithm != algPrehashed) {
			t.Fatalf("signature %+v", sig)
		}
		if pk.Verify([]byte("other message"), sig) == nil {
			t.Fatal("a signature of another message verified")
		}
		if pk.Verify(msg, sig) == nil && !strings.Contains(string(data), sig.TrustedComment) {
			t.Fatal("verified with a trusted comment the file does not hold")
		}
	})
}

// FuzzParsePublicKey: no panic; a parsed key prints back to itself.
func FuzzParsePublicKey(f *testing.F) {
	k, _ := fuzzKey(f)
	f.Add(k.PublicKeyFile())
	f.Add(k.PublicKey())
	f.Add("untrusted comment: x\n\n")
	f.Add("RWQ=")
	f.Fuzz(func(t *testing.T, s string) {
		pk, err := ParsePublicKey(s)
		if err != nil {
			return
		}
		if len(pk.Key) != ed25519.PublicKeySize {
			t.Fatalf("key of %d bytes", len(pk.Key))
		}
		again, err := ParsePublicKey(pk.String())
		if err != nil || again.KeyID != pk.KeyID || !bytes.Equal(again.Key, pk.Key) {
			t.Fatalf("String() = %q does not parse back: %v", pk.String(), err)
		}
	})
}

// FuzzTrustedVersion: the version a trusted comment names is one field,
// and TrustedComment writes what trustedVersion reads.
func FuzzTrustedVersion(f *testing.F) {
	for _, s := range []string{"lectureflow update version:1.0.0", "lectureflow update version:1.0.0 version:2.0.0",
		"lectureflow  update\tversion:1.0.0 timestamp:1", "lectureflow update version:", "lectureflow update x version:1", "lectureflow update"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, tc string) {
		v, ok := trustedVersion(tc)
		if !ok {
			return
		}
		if v == "" || strings.IndexFunc(v, func(r rune) bool { return strings.ContainsRune(" \t\r\n\v\f\u0085\u00a0", r) }) >= 0 {
			t.Fatalf("trustedVersion(%q) = %q", tc, v)
		}
		if again, ok := trustedVersion(TrustedComment(v)); !ok || again != v {
			t.Fatalf("TrustedComment(%q) reads back as %q, %v", v, again, ok)
		}
	})
}

// FuzzParseVersion: a version that parses is a plain directory name (it
// names updates/<version>/).
func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{"3.2.0", "3.2.0-beta.1", "1.0.0+build.5", "1.0.0+x/../../../evil", "1.0.0-a\\b", "v1.0.0", " 1.0.0",
		"1.0.0-", "1.0.0+", "01.0.0", "1.0.0-CON", "1.0.0+a:b"} {
		f.Add(s)
	}
	dir := filepath.Join("updates", "x")
	f.Fuzz(func(t *testing.T, s string) {
		if _, err := parseVersion(s); err != nil {
			return
		}
		if !semverChars.MatchString(s) || !isLocalName(s) {
			t.Fatalf("parseVersion accepted %q", s)
		}
		if rel, err := filepath.Rel(dir, filepath.Join(dir, s)); err != nil || rel != s {
			t.Fatalf("version %q leaves its directory (%q)", s, rel)
		}
	})
}

// FuzzParseSource: an extra source of the settings is refused or gives
// update.json and signature URLs without a query or fragment, on the
// source's own host.
func FuzzParseSource(f *testing.F) {
	for _, s := range []string{"https://p.invalid/{url}", "https://m.invalid/lf", "https://m.invalid/x/beta.json", "https://m.invalid/x/a.json?",
		"https://m.invalid/x/#", "https://m.invalid/x/a.json#", "http://u:p@m.invalid/", "https://{url}", "{url}", "https://p/{url}{url}",
		"https://m.invalid", "https://m.invalid/%zz", "  https://m.invalid/a  "} {
		f.Add(s)
	}
	const latest = "https://github.com/o/r/releases/latest/download/update.json"
	f.Fuzz(func(t *testing.T, raw string) {
		s, err := parseSource(raw, latest)
		if err != nil {
			return
		}
		a := Asset{Name: "LectureFlow Setup.exe", URL: "https://github.com/o/r/releases/download/v1.0.0/LectureFlow-Setup.exe"}
		for _, u := range []string{s.manifest, s.sig, s.assetURL(a)} {
			if !isHTTPURL(u, false) {
				t.Fatalf("parseSource(%q): URL %q", raw, u)
			}
		}
		if s.kind != srcDir {
			return
		}
		src, _ := url.Parse(strings.TrimSpace(raw))
		for _, p := range []string{s.manifest, s.sig, s.assetURL(a)} {
			u, _ := url.Parse(p)
			if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(p, "?#") || u.Host != src.Host {
				t.Fatalf("parseSource(%q): %q", raw, p)
			}
		}
		if s.sig != s.manifest+".minisig" || !strings.HasSuffix(s.base, "/") || !strings.HasPrefix(s.manifest, s.base) {
			t.Fatalf("parseSource(%q) = %+v", raw, s)
		}
	})
}

// FuzzAbsPath: a path from pending.json resolves inside the updates
// directory, to an entry this system can open as a file.
func FuzzAbsPath(f *testing.F) {
	for _, s := range []string{"3.2.0/update.json", "../x", "a/../../x", "/etc/passwd", `C:\x`, "C:x", "a/b:c", "NUL", "3.2.0/CON.exe",
		"a//b/./c", ".", "", "a/..", "\\\\server\\share"} {
		f.Add(s)
	}
	dir := filepath.Join(f.TempDir(), "updates")
	f.Fuzz(func(t *testing.T, rel string) {
		p, ok := absPath(dir, rel)
		if !ok {
			return
		}
		// (filepath.IsLocal knows this system's device names: "CON.exe"
		// is one on Windows 10, not on Windows 11)
		r, err := filepath.Rel(dir, p)
		if err != nil || r == "." || !filepath.IsLocal(r) || (runtime.GOOS == "windows" && strings.Contains(r, ":")) {
			t.Fatalf("absPath(%q) = %q, outside %q or not a plain file", rel, p, dir)
		}
	})
}

// FuzzStagingName: a file of an offline update gets a plain, valid name in
// the staging directory that keeps the end of its own name.
func FuzzStagingName(f *testing.F) {
	for _, s := range []string{"update.json", `C:\Users\x\a.exe`, "../../evil", "..", "a:b*c?.json", strings.Repeat("a", 300),
		strings.Repeat("长", 100) + ".exe", "CON", " .x", "a\x00b"} {
		f.Add(uint8(0), s)
	}
	f.Fuzz(func(t *testing.T, i uint8, name string) {
		got := StagingName(int(i)%MaxImportFiles, name)
		prefix, rest, ok := strings.Cut(got, "-")
		if !ok || prefix == "" || rest == "" || len(rest) > 120 || !isLocalName(got) || strings.ContainsAny(got, `<>:"/\|?*`) {
			t.Fatalf("StagingName(%q) = %q", name, got)
		}
		for _, r := range got {
			if r < 0x20 {
				t.Fatalf("StagingName(%q) = %q", name, got)
			}
		}
		if utf8.ValidString(name) && !utf8.ValidString(got) {
			t.Fatalf("StagingName(%q) = %q is not UTF-8", name, got)
		}
	})
}

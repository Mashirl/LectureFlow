// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noNetwork fails the test on any request: offline updates must work
// without a network.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("network request during an offline update: %s", r.URL)
	return nil, errors.New("offline")
}

type noDownload struct{ t *testing.T }

func (n noDownload) Download(context.Context, DownloadRequest) error {
	n.t.Error("download during an offline update")
	return errors.New("offline")
}

func offlineUpdater(t *testing.T, k testKey, mod func(*Options)) *Updater {
	return newTestUpdater(t, nil, k, func(o *Options) {
		o.Client = &http.Client{Transport: noNetwork{t}}
		o.Downloader = noDownload{t}
		if mod != nil {
			mod(o)
		}
	})
}

// userFile writes a file the user picked.
func userFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func makeZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, b := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(b)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func noStaging(t *testing.T, u *Updater) {
	t.Helper()
	entries, _ := os.ReadDir(u.Dir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), importPrefix) {
			t.Fatalf("staging directory %s left behind", e.Name())
		}
	}
}

func TestImportThreeFiles(t *testing.T) {
	k := newTestKey(t)
	rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	src := t.TempDir()
	// names as a browser may have changed them; any order
	paths := []string{
		userFile(t, src, "LectureFlow-Setup-3.2.0-x64 (1).exe", rel.pkgs[name]),
		userFile(t, src, "update.json.minisig.txt", rel.sig),
		userFile(t, src, "update (1).json", rel.raw),
	}
	u := offlineUpdater(t, k, nil)
	st, err := u.ImportPaths(bg, paths)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StatePending || st.Pending.Version != "3.2.0" || st.Pending.Origin != OriginFile || st.Pending.File != name {
		t.Fatalf("%+v %+v", st, st.Pending)
	}
	b, err := os.ReadFile(filepath.Join(u.Dir(), "3.2.0", name))
	if err != nil || !bytes.Equal(b, rel.pkgs[name]) {
		t.Fatalf("package: %v", err)
	}
	var p Pending
	readJSON(filepath.Join(u.Dir(), pendingFile), &p)
	if p.Origin != OriginFile || p.File != "3.2.0/"+name || p.SHA256 != sha(rel.pkgs[name]) {
		t.Fatalf("pending.json %+v", p)
	}
	for _, path := range paths { // the user's files are left alone
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	noStaging(t, u)
}

func TestImportZipBundle(t *testing.T) {
	k := newTestKey(t)
	rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
	name := "LectureFlow-3.2.0-x64-portable.exe"
	z := makeZip(t, map[string][]byte{
		"LectureFlow-3.2.0/update.json":         rel.raw,
		"LectureFlow-3.2.0/update.json.minisig": rel.sig,
		"LectureFlow-3.2.0/" + name:             rel.pkgs[name],
		"__MACOSX/LectureFlow-3.2.0/._" + name:  []byte("resource fork"),
		"LectureFlow-3.2.0/README.txt":          []byte("other files are ignored when the package is found by name"),
	})
	u := offlineUpdater(t, k, func(o *Options) { o.Portable = true })
	st, err := u.ImportPaths(bg, []string{userFile(t, t.TempDir(), "update.zip", z)})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StatePending || st.Pending.File != name {
		t.Fatalf("%+v", st)
	}
	noStaging(t, u)
}

func TestImportMacOS(t *testing.T) {
	k := newTestKey(t)
	rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
	mac := func() *Updater {
		return offlineUpdater(t, k, func(o *Options) { o.Platform = PlatformDarwinArm64 })
	}
	src := t.TempDir()
	zipName, dmgName := "LectureFlow-3.2.0-arm64-mac.zip", "LectureFlow-3.2.0-arm64.dmg"
	// the dmg with update.json and its signature
	u := mac()
	st, err := u.ImportPaths(bg, []string{userFile(t, src, dmgName, rel.pkgs[dmgName]),
		userFile(t, src, "update.json", rel.raw), userFile(t, src, "update.json.minisig", rel.sig)})
	if err != nil || st.Pending == nil || st.Kind != KindZip {
		t.Fatalf("dmg: %+v %v", st, err)
	}
	var p Pending
	readJSON(filepath.Join(u.Dir(), pendingFile), &p)
	if p.Kind != KindDMG {
		t.Fatalf("pending kind %q", p.Kind)
	}
	// the macOS zip alone is a package, not a bundle
	_, err = mac().ImportPaths(bg, []string{userFile(t, src, zipName, rel.pkgs[zipName])})
	wantCode(t, err, CodeImportIncomplete)
	// ... and inside a bundle it is found
	z := makeZip(t, map[string][]byte{"update.json": rel.raw, "update.json.minisig": rel.sig, zipName: rel.pkgs[zipName]})
	st, err = mac().ImportPaths(bg, []string{userFile(t, src, "bundle.zip", z)})
	if err != nil || st.Pending == nil || st.Pending.File != zipName {
		t.Fatalf("bundle: %+v %v", st, err)
	}
	// a Windows package does not fit a Mac
	_, err = mac().ImportPaths(bg, []string{userFile(t, src, "w.exe", rel.pkgs["LectureFlow-Setup-3.2.0-x64.exe"]),
		userFile(t, src, "update.json", rel.raw), userFile(t, src, "update.json.minisig", rel.sig)})
	wantCode(t, err, CodeChecksum)
}

func TestImportRejected(t *testing.T) {
	k, other := newTestKey(t), newTestKey(t)
	rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	pkg := rel.pkgs[name]
	flip := func(b []byte, i int) []byte {
		c := bytes.Clone(b)
		c[i] ^= 0x01
		return c
	}
	same := makeRelease(t, nil, k, "3.1.0", ChannelStable, nil)
	older := makeRelease(t, nil, k, "3.0.5", ChannelStable, nil)
	minv := makeRelease(t, nil, k, "4.0.0", ChannelStable, func(m *Manifest) { m.MinVersion = "3.5.0" })
	type files = [][2]any // name, content
	cases := map[string]struct {
		files files
		code  string
		param [2]string
	}{
		"manifest byte 0":       {files{{"update.json", flip(rel.raw, 0)}, {"update.json.minisig", rel.sig}, {name, pkg}}, CodeSignatureInvalid, [2]string{}},
		"manifest byte last":    {files{{"update.json", flip(rel.raw, len(rel.raw)-1)}, {"update.json.minisig", rel.sig}, {name, pkg}}, CodeSignatureInvalid, [2]string{}},
		"package byte 0":        {files{{"update.json", rel.raw}, {"update.json.minisig", rel.sig}, {name, flip(pkg, 0)}}, CodeChecksum, [2]string{"file", name}},
		"package byte middle":   {files{{"update.json", rel.raw}, {"update.json.minisig", rel.sig}, {name, flip(pkg, len(pkg)/2)}}, CodeChecksum, [2]string{"file", name}},
		"package byte last":     {files{{"update.json", rel.raw}, {"update.json.minisig", rel.sig}, {name, flip(pkg, len(pkg)-1)}}, CodeChecksum, [2]string{"file", name}},
		"package truncated":     {files{{"update.json", rel.raw}, {"update.json.minisig", rel.sig}, {name, pkg[:len(pkg)-1]}}, CodeChecksum, [2]string{"file", name}},
		"other key":             {files{{"update.json", rel.raw}, {"update.json.minisig", other.sign(rel.raw, TrustedComment("3.2.0"), true)}, {name, pkg}}, CodeSignatureInvalid, [2]string{}},
		"signature tampered":    {files{{"update.json", rel.raw}, {"update.json.minisig", bytes.Replace(rel.sig, []byte("trusted comment: lectureflow update version:3.2.0"), []byte("trusted comment: lectureflow update version:3.2.1"), 1)}, {name, pkg}}, CodeSignatureInvalid, [2]string{}},
		"version not signed":    {files{{"update.json", rel.raw}, {"update.json.minisig", k.sign(rel.raw, TrustedComment("3.2.1"), true)}, {name, pkg}}, CodeVersionMismatch, [2]string{"signed", "3.2.1"}},
		"same version":          {files{{"update.json", same.raw}, {"update.json.minisig", same.sig}, {"LectureFlow-Setup-3.1.0-x64.exe", same.pkgs["LectureFlow-Setup-3.1.0-x64.exe"]}}, CodeNotNewer, [2]string{"version", "3.1.0"}},
		"downgrade":             {files{{"update.json", older.raw}, {"update.json.minisig", older.sig}, {"LectureFlow-Setup-3.0.5-x64.exe", older.pkgs["LectureFlow-Setup-3.0.5-x64.exe"]}}, CodeNotNewer, [2]string{"version", "3.0.5"}},
		"too old":               {files{{"update.json", minv.raw}, {"update.json.minisig", minv.sig}, {"LectureFlow-Setup-4.0.0-x64.exe", minv.pkgs["LectureFlow-Setup-4.0.0-x64.exe"]}}, CodeTooOld, [2]string{"min_version", "3.5.0"}},
		"portable on installer": {files{{"update.json", rel.raw}, {"update.json.minisig", rel.sig}, {"p.exe", rel.pkgs["LectureFlow-3.2.0-x64-portable.exe"]}}, CodeChecksum, [2]string{"file", "p.exe"}},
		"no signature":          {files{{"update.json", rel.raw}, {name, pkg}}, CodeImportIncomplete, [2]string{"missing", "signature"}},
		"no manifest":           {files{{"update.json.minisig", rel.sig}, {name, pkg}}, CodeImportIncomplete, [2]string{"missing", "manifest"}},
		"no package":            {files{{"update.json", rel.raw}, {"update.json.minisig", rel.sig}}, CodeImportIncomplete, [2]string{"missing", "package"}},
		"package alone":         {files{{name, pkg}}, CodeImportIncomplete, [2]string{"missing", "manifest"}},
		"two packages":          {files{{"update.json", rel.raw}, {name, pkg}, {"b.exe", pkg}}, CodeImportInvalid, [2]string{}},
		"two signatures":        {files{{"a.minisig", rel.sig}, {"b.minisig", rel.sig}, {name, pkg}}, CodeImportInvalid, [2]string{"file", "b.minisig"}},
		"four files":            {files{{"update.json", rel.raw}, {"update.json.minisig", rel.sig}, {name, pkg}, {"x", []byte("x")}}, CodeImportInvalid, [2]string{"file", "x"}},
		"zip without signature": {files{{"u.zip", makeZip(t, map[string][]byte{"update.json": rel.raw, name: pkg})}}, CodeImportIncomplete, [2]string{"missing", "signature"}},
		"zip without package":   {files{{"u.zip", makeZip(t, map[string][]byte{"update.json": rel.raw, "update.json.minisig": rel.sig})}}, CodeImportIncomplete, [2]string{"missing", "package"}},
		"zip tampered package":  {files{{"u.zip", makeZip(t, map[string][]byte{"update.json": rel.raw, "update.json.minisig": rel.sig, name: flip(pkg, 7)})}}, CodeChecksum, [2]string{"file", name}},
		"zip larger package":    {files{{"u.zip", makeZip(t, map[string][]byte{"update.json": rel.raw, "update.json.minisig": rel.sig, name: append(bytes.Clone(pkg), 0)})}}, CodeChecksum, [2]string{"file", name}},
		"zip tampered manifest": {files{{"u.zip", makeZip(t, map[string][]byte{"update.json": flip(rel.raw, 40), "update.json.minisig": rel.sig, name: pkg})}}, CodeSignatureInvalid, [2]string{}},
	}
	for cname, c := range cases {
		src := t.TempDir()
		var paths []string
		for i, f := range c.files {
			paths = append(paths, userFile(t, src, strings.Repeat("_", i)+f[0].(string), f[1].([]byte)))
		}
		u := offlineUpdater(t, k, nil)
		st, err := u.ImportPaths(bg, paths)
		if CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", cname, err, c.code)
			continue
		}
		if c.param[0] != "" {
			got, _ := InfoOf(err).Params[c.param[0]].(string)
			if strings.TrimLeft(got, "_") != c.param[1] {
				t.Errorf("%s: %s = %q, want %q", cname, c.param[0], got, c.param[1])
			}
		}
		if st.Pending != nil || st.State == StateImporting {
			t.Errorf("%s: status %+v", cname, st)
		}
		if _, err := os.Stat(filepath.Join(u.Dir(), pendingFile)); !os.IsNotExist(err) {
			t.Errorf("%s: pending.json written", cname)
		}
		noStaging(t, u)
	}
	// a file that cannot be read
	u := offlineUpdater(t, k, nil)
	_, err := u.ImportPaths(bg, []string{filepath.Join(t.TempDir(), "missing.json")})
	wantCode(t, err, CodeImportInvalid)
}

func TestStagingName(t *testing.T) {
	for in, want := range map[string]string{
		"update.json":            "0-update.json",
		`C:\Users\x\a.exe`:       "0-a.exe",
		"../../evil":             "0-evil",
		"..":                     "0-file",
		"a:b*c?.json":            "0-a_b_c_.json",
		strings.Repeat("a", 300): "0-" + strings.Repeat("a", 120),
	} {
		if got := StagingName(0, in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stagingPathRe matches a file under a staging directory: {dir}/.s{n}/…
// (and the longer name used before V12.2).
var stagingPathRe = regexp.MustCompile(`(^|/)(\.s\d+|\.staging-[^/]*)/`)

func isStagingPath(rel string) bool { return stagingPathRe.MatchString(filepath.ToSlash(rel)) }

func TestPathLen(t *testing.T) {
	for s, want := range map[string]int{
		``:                0,
		`D:\LectureFlow`:  14,
		`D:\讲座`:           5, // CJK: one UTF-16 unit each (3 bytes in Go)
		"D:\\\U0001F600x": 6, // outside the BMP: a surrogate pair
	} {
		if got := PathLen(s); got != want {
			t.Errorf("PathLen(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestRelPathNeed(t *testing.T) {
	model := &Component{ID: "model:openvino:tiny:int8", Version: "1.0.0", Kind: KindModel}
	b := &Build{Files: []File{{Name: "a.bin"}, {Name: "openvino_encoder_model.xml"}}}
	want := len("components/model__openvino__tiny__int8/1.0.0-r99/") + len("model/openvino_encoder_model.xml")
	if got := RelPathNeed(model, b); got != want {
		t.Errorf("model: %d, want %d", got, want)
	}
	// measured value wins
	b.MaxPath = 40
	if got := RelPathNeed(model, b); got != len("components/model__openvino__tiny__int8/1.0.0-r99/")+40 {
		t.Errorf("model with max_path: %d", got)
	}
	// a venv without a measurement and an archive to unpack are unknown
	venv := &Component{ID: "asr-x", Version: "1.0.0", Kind: KindVenv}
	if got := RelPathNeed(venv, &Build{}); got != 0 {
		t.Errorf("venv without max_path: %d", got)
	}
	bin := &Component{ID: "tool", Version: "1.0.0", Kind: KindBinary}
	if got := RelPathNeed(bin, &Build{Files: []File{{Name: "t.zip", Unpack: "zip"}}}); got != 0 {
		t.Errorf("binary archive: %d", got)
	}
	if got := RelPathNeed(venv, &Build{MaxPath: 123}); got != len("components/asr-x/1.0.0-r99/")+123 {
		t.Errorf("venv with max_path: %d", got)
	}
}

// longDataDir is a data directory whose absolute path has n characters.
func longDataDir(t *testing.T, n int) string {
	base := t.TempDir()
	pad := n - PathLen(base) - 1
	if pad < 1 {
		t.Fatalf("temp dir %s is already longer than %d", base, n)
	}
	return filepath.Join(base, strings.Repeat("d", pad))
}

func pathTooLongInfo(t *testing.T, err error) map[string]any {
	t.Helper()
	if CodeOf(err) != CodePathTooLong {
		t.Fatalf("want %s, got %v", CodePathTooLong, err)
	}
	return ErrorInfoOf(err).Params
}

// Plan and Install refuse a component whose deepest file would exceed
// MaxPathLen under the data directory, before anything is downloaded.
func TestPlanRefusesTooLongPaths(t *testing.T) {
	e := newTestEnv(t)
	// asr-cuda measured at 100 characters below its version directory
	man := e.manifest(defaultSpec())
	man.Component(IDASRCUDA).Platforms[PlatformWindowsX64].MaxPath = 100
	if err := os.WriteFile(filepath.Join(e.manDir, "components.json"), marshalManifest(t, man), 0o644); err != nil {
		t.Fatal(err)
	}
	need := RelPathNeed(man.Component(IDASRCUDA), man.Component(IDASRCUDA).Build(PlatformWindowsX64))
	// the longest data directory that still fits, and one character more
	fits := MaxPathLen - 1 - need
	e.data = longDataDir(t, fits+1)
	m := e.open()
	_, err := m.Plan([]string{IDASRCUDA})
	p := pathTooLongInfo(t, err)
	if p["id"] != IDASRCUDA || p["path_len"] != MaxPathLen+1 || p["max"] != MaxPathLen || p["data_dir"] != e.data {
		t.Errorf("params %+v (data dir %s)", p, e.data)
	}
	if _, err := m.Install([]string{IDASRCUDA}, Sources{}); CodeOf(err) != CodePathTooLong {
		t.Fatalf("install: %v", err)
	}
	if len(m.Tasks()) != 0 {
		t.Error("a task was created")
	}
	if len(e.srv.log()) != 0 {
		t.Errorf("downloaded: %v", e.srv.log())
	}
	// python has no measurement in this manifest: not refused
	if _, err := m.Plan([]string{IDPython}); err != nil {
		t.Errorf("python: %v", err)
	}
	m.Close()

	// one character shorter fits
	e.data = longDataDir(t, fits)
	m2 := e.open()
	if _, err := m2.Plan([]string{IDASRCUDA}); err != nil {
		t.Fatalf("fits: %v", err)
	}
	m2.Close()

	// models are checked from their file names; other platforms not at all
	e.data = longDataDir(t, 240)
	m3 := e.open()
	if _, err := m3.Plan([]string{ovModel}); CodeOf(err) != CodePathTooLong {
		t.Errorf("model under a 240-character data directory: %v", err)
	}
	m3.Close()
	o := e.options()
	o.Platform = PlatformDarwinARM64
	m4, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer m4.Close()
	if err := m4.checkPathLen(man.Component(IDASRCUDA), man.Component(IDASRCUDA).Build(PlatformWindowsX64)); err != nil {
		t.Errorf("macOS: %v", err)
	}
}

// An installed component is not checked again (Plan lists it as installed).
func TestPlanSkipsInstalledComponents(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	m.Close()
	// the same files, now moved below a long directory by hand
	long := longDataDir(t, 240)
	if err := os.MkdirAll(filepath.Dir(long), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.data, long); err != nil {
		t.Fatal(err)
	}
	e.data = long
	m2 := e.open()
	defer m2.Close()
	p, err := m2.Plan([]string{ovModel})
	if err != nil || len(p.Items) != 1 || !p.Items[0].Installed {
		t.Fatalf("plan %+v, %v", p, err)
	}
}

// ImportFile refuses at once, from the archive's component.json.
func TestImportRefusesTooLongPaths(t *testing.T) {
	e, zips := offlineEnv(t)
	e.data = longDataDir(t, 240)
	m := e.open()
	defer m.Close()
	_, err := m.ImportFile(zips[ovModel])
	if p := pathTooLongInfo(t, err); p["id"] != ovModel {
		t.Errorf("params %+v", p)
	}
	if len(m.Tasks()) != 0 {
		t.Error("a task was created")
	}
}

// The task checks again when it identifies the archive by its SHA-256
// (the private copy, not the file ImportFile looked at).
func TestIdentifyImportChecksPathLength(t *testing.T) {
	e, zips := offlineEnv(t)
	e.data = longDataDir(t, 240)
	m := e.open()
	defer m.Close()
	fi, err := os.Stat(zips[ovModel])
	if err != nil {
		t.Fatal(err)
	}
	off := m.manifest().Component(ovModel).Build(PlatformWindowsX64).Offline
	tk := &task{id: "t1", man: m.manifest()}
	it := &item{offline: &offlineSource{orig: zips[ovModel], path: zips[ovModel], sha256: off.SHA256, size: fi.Size()}}
	err = m.identifyImport(context.Background(), tk, it)
	if p := pathTooLongInfo(t, err); p["id"] != ovModel {
		t.Errorf("params %+v", p)
	}
	if it.id != "" {
		t.Error("the item was identified: a retry would skip the check")
	}
}

// Staging directories are short and never shared.
func TestStagingDirNames(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	defer m.Close()
	tk := &task{id: "t1792863891255-1"}
	a, b := &item{id: IDASRCUDA, version: "1.0.0"}, &item{id: IDASRCUDA, version: "1.0.0"}
	da, err := m.stagingDir(tk, a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := m.stagingDir(tk, b)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(da) != ".s1" || filepath.Base(db) != ".s2" {
		t.Errorf("staging %s, %s", da, db)
	}
	if again, _ := m.stagingDir(tk, a); again != da {
		t.Errorf("a paused item gets another directory: %s", again)
	}
	// never longer than the version directory it becomes
	if len(filepath.Base(db)) > len("1.0.0"+rebuildSuffix) {
		t.Error("staging name longer than {version}-r99")
	}
}

// Every Windows build of the shipped manifest has a known path need, and
// the default data directory (%APPDATA%\LectureFlow, about 40 characters)
// leaves ample room.
func TestShippedManifestPathNeeds(t *testing.T) {
	man, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	longest := 0
	for i := range man.Components {
		c := &man.Components[i]
		b := c.Build(PlatformWindowsX64)
		if b == nil || c.Bundled || c.Unavailable != "" {
			continue
		}
		if (c.Kind == KindVenv || c.Kind == KindPythonRuntime) && b.MaxPath == 0 {
			t.Errorf("%s: windows-x64 build without max_path", c.ID)
		}
		need := RelPathNeed(c, b)
		if need == 0 {
			t.Errorf("%s: path need unknown", c.ID)
		}
		if need > MaxPathLen-1-80 {
			t.Errorf("%s needs %d characters below the data directory: data directories over %d characters fail", c.ID, need, MaxPathLen-1-need)
		}
		longest = max(longest, need)
	}
	t.Logf("longest need %d: data directories up to %d characters work", longest, MaxPathLen-1-longest)
}

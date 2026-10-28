// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

// ssh-mvp: linux-x64 builds are only a target of the SSH inference servers.
// This machine's platform is never linux-x64 (on Linux CurrentPlatform is
// "linux-amd64"), so the component manager lists, plans, downloads and
// activates no linux-x64 build here, whatever it runs on.

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLinuxX64IsRemoteOnly(t *testing.T) {
	if CurrentPlatform() == PlatformLinuxX64 {
		t.Fatal("this machine's platform is the remote-only linux-x64")
	}
	// CurrentPlatform's rule for systems without components (Linux among
	// them): GOOS-GOARCH, which can never spell linux-x64.
	if runtime.GOOS+"-"+runtime.GOARCH == PlatformLinuxX64 || "linux"+"-"+"amd64" == PlatformLinuxX64 {
		t.Fatal("GOOS-GOARCH could name linux-x64")
	}
	m, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	// The shipped manifest has linux-x64 builds (the SSH servers' kits)…
	for _, id := range []string{IDPython, IDASRCUDA, "model:faster-whisper:large-v3:float16"} {
		if m.Component(id).Build(PlatformLinuxX64) == nil {
			t.Errorf("%s: no linux-x64 build", id)
		}
	}
	// …and the asr-cuda locks of both platforms pin the same top-level versions.
	top := func(name string) map[string]string {
		b, err := readLock(t, name)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, line := range strings.Split(b, "\n") {
			if pkg, ver, ok := strings.Cut(strings.TrimSuffix(strings.TrimSpace(line), `\`), "=="); ok && line != "" && line[0] != ' ' && line[0] != '#' {
				out[pkg] = strings.TrimSpace(ver)
			}
		}
		return out
	}
	win, lin := top("asr-cuda-windows-x64.txt"), top("asr-cuda-linux-x64.txt")
	for _, pkg := range []string{"faster-whisper", "ctranslate2", "av", "nvidia-cublas-cu12", "nvidia-cudnn-cu12"} {
		if win[pkg] == "" || win[pkg] != lin[pkg] {
			t.Errorf("%s: windows %q, linux %q", pkg, win[pkg], lin[pkg])
		}
	}

	// A manager on any local platform — the two with components and a
	// Linux machine's "linux-amd64" — never offers a linux-x64 build.
	abs, _ := filepath.Abs(builtinManifest)
	for _, plat := range []string{PlatformWindowsX64, PlatformDarwinARM64, "linux-amd64"} {
		mgr, err := New(Options{DataDir: t.TempDir(), AppVersion: "3.0.0", Platform: plat, BuiltinManifest: abs})
		if err != nil {
			t.Fatal(err)
		}
		if mgr.Platform() == PlatformLinuxX64 {
			t.Fatalf("%s: the manager installs for linux-x64", plat)
		}
		for _, s := range mgr.List() {
			c := m.Component(s.ID)
			if c != nil && c.Build(plat) == nil && !c.Bundled {
				t.Errorf("%s: lists %s, which has no build for this platform", plat, s.ID)
			}
		}
		if plat == "linux-amd64" {
			// Nothing can be planned or installed: the linux-x64 builds are not this machine's.
			if _, err := mgr.Plan([]string{IDASRCUDA}); err == nil {
				t.Errorf("%s: asr-cuda can be planned", plat)
			}
			if _, err := mgr.Install([]string{"model:faster-whisper:large-v3:float16"}, Sources{}); err == nil {
				t.Errorf("%s: a model can be installed", plat)
			}
			if p, ok := mgr.Python(BackendFasterWhisper); ok || p != "" {
				t.Errorf("%s: an interpreter is active", plat)
			}
		}
		mgr.Close()
	}
}

func readLock(t *testing.T, name string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(builtinManifest), "locks", name))
	if err != nil {
		return "", errors.Join(errors.New(name), err)
	}
	return string(b), nil
}

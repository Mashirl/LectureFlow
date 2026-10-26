// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realnet

package components

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealNetGcloud installs the real gcloud component (Google's archive
// from dl.google.com; on macOS also the python component it runs on) with
// the shipped manifest, runs "gcloud --version" and
// "gcloud auth application-default login --help" the way the application
// does (ToolPath, ToolBaseEnv + ToolEnv, its own CLOUDSDK_CONFIG), and
// logs the installed size and the longest path below the version directory
// (the manifest's installed_size and max_path). Not part of the normal
// suite (build tag realnet; it downloads about 105 MB on Windows, 55 MB
// plus the python component on macOS):
//
//	go test -tags realnet -run TestRealNetGcloud -v ./internal/components/
//
// LF_REALNET_DATA keeps the data directory; LF_REALNET_UV is the uv for the
// python component on macOS (default tools/uv); LF_REALNET_UNINSTALL=1
// uninstalls gcloud at the end and checks that nothing is left.
func TestRealNetGcloud(t *testing.T) {
	manifest, _ := filepath.Abs(builtinManifest)
	data := os.Getenv("LF_REALNET_DATA")
	if data == "" {
		data = t.TempDir()
	}
	uv := os.Getenv("LF_REALNET_UV")
	if uv == "" {
		uv, _ = filepath.Abs(filepath.Join("..", "..", "tools", "uv", "uv"+exeSuffix))
	}
	m, err := New(Options{DataDir: data, AppVersion: "3.1.0", BuiltinManifest: manifest, UV: uv})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := m.manifest().Component(IDGcloud)
	if c == nil || c.Build(m.Platform()) == nil {
		t.Fatalf("no gcloud build for %s", m.Platform())
	}
	p, err := m.Plan([]string{IDGcloud})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		t.Logf("plan item %s %s dependency=%v installed=%v download %d B installed %d B", it.ID, it.Version, it.Dependency, it.Installed, it.DownloadBytes, it.InstalledBytes)
	}
	t.Logf("plan: download %d MB, installed %d MB, need %d MB, free %d MB", mb(p.DownloadBytes), mb(p.InstalledBytes), mb(p.RequiredBytes), mb(p.FreeBytes))
	start := time.Now()
	id, err := m.Install([]string{IDGcloud}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	var st TaskStatus
	for {
		st, _ = m.Task(id)
		if st.State == TaskDone || st.State == TaskFailed || st.State == TaskCancelled {
			break
		}
		time.Sleep(2 * time.Second)
		phase := ""
		for _, it := range st.Items {
			phase += it.ID + ":" + it.Phase + " "
		}
		t.Logf("%s %s%d/%d MB %d KB/s", st.State, phase, mb(st.Downloaded), mb(st.Total), st.SpeedBps>>10)
	}
	if st.State != TaskDone {
		t.Fatalf("%s %+v notices %+v", st.State, st.Error, st.Notices)
	}
	t.Logf("installed in %v; notices %+v", time.Since(start).Round(time.Second), st.Notices)

	exe, ok := m.ToolPath(IDGcloud)
	if !ok {
		t.Fatal("ToolPath: not installed")
	}
	env := append(ToolBaseEnv(os.Environ()), m.ToolEnv(IDGcloud)...)
	env = append(env, "CLOUDSDK_CONFIG="+t.TempDir())
	t.Logf("gcloud: %s; its variables %v", exe, m.ToolEnv(IDGcloud))
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(exe, args...)
		cmd.Env = env
		cmd.Dir = os.TempDir()
		hideWindow(cmd)
		began := time.Now()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %v: %v\n%s", args, err, out)
		}
		t.Logf("gcloud %s (%v):\n%s", strings.Join(args, " "), time.Since(began).Round(time.Millisecond), firstLines(string(out), 12))
		return string(out)
	}
	if out := run("--version"); !strings.Contains(out, "Google Cloud SDK "+c.Revision) {
		t.Errorf("gcloud --version does not say %s", c.Revision)
	}
	if out := run("auth", "application-default", "login", "--help"); !strings.Contains(out, "application-default login") {
		t.Error("gcloud auth application-default login --help printed no help")
	}
	// The sign-in itself up to its link (the code path the Colab login
	// runs, with what the Windows build leaves out): --no-launch-browser
	// prints the link and asks for a code, which an empty standard input
	// never gives, so nothing signs in.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(ctx, exe, "auth", "application-default", "login", "--no-launch-browser",
			"--scopes=openid,https://www.googleapis.com/auth/cloud-platform,https://www.googleapis.com/auth/userinfo.email,https://www.googleapis.com/auth/colaboratory")
		cmd.Env = env
		cmd.Dir = os.TempDir()
		cmd.Stdin = strings.NewReader("")
		cmd.WaitDelay = 5 * time.Second
		hideWindow(cmd)
		out, err := cmd.CombinedOutput()
		cancel()
		t.Logf("gcloud auth application-default login --no-launch-browser (exit %v):\n%s", err, firstLines(string(out), 8))
		if !strings.Contains(string(out), "https://accounts.google.com/") {
			t.Error("the sign-in did not get as far as its link")
		}
		if strings.Contains(string(out), "Traceback") || strings.Contains(string(out), "ModuleNotFoundError") {
			t.Error("the sign-in failed in Python")
		}
	}

	e := m.st.Active[IDGcloud]
	ver := m.abs(e.Dir)
	size, longest, longestLoadable, deepest := measureTree(ver)
	t.Logf("installed: %d bytes (receipt %d); longest path below %s: %d (%s); longest .py/.pyd/.dll/.exe/.cmd: %d",
		size, e.SizeBytes, e.Dir, longest, deepest, longestLoadable)
	t.Logf("data dir %s (%d characters): deepest absolute path %d characters", data, PathLen(data), PathLen(data)+1+PathLen("components/"+e.Dir+"/")+longest)
	if b := c.Build(m.Platform()); b.MaxPath > 0 && longest > b.MaxPath {
		t.Errorf("the longest path %d exceeds the manifest's max_path %d", longest, b.MaxPath)
	}
	for _, x := range c.Build(m.Platform()).Files[0].Exclude {
		if _, err := os.Stat(filepath.Join(ver, "tool", filepath.FromSlash(strings.TrimSuffix(x, "/")))); err == nil {
			t.Errorf("excluded %s was unpacked", x)
		}
	}
	for _, s := range m.List() {
		if s.Installed {
			t.Logf("%-10s %-8s %6d MB broken=%v depends=%v", s.ID, s.InstalledVersion, mb(s.DiskUsage), s.Broken, s.Depends)
			if s.Broken {
				t.Errorf("%s is broken", s.ID)
			}
		}
	}
	if os.Getenv("LF_REALNET_UNINSTALL") == "1" {
		if err := m.Uninstall(IDGcloud); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(ver); err == nil {
			t.Errorf("%s still exists", ver)
		}
		if _, ok := m.ToolPath(IDGcloud); ok {
			t.Error("ToolPath after uninstalling")
		}
	}
}

// measureTree returns the bytes of the regular files under dir and the
// longest file path relative to it (all files; files Python or Windows
// load), with the deepest one.
func measureTree(dir string) (size int64, longest, loadable int, deepest string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if fi, err := d.Info(); err == nil && d.Type().IsRegular() {
			size += fi.Size()
		}
		n := PathLen(rel)
		if n > longest {
			longest, deepest = n, rel
		}
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".py", ".pyd", ".dll", ".exe", ".cmd":
			loadable = max(loadable, n)
		}
		return nil
	})
	return
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

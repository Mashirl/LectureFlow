// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// How the host starts the app (mac-fix): on macOS an app bundle through
// open(1), elsewhere the program itself. The command is recorded, not run.

type launchRec struct {
	cmd  *exec.Cmd
	wait bool
}

func recordLaunch(rec *launchRec) func(*exec.Cmd, bool) error {
	return func(c *exec.Cmd, wait bool) error {
		rec.cmd, rec.wait = c, wait
		return nil
	}
}

// macApp makes dir/LectureFlow.app/Contents/MacOS/LectureFlow and returns
// its path with forward slashes (the macOS form; Windows accepts it too).
func macApp(t *testing.T) (exe, bundle string) {
	t.Helper()
	bundle = filepath.ToSlash(filepath.Join(t.TempDir(), "LectureFlow.app"))
	exe = bundle + "/Contents/MacOS/LectureFlow"
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, bundle
}

func TestStartAppMacOpensTheBundle(t *testing.T) {
	exe, bundle := macApp(t)
	var rec launchRec
	if err := startApp("darwin", LaunchConfig{Exe: exe, Args: []string{BackgroundArg}}, recordLaunch(&rec)); err != nil {
		t.Fatal(err)
	}
	want := []string{openTool, "-n", "-g", "-a", bundle, "--args", BackgroundArg}
	if rec.cmd == nil || rec.cmd.Path != openTool || !slices.Equal(rec.cmd.Args, want) || !rec.wait {
		t.Fatalf("command %v (wait %v), want %v", rec.cmd, rec.wait, want)
	}
	// Without arguments there is no --args.
	if err := startApp("darwin", LaunchConfig{Exe: exe}, recordLaunch(&rec)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.cmd.Args, []string{openTool, "-n", "-g", "-a", bundle}) {
		t.Fatalf("command %v", rec.cmd.Args)
	}
}

func TestStartAppRunsTheProgramOtherwise(t *testing.T) {
	exe, _ := macApp(t)
	plain := filepath.Join(t.TempDir(), "lectureflow")
	os.WriteFile(plain, []byte("x"), 0o755)
	for _, c := range []struct{ goos, exe string }{
		{"windows", exe},  // the bundle layout means nothing on Windows
		{"darwin", plain}, // not inside an app bundle
		{"linux", exe},
	} {
		var rec launchRec
		if err := startApp(c.goos, LaunchConfig{Exe: c.exe, Args: []string{BackgroundArg}}, recordLaunch(&rec)); err != nil {
			t.Fatal(err)
		}
		if rec.cmd.Path != c.exe || !slices.Equal(rec.cmd.Args, []string{c.exe, BackgroundArg}) || rec.wait || rec.cmd.Dir != filepath.Dir(c.exe) {
			t.Errorf("%s %s: %v %v dir %s", c.goos, c.exe, rec.cmd.Args, rec.wait, rec.cmd.Dir)
		}
	}
}

func TestStartAppMissingProgram(t *testing.T) {
	ran := false
	err := startApp("darwin", LaunchConfig{Exe: filepath.ToSlash(filepath.Join(t.TempDir(), "Gone.app", "Contents", "MacOS", "Gone"))},
		func(*exec.Cmd, bool) error { ran = true; return nil })
	if !errors.Is(err, os.ErrNotExist) || ran {
		t.Fatalf("err %v, ran %v", err, ran)
	}
}

func TestMacAppBundle(t *testing.T) {
	for exe, want := range map[string]string{
		"/Applications/LectureFlow.app/Contents/MacOS/LectureFlow": "/Applications/LectureFlow.app",
		"/Users/u/Applications/LF.app/Contents/MacOS/Electron":     "/Users/u/Applications/LF.app",
		"/Applications/LectureFlow.app/Contents/Resources/x":       "",
		"/opt/lectureflow/lectureflow":                             "",
		"/Contents/MacOS/LectureFlow":                              "",
		"/.app/Contents/MacOS/LectureFlow":                         "",
	} {
		if got := macAppBundle("darwin", exe); got != want {
			t.Errorf("%s: %q, want %q", exe, got, want)
		}
	}
	if macAppBundle("windows", "/Applications/LectureFlow.app/Contents/MacOS/LectureFlow") != "" {
		t.Error("a bundle on Windows")
	}
}

// runLaunch with wait reports open(1)'s exit status, and gives up waiting
// (as started) after openWait. The commands are this test binary.
func TestRunLaunchWaitsForTheExitStatus(t *testing.T) {
	if err := runLaunch(exec.Command(os.Args[0], "-test.run=^$"), true); err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	if err := runLaunch(exec.Command(os.Args[0], "-test.no-such-flag"), true); err == nil {
		t.Fatal("a failing command counts as started")
	}

	old := openWait
	openWait = 200 * time.Millisecond
	t.Cleanup(func() { openWait = old })
	// The fake app waits for a connection that never comes.
	slow := exec.Command(os.Args[0])
	slow.Env = append(os.Environ(), fakeAppEnv+"=1", EnvEndpoint+"="+testEndpointFor(t), EnvDir+"="+t.TempDir())
	t0 := time.Now()
	if err := runLaunch(slow, true); err != nil {
		t.Fatalf("still running: %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("waited %v", d)
	}
	slow.Process.Kill()
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// LaunchFile is the name of launch.json in ConfigDir: how the host starts
// LectureFlow. Written by the desktop app (-launcher); the web mode writes
// none, so its host only answers app.not_running.
const LaunchFile = "launch.json"

// BackgroundArg starts the desktop app without a window (tray / Dock only).
const BackgroundArg = "--background"

// LaunchConfig is the content of launch.json.
type LaunchConfig struct {
	Exe  string   `json:"exe"`
	Args []string `json:"args"`
}

// ReadLaunch reads dir/launch.json; an error wrapping os.ErrNotExist when
// there is none.
func ReadLaunch(dir string) (LaunchConfig, error) {
	var lc LaunchConfig
	b, err := os.ReadFile(filepath.Join(dir, LaunchFile))
	if err != nil {
		return lc, err
	}
	if err := json.Unmarshal(b, &lc); err != nil {
		return lc, fmt.Errorf("nmhost: %s: %w", LaunchFile, err)
	}
	if lc.Exe == "" {
		return lc, fmt.Errorf("nmhost: %s names no program", LaunchFile)
	}
	return lc, nil
}

// writeIfChanged writes b to path through a temporary file and a rename,
// unless the file already holds exactly b. It reports whether it wrote.
func writeIfChanged(path string, b []byte, perm os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, perm)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
		return false, werr
	}
	return true, nil
}

func marshalFile(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

// StartApp starts LectureFlow as launch.json says, detached from the host:
// the browser ends the host (on Windows by closing its job object) when the
// extension's port closes, and the app must outlive it.
//
// On macOS the executable of an application bundle (X.app/Contents/MacOS/X)
// is opened through LaunchServices instead of being executed:
//
//	/usr/bin/open -n -g -a X.app --args ARGS
//
// An executed app inherits the browser as its TCC "responsible process":
// a data directory on an external volume or in Documents, Desktop or
// Downloads would be asked for, and granted, in the browser's name. An app
// LaunchServices starts is responsible for itself. -g leaves it in the
// background (with --background it shows no window). -n starts a new
// instance even when LectureFlow already runs (for example still starting,
// or started a moment ago by the host of another port): without it open
// only activates the running app, which drops --args and makes Electron
// show the window ("activate"); with it the new instance loses the
// single-instance lock and quits, and the running one gets
// "second-instance" with --background and stays hidden, as on Windows.
// Either way the host then retries the endpoint as before.
func StartApp(lc LaunchConfig) error {
	return startApp(runtime.GOOS, lc, runLaunch)
}

// openTool is macOS's open(1).
const openTool = "/usr/bin/open"

// openWait bounds the wait for open(1)'s exit status (it exits once
// LaunchServices has started the app, normally well within a second).
var openWait = 10 * time.Second

// startApp is StartApp on goos; run starts the command (runLaunch; tests
// record it), waiting for its exit status when wait is set (open(1)).
func startApp(goos string, lc LaunchConfig, run func(cmd *exec.Cmd, wait bool) error) error {
	if _, err := os.Stat(lc.Exe); err != nil {
		return err
	}
	if bundle := macAppBundle(goos, lc.Exe); bundle != "" {
		return run(exec.Command(openTool, openArgs(bundle, lc.Args)...), true)
	}
	cmd := exec.Command(lc.Exe, lc.Args...)
	cmd.Dir = filepath.Dir(lc.Exe)
	return run(cmd, false)
}

// openArgs are open(1)'s arguments that start bundle with args.
func openArgs(bundle string, args []string) []string {
	out := []string{"-n", "-g", "-a", bundle}
	if len(args) > 0 {
		out = append(append(out, "--args"), args...)
	}
	return out
}

// macAppBundle is the application bundle whose executable exe is
// (X.app/Contents/MacOS/X) on macOS, else "".
func macAppBundle(goos, exe string) string {
	if goos != "darwin" {
		return ""
	}
	p := filepath.ToSlash(exe)
	macos := path.Dir(p)
	contents := path.Dir(macos)
	bundle := path.Dir(contents)
	if path.Base(macos) != "MacOS" || path.Base(contents) != "Contents" ||
		!strings.HasSuffix(bundle, ".app") || path.Base(bundle) == ".app" {
		return ""
	}
	return bundle
}

// runLaunch starts cmd detached. With wait (open(1)) it waits up to
// openWait for the exit status, so that a launch LaunchServices refused is
// reported (start_failed); an open(1) still running by then (a system
// dialog) counts as started and the host keeps retrying.
func runLaunch(cmd *exec.Cmd, wait bool) error {
	if err := startDetached(cmd); err != nil {
		return err
	}
	if !wait {
		return cmd.Process.Release()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(cmd.Path), err)
		}
		return nil
	case <-time.After(openWait):
		return nil
	}
}

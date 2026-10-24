// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// v3 step 5 (v5-integration-a): the staged import is applied before the
// database is opened (SPEC-v3 V1.3, internal/relocate.ApplyStaged).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/relocate"
)

func withApplyStaged(t *testing.T, f func(context.Context, string) error) {
	t.Helper()
	prev := applyStaged
	applyStaged = f
	t.Cleanup(func() { applyStaged = prev })
}

func TestApplyStagedDefaultIsRelocate(t *testing.T) {
	if applyStaged == nil {
		t.Fatal("applyStaged is not wired")
	}
	// No staging area: nothing to do.
	if err := runApplyStaged(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// A failed import whose data directory was restored: logged, the start goes on.
func TestRunApplyStagedRestoredStarts(t *testing.T) {
	called := make(chan string, 1)
	withApplyStaged(t, func(_ context.Context, dir string) error {
		called <- dir
		return &relocate.ApplyError{Restored: true, Err: errors.New("boom")}
	})
	cfgPath, dataDir := writeTestConfig(t)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	p.ready()
	if got := <-called; filepath.Clean(got) != filepath.Clean(dataDir) {
		t.Fatalf("applyStaged got %q, want %q", got, dataDir)
	}
	p.sigc <- os.Interrupt
	if c := p.wait(10 * time.Second); c != exitOK {
		t.Fatalf("exit code %d", c)
	}
}

// Not restored, or any other error: exit code 1, "ERROR: ..." last, no ready line.
func TestRunApplyStagedFailureStops(t *testing.T) {
	for name, e := range map[string]error{
		"not_restored": &relocate.ApplyError{Restored: false, Err: errors.New("undo failed")},
		"other":        errors.New("disk on fire"),
	} {
		t.Run(name, func(t *testing.T) {
			withApplyStaged(t, func(context.Context, string) error { return e })
			cfgPath, _ := writeTestConfig(t)
			p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
			if c := p.wait(10 * time.Second); c != exitFailure {
				t.Fatalf("exit code %d", c)
			}
			lines := strings.Split(strings.TrimRight(p.stderr.String(), "\n"), "\n")
			if last := lines[len(lines)-1]; !strings.HasPrefix(last, "ERROR: ") || !strings.Contains(last, "staged data") {
				t.Fatalf("stderr last line %q", last)
			}
			if strings.Contains(p.stdout(), ReadyPrefix) {
				t.Fatal("ready line after a failed start")
			}
		})
	}
}

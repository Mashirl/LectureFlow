// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package colab

import (
	"os/exec"
	"sync"
	"syscall"
)

// procTree is gcloud's process tree: gcloud runs in a process group of its
// own, and stopping it kills the whole group (the bin/gcloud script and the
// Python it runs). The browser gcloud opens is started by the system
// (macOS: LaunchServices through open / osascript), outside the group.
type procTree struct {
	mu  sync.Mutex
	pid int
}

func newProcTree(string) *procTree { return &procTree{} }

func (t *procTree) prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (t *procTree) started(cmd *exec.Cmd) error {
	t.mu.Lock()
	t.pid = cmd.Process.Pid
	t.mu.Unlock()
	return nil
}

// kill sends SIGKILL to the group (a group that is gone already is fine).
func (t *procTree) kill() {
	t.mu.Lock()
	pid := t.pid
	t.mu.Unlock()
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func (t *procTree) close() {}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package procutil

import "os/exec"

// Process is a child started by Start.
type Process struct{}

// Start starts cmd (cmd.Start). The memory limit is not enforced on this
// system (see the package comment): the worker applies it itself.
// EndWithParent (and a memory limit, as on Windows) sets the parent death
// signal where the system has one (endWithParent).
func Start(cmd *exec.Cmd, l Limits) (*Process, error) {
	if l.EndWithParent || l.MemoryBytes > 0 {
		endWithParent(cmd)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Process{}, nil
}

// Limited reports whether the limits are enforced by the OS for this child.
func (p *Process) Limited() bool { return false }

// Usage is zero on this system.
func (p *Process) Usage() Usage { return Usage{} }

// Close releases what Start set up (nothing here).
func (p *Process) Close() error { return nil }

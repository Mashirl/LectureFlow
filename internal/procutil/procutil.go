// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package procutil starts a child process under OS resource limits
// (security review 2, M2: the PDF text extraction worker, which parses
// untrusted files) and ties a child's life to this process (test-chaos 1:
// an ffmpeg that outlived a crashed or force-killed LectureFlow kept its
// .tmp output open, and the next start could not remove it).
//
// Windows: the child is created suspended, put into a new Job Object with
// KILL_ON_JOB_CLOSE and DIE_ON_UNHANDLED_EXCEPTION (plus
// JOB_OBJECT_LIMIT_PROCESS_MEMORY for a memory cap) and only then resumed,
// so none of its code runs outside the job; an allocation over the cap
// fails in the child. The job's only handle is this process's: when this
// process ends, however it ends, the system closes it and ends the child.
//
// Other systems: no memory limit is enforced here; the worker limits itself
// (worker/extract_pdf.py: RLIMIT_AS on Linux, a peak-RSS watchdog on macOS,
// whose kernel does not enforce RLIMIT_AS). EndWithParent is the parent
// death signal on Linux (SIGKILL); macOS has no such mechanism, so a child
// there can outlive a crash (an open file never blocks its removal on that
// system, so nothing waits for it).
package procutil

import "os/exec"

// Limits are the OS limits of one child process; zero = no limit.
type Limits struct {
	// MemoryBytes caps the child's committed (private) memory (Windows).
	// A child with a memory cap also ends with this process
	// (EndWithParent is implied on Windows).
	MemoryBytes uint64
	// EndWithParent ends the child when this process ends, also by a crash
	// or a forced kill (Windows: the job object; Linux: the parent death
	// signal; macOS: not available).
	EndWithParent bool
}

// Tied are the limits of a helper program (ffmpeg, ffprobe) whose work
// only matters to this process: no cap, but it never outlives it.
var Tied = Limits{EndWithParent: true}

// Run is cmd.Run for a child started with Start(cmd, l): it starts it,
// waits for it and releases what Start set up.
func Run(cmd *exec.Cmd, l Limits) error {
	p, err := Start(cmd, l)
	if err != nil {
		return err
	}
	defer p.Close()
	return cmd.Wait()
}

// Usage is what the job observed of the child; read it after cmd.Wait.
// Zero values where nothing is known (no job, other systems).
type Usage struct {
	// PeakMemory is the child's peak committed memory in bytes.
	PeakMemory uint64
	// MemoryLimitHit: an allocation of the child went over
	// Limits.MemoryBytes (the job's JOB_OBJECT_MSG_PROCESS_MEMORY_LIMIT
	// notification). The child usually fails then (MemoryError, an
	// out-of-memory abort), but it may also have swallowed the error.
	MemoryLimitHit bool
}

// MemoryExceeded reports whether a child with these limits ran out of its
// memory cap: the job said so, or a failed child (failed: non-zero exit)
// peaked within 10% of the cap (an allocation that would have crossed it
// failed; the notification is not guaranteed to arrive).
func (u Usage) MemoryExceeded(l Limits, failed bool) bool {
	if l.MemoryBytes == 0 {
		return false
	}
	return u.MemoryLimitHit || failed && u.PeakMemory >= l.MemoryBytes/10*9
}

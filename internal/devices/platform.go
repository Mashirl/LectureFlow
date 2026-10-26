// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"runtime"

	"lectureflow/internal/model"
)

// mac-opti M5 (user decision 2026-10-26: no CPU fallback on the Mac):
// fw:cpu, faster-whisper on the CPU, exists only on systems where a
// faster-whisper environment can exist. There is none for macOS (the
// asr-cuda component is windows-x64 only, README: "CPU 只做桌面电脑的兜底"),
// so on darwin the registry neither creates fw:cpu nor shows a row an
// earlier version made: List and Get leave it out, Present is false, a
// launch never names it and the scheduler never binds it. Without the MLX
// component the device table is then empty and the front end says that
// nothing can transcribe yet. The row itself stays in the database (the
// Store has no delete; it is harmless and comes back if the data directory
// moves to a Windows machine).
//
// Test mode (asr.fake_result: no worker runs at all, the registry already
// fakes an NVIDIA GPU) keeps the rules of the other systems on every host,
// so the end-to-end tests behave the same on a Mac.

// GOOS is the operating system whose device rules apply, runtime.GOOS. A
// registry takes it when it is made (NewRegistry); tests of other packages
// pin it so their expectations hold on every host.
var GOOS = runtime.GOOS

// FWCPUSupported reports whether goos can run faster-whisper on the CPU
// (has fw:cpu).
func FWCPUSupported(goos string) bool { return goos != "darwin" }

// fwCPU reports whether this registry has fw:cpu.
func (r *Registry) fwCPU() bool { return FWCPUSupported(r.goos) || r.testMode() }

// hiddenLocked reports whether List and Get leave the row out on this
// system (fw:cpu where it cannot run). colab:remote is hidden differently:
// List hides it while Colab is not enabled, Get still returns it (remote.go).
func (r *Registry) hiddenLocked(id string) bool {
	return id == model.DeviceFWCPU && !r.fwCPU()
}

// hiddenIDs are the ids of rows List leaves out now: a colab:remote that
// is not present and fw:cpu where it cannot run (Reorder keeps them at the
// end; caller holds wmu, so r.remote does not change).
func (r *Registry) hiddenIDs(rows []model.ASRDevice) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, d := range rows {
		if (isRemoteID(d.ID) && !r.remote) || r.hiddenLocked(d.ID) {
			out = append(out, d.ID)
		}
	}
	return out
}

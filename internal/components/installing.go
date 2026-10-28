// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

// Unfinished installs as the scheduler sees them (fresh-cuda; NOTES
// "决定（fresh-cuda：core-builder，2026-10-27）"; devices.ComponentTasks):
// a local device whose environment or model is missing takes no lecture,
// and its waiting_for tells "being installed" from "not installed".

// PythonInstalling reports whether an unfinished task (queued, running or
// paused) installs or updates the environment of backend ("base",
// "faster-whisper", "openvino", "mlx"): its venv component is an item of
// the task that is not done yet.
func (m *Manager) PythonInstalling(backend string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.venvComponent(backend)
	if id == "" {
		return false
	}
	_, busy := m.busyIDs()[id]
	return busy
}

// ModelInstalling reports whether an unfinished task installs or updates
// the model: model:{backend}:{name}:{precision}, or for faster-whisper any
// precision of name (ModelDir serves any installed precision of a
// faster-whisper model).
func (m *Manager) ModelInstalling(backend, name, precision string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.busyIDs() {
		b, n, p, ok := ParseModelID(id)
		if ok && b == backend && n == name && (p == precision || backend == BackendFasterWhisper) {
			return true
		}
	}
	return false
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package secrets

// ResetForTest forgets the data key, the fallback keys and the status, as
// in a process that never ran Setup. Tests only (other packages' tests set
// keys and must leave none behind).
func ResetForTest() {
	mu.Lock()
	aead, fallbacks = nil, nil
	mu.Unlock()
	SetStatusForTest(Status{})
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows && !darwin

package hwdetect

import "runtime"

// Other platforms (development only): OS and architecture, nothing else.
func platformOptions() Options {
	return Options{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

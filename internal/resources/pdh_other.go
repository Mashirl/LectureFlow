// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package resources

import "errors"

// newEngineReader: the PDH counters exist on Windows only; elsewhere the
// Intel device cards show "--" (I10).
func newEngineReader() (engineReader, error) {
	return nil, errors.New("GPU engine counters are only available on Windows")
}

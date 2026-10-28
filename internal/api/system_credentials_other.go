// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows && !darwin && !linux

package api

// fsType is not known here: no FAT / exFAT warning.
func fsType(string) (string, bool) { return "", false }

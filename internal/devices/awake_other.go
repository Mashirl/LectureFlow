// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package devices

// Outside Windows there is no power request: the keep-awake does nothing.

func platformAwakeCreate(string) (uintptr, []uint16, error) { return 0, nil, nil }
func platformAwakeSet(uintptr) error                        { return nil }
func platformAwakeClear(uintptr) error                      { return nil }
func platformAwakeClose(uintptr) error                      { return nil }

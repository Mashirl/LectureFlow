// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !darwin

package devices

import "context"

// detectMLX: MLX exists on macOS only.
func detectMLX(context.Context, string) (string, bool) { return "", false }

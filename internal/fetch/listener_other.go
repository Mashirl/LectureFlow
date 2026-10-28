// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows && !darwin && !linux

package fetch

import "errors"

// rpcListener: no way to tell who listens on a port here, so aria2c is not
// trusted with the secret (downloads fail with downloads.engine_failed).
func rpcListener(int, int) (listenerState, int, error) {
	return listenerNone, 0, errors.New("the listener of a port cannot be looked up on this system")
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build darwin

package fetch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// lsofPath is the system's lsof (part of macOS; an absolute path, so PATH
// cannot replace it).
const lsofPath = "/usr/sbin/lsof"

// rpcListener asks lsof for pid's TCP listeners on port (an unprivileged
// user sees the sockets of their own processes, aria2c's among them; not
// those of other accounts, which is why parseLsof reads "[::1] only" as
// taken). The application is built without cgo, and libproc's socket
// structures would have to be laid out by hand; lsof is in every macOS.
func rpcListener(port, pid int) (listenerState, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, lsofPath, "-nP", "-w", "-a", "-p", strconv.Itoa(pid),
		"-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fn")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil && len(out) == 0 {
			return listenerNone, 0, nil // lsof exits 1 when nothing matches
		}
		return listenerNone, 0, fmt.Errorf("lsof: %w", err)
	}
	return parseLsof(out, port), 0, nil
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command lectureflow-nmhost is the browser extension's Native Messaging
// host (SPEC-ext "Native Messaging 宿主", .claude/ext-brief.md §4).
//
// The browser starts it when the extension connects and talks to it over
// stdin / stdout (4-byte length + JSON frames). It relays those frames,
// unparsed, to the running LectureFlow over the local IPC endpoint (a named
// pipe on Windows, a Unix socket on macOS; LECTUREFLOW_NM_ENDPOINT
// overrides), and exits when either side closes. When LectureFlow is not
// running it starts it as {UserConfigDir}/LectureFlow/nmhost/launch.json
// says (LECTUREFLOW_NM_DIR overrides) and retries for 20 seconds; failing
// that it answers each request with app.not_running.
//
// The browser's arguments (the caller's origin, --parent-window=…) are
// ignored. No log file; at most one line on stderr, without any URL.
package main

import (
	"fmt"
	"os"

	"lectureflow/internal/nmhost"
)

func main() {
	os.Exit(run())
}

func run() int {
	endpoint, err := nmhost.ResolveEndpoint("", os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lectureflow-nmhost: no IPC endpoint:", err)
		endpoint = ""
	}
	dir, err := nmhost.ConfigDir(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lectureflow-nmhost: no configuration directory:", err)
	}
	h := &nmhost.Host{
		In:        os.Stdin,
		Out:       os.Stdout,
		Err:       os.Stderr,
		Endpoint:  endpoint,
		ConfigDir: dir,
	}
	return h.Run()
}

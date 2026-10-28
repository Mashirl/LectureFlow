// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command lf-agent runs on a user's SSH inference server for LectureFlow
// (internal/agent). Build: CGO_ENABLED=0 go build -trimpath
// -ldflags "-s -w -X lectureflow/internal/agent.Version=X.Y.Z" ./cmd/lf-agent
package main

import (
	"os"

	"lectureflow/internal/agent"
)

func main() {
	os.Exit(agent.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

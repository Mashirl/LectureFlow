// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package procutil

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runChild(mode string) int {
	if mode == "hello" {
		fmt.Println("hello")
		return 0
	}
	return 3
}

// On this system Start is cmd.Start: the child runs, nothing is reported.
func TestStartRunsTheChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), childEnv+"=hello")
	var out strings.Builder
	cmd.Stdout = &out
	p, err := Start(cmd, Limits{MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := cmd.Wait(); err != nil || strings.TrimSpace(out.String()) != "hello" {
		t.Fatalf("%v: %q", err, out.String())
	}
	if p.Limited() || p.Usage() != (Usage{}) {
		t.Errorf("limited %v, usage %+v", p.Limited(), p.Usage())
	}
}

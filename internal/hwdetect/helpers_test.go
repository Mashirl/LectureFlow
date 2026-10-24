// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package hwdetect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// checkGolden compares v (as indented JSON) with testdata/<name>; -update
// rewrites the file.
func checkGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/hwdetect/ -update to create it)", err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update after checking the change)\n--- got\n%s", name, got)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var errNotFound = errors.New(`exec: "x": executable file not found in %PATH%`)

// fakeCmd is one canned command result.
type fakeCmd struct {
	out []byte
	err error
}

// fakeRunner answers by the command line ("name arg1 arg2"; the PowerShell
// script is shortened to "powershell.exe"). Unknown commands fail like a
// missing executable. calls records every command line.
type fakeRunner struct {
	mu    sync.Mutex
	cmds  map[string]fakeCmd
	calls []string
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := name
	if name != "powershell.exe" && len(args) > 0 {
		key += " " + strings.Join(args, " ")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	c, ok := f.cmds[key]
	if !ok {
		return nil, errNotFound
	}
	return c.out, c.err
}

const smiKey = "nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader,nounits"

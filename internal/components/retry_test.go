// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// uvCalls reads the fake uv's call log.
func uvCalls(t *testing.T, log string) [][]string {
	t.Helper()
	data, _ := os.ReadFile(log)
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatal(err)
		}
		out = append(out, args)
	}
	return out
}

// hashFailures makes the next n uv pip sync calls report a hash mismatch.
func hashFailures(t *testing.T, n int) {
	p := filepath.Join(t.TempDir(), "hashfail")
	if err := os.WriteFile(p, []byte(strconv.Itoa(n)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LF_FAKE_UV_HASH_FAIL", p)
}

func syncIndexes(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		if len(c) > 1 && c[0] == "pip" && c[1] == "sync" {
			out = append(out, flagValue(c, "--index-url"))
		}
	}
	return out
}

func cacheCleans(calls [][]string) [][]string {
	var out [][]string
	for _, c := range calls {
		if len(c) > 1 && c[0] == "cache" && c[1] == "clean" {
			out = append(out, c)
		}
	}
	return out
}

// A wheel with the wrong hash on the only index: uv's cache entry of that
// package is cleared and the same index is tried once more.
func TestUVHashMismatchRetriedOnce(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	log := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("LF_FAKE_UV_LOG", log)
	hashFailures(t, 1)
	mustInstall(t, m, Sources{}, IDASRCUDA)
	calls := uvCalls(t, log)
	idx := syncIndexes(calls)
	if len(idx) != 2 || idx[0] != idx[1] || !strings.Contains(idx[0], "/default/") {
		t.Errorf("sync calls on %v, want the default index twice", idx)
	}
	cc := cacheCleans(calls)
	if len(cc) != 1 || cc[0][len(cc[0])-1] != "pkg-a" {
		t.Errorf("cache clean calls %v", cc)
	}
	if _, ok := m.Python(BackendFasterWhisper); !ok {
		t.Error("not installed")
	}
}

// Twice the wrong hash on the only index: the install fails with
// components.checksum after exactly two attempts, nothing is installed.
func TestUVHashMismatchTwiceFails(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	log := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("LF_FAKE_UV_LOG", log)
	hashFailures(t, 2)
	id, err := m.Install([]string{IDASRCUDA}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskFailed || st.Error.Code != CodeChecksum || !strings.Contains(st.Error.Detail, "Hash mismatch") {
		t.Fatalf("%s %+v", st.State, st.Error)
	}
	calls := uvCalls(t, log)
	if idx := syncIndexes(calls); len(idx) != 2 {
		t.Errorf("sync calls %v, want 2", idx)
	}
	if cc := cacheCleans(calls); len(cc) != 1 {
		t.Errorf("cache clean calls %v", cc)
	}
	if s, _ := statusOf(m, IDASRCUDA); s.Installed {
		t.Error("installed after two hash mismatches")
	}
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if strings.HasPrefix(f, "asr-cuda/") {
			t.Errorf("left behind: %s", f)
		}
	}
}

// On a mirror: two mismatches there, then the default index (which works).
func TestUVHashMismatchOnMirrorFallsBack(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	log := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("LF_FAKE_UV_LOG", log)
	hashFailures(t, 2)
	st := mustInstall(t, m, Sources{PyPI: "mirror"}, IDASRCUDA)
	idx := syncIndexes(uvCalls(t, log))
	if len(idx) != 3 || !strings.Contains(idx[0], "/mirror/") || idx[0] != idx[1] || !strings.Contains(idx[2], "/default/") {
		t.Errorf("sync calls on %v", idx)
	}
	if len(st.Notices) != 1 || st.Notices[0].Code != CodeMirrorFallback || st.Notices[0].Params["reason"] != CodeChecksum {
		t.Errorf("notices %+v", st.Notices)
	}
}

// The lock file comes from its URL (not next to the manifest): a damaged
// answer is fetched once more from the same URL.
func TestLockChecksumRetriedOnce(t *testing.T) {
	e := newTestEnv(t)
	lockPath := "/default/files/locks/asr-cuda-windows-x64.txt"
	os.Remove(filepath.Join(e.manDir, "locks", "asr-cuda-windows-x64.txt"))
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	e.srv.mu.Lock()
	e.srv.corrupt[lockPath] = 1
	e.srv.mu.Unlock()
	mustInstall(t, m, Sources{}, IDASRCUDA)
	if n := e.srv.count(lockPath); n != 2 {
		t.Errorf("%d lock requests, want 2", n)
	}
}

func TestLockChecksumTwiceFails(t *testing.T) {
	e := newTestEnv(t)
	lockPath := "/default/files/locks/asr-cuda-windows-x64.txt"
	os.Remove(filepath.Join(e.manDir, "locks", "asr-cuda-windows-x64.txt"))
	m := e.open()
	mustInstall(t, m, Sources{}, IDPython)
	e.srv.mu.Lock()
	e.srv.corrupt[lockPath] = 2
	e.srv.mu.Unlock()
	id, err := m.Install([]string{IDASRCUDA}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed)
	if st.State != TaskFailed || st.Error.Code != CodeChecksum {
		t.Fatalf("%s %+v", st.State, st.Error)
	}
	if n := e.srv.count(lockPath); n != 2 {
		t.Errorf("%d lock requests, want 2", n)
	}
	if s, _ := statusOf(m, IDASRCUDA); s.Installed {
		t.Error("installed without a verified lock file")
	}
}

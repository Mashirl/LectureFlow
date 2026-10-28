// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The data directory's instance lock (test-upgrade F4): one service per
// data directory. Startup recovery (§4.7) re-queues every running job as
// interrupted; a second service on the same directory would run them a
// second time, and an older one (3.1.0) deleted the credentials the newer
// one had sealed. The desktop shell's single-instance lock covers two
// shells only, not web mode, a portable next to an installed copy, or a
// service left behind.
//
// {data_dir}/lectureflow.lock is held with an exclusive lock for the life
// of the service: LockFileEx on Windows (a one-byte range far beyond the
// file's content, so copying or reading the file is never refused),
// flock(2) elsewhere. The system releases it when the process ends,
// however it ends. The file holds the holder's process id, read by the
// instance that is refused. It is never deleted (deleting a lock file lets
// two processes lock two different files).
//
// Not taken: -selfupdate-verify (it returns before the data directory is
// looked at), the browser extension's native host (another program, it
// never opens the data directory: it relays to the running service), the
// desktop shell (its own lock). An import reads another data directory
// read-only and does not lock it.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LockFileName is the instance lock in the data directory.
const LockFileName = "lectureflow.lock"

// lockWait: how long a start waits for the lock. The system releases the
// lock of a process that has ended, but on Windows not always at once: a
// restart (exit code 3) must not fail on its predecessor's lock.
var lockWait = 5 * time.Second

// errLockHeld: another open file holds the lock (lockFile).
var errLockHeld = errors.New("the lock is held")

// instanceLock is the held lock; Release ends it (the system does when the
// process ends).
type instanceLock struct{ f *os.File }

// acquireInstanceLock takes the instance lock of dataDir (which exists),
// waiting up to lockWait. Held by another process: a *startFailure
// startup.data_dir_in_use naming its process id when known. A file system
// that has no locks (some network shares) is logged and the start goes on
// without one; nil is returned then.
func acquireInstanceLock(dataDir string) (*instanceLock, error) {
	path := filepath.Join(dataDir, LockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("instance lock %s: %w", path, err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		err = lockFile(f)
		if err == nil {
			break
		}
		if !errors.Is(err, errLockHeld) {
			f.Close()
			slog.Warn("instance lock: the file system of the data directory does not lock files; another LectureFlow on this directory would not be noticed",
				"file", path, "err", err)
			return nil, nil
		}
		if !time.Now().Before(deadline) {
			pid := readLockPID(path)
			f.Close()
			return nil, inUseFailure(dataDir, pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// the holder's process id, for the instance that is refused
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &instanceLock{f: f}, nil
}

// Release ends the lock (nil-safe, once).
func (l *instanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	l.f.Close()
	l.f = nil
}

// readLockPID reads the process id the holder wrote; 0 when unknown.
func readLockPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// inUseFailure is the refusal of a start on a data directory another
// LectureFlow uses.
func inUseFailure(dataDir string, pid int) *startFailure {
	dataDir = filepath.Clean(dataDir)
	params := map[string]any{"data_dir": dataDir}
	who, whoEn := "另一个 LectureFlow", "Another LectureFlow"
	if pid > 0 {
		params["pid"] = pid
		who, whoEn = fmt.Sprintf("另一个 LectureFlow（进程 %d）", pid), fmt.Sprintf("Another LectureFlow (process %d)", pid)
	}
	return &startFailure{
		Code:   failDataDirInUse,
		Params: params,
		Msg: fmt.Sprintf("%s正在使用这个数据目录：%s。请先关闭它再启动。 / %s is using this data directory: %s. Close it first.",
			who, dataDir, whoEn, dataDir),
	}
}

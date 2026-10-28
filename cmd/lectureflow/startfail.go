// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Start failures the user can act on, and the lines that tell the desktop
// shell about a long start (NOTES "决定（升级前备份与降级保护：core-builder，
// 2026-10-27）"). The command-line contract stays (v3-brief §1): the last
// line of stderr is "ERROR: <reason>" (here in Chinese and English, for
// web mode); in desktop mode a coded failure adds, just before it,
//
//	LECTUREFLOW_FAILURE {"code":"startup.…","params":{…}}
//
// and the shell shows the code's text in the interface language
// (desktop/locales "startfail.*"), the ERROR line under "Details". These
// codes are the shell's only: the web page never sees them, so they are
// not in testdata/api/error_codes_backend.json.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// FailurePrefix starts the stderr line naming a coded start failure.
const FailurePrefix = "LECTUREFLOW_FAILURE "

// StartingPrefix starts the stdout lines printed, in desktop mode, while a
// long step runs before the ready line: "LECTUREFLOW_STARTING <step>",
// one second into the step and then every heartbeatEvery. The shell keeps
// waiting while they come (its start timeout counts from the last one)
// and shows that the data is being upgraded (test-upgrade F3: a large
// database's first start took longer than the shell's 30 seconds).
const StartingPrefix = "LECTUREFLOW_STARTING "

// Start failure codes.
const (
	failDataDirInUse        = "startup.data_dir_in_use"         // params: data_dir, pid (when known)
	failDataDirNewer        = "startup.data_dir_newer"          // params: found, known, backups
	failUpgradeBackupSpace  = "startup.upgrade_backup_no_space" // params: dir, need_bytes, free_bytes
	failUpgradeBackupFailed = "startup.upgrade_backup_failed"   // params: dir
)

// Steps named on the StartingPrefix lines.
const (
	stepCheck         = "check-database"
	stepUpgradeBackup = "upgrade-backup"
	stepMigrate       = "migrate"
	stepSeal          = "seal-credentials"
	stepScrubBackups  = "scrub-import-backups"
)

// startFailure is a start failure with a code (see above).
type startFailure struct {
	Code   string
	Params map[string]any
	Msg    string // the ERROR line: Chinese / English
	Err    error  // the cause, if any
}

func (e *startFailure) Error() string { return e.Msg }
func (e *startFailure) Unwrap() error { return e.Err }

// failureLine is the FailurePrefix line of e.
func failureLine(e *startFailure) string {
	b, err := json.Marshal(struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}{e.Code, e.Params})
	if err != nil {
		b = []byte(fmt.Sprintf(`{"code":%q,"params":{}}`, e.Code))
	}
	return FailurePrefix + string(b)
}

// heartbeatFirst: the first line of a step comes this far into it (a step
// done sooner prints none; 0 = at once); then one every heartbeatEvery.
var (
	heartbeatFirst = time.Second
	heartbeatEvery = 5 * time.Second
)

// progress prints the StartingPrefix lines to w (nil: none, web mode).
type progress struct {
	w  io.Writer
	mu sync.Mutex
}

// step starts reporting the long step name; end stops it (no line after
// end returns) and logs how long the step took when that was a second or
// more. Nil-safe.
func (p *progress) step(name string) (end func()) {
	t0 := time.Now()
	done := make(chan struct{})
	var wg sync.WaitGroup
	if p != nil && p.w != nil {
		beat := func() {
			p.mu.Lock()
			fmt.Fprintln(p.w, StartingPrefix+name)
			p.mu.Unlock()
		}
		first := heartbeatFirst
		if first <= 0 {
			beat()
			first = heartbeatEvery
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTimer(first)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					beat()
					t.Reset(heartbeatEvery)
				}
			}
		}()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			if d := time.Since(t0); d >= time.Second {
				slog.Info("startup: step finished", "step", name, "took", d.Round(100*time.Millisecond).String())
			}
		})
	}
}

// slowTimeout bounds a startup step that reads or writes about n bytes of
// database (a copy, a VACUUM): generous, it only stops a step that hangs;
// the shell waits as long as the heartbeat comes. A fixed bound would fail
// a large database's step at every start, and it would never finish.
func slowTimeout(n int64) time.Duration {
	if n < 0 {
		n = 0
	}
	return 10*time.Minute + time.Duration(n>>20)*time.Second // + 1 s per MiB
}

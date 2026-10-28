// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package colab

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"lectureflow/internal/colab/loginfake"
)

const stillActive = 259 // STILL_ACTIVE

func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// Stopping gcloud's tree ends gcloud's own processes but not a program
// gcloud started from outside the Cloud SDK — the browser it opens through
// the shell is a member of the job as well and must survive.
func TestLoginCancelSparesTheBrowser(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeBrowser)
	e.withChild()
	browser := copyTestBinary(t, filepath.Join(e.dir, "Program Files", "Browser"), "browser")
	e.env = append(e.env, loginfake.EnvBrowser+"="+browser)
	l := b.Login(e.options())
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	bpid := waitPID(t, e.out, "browser")
	t.Cleanup(func() {
		// Gone before the temporary directory is removed: the browser's
		// working directory is the sign-in directory (gcloud's, which it
		// inherited), and a process that is still exiting keeps it in use
		// (TempDir RemoveAll failed on a loaded machine, 2026-10-28).
		if p, err := os.FindProcess(bpid); err == nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	})
	pid := waitPID(t, e.out, "python")
	if st := l.Cancel(10 * time.Second); st.State != LoginCancelled {
		t.Fatalf("after cancel %+v", st)
	}
	waitGone(t, pid, "gcloud's python")
	time.Sleep(300 * time.Millisecond)
	if !processAlive(bpid) {
		t.Fatal("the browser gcloud started was stopped with gcloud")
	}
}

func TestOwnedImage(t *testing.T) {
	tr := &procTree{sdkRoot: `C:\Users\u\AppData\Local\LectureFlow\components\gcloud\google-cloud-sdk`,
		sysDirs: []string{`C:\Windows\System32`, `C:\Windows\SysWOW64`}}
	for img, want := range map[string]bool{
		`C:\Users\u\AppData\Local\LectureFlow\components\gcloud\google-cloud-sdk\platform\bundledpython\python.exe`: true,
		`c:\windows\system32\cmd.exe`:                                                         true,
		`C:\Windows\System32\conhost.exe`:                                                     true,
		`C:\Python313\python.exe`:                                                             true,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`:                               false,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`:                        false,
		`C:\Windows\explorer.exe`:                                                             false,
		`C:\Users\u\AppData\Local\LectureFlow\components\gcloud\google-cloud-sdk-other\x.exe`: false,
	} {
		if got := tr.ownedImage(img); got != want {
			t.Errorf("ownedImage(%s) = %v", img, got)
		}
	}
}

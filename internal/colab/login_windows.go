// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package colab

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// procTree is gcloud's process tree on Windows: gcloud.cmd (cmd.exe) and the
// Python it starts. gcloud is created suspended, put into a job object of
// its own and only then resumed, so every process it starts (even one whose
// parent already ended) is a member of the job. Stopping the tree
// terminates the members that are part of gcloud — executables inside the
// Cloud SDK directory, the Windows system directories (cmd.exe, conhost.exe)
// and Python interpreters — and leaves every other member alone: a browser
// that gcloud started through the shell is a member of the job too, and it
// must survive (it may hold the user's other tabs). The job is not
// kill-on-close for the same reason.
type procTree struct {
	mu      sync.Mutex
	job     syscall.Handle // 0 = no job (creation failed): only gcloud itself is stopped
	proc    *os.Process
	sdkRoot string // the Cloud SDK directory: the parent of gcloud.cmd's bin directory
	sysDirs []string
}

var (
	kernel32Login                 = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW          = kernel32Login.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject  = kernel32Login.NewProc("AssignProcessToJobObject")
	procQueryInformationJobObject = kernel32Login.NewProc("QueryInformationJobObject")
	procQueryFullProcessImageName = kernel32Login.NewProc("QueryFullProcessImageNameW")
	procNtResumeProcess           = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
)

const (
	createSuspended          = 0x00000004
	createNoWindow           = 0x08000000
	processTerminate         = 0x0001
	processSetQuota          = 0x0100
	processSuspendResume     = 0x0800
	processQueryLimitedInfo  = 0x1000
	jobObjectBasicProcessIDs = 3 // JobObjectBasicProcessIdList
)

func newProcTree(gcloud string) *procTree {
	t := &procTree{}
	if abs, err := filepath.Abs(gcloud); err == nil {
		t.sdkRoot = filepath.Dir(filepath.Dir(abs)) // …\google-cloud-sdk\bin\gcloud.cmd → …\google-cloud-sdk
	}
	if root := os.Getenv("SystemRoot"); root != "" {
		t.sysDirs = []string{filepath.Join(root, "System32"), filepath.Join(root, "SysWOW64")}
	}
	if h, _, err := procCreateJobObjectW.Call(0, 0); h != 0 {
		t.job = syscall.Handle(h)
	} else {
		slog.Warn("colab: no job object for gcloud; only gcloud itself can be stopped", "err", err)
	}
	return t
}

func (t *procTree) prepare(cmd *exec.Cmd) {
	flags := uint32(createNoWindow)
	if t.job != 0 {
		flags |= createSuspended // resumed in started, once it is in the job
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
}

// started puts the suspended gcloud into the job and resumes it.
func (t *procTree) started(cmd *exec.Cmd) error {
	t.mu.Lock()
	t.proc = cmd.Process
	job := t.job
	t.mu.Unlock()
	if job == 0 {
		return nil
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate|processSuspendResume, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("open gcloud's process: %w", err)
	}
	defer syscall.CloseHandle(h)
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h)); r == 0 {
		// Still runs (resumed below); stopping it then reaches gcloud only.
		slog.Warn("colab: gcloud could not be put into its job object", "err", err)
		t.mu.Lock()
		syscall.CloseHandle(t.job)
		t.job = 0
		t.mu.Unlock()
	}
	if st, _, _ := procNtResumeProcess.Call(uintptr(h)); st != 0 {
		return fmt.Errorf("resume gcloud: NTSTATUS 0x%x", st)
	}
	return nil
}

// kill terminates the job's gcloud members and gcloud itself.
func (t *procTree) kill() {
	t.mu.Lock()
	job, proc := t.job, t.proc
	t.mu.Unlock()
	if job != 0 {
		for _, pid := range jobPIDs(job) {
			t.terminate(pid)
		}
	}
	if proc != nil {
		_ = proc.Kill()
	}
}

func (t *procTree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		syscall.CloseHandle(t.job)
		t.job = 0
	}
}

// terminate ends pid when its executable belongs to gcloud (ownedImage).
func (t *procTree) terminate(pid uint32) {
	h, err := syscall.OpenProcess(processTerminate|processQueryLimitedInfo, false, pid)
	if err != nil {
		return // ended already
	}
	defer syscall.CloseHandle(h)
	img, err := imagePath(h)
	if err != nil || !t.ownedImage(img) {
		return
	}
	_ = syscall.TerminateProcess(h, 1)
}

// ownedImage: the executables gcloud itself runs.
func (t *procTree) ownedImage(img string) bool {
	if t.sdkRoot != "" && insideDir(img, t.sdkRoot) {
		return true
	}
	for _, d := range t.sysDirs {
		if insideDir(img, d) {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(img))
	return strings.HasPrefix(base, "python") && strings.HasSuffix(base, ".exe")
}

// insideDir compares case-insensitively (Windows paths).
func insideDir(p, dir string) bool {
	p, dir = strings.ToLower(filepath.Clean(p)), strings.ToLower(filepath.Clean(dir))
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func imagePath(h syscall.Handle) (string, error) {
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if r, _, err := procQueryFullProcessImageName.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return "", err
	}
	return syscall.UTF16ToString(buf[:n]), nil
}

// jobPIDs lists the job's processes (JOBOBJECT_BASIC_PROCESS_ID_LIST).
func jobPIDs(job syscall.Handle) []uint32 {
	var list struct {
		Assigned uint32
		InList   uint32
		IDs      [256]uintptr
	}
	if r, _, _ := procQueryInformationJobObject.Call(uintptr(job), jobObjectBasicProcessIDs,
		uintptr(unsafe.Pointer(&list)), unsafe.Sizeof(list), 0); r == 0 && list.InList == 0 {
		return nil
	}
	n := min(int(list.InList), len(list.IDs))
	out := make([]uint32, 0, n)
	for _, id := range list.IDs[:n] {
		out = append(out, uint32(id))
	}
	return out
}

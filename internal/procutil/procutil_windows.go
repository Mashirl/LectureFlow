// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package procutil

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job notification messages (winnt.h; not in x/sys/windows).
const (
	jobMsgActiveProcessZero  = 4 // JOB_OBJECT_MSG_ACTIVE_PROCESS_ZERO
	jobMsgProcessMemoryLimit = 9 // JOB_OBJECT_MSG_PROCESS_MEMORY_LIMIT
)

// jobAssociateCompletionPort is JOBOBJECT_ASSOCIATE_COMPLETION_PORT.
type jobAssociateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// Process is a child started by Start.
type Process struct {
	mu    sync.Mutex
	job   windows.Handle // 0 = no job: the child runs without the limits
	port  windows.Handle // 0 = no notifications (the peak still tells)
	usage *Usage         // read once, after the child exited
}

// Start starts cmd (like cmd.Start) inside l. With a memory limit or
// EndWithParent the child is created suspended (CREATE_SUSPENDED is added
// to cmd.SysProcAttr), put into a new Job Object and only then resumed:
// there is no moment in which the child runs outside the job. Without
// either, cmd.Start is all.
//
// If the job cannot be created or the child cannot be put into it (both
// unexpected on Windows 8 and later, which allow nested jobs), the child
// still runs, without the cap and without being tied to this process, and
// a warning is logged: the worker's own limits (decode budget, timeout)
// still apply. If the suspended child cannot be resumed it is killed and
// Start fails; a child that was killed before it could be resumed (its
// context ended) is not a failed start: Wait reports its end.
func Start(cmd *exec.Cmd, l Limits) (*Process, error) {
	p := &Process{}
	if l.MemoryBytes == 0 && !l.EndWithParent {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return p, nil
	}
	if err := p.newJob(l); err != nil {
		slog.Warn("procutil: no job object; the child runs without its limits", "err", err)
		p.Close()
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return p, nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		p.Close()
		return nil, err
	}
	pid := uint32(cmd.Process.Pid)
	if err := p.assign(pid); err != nil {
		if !ended(pid, 0) {
			slog.Warn("procutil: the child could not be put into its job object; it runs without its limits",
				"pid", pid, "err", err)
		}
		p.Close()
	}
	if err := resumeThreads(pid); err != nil {
		if ended(pid, 2*time.Second) {
			// Killed while it was still suspended: exec.CommandContext kills
			// the child as soon as its context is done (a time limit that
			// passed while a loaded machine was creating the process). As
			// with cmd.Start for a child that ends at once, Start succeeds
			// and Wait reports the end, so that the caller sees its context
			// error (a timeout, a cancellation) and not a failed start.
			return p, nil
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		p.Close()
		return nil, fmt.Errorf("resume the child process: %w", err)
	}
	return p, nil
}

// ended reports whether process pid has ended, waiting up to wait for it
// (its threads may be gone a moment before the process is).
func ended(pid uint32, wait time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false // exec.Cmd holds a handle until Wait: the process object exists
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(wait.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

func (p *Process) newJob(l Limits) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	p.job = job
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// KILL_ON_JOB_CLOSE: the child does not outlive this process (the job
	// handle is not inheritable, so this process holds the only one and the
	// system closes it when the process ends, however it ends);
	// DIE_ON_UNHANDLED_EXCEPTION: a crash (an allocation that failed where it
	// cannot be handled) ends the child instead of showing an error dialog.
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION
	if l.MemoryBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = uintptr(l.MemoryBytes)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("set the job's limits: %w", err)
	}
	if l.MemoryBytes == 0 {
		return nil // no cap: no notifications to read (Usage reads the peak only)
	}
	// The notifications say when an allocation went over the cap. Optional:
	// without them the peak memory still tells (Usage.MemoryExceeded).
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		slog.Debug("procutil: no completion port for the job's notifications", "err", err)
		return nil
	}
	assoc := jobAssociateCompletionPort{CompletionKey: uintptr(job), CompletionPort: port}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc))); err != nil {
		slog.Debug("procutil: the job's notifications are not available", "err", err)
		windows.CloseHandle(port)
		return nil
	}
	p.port = port
	return nil
}

func (p *Process) assign(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(p.job, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// resumeThreads resumes the threads of the (suspended) process pid: its
// initial thread. The pid cannot have been reused, since exec.Cmd holds a
// handle to the process until Wait.
//
// The threads are listed first (threadsOf) and then opened one by one, so
// a listed thread can end in between: a short-lived thread that another
// program (an antivirus) started in the new process. OpenThread then fails
// with ERROR_INVALID_PARAMETER, and before this was handled the whole start
// failed ("resume the child process: OpenThread …: The parameter is
// incorrect", flake-hunt race B). Such a thread is skipped, and so is one
// whose id has meanwhile been reused by a thread of another process (it is
// never resumed). The initial thread cannot end while it is suspended, so
// resuming fails only when no thread of pid is left (the process was killed)
// or a thread of pid cannot be opened or resumed for another reason.
//
// (Not NtResumeProcess, which resumes every thread of the process in one
// call without listing them: it is not a documented API.)
func resumeThreads(pid uint32) error {
	tids, err := threadsOf(pid)
	if err != nil {
		return err
	}
	n := 0
	for _, tid := range tids {
		ok, err := resumeThread(pid, tid)
		if err != nil {
			return err
		}
		if ok {
			n++
		}
	}
	if n == 0 {
		return errors.New("the process has no thread (it has already ended)")
	}
	return nil
}

// threadsOf lists the ids of the threads of process pid (a variable: tests
// add ids of threads that are gone or belong to another process).
var threadsOf = listThreads

// listThreads lists the ids of the threads of process pid from a snapshot
// of the system's threads.
func listThreads(pid uint32) ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	var tids []uint32
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID == pid {
			tids = append(tids, te.ThreadID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("Thread32Next: %w", err)
	}
	return tids, nil
}

// resumeThread resumes thread tid of process pid. false (no error): the
// thread has ended since it was listed (ERROR_INVALID_PARAMETER: no thread
// has this id), or the id now names a thread of another process.
func resumeThread(pid, tid uint32) (bool, error) {
	th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME|windows.THREAD_QUERY_LIMITED_INFORMATION, false, tid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("OpenThread %d: %w", tid, err)
	}
	defer windows.CloseHandle(th)
	owner, err := processIDOfThread(th)
	if err != nil {
		return false, fmt.Errorf("GetProcessIdOfThread %d: %w", tid, err)
	}
	if owner != pid {
		return false, nil
	}
	if _, err := windows.ResumeThread(th); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return false, fmt.Errorf("ResumeThread %d: %w", tid, err)
	}
	return true, nil
}

var procGetProcessIdOfThread = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIdOfThread")

// processIDOfThread is GetProcessIdOfThread (not in x/sys/windows): the id
// of the process the thread belongs to.
func processIDOfThread(th windows.Handle) (uint32, error) {
	r, _, err := procGetProcessIdOfThread.Call(uintptr(th))
	if r == 0 {
		return 0, err
	}
	return uint32(r), nil
}

// Limited reports whether the limits are enforced for this child (it is in
// its job: its memory cap, and it ends with this process).
func (p *Process) Limited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.job != 0
}

// Usage is what the job saw of the child. Call it after cmd.Wait: it reads
// the job's notifications until the job reports that no process is left
// (at most 1 s).
func (p *Process) Usage() Usage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.usage != nil {
		return *p.usage
	}
	if p.job == 0 {
		return Usage{}
	}
	u := Usage{MemoryLimitHit: p.drain()}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(p.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err == nil {
		u.PeakMemory = uint64(info.PeakProcessMemoryUsed)
	}
	p.usage = &u
	return u
}

// drain reads the job's notifications; true when one of them was the
// memory limit.
func (p *Process) drain() bool {
	if p.port == 0 {
		return false
	}
	hit := false
	deadline := time.Now().Add(time.Second)
	for {
		wait := time.Until(deadline)
		if wait < 0 {
			wait = 0
		}
		var (
			msg uint32
			key uintptr
			ov  *windows.Overlapped // for job notifications: the process id, not a pointer
		)
		if err := windows.GetQueuedCompletionStatus(p.port, &msg, &key, &ov, uint32(wait.Milliseconds())); err != nil {
			return hit // WAIT_TIMEOUT: nothing more within the second
		}
		switch msg {
		case jobMsgProcessMemoryLimit:
			hit = true
		case jobMsgActiveProcessZero:
			return hit
		}
	}
}

// Close releases the job. A child still in it is killed
// (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE).
func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var err error
	if p.port != 0 {
		err = windows.CloseHandle(p.port)
		p.port = 0
	}
	if p.job != 0 {
		if cerr := windows.CloseHandle(p.job); err == nil {
			err = cerr
		}
		p.job = 0
	}
	return err
}

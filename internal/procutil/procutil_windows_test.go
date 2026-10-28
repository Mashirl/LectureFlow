// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package procutil

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childJobLimit is the per-process memory limit of the job the calling
// process is in (0: none, or not in a job).
func childJobLimit() uint64 {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY == 0 {
		return 0
	}
	return uint64(info.ProcessMemoryLimit)
}

// runChild is the child's side (TestMain): "limit_alloc" prints the job's
// memory limit as its very first action ("limit N"), then allocates
// PROCUTIL_TEST_ALLOC_MB MiB and prints "allocated"; "sleep" prints "ready"
// and sleeps.
func runChild(mode string) int {
	switch mode {
	case "limit_alloc":
		fmt.Printf("limit %d\n", childJobLimit())
		mb, _ := strconv.Atoi(os.Getenv("PROCUTIL_TEST_ALLOC_MB"))
		b := make([]byte, mb<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		fmt.Println("allocated", len(b))
		return 0
	case "sleep":
		fmt.Println("ready")
		time.Sleep(30 * time.Second)
		return 0
	case "spawn":
		// A LectureFlow stand-in: one helper started Tied, one without
		// limits; both pids printed, then it waits to be killed.
		for _, c := range []struct {
			name string
			l    Limits
		}{{"tied", Tied}, {"free", Limits{}}} {
			exe, _ := os.Executable()
			cmd := exec.Command(exe)
			cmd.Env = append(os.Environ(), childEnv+"=sleep")
			if _, err := Start(cmd, c.l); err != nil {
				fmt.Println("error", err)
				return 1
			}
			fmt.Println(c.name, cmd.Process.Pid)
		}
		time.Sleep(30 * time.Second)
		return 0
	}
	return 3
}

// waitExit waits up to d for process pid to end; false when it still runs.
func waitExit(t *testing.T, pid int, d time.Duration) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // gone already
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
	if err != nil {
		t.Fatal(err)
	}
	return ev == windows.WAIT_OBJECT_0
}

func killPID(pid int) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		windows.TerminateProcess(h, 1)
		windows.CloseHandle(h)
	}
}

func childCmd(t *testing.T, mode string, allocMB int) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), childEnv+"="+mode, "PROCUTIL_TEST_ALLOC_MB="+strconv.Itoa(allocMB))
	return cmd
}

func output(t *testing.T, cmd *exec.Cmd) (*bufio.Scanner, func() []string) {
	t.Helper()
	r, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(r)
	return sc, func() []string {
		var lines []string
		for sc.Scan() {
			lines = append(lines, strings.TrimSpace(sc.Text()))
		}
		return lines
	}
}

// The child runs inside the job from its first instruction (it was created
// suspended), and an allocation over the cap fails: the job reports the
// limit and the peak stays under it.
func TestStartAppliesMemoryLimit(t *testing.T) {
	const limit = 128 << 20
	cmd := childCmd(t, "limit_alloc", 512)
	_, rest := output(t, cmd)
	p, err := Start(cmd, Limits{MemoryBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Limited() {
		t.Fatal("the child is not in its job")
	}
	lines := rest()
	werr := cmd.Wait()
	if len(lines) == 0 || lines[0] != fmt.Sprintf("limit %d", limit) {
		t.Fatalf("the child's first line %q, want limit %d (it ran before it was in the job?)", lines, limit)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "allocated") {
			t.Fatalf("512 MiB allocated under a 128 MiB cap: %q", lines)
		}
	}
	if werr == nil {
		t.Fatal("the child succeeded over its memory cap")
	}
	u := p.Usage()
	if !u.MemoryLimitHit {
		t.Errorf("no memory-limit notification: %+v", u)
	}
	if u.PeakMemory == 0 || u.PeakMemory > limit {
		t.Errorf("peak %d, cap %d", u.PeakMemory, limit)
	}
	if !u.MemoryExceeded(Limits{MemoryBytes: limit}, true) {
		t.Errorf("MemoryExceeded false for %+v", u)
	}
}

// Under the cap the child runs normally.
func TestStartUnderTheLimit(t *testing.T) {
	cmd := childCmd(t, "limit_alloc", 32)
	_, rest := output(t, cmd)
	p, err := Start(cmd, Limits{MemoryBytes: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	lines := rest()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %q", err, lines)
	}
	if len(lines) != 2 || lines[1] != fmt.Sprintf("allocated %d", 32<<20) {
		t.Fatalf("output %q", lines)
	}
	u := p.Usage()
	if u.MemoryLimitHit || u.MemoryExceeded(Limits{MemoryBytes: 256 << 20}, false) {
		t.Errorf("usage %+v", u)
	}
	if u.PeakMemory < 32<<20 {
		t.Errorf("peak %d below the 32 MiB the child allocated", u.PeakMemory)
	}
}

// Without limits no job is created and nothing changes.
func TestStartWithoutLimits(t *testing.T) {
	cmd := childCmd(t, "limit_alloc", 1)
	_, rest := output(t, cmd)
	p, err := Start(cmd, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	lines := rest()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %q", err, lines)
	}
	if p.Limited() || p.Usage() != (Usage{}) {
		t.Errorf("limited %v, usage %+v", p.Limited(), p.Usage())
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED != 0 {
		t.Error("created suspended without a job")
	}
}

// Closing the job kills a child still in it (the child never outlives the
// application).
func TestCloseKillsTheChild(t *testing.T) {
	cmd := childCmd(t, "sleep", 0)
	sc, _ := output(t, cmd)
	p, err := Start(cmd, Limits{MemoryBytes: 256 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Scan() || sc.Text() != "ready" {
		t.Fatalf("child: %q", sc.Text())
	}
	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done: // terminated by the job (exit code 0: TerminateJobObject's)
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the child survived closing its job")
	}
	if d := time.Since(start); d > 5*time.Second { // it sleeps 30 s by itself
		t.Errorf("took %v", d)
	}
}

// A start failure leaves nothing behind.
func TestStartFailure(t *testing.T) {
	cmd := exec.Command(`C:\does\not\exist\python.exe`)
	if _, err := Start(cmd, Limits{MemoryBytes: 64 << 20}); err == nil {
		t.Fatal("started a missing program")
	}
}

// Tied (EndWithParent without a memory cap): the child is in a job of its
// own without a memory limit, and runs normally.
func TestStartTiedWithoutCap(t *testing.T) {
	cmd := childCmd(t, "limit_alloc", 64)
	_, rest := output(t, cmd)
	p, err := Start(cmd, Tied)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Limited() {
		t.Fatal("the tied child is not in its job")
	}
	lines := rest()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %q", err, lines)
	}
	if len(lines) != 2 || lines[0] != "limit 0" || lines[1] != fmt.Sprintf("allocated %d", 64<<20) {
		t.Fatalf("output %q (want no memory limit, 64 MiB allocated)", lines)
	}
	if u := p.Usage(); u.MemoryLimitHit || u.MemoryExceeded(Tied, true) {
		t.Errorf("usage %+v", u)
	}
}

// Run is Start + Wait + Close.
func TestRunTied(t *testing.T) {
	cmd := childCmd(t, "limit_alloc", 1)
	var out strings.Builder
	cmd.Stdout = &out
	if err := Run(cmd, Tied); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "limit 0") {
		t.Fatalf("output %q", out.String())
	}
	if err := Run(exec.Command(`C:\does\not\exist\ffmpeg.exe`), Tied); err == nil {
		t.Fatal("ran a missing program")
	}
}

// test-chaos 1: a Tied child ends when the process that started it is
// killed (TerminateProcess, what taskkill /F and a crash amount to); a child
// started without limits outlives it, as ffmpeg did before.
func TestTiedChildEndsWithItsParent(t *testing.T) {
	parent := childCmd(t, "spawn", 0)
	sc, _ := output(t, parent)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	pids := map[string]int{}
	for len(pids) < 2 && sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || f[0] == "error" {
			_ = parent.Process.Kill()
			t.Fatalf("parent: %q", sc.Text())
		}
		pid, _ := strconv.Atoi(f[1])
		pids[f[0]] = pid
	}
	t.Cleanup(func() { killPID(pids["tied"]); killPID(pids["free"]) })
	if pids["tied"] == 0 || pids["free"] == 0 {
		_ = parent.Process.Kill()
		t.Fatalf("pids %v", pids)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	if !waitExit(t, pids["tied"], 10*time.Second) {
		t.Fatal("the tied child survived its killed parent")
	}
	if waitExit(t, pids["free"], 500*time.Millisecond) {
		t.Fatal("control: the child without limits ended with its parent too (the test proves nothing)")
	}
}

// withThreads makes Start see the threads of its child as listThreads
// lists them, plus extra (first) for that one pid.
func withThreads(t *testing.T, extra func(pid uint32) []uint32) {
	t.Helper()
	t.Cleanup(func() { threadsOf = listThreads })
	threadsOf = func(pid uint32) ([]uint32, error) {
		tids, err := listThreads(pid)
		return append(extra(pid), tids...), err
	}
}

// goneThreadID returns the id of a thread that has ended and whose id no
// thread has now: OpenThread fails on it with ERROR_INVALID_PARAMETER.
func goneThreadID(t *testing.T) uint32 {
	t.Helper()
	for try := 0; try < 50; try++ {
		ch := make(chan uint32)
		go func() {
			// Never unlocked: the runtime ends the thread with the goroutine
			// (not the main thread, which it keeps: then the next try).
			runtime.LockOSThread()
			ch <- windows.GetCurrentThreadId()
		}()
		tid := <-ch
		for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			h, err := windows.OpenThread(windows.THREAD_QUERY_LIMITED_INFORMATION, false, tid)
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				return tid
			}
			if err != nil {
				t.Fatalf("OpenThread %d: %v", tid, err)
			}
			owner, _ := processIDOfThread(h)
			windows.CloseHandle(h)
			if owner != uint32(os.Getpid()) {
				break // the id names another process's thread already
			}
		}
	}
	t.Fatal("no thread id that stays unused")
	return 0
}

// flake-hunt race B: a thread of the child that is listed but has ended
// before it is opened (a short-lived thread an antivirus started in the
// new process) is skipped; the start does not fail. Here it comes first in
// the list, as a thread id that is gone.
func TestStartSkipsAThreadThatEnded(t *testing.T) {
	gone := goneThreadID(t)
	var child uint32
	withThreads(t, func(pid uint32) []uint32 {
		child = pid
		return []uint32{gone}
	})
	for _, l := range []Limits{Tied, {MemoryBytes: 256 << 20}} {
		cmd := childCmd(t, "limit_alloc", 1)
		_, rest := output(t, cmd)
		p, err := Start(cmd, l)
		if err != nil {
			t.Fatalf("%+v: %v", l, err)
		}
		lines := rest()
		werr := cmd.Wait()
		p.Close()
		if werr != nil || len(lines) != 2 || !strings.HasPrefix(lines[1], "allocated") {
			t.Fatalf("%+v: %v %q", l, werr, lines)
		}
		if child != uint32(cmd.Process.Pid) {
			t.Fatalf("the threads of %d were listed, the child is %d", child, cmd.Process.Pid)
		}
	}
}

var procSuspendThread = windows.NewLazySystemDLL("kernel32.dll").NewProc("SuspendThread")

// suspendCount reads a thread's suspend count (SuspendThread returns the
// previous count; ResumeThread undoes it).
func suspendCount(t *testing.T, tid uint32) uint32 {
	t.Helper()
	th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, tid)
	if err != nil {
		t.Fatalf("OpenThread %d: %v", tid, err)
	}
	defer windows.CloseHandle(th)
	r, _, err := procSuspendThread.Call(uintptr(th))
	if int32(r) == -1 {
		t.Fatalf("SuspendThread %d: %v", tid, err)
	}
	if _, err := windows.ResumeThread(th); err != nil {
		t.Fatalf("ResumeThread %d: %v", tid, err)
	}
	return uint32(r)
}

// A listed thread id that now names a thread of another process (the
// listed thread ended and its id was reused) is never resumed: here the
// threads of another suspended process keep their suspend counts.
func TestStartLeavesOtherProcessesThreads(t *testing.T) {
	other := childCmd(t, "sleep", 0)
	other.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	theirs, err := listThreads(uint32(other.Process.Pid))
	if err != nil || len(theirs) == 0 {
		t.Fatalf("threads of the other process: %v %v", theirs, err)
	}
	before := map[uint32]uint32{}
	suspended := false
	for _, tid := range theirs {
		before[tid] = suspendCount(t, tid)
		suspended = suspended || before[tid] > 0
	}
	if !suspended {
		t.Fatalf("the other process runs already: %v", before)
	}
	withThreads(t, func(uint32) []uint32 { return theirs })
	cmd := childCmd(t, "limit_alloc", 1)
	_, rest := output(t, cmd)
	p, err := Start(cmd, Tied)
	if err != nil {
		t.Fatal(err)
	}
	lines := rest()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %q", err, lines)
	}
	p.Close()
	for _, tid := range theirs {
		if n := suspendCount(t, tid); n != before[tid] {
			t.Fatalf("thread %d of the other process: suspend count %d, was %d (the child's start resumed it)", tid, n, before[tid])
		}
	}
}

// flake-hunt race B as it was seen (TestSyllabusPDFTimeout under load): the
// child's context ended while the child was still suspended, and
// exec.CommandContext killed it before Start resumed it. That is not a
// failed start: Start succeeds and Wait reports the end, so the caller sees
// its own context error (the PDF time limit) and not "无法启动".
func TestStartChildKilledBeforeResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), childEnv+"=sleep")
	withThreads(t, func(pid uint32) []uint32 {
		cancel() // the context ends now: exec kills the suspended child
		if !waitExit(t, int(pid), 10*time.Second) {
			t.Error("the child its context ended is still running")
		}
		return nil
	})
	p, err := Start(cmd, Tied)
	if err != nil {
		t.Fatalf("Start: %v (a child killed by its context is not a failed start)", err)
	}
	defer p.Close()
	if err := cmd.Wait(); err == nil {
		t.Fatal("Wait: nil for a killed child")
	}
}

// A live child none of whose threads can be resumed (here: only a gone
// thread id is listed) is killed, and Start fails.
func TestStartFailsWhenNothingIsResumed(t *testing.T) {
	gone := goneThreadID(t)
	threadsOf = func(uint32) ([]uint32, error) { return []uint32{gone}, nil }
	t.Cleanup(func() { threadsOf = listThreads })
	cmd := childCmd(t, "sleep", 0)
	if _, err := Start(cmd, Tied); err == nil || !strings.Contains(err.Error(), "resume the child process") {
		t.Fatalf("Start: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("the child was left behind: %v", cmd.ProcessState)
	}
}

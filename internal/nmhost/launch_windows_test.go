// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startDetached in a job that refuses breakaway (Chrome's, or a CI
// runner's) starts the app inside the job, as the same command: its
// environment and standard streams are kept. Before the fix the fallback
// dropped cmd.Env; this test binary, started that way by
// TestRunLaunchWaitsForTheExitStatus, then ran the whole test suite in the
// background instead of acting as the fake app (flake-hunt #9).
//
// This test binary plays two roles besides the tests (init below):
//
//	starter  (detachStarterEnv = output file) waits for a line on stdin
//	         (the test has put it in the job by then), starts the started
//	         role with startDetached, its Env carrying detachMarkEnv and its
//	         stdout going to the output file, and prints "started PID"
//	started  (detachMarkEnv set) prints "mark=VALUE" on its stdout and
//	         waits (the job's closing ends it)
const (
	detachStarterEnv = "LECTUREFLOW_NMHOST_TEST_DETACH_STARTER"
	detachMarkEnv    = "LECTUREFLOW_NMHOST_TEST_DETACH_MARK"
)

func init() {
	if mark := os.Getenv(detachMarkEnv); mark != "" {
		fmt.Printf("mark=%s\n", mark)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if out := os.Getenv(detachStarterEnv); out != "" {
		os.Exit(runDetachStarter(out))
	}
}

func runDetachStarter(out string) int {
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		fmt.Println("error: stdin:", err)
		return 3
	}
	f, err := os.Create(out)
	if err != nil {
		fmt.Println("error:", err)
		return 3
	}
	defer f.Close()
	// -test.run=^$: were the environment lost, the program would run no
	// test and print no mark (rather than the whole suite)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), detachStarterEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, detachMarkEnv+"=kept-through-the-fallback")
	cmd.Stdout = f
	if err := startDetached(cmd); err != nil {
		fmt.Println("error: startDetached:", err)
		return 1
	}
	fmt.Printf("started %d\n", cmd.Process.Pid)
	cmd.Process.Release()
	return 0
}

// noBreakawayJob is a job without JOB_OBJECT_LIMIT_BREAKAWAY_OK that ends
// its processes when the test closes it.
func noBreakawayJob(t *testing.T) windows.Handle {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		t.Fatal(err)
	}
	return job
}

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func isProcessInJob(process, job windows.Handle) (bool, error) {
	var in int32
	r, _, err := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&in)))
	if r == 0 {
		return false, err
	}
	return in != 0, nil
}

func TestStartDetachedInAJobWithoutBreakawayKeepsTheCommand(t *testing.T) {
	job := noBreakawayJob(t)
	closed := false
	closeJob := func() {
		if !closed {
			closed = true
			windows.CloseHandle(job) // ends what is still running in it
		}
	}
	t.Cleanup(closeJob)

	out := filepath.Join(t.TempDir(), "started-stdout.txt")
	starter := exec.Command(os.Args[0], "-test.run=^$")
	starter.Env = append(os.Environ(), detachStarterEnv+"="+out)
	stdin, err := starter.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := starter.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { starter.Process.Kill(); starter.Wait() })
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(starter.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	err = windows.AssignProcessToJobObject(job, h)
	windows.CloseHandle(h)
	if err != nil {
		t.Fatalf("put the starter in the job: %v", err)
	}
	if _, err := stdin.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if err := starter.Wait(); err != nil {
		t.Fatalf("starter: %v (%q)", err, line)
	}
	pidText, ok := strings.CutPrefix(strings.TrimSpace(line), "started ")
	pid, err := strconv.Atoi(pidText)
	if !ok || err != nil {
		t.Fatalf("starter said %q", line)
	}
	started, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("the started program (pid %d) is gone: %v", pid, err)
	}
	defer windows.CloseHandle(started)
	// ended also when the fallback was not taken (then it is outside the job)
	t.Cleanup(func() { windows.TerminateProcess(started, 1) })

	// its stdout is the file it was given, its environment the one it was given
	var got string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		b, _ := os.ReadFile(out)
		if got = string(b); strings.Contains(got, "\n") {
			break
		}
		if ev, _ := windows.WaitForSingleObject(started, 0); ev == windows.WAIT_OBJECT_0 {
			b, _ := os.ReadFile(out)
			got = string(b)
			break
		}
	}
	if strings.TrimSpace(got) != "mark=kept-through-the-fallback" {
		t.Fatalf("the started program printed %q to its stdout, want the mark from its environment", got)
	}
	in, err := isProcessInJob(started, job)
	if err != nil {
		t.Fatal(err)
	}
	if !in {
		t.Fatal("the program broke away from a job that does not allow it: the fallback was not tested")
	}
	closeJob()
	if ev, _ := windows.WaitForSingleObject(started, 10_000); ev != windows.WAIT_OBJECT_0 {
		t.Fatal("the started program outlived the job")
	}
}

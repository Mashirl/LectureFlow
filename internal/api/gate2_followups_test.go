// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// v3 Gate round 2 follow-ups: components.in_use_by_task is a 409, and the
// syllabus PDF extraction reports its interpreter while it runs.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// fakeExtractEnv makes the test binary act as the PDF extraction worker
// (TestMain): it creates {dir}/started, waits for {dir}/release and
// writes one page to the --out path.
const fakeExtractEnv = "LECTUREFLOW_TEST_FAKE_EXTRACT_PDF"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeExtractEnv); dir != "" {
		os.Exit(fakeExtractPDF(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeExtractPDF(dir string, args []string) int {
	out := ""
	for i, a := range args {
		if a == "--out" && i+1 < len(args) {
			out = args[i+1]
		}
	}
	if out == "" {
		return 2
	}
	if os.WriteFile(filepath.Join(dir, "started"), nil, 0o644) != nil {
		return 3
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return 4
		}
		time.Sleep(10 * time.Millisecond)
	}
	if os.WriteFile(out, []byte(`{"pages":["page one"]}`), 0o644) != nil {
		return 5
	}
	return 0
}

func TestCompErrInUseByTaskIsConflict(t *testing.T) {
	err := model.Errorf(components.CodeInUseByTask, map[string]any{"id": components.IDPython, "reason": "material"},
		errors.New("in use"))
	for _, pathID := range []bool{true, false} {
		var he *httpError
		if !errors.As(compErr(err, pathID), &he) || he.status != http.StatusConflict || he.info.Code != components.CodeInUseByTask {
			t.Errorf("pathID %v: %+v", pathID, he)
		}
	}
}

// TestComponentsUninstallInUseByTask: Options.InUse refuses the uninstall
// with 409 components.in_use_by_task {id, reason}; once the task is over
// it goes through.
func TestComponentsUninstallInUseByTask(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	ce := &compEnv{fs: newFileServer(t), data: t.TempDir()}
	ce.free.Store(1 << 40)
	manPath := writeTestManifest(t, ce)
	m, err := components.New(components.Options{
		DataDir: ce.data, AppVersion: "3.0.0", Platform: components.PlatformWindowsX64, BuiltinManifest: manPath,
		DiskFree:     func(string) (int64, error) { return ce.free.Load(), nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		RetryBackoff: 5 * time.Millisecond, StallTimeout: 5 * time.Second, EventInterval: time.Millisecond,
		InUse: func(id string) (bool, string) {
			if id == tcTiny && busy.Load() {
				return true, "asr"
			}
			return false, ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	ce.m = m
	ce.env = newEnvWith(t, envConfig{server: func(s *Server) {
		s.comps = m
		cs := s.compState()
		cs.region = func() string { return "" }
		cs.getenv = func(string) string { return "" }
	}})
	ce.waitTask(ce.install(tcTiny).ID, "done")
	info := ce.callErrInfo("DELETE", "/api/components/"+tcTiny, nil, http.StatusConflict)
	if info.Code != components.CodeInUseByTask || info.Params["id"] != tcTiny || info.Params["reason"] != "asr" {
		t.Fatalf("in use by a task: %+v", info)
	}
	busy.Store(false)
	ce.call("DELETE", "/api/components/"+tcTiny, nil, http.StatusNoContent, nil)
}

// pdfLocator installs the python component's base environment at exe.
type pdfLocator struct{ exe string }

func (l pdfLocator) Python(backend string) (string, bool) {
	if backend == config.ComponentBase {
		return l.exe, true
	}
	return "", false
}

func (pdfLocator) ModelDir(string, string, string) (string, bool) { return "", false }

// TestExtractPDFReportsRunningPython: while the syllabus extraction runs,
// RunningPythons reports the base environment's interpreter with reason
// material (and SelfCheckPythons does not); afterwards nothing.
func TestExtractPDFReportsRunningPython(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	signal := t.TempDir()
	t.Setenv(fakeExtractEnv, signal)
	e := newEnvWith(t, envConfig{server: func(s *Server) { s.cfg.Components = pdfLocator{exe: exe} }})
	work := t.TempDir()
	pdf := filepath.Join(work, "syllabus.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	type result struct {
		pages []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		pages, err := e.srv.runExtractPDF(context.Background(), work, pdf)
		done <- result{pages, err}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(signal, "started")); err == nil {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("the extraction ended before it started: %+v", r)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake extraction never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := RunningPython{Python: exe, Reason: PythonUseMaterial}
	if got := e.srv.RunningPythons(); !slices.Contains(got, want) {
		t.Errorf("RunningPythons during the extraction = %+v, want %+v", got, want)
	}
	if got := e.srv.SelfCheckPythons(); slices.Contains(got, exe) {
		t.Errorf("SelfCheckPythons reports the extraction: %v", got)
	}
	if err := os.WriteFile(filepath.Join(signal, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || !slices.Equal(r.pages, []string{"page one"}) {
		t.Fatalf("extraction: %+v", r)
	}
	for _, rp := range e.srv.RunningPythons() {
		if rp.Reason == PythonUseMaterial {
			t.Errorf("still reported after the extraction: %+v", rp)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package stages

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// tryCommit commits n bytes outside the Go heap; false when the system (the
// job's memory cap) refuses.
func tryCommit(n uintptr) bool {
	p, err := windows.VirtualAlloc(0, n, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		return false
	}
	_ = windows.VirtualFree(p, 0, windows.MEM_RELEASE)
	return true
}

// The extraction process runs in a Job Object with the memory cap: a worker
// that allocates past it dies, and the run fails with pdf.memory_limit
// {limit_bytes}, not retried.
func TestExtractPDFMemoryLimitJobObject(t *testing.T) {
	e, pdf := pdfEnv(t, "pdf_alloc")
	setPDFLimits(t, pdfTimeoutBase, pdfTimeoutPerPage, 128<<20)
	_, err := e.deps(nil).extractPDF(e.ctx, e.root, pdf, nil)
	if !sched.IsPermanent(err) || codeOf(err) != CodePDFMemoryLimit {
		t.Fatalf("err %v (code %q)", err, codeOf(err))
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Params["limit_bytes"] != int64(128<<20) {
		t.Errorf("params %+v", ce.Info.Params)
	}
	if _, err := os.Stat(filepath.Join(e.root, "result.json")); !os.IsNotExist(err) {
		t.Errorf("a result was kept: %v", err)
	}
}

// A worker that hits the cap but swallows the failure and exits 0 (pypdf
// skips a form XObject it cannot read) still fails: its text may be
// incomplete.
func TestExtractPDFMemoryLimitSwallowed(t *testing.T) {
	e, pdf := pdfEnv(t, "pdf_swallow")
	setPDFLimits(t, pdfTimeoutBase, pdfTimeoutPerPage, 128<<20)
	_, err := e.deps(nil).extractPDF(e.ctx, e.root, pdf, nil)
	if !sched.IsPermanent(err) || codeOf(err) != CodePDFMemoryLimit {
		t.Fatalf("err %v (code %q)", err, codeOf(err))
	}
}

// Under the cap nothing changes (the same worker, a cap it does not reach).
func TestExtractPDFUnderMemoryLimit(t *testing.T) {
	e, pdf := pdfEnv(t, "pdf_alloc")
	setPDFLimits(t, pdfTimeoutBase, pdfTimeoutPerPage, 768<<20)
	pages, err := e.deps(nil).extractPDF(e.ctx, e.root, pdf, nil)
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages %q, err %v", pages, err)
	}
}

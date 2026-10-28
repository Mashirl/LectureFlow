// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/sched"
)

// fakePDFWorker: the fake extract_pdf.py modes of the limits tests
// (security review 2, M2).
//
//	pdf_pages_sleep  "STATUS pages 4", sleeps LF_FAKE_SLEEP_MS, writes the result
//	pdf_budget       the worker's decode-budget error (ERROR_CODE, exit 2)
//	pdf_alloc        allocates 512 MiB (over a test cap the Go runtime dies), writes the result
//	pdf_swallow      tries to commit 512 MiB, ignores the failure, writes the result (exit 0)
func fakePDFWorker(mode string, writeResult func() int) (int, bool) {
	switch mode {
	case "pdf_pages_sleep":
		fmt.Println("STATUS pages 4")
		ms, _ := strconv.Atoi(os.Getenv("LF_FAKE_SLEEP_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return writeResult(), true
	case "pdf_budget":
		fmt.Println("STATUS pages 24")
		fmt.Println(`ERROR_CODE decode_budget {"limit_bytes": 4194304}`)
		fmt.Fprintln(os.Stderr, "ERROR: input error: the PDF's streams decode to more than 4194304 bytes (decode budget)")
		return 2, true
	case "pdf_alloc":
		b := make([]byte, 512<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		return writeResult(), true
	case "pdf_swallow":
		fmt.Println("committed", tryCommit(512<<20))
		return writeResult(), true
	}
	return 0, false
}

// setPDFLimits shortens the limits for a test.
func setPDFLimits(t *testing.T, base, perPage time.Duration, memory uint64) {
	t.Helper()
	b, mib, page, mx, mem := pdfTimeoutBase, pdfTimeoutPerMiB, pdfTimeoutPerPage, pdfTimeoutMax, pdfMemoryLimit
	t.Cleanup(func() {
		pdfTimeoutBase, pdfTimeoutPerMiB, pdfTimeoutPerPage, pdfTimeoutMax, pdfMemoryLimit = b, mib, page, mx, mem
	})
	pdfTimeoutBase, pdfTimeoutPerMiB, pdfTimeoutPerPage = base, 0, perPage
	if memory != 0 {
		pdfMemoryLimit = memory
	}
}

// pdfEnv is a test env with the fake extraction worker in mode and a PDF
// file to extract.
func pdfEnv(t *testing.T, mode string) (*env, string) {
	t.Helper()
	e := newEnv(t)
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["one"]}`), 0o644))
	e.useFakeWorker(mode, pages)
	pdf := filepath.Join(e.root, "slides.pdf")
	e.must(os.WriteFile(pdf, []byte("%PDF-1.4 fake"), 0o644))
	return e, pdf
}

func TestPDFTimeoutFormula(t *testing.T) {
	for _, c := range []struct {
		size    int64
		pages   int
		ceiling time.Duration
		want    time.Duration
	}{
		{0, 0, 0, time.Minute},
		{1, 0, 0, 61 * time.Second},                          // a started MiB counts
		{5 << 20, 0, 0, 65 * time.Second},                    //
		{5<<20 + 1, 100, 0, 66*time.Second + 50*time.Second}, // + 0.5 s per page
		{4 << 30, 0, PDFTimeoutMax, PDFTimeoutMax},           // capped
		{0, 100000, 5 * time.Minute, 5 * time.Minute},        //
		{-1, -1, 0, time.Minute},                             // nonsense counts as nothing
	} {
		if got := PDFTimeout(c.size, c.pages, c.ceiling); got != c.want {
			t.Errorf("PDFTimeout(%d, %d, %v) = %v, want %v", c.size, c.pages, c.ceiling, got, c.want)
		}
	}
}

func TestPDFErrorCode(t *testing.T) {
	code, params := PDFErrorCode("decode_budget", `{"limit_bytes": 536870912}`)
	if code != CodePDFDecodeBudget || params["limit_bytes"] != int64(536870912) {
		t.Errorf("decode_budget: %s %v", code, params)
	}
	code, params = PDFErrorCode("memory_limit", `{"limit_bytes": 1073741824}`)
	if code != CodePDFMemoryLimit || params["limit_bytes"] != int64(1<<30) {
		t.Errorf("memory_limit: %s %v", code, params)
	}
	for _, c := range [][2]string{{"something_new", `{"limit_bytes": 1}`}, {"decode_budget", ""},
		{"decode_budget", `{"limit_bytes": 0}`}, {"memory_limit", `{"limit": 5}`}} {
		if code, _ := PDFErrorCode(c[0], c[1]); code != "" {
			t.Errorf("%q %q: %s", c[0], c[1], code)
		}
	}
}

// The limit starts from the file size and grows (never shrinks) with the
// page count the worker reports; other STATUS lines change nothing.
func TestPDFDeadlineGrowsWithPages(t *testing.T) {
	setPDFLimits(t, 200*time.Millisecond, time.Second, 0)
	d := NewPDFDeadline(context.Background(), 0, 10*time.Second)
	defer d.Stop()
	if d.Limit() != 200*time.Millisecond {
		t.Fatalf("limit %v", d.Limit())
	}
	for _, s := range []string{"downloading_model", "pages x", "pages 0", "pages -3"} {
		d.Status(s)
	}
	if d.Limit() != 200*time.Millisecond {
		t.Fatalf("limit %v after unrelated lines", d.Limit())
	}
	d.Status("pages 3")
	if d.Limit() != 3200*time.Millisecond {
		t.Fatalf("limit %v after 3 pages", d.Limit())
	}
	d.Status("pages 1") // never shorter
	if d.Limit() != 3200*time.Millisecond {
		t.Fatalf("limit %v", d.Limit())
	}
	d.Status("pages 100") // capped
	if d.Limit() != 10*time.Second {
		t.Fatalf("limit %v", d.Limit())
	}
	time.Sleep(400 * time.Millisecond) // past the first limit
	if err := d.Context().Err(); err != nil {
		t.Fatalf("expired at the first limit: %v", err)
	}
	if err := d.Err(context.Background()); err != nil {
		t.Errorf("Err while running: %v", err)
	}
}

// A worker that runs past the limit is killed: pdf.timeout {minutes}, an
// input error (not retried).
func TestExtractPDFTimeout(t *testing.T) {
	e, pdf := pdfEnv(t, "hang")
	setPDFLimits(t, 300*time.Millisecond, 0, 0)
	start := time.Now()
	_, err := e.deps(nil).extractPDF(e.ctx, e.root, pdf, nil)
	if !sched.IsPermanent(err) || codeOf(err) != CodePDFTimeout {
		t.Fatalf("err %v (code %q)", err, codeOf(err))
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Params["minutes"] != 1 {
		t.Errorf("params %+v", ce.Info.Params)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %v", d)
	}
	if _, err := os.Stat(filepath.Join(e.root, "result.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("result.json.tmp left behind: %v", err)
	}
}

// The page count extends the limit: a 4-page PDF gets base + 4 × 2 s. The
// worker runs longer than the base after it reported the pages (so without
// the extension it would time out) and well within the extended limit. The
// base also has to cover starting the (fake) worker process before it can
// report the pages: 3 s (flake-hunt #7: 500 ms did not on a loaded machine).
func TestExtractPDFTimeoutGrowsWithPages(t *testing.T) {
	e, pdf := pdfEnv(t, "pdf_pages_sleep")
	t.Setenv("LF_FAKE_SLEEP_MS", "4000")
	setPDFLimits(t, 3*time.Second, 2*time.Second, 0)
	pages, err := e.deps(nil).extractPDF(e.ctx, e.root, pdf, nil)
	if err != nil || len(pages) != 1 || pages[0] != "one" {
		t.Fatalf("pages %q, err %v", pages, err)
	}
}

// Cancelling the stage is a cancellation, not a timeout.
func TestExtractPDFCancelIsNotTimeout(t *testing.T) {
	e, pdf := pdfEnv(t, "hang")
	ctx, cancel := context.WithCancel(e.ctx)
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := e.deps(nil).extractPDF(ctx, e.root, pdf, nil)
	if !errors.Is(err, context.Canceled) || codeOf(err) != "" {
		t.Fatalf("err %v (code %q)", err, codeOf(err))
	}
}

// The worker's decode-budget error: pdf.decode_budget {limit_bytes}, not
// retried.
func TestExtractPDFDecodeBudget(t *testing.T) {
	e, pdf := pdfEnv(t, "pdf_budget")
	_, err := e.deps(nil).extractPDF(e.ctx, e.root, pdf, nil)
	if !sched.IsPermanent(err) || codeOf(err) != CodePDFDecodeBudget {
		t.Fatalf("err %v (code %q)", err, codeOf(err))
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Params["limit_bytes"] != int64(4194304) {
		t.Errorf("params %+v", ce.Info.Params)
	}
}

// job.json carries the memory limit (the worker enforces it itself where
// the OS cannot: Linux, macOS).
func TestExtractPDFJobHasMemoryLimit(t *testing.T) {
	e, pdf := pdfEnv(t, "ok")
	log := e.jobLog()
	if _, err := e.deps(nil).extractPDF(e.ctx, e.root, pdf, nil); err != nil {
		t.Fatal(err)
	}
	jobs := fakeJobs(t, log)
	if len(jobs) != 1 || jobs[0].Job["memory_limit_bytes"] != float64(PDFMemoryLimit) {
		t.Fatalf("jobs %+v", jobs)
	}
	abs, _ := filepath.Abs(pdf)
	if jobs[0].Job["pdf"] != abs {
		t.Errorf("pdf %v", jobs[0].Job["pdf"])
	}
}

func TestPDFMemoryError(t *testing.T) {
	if err := PDFMemoryError(procutil.Usage{PeakMemory: 40 << 20}, true); err != nil {
		t.Errorf("a failure far from the cap: %v", err)
	}
	err := PDFMemoryError(procutil.Usage{MemoryLimitHit: true, PeakMemory: 900 << 20}, false)
	var ce *model.CodedError
	if !sched.IsPermanent(err) || !errors.As(err, &ce) || ce.Info.Code != CodePDFMemoryLimit ||
		ce.Info.Params["limit_bytes"] != int64(PDFMemoryLimit) {
		t.Errorf("over the cap: %v", err)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/sched"
)

// Resource limits of worker/extract_pdf.py (security review 2, M2): it
// parses untrusted PDFs (slides, syllabi), and a few-MB file whose streams
// inflate ~1000x used to take gigabytes and as long as it liked. Three
// limits, each an input error (sched.Permanent: not retried automatically):
//
//   - the worker's decode budget (extract_pdf.py; ERROR_CODE decode_budget):
//     bytes decoded from the PDF's streams, 32x the file size within
//     512 MiB..2 GiB;
//   - a memory cap of PDFMemoryLimit: on Windows a Job Object the process is
//     started in (procutil; it is created suspended and only resumed inside
//     the job), elsewhere the worker's own limit (job.json
//     memory_limit_bytes: RLIMIT_AS on Linux, a peak-RSS watchdog on macOS);
//   - a time limit growing with the file size and, once the worker reports
//     it ("STATUS pages n"), the page count (PDFTimeout), capped.
//
// The material stage (and the glossary seed's syllabus PDF) runs through
// runWorker; the syllabus wizard's synchronous extraction
// (api.runExtractPDF) uses the same pieces.

// Job errors of the limits (kind error; testdata/api/error_codes_backend.json).
const (
	CodePDFDecodeBudget = "pdf.decode_budget" // limit_bytes
	CodePDFMemoryLimit  = "pdf.memory_limit"  // limit_bytes
	CodePDFTimeout      = "pdf.timeout"       // minutes
)

// PDFMemoryLimit is the memory cap of one extraction process: the CSC3631
// decks (65-104 pages) peak at 40-45 MB committed, a page-by-page bomb at
// ~50 MB since the worker releases each page, so 1 GiB is 20x what a real
// PDF needs; with several extractions running at once (the CPU pool) they
// still stay well inside the application's 8 GB.
const PDFMemoryLimit = 1 << 30

// PDFTimeoutMax caps PDFTimeout for the material stage (a background job;
// the syllabus wizard, which the user waits for, uses a shorter cap).
const PDFTimeoutMax = 20 * time.Minute

// The time-limit formula (PDFTimeout); variables so that tests can shorten
// them. Measured: the CSC3631 decks take 5-7 ms per page plus ~0.3 s to
// start Python and import pypdf, so 0.5 s per page is ~70x that; the base
// covers a cold start (an antivirus scanning the interpreter); the size
// term covers parsing a large file before the page count is known.
var (
	pdfTimeoutBase    = 60 * time.Second
	pdfTimeoutPerMiB  = time.Second
	pdfTimeoutPerPage = 500 * time.Millisecond
	pdfTimeoutMax     = PDFTimeoutMax
	pdfMemoryLimit    = uint64(PDFMemoryLimit)
)

// PDFTimeout is the time limit of extracting a PDF of size bytes and pages
// pages (0 = not known yet): base + 1 s per MiB + 0.5 s per page, at most
// ceiling (0 = no cap).
func PDFTimeout(size int64, pages int, ceiling time.Duration) time.Duration {
	mib := (max(size, 0) + 1<<20 - 1) >> 20
	d := pdfTimeoutBase + time.Duration(mib)*pdfTimeoutPerMiB + time.Duration(max(pages, 0))*pdfTimeoutPerPage
	if ceiling > 0 && d > ceiling {
		d = ceiling
	}
	return d
}

// PDFJob is the job.json of extracting the PDF at abs (an absolute path).
func PDFJob(abs string) map[string]any {
	return map[string]any{"pdf": abs, "memory_limit_bytes": pdfMemoryLimit}
}

// PDFLimits are the OS limits of the extraction process (procutil.Start).
func PDFLimits() procutil.Limits { return procutil.Limits{MemoryBytes: pdfMemoryLimit} }

// PDFErrorCode maps extract_pdf.py's "ERROR_CODE <name> <json>" line to a
// job error code and its params ("" = not one of its names, or no limit in
// the line).
func PDFErrorCode(name, raw string) (string, map[string]any) {
	var code string
	switch name {
	case "decode_budget":
		code = CodePDFDecodeBudget
	case "memory_limit":
		code = CodePDFMemoryLimit
	default:
		return "", nil
	}
	var p struct {
		LimitBytes *int64 `json:"limit_bytes"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.LimitBytes == nil || *p.LimitBytes <= 0 {
		return "", nil
	}
	return code, map[string]any{"limit_bytes": *p.LimitBytes}
}

// PDFMemoryError is the job error of a run that went over its memory cap
// (procutil.Usage.MemoryExceeded; failed: the process exited non-zero),
// else nil. A run that succeeded is failed too when the job reported an
// allocation over the cap: the worker may have swallowed the MemoryError
// (pypdf skips a form XObject it cannot read) and lost text without saying
// so.
func PDFMemoryError(u procutil.Usage, failed bool) error {
	if !u.MemoryExceeded(PDFLimits(), failed) {
		return nil
	}
	return sched.Permanent(model.Errorf(CodePDFMemoryLimit, map[string]any{"limit_bytes": int64(pdfMemoryLimit)},
		fmt.Errorf("抽取这份 PDF 的文字需要超过 %d MiB 内存，已停止（峰值 %d MiB）：文件可能已损坏或经过特殊构造",
			pdfMemoryLimit>>20, u.PeakMemory>>20)))
}

// errPDFTimeout is the cause of a PDFDeadline's cancellation.
var errPDFTimeout = errors.New("PDF extraction timed out")

// PDFDeadline is the time limit of one extraction: PDFTimeout of the file
// size from the start, extended (never shortened) once the worker reports
// its page count. Its Context is cancelled when the limit passes.
type PDFDeadline struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	start   time.Time
	size    int64
	ceiling time.Duration

	mu    sync.Mutex
	limit time.Duration
	timer *time.Timer
}

// NewPDFDeadline starts the limit of extracting a PDF of size bytes, capped
// at ceiling (0: PDFTimeoutMax). Stop it when the run is over.
func NewPDFDeadline(parent context.Context, size int64, ceiling time.Duration) *PDFDeadline {
	if ceiling <= 0 {
		ceiling = pdfTimeoutMax
	}
	ctx, cancel := context.WithCancelCause(parent)
	d := &PDFDeadline{ctx: ctx, cancel: cancel, start: time.Now(), size: size, ceiling: ceiling}
	d.limit = PDFTimeout(size, 0, ceiling)
	d.timer = time.AfterFunc(d.limit, func() { cancel(errPDFTimeout) })
	return d
}

// Context is the run's context: cancelled with the parent or at the limit.
func (d *PDFDeadline) Context() context.Context { return d.ctx }

// Status takes a worker "STATUS" line's text: "pages <n>" extends the
// limit to PDFTimeout(size, n).
func (d *PDFDeadline) Status(s string) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "pages ")
	if !ok {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil || n <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	limit := PDFTimeout(d.size, n, d.ceiling)
	if limit <= d.limit || !d.timer.Stop() { // not longer, or already expired
		return
	}
	d.limit = limit
	d.timer.Reset(time.Until(d.start.Add(limit)))
}

// Limit is the current time limit.
func (d *PDFDeadline) Limit() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.limit
}

// Stop ends the deadline (and cancels its context).
func (d *PDFDeadline) Stop() {
	d.mu.Lock()
	d.timer.Stop()
	d.mu.Unlock()
	d.cancel(context.Canceled)
}

// Err is the job error of a run that ended because the limit passed (not
// because parent was cancelled), else nil.
func (d *PDFDeadline) Err(parent context.Context) error {
	if parent.Err() != nil || !errors.Is(context.Cause(d.ctx), errPDFTimeout) {
		return nil
	}
	limit := d.Limit()
	minutes := max(1, int(math.Floor(limit.Minutes()))) // "more than N minutes" stays true
	return sched.Permanent(model.Errorf(CodePDFTimeout, map[string]any{"minutes": minutes},
		fmt.Errorf("抽取这份 PDF 的文字超过 %v 仍未完成，已停止：文件可能已损坏或经过特殊构造", limit.Round(time.Second))))
}

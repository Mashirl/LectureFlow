// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/pyenv"
)

// The syllabus PDF extraction runs with pyenv's switches even when the
// service's own environment turns them off (NOTES.md "无遥测").
func TestSyllabusPDFWithoutTelemetry(t *testing.T) {
	e := pdfLimitsEnv(t, "env")
	t.Setenv("HF_HUB_DISABLE_TELEMETRY", "0")
	work := t.TempDir()
	pdf := filepath.Join(work, "syllabus.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	pages, err := e.srv.runExtractPDF(context.Background(), work, pdf)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0] != "telemetry_off true" {
		t.Fatalf("pages %q", pages)
	}
}

// The diagnostics' commands (the Python checks and self-tests among them)
// run with pyenv's switches.
func TestDiagnosticCommandsWithoutTelemetry(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "0")
	name, args := "env", []string(nil)
	if runtime.GOOS == "windows" {
		name, args = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), []string{"/d", "/c", "set"}
	}
	out, _, err := runCommand(context.Background(), name, args...)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	if !pyenv.Has(env) {
		t.Fatalf("runCommand's environment lacks %q:\n%s", pyenv.TelemetryOff, out)
	}
}

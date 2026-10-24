// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exception writes a LICENSE-EXCEPTION.md whose library list names pkgs.
func (r *repo) exception(pkgs ...string) {
	r.t.Helper()
	var b strings.Builder
	b.WriteString("# Additional permission under GNU AGPL version 3 section 7\r\n\r\n### Additional permission\r\n\r\nText.\r\n\r\n" + exceptionLibraries + "\r\n\r\n")
	for _, p := range pkgs {
		b.WriteString("- `" + p + "`: a library.\r\n")
	}
	b.WriteString("\r\n### Removing this permission\r\n\r\nMentions `outside-blob` only here.\r\n")
	r.write(exceptionFile, b.String())
}

func exceptionViolations(rep *Report) []Violation {
	var out []Violation
	for _, v := range rep.Violations {
		if v.Kind == "exception" {
			out = append(out, v)
		}
	}
	return out
}

func TestUserFetchedNeedsException(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(pythonFile, PyFile{Packages: map[string]PyEntry{
		"cuda-blob":    {License: "LicenseRef-NVIDIA-Proprietary"},
		"outside-blob": {License: "LicenseRef-NVIDIA-Proprietary"},
		"ok":           {License: "MIT"},
	}})
	r.write(componentsDir+"/locks/asr-cuda-windows-x64.txt", "cuda-blob==12.9\noutside-blob==1.0\nok==1.0\n")
	r.write(componentsDir+"/locks/asr-cuda-linux-x64.txt", "cuda-blob==12.8\nok==1.0\n")
	why := "not distributed: downloaded from PyPI on the user's computer"
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{
		{Kind: KindPython, Name: "cuda-blob", Scope: ScopeUserFetched, Reason: why},
		{Kind: KindPython, Name: "outside-blob", Scope: ScopeUserFetched, Reason: why},
	}})

	// No exception file: every user-fetched package fails once.
	vs := exceptionViolations(audit(t, r.root, nil))
	if len(vs) != 2 || vs[0].Name != "cuda-blob" || vs[1].Name != "outside-blob" || !strings.Contains(vs[0].Reason, "does not exist") {
		t.Fatalf("no file: violations = %v", vs)
	}

	// A file without the library list.
	r.write(exceptionFile, "# Additional permission\n\n`cuda-blob` `outside-blob`\n")
	if vs := exceptionViolations(audit(t, r.root, nil)); len(vs) != 2 || !strings.Contains(vs[0].Reason, "has no") {
		t.Fatalf("no section: violations = %v", vs)
	}

	// Named outside the list only: still fails (CRLF file, case-insensitive name).
	r.exception("CUDA-Blob")
	vs = exceptionViolations(audit(t, r.root, nil))
	if len(vs) != 1 || vs[0].Name != "outside-blob" || !strings.Contains(vs[0].Reason, "does not name it") {
		t.Fatalf("partial list: violations = %v", vs)
	}

	r.exception("cuda-blob", "outside-blob")
	if rep := audit(t, r.root, nil); len(rep.Violations) != 0 {
		t.Fatalf("full list: violations = %v", rep.Violations)
	}

	// Packages that are not user-fetched need no exception.
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{}})
	os.Remove(filepath.Join(r.root, exceptionFile))
	if vs := exceptionViolations(audit(t, r.root, nil)); len(vs) != 0 {
		t.Fatalf("no user-fetched: exception violations = %v", vs)
	}
}

// The repository's own LICENSE-EXCEPTION.md names every user-fetched
// package of the real allowlist.
func TestRepositoryExceptionNamesUserFetched(t *testing.T) {
	root := filepath.Join("..", "..")
	b, err := os.ReadFile(filepath.Join(root, exceptionFile))
	if err != nil {
		t.Fatal(err)
	}
	list, problem := exceptionList(string(b), true)
	if problem != "" {
		t.Fatal(problem)
	}
	var rep Report
	al, err := loadAllowlist(root, &rep)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range al.Entries {
		if e.Scope != ScopeUserFetched {
			continue
		}
		n++
		if !strings.Contains(list, "`"+strings.ToLower(e.Name)+"`") {
			t.Errorf("%s does not list user-fetched package %s", exceptionFile, e.Name)
		}
		if !strings.Contains(e.Reason, exceptionFile) {
			t.Errorf("allowlist reason of %s does not mention %s", e.Name, exceptionFile)
		}
	}
	if n == 0 {
		t.Fatal("no user-fetched entries in the allowlist")
	}
}

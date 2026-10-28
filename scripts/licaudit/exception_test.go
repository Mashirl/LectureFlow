// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
// package of the real allowlist, and nothing else: the permission's list
// of libraries stays exactly the audit's list (user decision 2026-10-27,
// "scope written precisely").
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
	userFetched := map[string]bool{}
	for _, e := range al.Entries {
		if e.Scope != ScopeUserFetched {
			continue
		}
		userFetched[strings.ToLower(e.Name)] = true
		if !strings.Contains(list, "`"+strings.ToLower(e.Name)+"`") {
			t.Errorf("%s does not list user-fetched package %s", exceptionFile, e.Name)
		}
		if !strings.Contains(e.Reason, exceptionFile) {
			t.Errorf("allowlist reason of %s does not mention %s", e.Name, exceptionFile)
		}
	}
	if len(userFetched) == 0 {
		t.Fatal("no user-fetched entries in the allowlist")
	}
	named := regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(list, -1)
	if len(named) == 0 {
		t.Fatalf("%s names no package in %q", exceptionFile, exceptionLibraries)
	}
	for _, m := range named {
		if !userFetched[m[1]] {
			t.Errorf("%s names %s under %q, but the allowlist has no user-fetched entry for it", exceptionFile, m[1], exceptionLibraries)
		}
	}
}

// The worker sources point to the additional permission; tests, other
// directories and files deleted in the work tree do not need to.
func TestExceptionNoticeCheck(t *testing.T) {
	r := newRepo(t)
	spdx := "# " + spdxTag + "\n# Copyright (C) 2026 LectureFlow contributors.\n"
	full := spdx + "# " + exceptionNotice + "\n"
	files := map[string]string{
		"worker/ok.py":            full + "import sys\n",
		"worker/crlf.py":          strings.ReplaceAll(full, "\n", "\r\n"),
		"worker/plain.py":         spdx + "import sys\n",
		"worker/late.py":          spdx + "\n\n\n\n# " + exceptionNotice + "\n", // line 7
		"worker/sub/helper.py":    spdx,
		"worker/conftest.py":      spdx,
		"worker/test_plain.py":    spdx,
		"worker/plain_test.py":    spdx,
		"worker/testdata/fake.py": "x = 1\n",
		"worker/requirements.txt": "faster-whisper==1.2.1\n",
		"cmd/main.go":             "// " + spdxTag + "\npackage main\n",
	}
	var list []string
	for f, c := range files {
		r.write(f, c)
		list = append(list, f)
	}
	list = append(list, "worker/gone.py") // tracked, deleted in the work tree
	missing, err := CheckExceptionNotice(r.root, list)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"worker/late.py", "worker/plain.py", "worker/sub/helper.py"}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}

	// Part of the SPDX run: every file has its SPDX header, the check still fails.
	var out bytes.Buffer
	tracked := func(string) ([]string, error) { return list, nil }
	if code := auditMain([]string{"-root", r.root, "-check", "-only", "spdx"}, &out, &bytes.Buffer{}, nil, tracked); code != 1 ||
		!strings.Contains(out.String(), "SPDX check: 0 files") || !strings.Contains(out.String(), "FAIL worker/plain.py") {
		t.Fatalf("exit %d, output %s", code, out.String())
	}

	for _, f := range want {
		r.write(f, full)
	}
	out.Reset()
	if code := auditMain([]string{"-root", r.root, "-check", "-only", "spdx"}, &out, &bytes.Buffer{}, nil, tracked); code != 0 {
		t.Fatalf("all marked: exit %d, output %s", code, out.String())
	}
}

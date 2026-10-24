// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// P13.6 (phase 2 acceptance: the export list kept the file name of the
// export run after the automatic title replaced the file-name title): the
// lecture view lists a transcript or subtitle file under the name it
// downloads as — the lecture's current number and English title — while
// its URL is the file on disk, which keeps its name until export runs
// again. A citation JSON keeps its name (its content has the numbers of
// its run). The course list shows the same.
func TestExportViewNamesFollowTitleAndNumber(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.course("C")
	l := e.lecture(c.ID, "CSC1003-L01-01 Wed Oct 07")
	termStart, rec := "2026-09-28", "2026-10-07T09:35"
	if err := e.st.SetCourseCalendar(ctx, c.ID, &termStart, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetLectureRecordedAt(ctx, l.ID, &rec, model.RecordedAtUser); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.dirOf(l), model.ExportDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	disk := "W02L1 CSC1003-L01-01 Wed Oct 07"
	for _, suf := range []string{".bilingual.md", ".zh.vtt", ".cite.json"} {
		if err := os.WriteFile(filepath.Join(dir, disk+suf), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when, base string) {
		t.Helper()
		want := map[string]string{ // shown name → file on disk
			base + ".bilingual.md": disk + ".bilingual.md",
			base + ".zh.vtt":       disk + ".zh.vtt",
			disk + ".cite.json":    disk + ".cite.json",
		}
		var list []Lecture
		e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &list)
		if len(list) != 1 {
			t.Fatalf("%d lectures", len(list))
		}
		for _, v := range []Lecture{e.lectureView(l.ID), list[0]} {
			if len(v.Exports) != len(want) {
				t.Fatalf("%s: exports %+v", when, v.Exports)
			}
			for _, x := range v.Exports {
				file, ok := want[x.Name]
				if !ok || x.URL != "/api/lectures/"+l.ID+"/export/"+url.PathEscape(file) {
					t.Errorf("%s: export %q → %s, want one of %v", when, x.Name, x.URL, want)
					continue
				}
				// the download carries the name the list shows
				res, _ := e.do("GET", x.URL, nil)
				if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Disposition"), "filename*=UTF-8''"+url.PathEscape(x.Name)) {
					t.Errorf("%s: %s downloads as %q (%d)", when, x.Name, res.Header.Get("Content-Disposition"), res.StatusCode)
				}
			}
		}
	}
	check("as exported", disk)

	// The automatic title (P13.5) replaces the file-name title after export.
	zh := "密钥空间与复杂度"
	if err := e.st.SetLectureTitles(ctx, l.ID, "Key Space and Key-Ciphertext Complexity", &zh, model.TitleSourceLLM); err != nil {
		t.Fatal(err)
	}
	check("new title", "W02L1 Key Space and Key-Ciphertext Complexity")

	// A new recording time renumbers it (week 3).
	rec = "2026-10-14T09:35"
	if err := e.st.SetLectureRecordedAt(ctx, l.ID, &rec, model.RecordedAtUser); err != nil {
		t.Fatal(err)
	}
	check("renumbered", "W03L1 Key Space and Key-Ciphertext Complexity")
}

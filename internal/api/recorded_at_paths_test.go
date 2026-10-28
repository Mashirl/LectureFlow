// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones below, independent of the machine

	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
)

// pathsRule reads a Panopto download name ("… Mon Oct 20 08-00-00 AEDT
// 2025.mp4"); like every file-name rule it takes the clock as written and
// ignores the zone abbreviation.
const pathsRule = `(?P<month>[A-Z][a-z]{2}) (?P<day>\d{2}) (?P<hour>\d{2})-(?P<minute>\d{2})-\d{2} [A-Z]{3,4} (?P<year>\d{4})`

// tz-fix (Mac acceptance of 3.2.0): one lecture brought in both ways, a
// recording downloaded by hand and uploaded (file-name rule) and the same
// recording sent by the browser extension (download import, the time the
// extension sends: the page's clock, see extension/test/unit/dates.test.mjs
// and fetch's TestEnqueueKeepsThePagesClock), gets the same recording
// time, teaching week, number and export file name prefix, on a computer
// whose zone is not the university's. Each zone has a case that a
// conversion to it would move across a week boundary: in Los Angeles a
// Monday 08:00 in Sydney is Sunday afternoon (week 2); in Sydney Sunday
// 23:30 in the UK is Monday morning (week 4).
func TestRecordedAtSameForUploadAndDownload(t *testing.T) {
	lectures := []struct {
		file    string // the downloaded file's name (upload)
		title   string // the platform's title (download)
		sent    string // what the extension sends for that title
		week, n int
		label   string
	}{
		{"CSC1003-L01-04 Mon Oct 20 08-00-00 AEDT 2025.mp4", "CSC1003/L01/04 Mon Oct 20 08:00:00 AEDT 2025", "2025-10-20T08:00", 3, 1, "W03L1"},
		{"CSC1003-L01-05 Sun Oct 26 23-30-00 GMT 2025.mp4", "CSC1003/L01/05 Sun Oct 26 23:30:00 GMT 2025", "2025-10-26T23:30", 3, 2, "W03L2"},
	}
	for _, zone := range []string{"America/Los_Angeles", "Australia/Sydney"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			old := time.Local
			time.Local = loc
			t.Cleanup(func() { time.Local = old })

			e := newEnv(t)
			ctx := context.Background()
			term := CoursePatch{TermStart: ptr("2025-10-06")}
			up, dl := e.course("CSC1003 uploaded"), e.course("CSC1003 sent")
			e.call("PATCH", "/api/courses/"+up.ID, term, http.StatusOK, nil)
			e.call("PATCH", "/api/courses/"+dl.ID, term, http.StatusOK, nil)

			// by hand: the first file teaches the course its rule, the
			// second is read with the saved rule at once
			a := e.uploadNamed(up.ID, lectures[0].file)
			e.call("PUT", "/api/courses/"+up.ID+"/filename-rule", FilenamePreviewInput{Regex: pathsRule}, http.StatusOK, nil)
			uploaded := []string{a.ID, e.uploadNamed(up.ID, lectures[1].file).ID}

			// by the extension, into a course with the same rule (a user's
			// recording time is kept)
			rule, _ := json.Marshal(FilenameRule{Regex: pathsRule, Source: "user", CreatedAt: "2026-10-27T00:00:00Z", SamplesMatched: 1, SamplesTotal: 1})
			rs := string(rule)
			if err := e.st.SetCourseFilenameRule(ctx, dl.ID, &rs); err != nil {
				t.Fatal(err)
			}
			var sent []string
			for _, l := range lectures {
				rec := l.sent
				res, err := e.srv.ImportDownload(ctx, fetch.ImportRequest{TaskID: 7, CourseID: dl.ID, Path: writeDownload(t, e, "media bytes"),
					Filename: strings.ReplaceAll(l.title, ":", " ") + ".mp4", Title: l.title, RecordedAt: &rec})
				if err != nil {
					t.Fatalf("import %s: %v", l.title, err)
				}
				sent = append(sent, res.LectureID)
			}

			for i, l := range lectures {
				u, d := e.lectureView(uploaded[i]), e.lectureView(sent[i])
				if u.RecordedAtSource != model.RecordedAtFilename || d.RecordedAtSource != model.RecordedAtUser {
					t.Fatalf("%s: sources %s / %s", l.title, u.RecordedAtSource, d.RecordedAtSource)
				}
				for _, v := range []Lecture{u, d} {
					if v.RecordedAt == nil || *v.RecordedAt != l.sent || !v.RecordedHasTime || v.Week != l.week || v.N != l.n || v.Label != l.label || v.CiteLabel != l.label {
						t.Errorf("%s (%s): recorded %v week %d n %d label %s cite %s; want %s, week %d, %s",
							l.title, v.RecordedAtSource, v.RecordedAt, v.Week, v.N, v.Label, v.CiteLabel, l.sent, l.week, l.label)
					}
				}
				// the export files are named after the number
				for _, id := range []string{uploaded[i], sent[i]} {
					m, err := e.st.GetLecture(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if name := e.srv.exportDownloadName(ctx, m, "x.bilingual.md"); !strings.HasPrefix(name, l.label+" ") {
						t.Errorf("%s: export name %q, want the prefix %s", l.title, name, l.label)
					}
				}
			}
		})
	}
}

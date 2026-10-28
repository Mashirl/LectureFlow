// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"testing"
	"time"
	_ "time/tzdata" // the zones below, independent of the machine

	"lectureflow/internal/bridge"
)

// tzZones: a student abroad (Los Angeles for a UK or Australian course),
// the universities' zones and the extremes (UTC−11, UTC+14).
var tzZones = []string{"America/Los_Angeles", "Europe/London", "UTC", "Asia/Shanghai", "Australia/Sydney", "Pacific/Kiritimati", "Pacific/Pago_Pago"}

// setLocal makes loc this process's local zone for the test.
func setLocal(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

// tz-fix (Mac acceptance of 3.2.0): the recording time of a sent lecture is
// the clock the page stated, whatever this computer's zone, as a file-name
// rule reads it from the name of the same recording downloaded by hand.
func TestRecordedAtIgnoresTheComputersZone(t *testing.T) {
	cases := map[string]string{
		"2026-10-21T09:35":              "2026-10-21T09:35",
		"2026-10-21 09:35:00":           "2026-10-21T09:35",
		"2026-10-21":                    "2026-10-21T00:00", // date only: no time of day (calendar.HasClock)
		"2026-10-21T09:35:00+01:00":     "2026-10-21T09:35", // BST, as the page said
		"2026-10-19T08:00:00+11:00":     "2026-10-19T08:00", // a Monday in Sydney: Sunday in Los Angeles
		"2026-10-23T23:30:00+01:00":     "2026-10-23T23:30", // Friday night in the UK: Saturday in Sydney
		"2026-10-26T00:15:00Z":          "2026-10-26T00:15",
		" 2026-10-21T09:35:00.5-05:00 ": "2026-10-21T09:35",
		"":                              "",
		"yesterday":                     "",
	}
	for _, zone := range tzZones {
		t.Run(zone, func(t *testing.T) {
			setLocal(t, zone)
			for in, want := range cases {
				got := ""
				if v := recordedAt(in); v != nil {
					got = *v
				}
				if got != want {
					t.Errorf("recordedAt(%q) = %q, want %q", in, got, want)
				}
			}
		})
	}
}

// Through the queue: the time an enqueue states reaches the task, the
// download view and the lecture import unchanged on a computer in another
// zone (Los Angeles) than the page's (Sydney).
func TestEnqueueKeepsThePagesClock(t *testing.T) {
	setLocal(t, "America/Los_Angeles")
	e := newQEnv(t, nil)
	e.media.put("/files/a.m4a", pattern(1, 4_000))
	for i, stated := range []string{"2026-10-19T08:00", "2026-10-19T08:00:00+11:00"} {
		req := e.request("tz"+string(rune('a'+i)), e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
		req.Lecture.RecordedAt = stated
		id := e.enqueue(req)
		info := e.wait(id, bridge.StateDone)
		if info.RecordedAt != "2026-10-19T08:00" {
			t.Errorf("%s: view %q", stated, info.RecordedAt)
		}
		if r := e.row(id); r.RecordedAt == nil || *r.RecordedAt != "2026-10-19T08:00" {
			t.Errorf("%s: task row %v", stated, r.RecordedAt)
		}
		imps := e.importsSnapshot()
		if len(imps) != i+1 || imps[i].RecordedAt == nil || *imps[i].RecordedAt != "2026-10-19T08:00" {
			t.Fatalf("%s: import %+v", stated, imps)
		}
	}
}

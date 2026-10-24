// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lectureflow/internal/cite"
	"lectureflow/internal/model"
)

// Lecture.duration_ms is the media's length (real acceptance issue 8):
// audio.wav once prep has made it, the last sentence's end only before
// that. A recording that ends with minutes nobody transcribed (silence,
// questions) is shown with its real length.
func TestLectureDurationFromMedia(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	dur := func() (view, list *int64) {
		t.Helper()
		var ls []Lecture
		e.call("GET", "/api/courses/"+c.ID+"/lectures", nil, http.StatusOK, &ls)
		for _, x := range ls {
			if x.ID == l.ID {
				list = x.DurationMS
			}
		}
		return e.lectureView(l.ID).DurationMS, list
	}
	want := func(what string, ms int64) {
		t.Helper()
		v, li := dur()
		if v == nil || li == nil || *v != ms || *li != ms {
			t.Fatalf("%s: duration view %v list %v, want %d", what, deref(v), deref(li), ms)
		}
	}
	if v, li := dur(); v != nil || li != nil {
		t.Fatalf("nothing yet: %v %v", deref(v), deref(li))
	}

	// A transcript (last sentence ends at 4 s) but no audio.wav: its end.
	e.insertTranscript(l.ID, true)
	want("transcript only", 4000)

	// audio.wav from a prep that has not finished is not trusted.
	wav := filepath.Join(e.dirOf(l), model.AudioFile)
	writeWAV(t, wav, 6000)
	want("prep pending", 4000)

	// prep done: the media's length, longer than the transcript.
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	want("prep done", 6000)

	// A new audio.wav (the cache follows the file) — and stale prep still
	// has its file.
	writeWAV(t, wav, 7500)
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(wav, future, future); err != nil {
		t.Fatal(err)
	}
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusStale)
	want("new audio.wav, prep stale", 7500)

	// Without the file (deleted by hand) the transcript's end again.
	if err := os.Remove(wav); err != nil {
		t.Fatal(err)
	}
	want("audio.wav gone", 4000)

	// A lecture without a transcript yet but with its audio: the media.
	m := e.lecture(c.ID, "M")
	writeWAV(t, filepath.Join(e.dirOf(m), model.AudioFile), 3000)
	e.moveJob(model.ScopeLecture, m.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	if d := e.lectureView(m.ID).DurationMS; d == nil || *d != 3000 {
		t.Fatalf("before asr: %v", deref(d))
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// The course citation JSON gives each lecture the same duration_ms as the
// lecture page: the media's length once prep has made audio.wav (the
// export stage's lecture cite.json does the same, stages
// TestExportCiteDurationIsMedia).
func TestCourseCiteDurationIsMedia(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.insertTranscript(l.ID, true) // the last sentence ends at 4 s
	e.moveJob(model.ScopeLecture, l.ID, model.StageExport, model.StatusQueued, model.StatusRunning, model.StatusDone)
	citeDuration := func() int64 {
		t.Helper()
		res, b := e.do("GET", "/api/courses/"+c.ID+"/export/cite.json", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("cite.json: %d %s", res.StatusCode, b)
		}
		var doc cite.Doc
		decodeStrict(t, b, &doc)
		if len(doc.Lectures) != 1 {
			t.Fatalf("lectures %+v", doc.Lectures)
		}
		return doc.Lectures[0].DurationMS
	}
	if got := citeDuration(); got != 4000 {
		t.Fatalf("without audio.wav: %d", got)
	}
	writeWAV(t, filepath.Join(e.dirOf(l), model.AudioFile), 6000)
	e.moveJob(model.ScopeLecture, l.ID, model.StagePrep, model.StatusQueued, model.StatusRunning, model.StatusDone)
	if got, page := citeDuration(), e.lectureView(l.ID).DurationMS; got != 6000 || page == nil || *page != got {
		t.Fatalf("cite %d, lecture page %v", got, deref(page))
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/subs"
)

const (
	lecReady   = "01JD3631000000000000000001" // CSC1003 L01: stream ready, chapters, saved position
	lecPending = "01JD3631000000000000000002" // CSC1003 L02: stream pending (generated on first /media)
	lecAudio   = "01JD3631000000000000000005" // CSC1003 L05: audio only
	lecASR     = "01JD3631000000000000000007" // CSC1003 L07: asr running, not playable
)

// playerServer is testServer with -media set to a file of known bytes.
func playerServer(t *testing.T, media []byte) *httptest.Server {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	h := newHandler(filepath.Join(root, "web"), filepath.Join(root, "testdata", "api"), 10*time.Millisecond)
	if media != nil {
		p := filepath.Join(t.TempDir(), "clip.mp4")
		if err := os.WriteFile(p, media, 0o644); err != nil {
			t.Fatal(err)
		}
		h.(*handler).m.mediaFile = p
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func fastPlayer(t *testing.T) {
	t.Helper()
	old := playerTiming
	playerTiming.chapters = 30 * time.Millisecond
	playerTiming.stream = 50 * time.Millisecond
	t.Cleanup(func() { playerTiming = old })
}

func get(t *testing.T, url string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, b
}

// P8.2: /media is served with http.ServeContent, so a Range request answers
// 206 with exactly the requested bytes.
func TestMediaRange(t *testing.T) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i * 7)
	}
	srv := playerServer(t, data)
	base := srv.URL + "/api/lectures/" + lecReady + "/media"

	res, b := get(t, base, map[string]string{"Range": "bytes=100-199"})
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range: status %d, want 206: %s", res.StatusCode, b)
	}
	if cr := res.Header.Get("Content-Range"); cr != "bytes 100-199/4096" {
		t.Errorf("Content-Range %q", cr)
	}
	if !bytes.Equal(b, data[100:200]) {
		t.Errorf("Range body: %d bytes, not data[100:200]", len(b))
	}
	if ct := res.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type %q", ct)
	}

	res, b = get(t, base, map[string]string{"Range": "bytes=4000-"})
	if res.StatusCode != http.StatusPartialContent || !bytes.Equal(b, data[4000:]) {
		t.Errorf("open-ended Range: status %d, %d bytes", res.StatusCode, len(b))
	}

	res, b = get(t, base, nil)
	if res.StatusCode != http.StatusOK || !bytes.Equal(b, data) || res.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("whole file: status %d, %d bytes, Accept-Ranges %q", res.StatusCode, len(b), res.Header.Get("Accept-Ranges"))
	}

	// audio only, not playable, unknown lecture
	for _, c := range []struct {
		id   string
		want int
	}{{lecAudio, 404}, {lecASR, 409}, {"nope", 404}} {
		if res, b := get(t, srv.URL+"/api/lectures/"+c.id+"/media", nil); res.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d: %s", c.id, res.StatusCode, c.want, b)
		}
	}
}

// Without -media there is no video: 404, and the player uses audio mode.
func TestMediaWithoutFile(t *testing.T) {
	srv := playerServer(t, nil)
	if res, _ := get(t, srv.URL+"/api/lectures/"+lecReady+"/media", map[string]string{"Range": "bytes=0-1"}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", res.StatusCode)
	}
}

// ?audio=1 is the audio mode's audio.wav (lead ruling 7 of phase 2): also
// served with Range requests, as long as the lecture, for audio-only
// lectures too.
func TestMediaAudioRange(t *testing.T) {
	srv := playerServer(t, nil)
	whole := newToneWAV(3480000) // CSC1003 L05: 58 min
	want := make([]byte, 2000)
	if _, err := whole.Seek(100000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(whole, want); err != nil {
		t.Fatal(err)
	}
	res, b := get(t, srv.URL+"/api/lectures/"+lecAudio+"/media?audio=1", map[string]string{"Range": "bytes=100000-101999"})
	if res.StatusCode != http.StatusPartialContent || !bytes.Equal(b, want) {
		t.Fatalf("status %d, %d bytes (match %v)", res.StatusCode, len(b), bytes.Equal(b, want))
	}
	if cr := res.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 100000-101999/%d", whole.size) || res.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("headers %v", res.Header)
	}
	res, b = get(t, srv.URL+"/api/lectures/"+lecAudio+"/media?audio=1", map[string]string{"Range": "bytes=0-43"})
	if res.StatusCode != http.StatusPartialContent || string(b[:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || binary.LittleEndian.Uint32(b[40:44]) != uint32(whole.size-44) {
		t.Errorf("header: status %d % x", res.StatusCode, b)
	}
	if res, _ := get(t, srv.URL+"/api/lectures/"+lecASR+"/media?audio=1", nil); res.StatusCode != http.StatusConflict {
		t.Errorf("not playable: status %d", res.StatusCode)
	}
}

// P8.1: the first /media request of an old lecture starts the background
// generation: generating (with progress) → ready, each step announced.
func TestStreamGeneratedOnFirstRequest(t *testing.T) {
	fastPlayer(t)
	srv := playerServer(t, []byte("0123456789"))
	url := srv.URL + "/api/lectures/" + lecPending
	var l api.Lecture
	doJSON(t, "GET", url, nil, 200, &l)
	if l.Stream == nil || l.Stream.Status != "pending" || !l.Playable {
		t.Fatalf("fixture: stream %+v playable %v", l.Stream, l.Playable)
	}
	if res, _ := get(t, url+"/media", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("while generating: status %d, want 404", res.StatusCode)
	}
	doJSON(t, "GET", url, nil, 200, &l)
	if l.Stream.Status != "generating" {
		t.Fatalf("after the first request: %+v", l.Stream)
	}
	deadline := time.Now().Add(3 * time.Second)
	for l.Stream.Status != "ready" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		doJSON(t, "GET", url, nil, 200, &l)
	}
	if l.Stream.Status != "ready" {
		t.Fatalf("never ready: %+v", l.Stream)
	}
	if res, b := get(t, url+"/media", map[string]string{"Range": "bytes=2-4"}); res.StatusCode != 206 || string(b) != "234" {
		t.Errorf("after generation: status %d body %q", res.StatusCode, b)
	}
}

// P8.2: /cues is subs.SegmentLecture of the lecture's sentences (English =
// sentence.text), in the default and the "many" scenario.
func TestCuesMatchSegmentLecture(t *testing.T) {
	srv := playerServer(t, nil)
	for _, sc := range []string{"1", "many"} {
		var page api.SentencePage
		doJSON(t, "GET", srv.URL+"/api/lectures/"+lecReady+"/sentences?limit=1000&mock="+sc, nil, 200, &page)
		if page.Total != len(page.Sentences) {
			t.Fatalf("%s: %d of %d sentences", sc, len(page.Sentences), page.Total)
		}
		want := subs.SegmentLecture(subsSentences(page.Sentences))
		var got []subs.Cue
		doJSON(t, "GET", srv.URL+"/api/lectures/"+lecReady+"/cues?mock="+sc, nil, 200, &got)
		if len(got) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %d cues, want %d (SegmentLecture)", sc, len(got), len(want))
		}
		// spot checks: the untranslated sentence has English only, a stale
		// translation is still shown, cues never overlap
		for i, c := range got {
			if i > 0 && c.StartMS < got[i-1].EndMS {
				t.Errorf("%s: cue %d overlaps the previous one", sc, i)
			}
			if c.SentenceIdx == 12 && c.Zh != "" {
				t.Errorf("%s: untranslated sentence 12 has zh %q", sc, c.Zh)
			}
		}
	}
	if res, b := get(t, srv.URL+"/api/lectures/"+lecASR+"/cues", nil); res.StatusCode != 409 {
		t.Errorf("not playable: status %d: %s", res.StatusCode, b)
	}
}

// P6.4: the position round-trips through the server.
func TestResumeRoundTrip(t *testing.T) {
	srv := playerServer(t, nil)
	url := srv.URL + "/api/lectures/" + lecAudio + "/resume"
	var v api.ResumeView
	doJSON(t, "GET", url, nil, 200, &v)
	if v.PositionMS != 0 || v.SavedAt != "" {
		t.Fatalf("nothing saved yet: %+v", v)
	}
	doJSON(t, "PUT", url, map[string]int64{"position_ms": 1613000}, 200, &v)
	if v.PositionMS != 1613000 {
		t.Fatalf("PUT answered %+v", v)
	}
	doJSON(t, "GET", url, nil, 200, &v)
	saved, err := time.Parse(time.RFC3339, v.SavedAt)
	if v.PositionMS != 1613000 || err != nil || time.Since(saved) > time.Minute {
		t.Fatalf("GET after PUT: %+v (%v)", v, err)
	}
	doJSON(t, "PUT", url, map[string]int64{"position_ms": -5}, 400, nil)
	doJSON(t, "PUT", url, map[string]string{}, 400, nil)
	doJSON(t, "GET", srv.URL+"/api/lectures/nope/resume", nil, 404, nil)
	// the example position of L01
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecReady+"/resume", nil, 200, &v)
	if v.PositionMS != 70500 {
		t.Errorf("example position %+v", v)
	}
}

// P7: an empty 200 before generation (lead ruling 21 of power management,
// formerly 404); POST answers 202 running and announces the result with SSE
// changed {chapters}; the result follows the constraints; an edited
// sentence makes it stale; the error scenario reports a failure.
func TestChaptersGenerate(t *testing.T) {
	fastPlayer(t)
	srv := playerServer(t, nil)
	h := srv.Config.Handler.(*handler)
	events, unsubscribe := h.m.main.subscribe()
	defer unsubscribe()
	url := srv.URL + "/api/lectures/" + lecAudio + "/chapters"
	var v api.ChaptersView
	doJSON(t, "GET", url, nil, 200, &v)
	if v.Chapters == nil || len(v.Chapters) != 0 || v.Stale || v.Running || v.GeneratedAt != "" || v.Error != "" {
		t.Fatalf("never generated: %+v", v)
	}
	doJSON(t, "GET", srv.URL+"/api/lectures/01JD9999000000000000000001/chapters", nil, 404, nil) // no such lecture
	doJSON(t, "POST", url+"?mock=many", nil, 202, &v)
	if !v.Running {
		t.Fatalf("POST: %+v", v)
	}
	waitChanged(t, events, "chapters", lecAudio) // started
	waitChanged(t, events, "chapters", lecAudio) // done
	doJSON(t, "GET", url+"?mock=many", nil, 200, &v)
	var page api.SentencePage
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecAudio+"/sentences?limit=1000&mock=many", nil, 200, &page)
	if v.Running || v.Stale || v.Error != "" || v.GeneratedAt == "" || len(v.Chapters) < 3 || len(v.Chapters) > 12 {
		t.Fatalf("after generation: %+v", v)
	}
	for i, c := range v.Chapters {
		if (i == 0 && c.StartIdx != 0) || (i > 0 && c.StartIdx <= v.Chapters[i-1].StartIdx) {
			t.Errorf("chapter %d start_idx %d", i, c.StartIdx)
		}
		if c.StartMS != page.Sentences[c.StartIdx].StartMS || c.TitleEn == "" || c.TitleZh == "" {
			t.Errorf("chapter %d: %+v", i, c)
		}
	}

	// editing a sentence changes the input: stale, not regenerated
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecAudio+"/sentences/1/edit", map[string]string{"text": "Today we talk about caches."}, 200, nil)
	doJSON(t, "GET", url+"?mock=many", nil, 200, &v)
	if !v.Stale || v.Running {
		t.Errorf("after an edit: stale %v running %v", v.Stale, v.Running)
	}

	// the example data
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecReady+"/chapters", nil, 200, &v)
	if len(v.Chapters) < 3 || v.Stale {
		t.Errorf("L01 example: %+v", v)
	}
	doJSON(t, "GET", srv.URL+"/api/lectures/01JD3631000000000000000004/chapters", nil, 200, &v)
	if !v.Stale {
		t.Errorf("L04 example is not stale: %+v", v)
	}

	// validation failed twice (P7): reported, chapters unchanged
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecReady+"/chapters?mock=error", nil, 202, nil)
	waitChanged(t, events, "chapters", lecReady)
	waitChanged(t, events, "chapters", lecReady)
	doJSON(t, "GET", srv.URL+"/api/lectures/"+lecReady+"/chapters", nil, 200, &v)
	if v.Error == "" || v.Running || len(v.Chapters) == 0 {
		t.Errorf("failed generation: %+v", v)
	}
	doJSON(t, "POST", srv.URL+"/api/lectures/"+lecASR+"/chapters", nil, 409, nil)
}

func waitChanged(t *testing.T, ch chan api.ChangedEvent, kind, id string) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind && ev.ID == id {
				return
			}
		case <-timeout:
			t.Fatalf("no changed %s %s", kind, id)
		}
	}
}

// The lecture JSON carries playable and stream (types.go v2 fields).
func TestLecturePlayerFields(t *testing.T) {
	srv := playerServer(t, nil)
	var ls []api.Lecture
	doJSON(t, "GET", srv.URL+"/api/courses/01JC0000000000000000003631/lectures", nil, 200, &ls)
	got := map[string]string{}
	for _, l := range ls {
		s := "-"
		if l.Stream != nil {
			s = l.Stream.Status
		}
		b, _ := json.Marshal(l.Playable)
		got[l.ID] = string(b) + "/" + s
	}
	want := map[string]string{lecReady: "true/ready", lecPending: "true/pending", lecAudio: "true/audio_only", lecASR: "false/-", "01JD3631000000000000000003": "true/ready"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %s, want %s", id, got[id], w)
		}
	}
}

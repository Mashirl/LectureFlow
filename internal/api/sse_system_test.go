// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// sseReader collects SSE frames from a live connection.
type sseReader struct {
	t      *testing.T
	frames chan sseFrame
}

type sseFrame struct {
	event, data string
	comment     bool
}

func openSSE(t *testing.T, url string) *sseReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		t.Fatalf("content-type %q", ct)
	}
	r := &sseReader{t: t, frames: make(chan sseFrame, 1000)}
	go func() {
		defer close(r.frames)
		sc := bufio.NewScanner(res.Body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f.event != "" || f.comment {
					r.frames <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(line, ":"):
				f.comment, f.data = true, strings.TrimSpace(line[1:])
			case strings.HasPrefix(line, "event: "):
				f.event = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				f.data = line[len("data: "):]
			}
		}
	}()
	t.Cleanup(func() { cancel(); res.Body.Close() })
	return r
}

// next returns the next frame matching keep, failing after timeout.
func (r *sseReader) next(timeout time.Duration, keep func(sseFrame) bool) sseFrame {
	r.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-r.frames:
			if !ok {
				r.t.Fatal("SSE stream closed")
			}
			if keep(f) {
				return f
			}
		case <-deadline:
			r.t.Fatal("timed out waiting for an SSE frame")
		}
	}
}

func waitClients(t *testing.T, h *Hub, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if h.Clients() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("hub has %d clients, want %d", h.Clients(), n)
}

func TestSSEFormatThrottleAndFinalState(t *testing.T) {
	hub := NewHub()
	hub.MinInterval = 300 * time.Millisecond
	hub.PingInterval = 100 * time.Millisecond
	ts := httptest.NewServer(hub)
	t.Cleanup(ts.Close) // registered first, so it runs after openSSE's cleanup closed the stream
	r := openSSE(t, ts.URL)
	waitClients(t, hub, 1)

	// Ping comments keep the connection alive (SPEC §8.2).
	r.next(2*time.Second, func(f sseFrame) bool { return f.comment && f.data == "ping" })

	job := model.Job{ID: "J1", Scope: "lecture", ScopeID: "L1", Stage: "asr", Status: "running"}
	start := time.Now()
	for i := 1; i <= 20; i++ { // a burst of progress updates
		job.Progress = float64(i) / 100
		hub.Job(job)
		time.Sleep(5 * time.Millisecond)
	}
	job.Status, job.Progress = "done", 1 // the final state must arrive
	hub.Job(job)
	hub.Changed("lecture", "L1")

	var jobEvents []JobEvent
	gotChanged := false
	for {
		f := r.next(3*time.Second, func(f sseFrame) bool { return !f.comment })
		switch f.event {
		case "job":
			var ev JobEvent
			dec := json.NewDecoder(strings.NewReader(f.data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ev); err != nil {
				t.Fatalf("job data %q: %v", f.data, err)
			}
			jobEvents = append(jobEvents, ev)
		case "changed":
			var ev ChangedEvent
			if err := json.Unmarshal([]byte(f.data), &ev); err != nil || ev.Kind != "lecture" || ev.ID != "L1" {
				t.Fatalf("changed data %q", f.data)
			}
			gotChanged = true
		default:
			t.Fatalf("unexpected event %q", f.event)
		}
		if gotChanged && len(jobEvents) > 0 && jobEvents[len(jobEvents)-1].Status == "done" {
			break
		}
	}
	// 20 progress updates in ~100ms + the final state: throttling to one per
	// 300ms leaves at most two "running" events.
	running := 0
	for _, ev := range jobEvents {
		if ev.Status == "running" {
			running++
		}
	}
	if running > 2 {
		t.Errorf("%d running events within %v, want ≤ 2 (throttle)", running, time.Since(start))
	}
	last := jobEvents[len(jobEvents)-1]
	if last.Progress != 1 || last.ScopeID != "L1" || last.Stage != "asr" || last.Scope != "lecture" {
		t.Errorf("final event %+v", last)
	}
}

func TestSSESlowClientDoesNotBlock(t *testing.T) {
	hub := NewHub()
	// A client that never reads: Job/Changed must still return immediately.
	ts := httptest.NewServer(hub)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	waitClients(t, hub, 1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			hub.Job(model.Job{ID: "J", Scope: "lecture", ScopeID: "L", Stage: "translate", Status: "running", Progress: float64(i%100) / 100})
			hub.Changed("sentences", "L")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub calls blocked on a slow client")
	}
}

func TestSchedulerEventsReachSSE(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	r := openSSE(t, e.ts.URL+"/api/events")
	waitClients(t, e.hub, 1)
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued")
	var lv Lecture
	e.call("POST", "/api/lectures/"+l.ID+"/stages/prep/cancel", nil, http.StatusOK, &lv)
	f := r.next(3*time.Second, func(f sseFrame) bool { return f.event == "job" && strings.Contains(f.data, `"cancelled"`) })
	var ev JobEvent
	json.Unmarshal([]byte(f.data), &ev)
	if ev.ScopeID != l.ID || ev.Stage != "prep" {
		t.Fatalf("event %+v", ev)
	}
	r.next(3*time.Second, func(f sseFrame) bool { return f.event == "changed" && strings.Contains(f.data, l.ID) })
}

func TestSystemEndpoints(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	byName := map[string]SystemCheck{}
	var names []string
	for _, c := range sys.Checks {
		byName[c.Name] = c
		names = append(names, c.Name)
	}
	want := []string{"data_dir", "ffmpeg", "python", "faster-whisper", "pypdf", "gpu", "cuda", "api_key:deepseek", "api:deepseek", "disk"}
	if !eqStrings(names, want) {
		t.Fatalf("checks %v, want %v", names, want)
	}
	for _, n := range []string{"data_dir", "ffmpeg", "python", "faster-whisper", "pypdf", "gpu", "disk"} {
		if byName[n].Status != "ok" {
			t.Errorf("%s: %+v", n, byName[n])
		}
	}
	if c := byName["cuda"]; c.Status != "warn" || !strings.Contains(c.Detail, "尚未运行") {
		t.Errorf("cuda before recheck: %+v", c)
	}
	if c := byName["ffmpeg"]; c.Detail != "ffmpeg version 9.0.1-test Copyright" {
		t.Errorf("ffmpeg detail %q", c.Detail)
	}
	if sys.GPU.TotalMB != 12000 || sys.GPU.FreeMB != 9000 || sys.GPU.SlotsMax != 1 || sys.GPU.EstimatedJobMB < 5000 {
		t.Errorf("gpu %+v", sys.GPU)
	}
	if a, ok := sys.API["deepseek"]; !ok || a.Max != 8 || a.RPMLimit != 300 {
		t.Errorf("api %+v", sys.API)
	}
	cfg, _ := sys.Config.(map[string]any)
	asr, _ := cfg["asr"].(map[string]any)
	if asr == nil || asr["vram_reserve_mb"] == nil {
		t.Errorf("config view lacks asr.vram_reserve_mb: %v", sys.Config)
	}
	if sys.Disk.FreeMB != 100<<10 {
		t.Errorf("disk %+v", sys.Disk)
	}

	// Recheck runs the CUDA self-test too.
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	for _, c := range sys.Checks {
		if c.Name == "cuda" && (c.Status != "ok" || !strings.HasPrefix(c.Detail, "SELFTEST ok")) {
			t.Errorf("cuda after recheck: %+v", c)
		}
	}

	e.callErr("POST", "/api/system/gpu", map[string]any{}, http.StatusBadRequest)
	e.call("POST", "/api/system/gpu", map[string]any{"paused": true}, http.StatusOK, &sys)
	if !sys.GPU.Paused || !e.sc.GPUPaused() {
		t.Fatal("GPU not paused")
	}
	e.call("POST", "/api/system/gpu", map[string]any{"paused": false}, http.StatusOK, &sys)
	if sys.GPU.Paused {
		t.Fatal("GPU still paused")
	}
}

func TestSystemCheckFailures(t *testing.T) {
	e := newEnv(t, func(s *Server) {
		s.runCmd = func(context.Context, string, ...string) (string, string, error) {
			return "", "Traceback ...\nModuleNotFoundError: No module named 'faster_whisper'", errUnknownCmd
		}
		s.freeSpace = func(string) (int64, error) { return 1 << 30, nil }
	})
	e.srv.diag.waitStartup()
	// v1.1: keys live in the settings; a provider without one is an error row.
	name, typ, base := "nokey", model.ProviderOpenAI, "https://llm.example.invalid/v1"
	if _, err := e.set.CreateProvider(context.Background(), settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base}); err != nil {
		t.Fatal(err)
	}
	var sys System
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	seen := 0
	for _, c := range sys.Checks {
		switch c.Name {
		case "ffmpeg", "python", "faster-whisper", "pypdf", "disk":
			if c.Status != "error" || c.Hint == "" {
				t.Errorf("%s: %+v", c.Name, c)
			}
		case "api_key:nokey":
			seen++
			if c.Status != "error" || c.Hint == "" {
				t.Errorf("api key: %+v", c)
			}
		case "api:nokey":
			seen++
			if c.Status != "warn" || !strings.Contains(c.Detail, "缺少 API key") {
				t.Errorf("api check without key: %+v", c)
			}
		case "api_key:deepseek":
			seen++
			if c.Status != "ok" || !strings.Contains(c.Detail, "sk-…") || strings.Contains(c.Detail, llmtest.TestKey) {
				t.Errorf("api key with key: %+v", c)
			}
		}
		if c.Name == "faster-whisper" && !strings.Contains(c.Detail, "No module named") {
			t.Errorf("import detail %q", c.Detail)
		}
	}
	if seen != 3 {
		t.Errorf("saw %d of the 3 provider rows", seen)
	}
	// Uploads are refused below 2 GB free (SPEC §17.5).
	c := e.course("C")
	res, _ := e.upload("/api/courses/"+c.ID+"/lectures", nil, "a.mp4", []byte("x"))
	if res.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("upload with 1 GB free: %d", res.StatusCode)
	}
}

func TestAudioClip(t *testing.T) {
	e := newEnv(t)
	ffmpeg, err := exec.LookPath(e.cfg.FFmpeg)
	if err != nil {
		t.Skip("ffmpeg not found; audio clip test skipped (SPEC §14)")
	}
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	base := "/api/lectures/" + l.ID + "/audio"
	e.callErr("GET", base+"?start_ms=x&end_ms=1", nil, http.StatusBadRequest)
	e.callErr("GET", base+"?start_ms=5&end_ms=1", nil, http.StatusBadRequest)
	e.callErr("GET", base+"?start_ms=0&end_ms=1000", nil, http.StatusConflict) // no audio.wav yet

	wav := filepath.Join(model.LectureDir(e.cfg.DataDir, c.ID, l.ID), model.AudioFile)
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", wav).CombinedOutput()
	if err != nil {
		t.Fatalf("make audio.wav: %v %s", err, out)
	}
	res, b := e.do("GET", base+"?start_ms=2000&end_ms=3000", nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("clip: %d %v %s", res.StatusCode, res.Header, b[:min(len(b), 200)])
	}
	if !bytes.HasPrefix(b, []byte("RIFF")) || !bytes.Contains(b[:64], []byte("WAVE")) {
		t.Fatalf("not a WAV: % x", b[:min(len(b), 16)])
	}
	// [start-1s, end+1s] = 3 s of 16 kHz 16-bit mono ≈ 96000 bytes of samples.
	i := bytes.Index(b, []byte("data"))
	if i < 0 {
		t.Fatal("no data chunk")
	}
	samples := len(b) - i - 8
	if samples < 94000 || samples > 98000 {
		t.Errorf("clip has %d data bytes, want ≈ 96000", samples)
	}
	// Near the start the window is clamped at 0.
	res, b = e.do("GET", base+"?start_ms=0&end_ms=500", nil)
	if res.StatusCode != http.StatusOK || len(b) < 40000 {
		t.Errorf("clip at 0: %d, %d bytes", res.StatusCode, len(b))
	}
}

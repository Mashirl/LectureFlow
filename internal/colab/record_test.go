// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecorderRoundTrip: recording a full borrowing (played by the replay
// server) yields a redacted sample that replays the same run again. This
// is what cmd/record does against the real service.
func TestRecorderRoundTrip(t *testing.T) {
	var rec *Recorder
	e := newEnv(t, "borrow_run_release.json", func(o *Options) {
		rec = NewRecorder(o.ColabURL, o.TokenURL, o.RevokeURL)
		o.HTTPClient = &http.Client{Transport: rec.Transport(nil)}
		o.Dial = rec.Dialer(DialWebSocket(nil))
	}).withCredential(t).startup(t)
	ctx := context.Background()
	if _, err := e.b.Transcribe(ctx, e.job(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatal(err)
	}
	e.rs.finish()

	f := rec.File("用 cmd/record 录制（测试）", ReplayUpstream{Package: "google-colab-cli", Version: "0.7.5", Files: []string{"(recorded)"}})
	if !f.Recorded || len(f.Steps) != len(e.rs.steps) {
		t.Fatalf("recording: recorded %v, %d steps", f.Recorded, len(f.Steps))
	}
	b, _ := json.MarshalIndent(f, "", " ")
	text := string(b)
	assertNoSecrets(t, "recording", text)
	for _, w := range []string{"Welcome", "Zebrafish", "Vigenère", "127.0.0.1"} {
		if strings.Contains(text, w) {
			t.Errorf("recording contains %q", w)
		}
	}
	path := filepath.Join(t.TempDir(), "recorded.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// Replay the recording.
	rs := newReplayPath(t, path)
	kv := newDiskKV()
	clk := newFakeClock()
	lg, lb := newTestLogger()
	b2, err := New(Options{KV: kv, ColabURL: rs.colabURL(), TokenURL: rs.tokenURL(), RevokeURL: rs.revokeURL(),
		WorkerDir: workerDir(t), FFmpeg: "ffmpeg", RunFFmpeg: fakeFFmpeg(300), Logger: lg, Now: clk.Now, Sleep: clk.Sleep,
		RunID: "run-1", AllowHTTP: true, ChunkSize: 200, ADCPath: filepath.Join(t.TempDir(), "absent.json")})
	if err != nil {
		t.Fatal(err)
	}
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "run-1")
	if err := b2.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "audio.wav")
	_ = os.WriteFile(wav, []byte("RIFF"), 0o644)
	out, err := b2.Transcribe(ctx, Job{ID: "lecture-1", AudioPath: wav, DurationSec: 600, Language: "en", OutDir: dir}, nil)
	if err != nil {
		t.Fatalf("replaying the recording: %v", err)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "[redacted]") {
		t.Errorf("result of the replayed recording: %s", got)
	}
	if err := b2.QueueDrained(ctx); err != nil {
		t.Fatal(err)
	}
	rs.finish()
	assertNoSecrets(t, "log", lb.String())
}

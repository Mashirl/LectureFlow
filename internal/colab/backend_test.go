// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestSettingsLoadAndValidate(t *testing.T) {
	ctx := context.Background()
	kv := NewMemKV()
	if s, err := LoadSettings(ctx, kv); err != nil || s != DefaultSettings() {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	_ = kv.SetKV(ctx, KVSettings, `{"accelerator":"Z9","tier":"free","max_queue_wait_min":5000,"model":"large-v2","compute_type":"int8","idle_release_min":-1}`)
	s, err := LoadSettings(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultSettings()
	// colab-obs-core: the stored tier is ignored (the account's level
	// decides); a never-picked accelerator is stored as the free level's T4
	want.Model, want.ComputeType = "large-v2", "int8"
	if s != want || s.Tier != "" || s.Accelerator != "T4" {
		t.Errorf("field-by-field fallback: %+v", s)
	}
	// Saving never stores a tier.
	withTier := DefaultSettings()
	withTier.Tier = TierPaid
	if err := SaveSettings(ctx, kv, withTier); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := kv.GetKV(ctx, KVSettings); !strings.Contains(v, `"tier":""`) {
		t.Errorf("stored %s", v)
	}
	_ = kv.SetKV(ctx, KVSettings, `not json`)
	if s, _ := LoadSettings(ctx, kv); s != DefaultSettings() {
		t.Errorf("unreadable: %+v", s)
	}
	bad := DefaultSettings()
	bad.Accelerator, bad.AcceleratorChosen = "V6E1", true // a TPU cannot run faster-whisper
	if err := SaveSettings(ctx, kv, bad); Code(err) != CodeSettingsInvalid {
		t.Errorf("TPU accepted: %v", err)
	}
	if f, _ := codedParam(SaveSettings(ctx, kv, Settings{AcceleratorChosen: true}), "field"); f != "accelerator" {
		t.Errorf("first invalid field %q", f)
	}
	hm := DefaultSettings()
	hm.Accelerator, hm.HighMem = "T4", true
	if hm.shape() != "hm" {
		t.Error("high-mem T4 without shape")
	}
	hm.Accelerator = "L4"
	if hm.shape() != "" {
		t.Error("L4 has a single shape (upstream HIGH_MEM_ONLY_ACCELERATORS)")
	}
}

func TestNewNeedsKV(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without KV")
	}
}

func TestTranscribeLocalProblems(t *testing.T) {
	b, kv, clk, _ := newCredBackend(t, nil, "run-1")
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
	ctx := context.Background()
	dir := t.TempDir()
	_, err := b.Transcribe(ctx, Job{ID: "x", AudioPath: filepath.Join(dir, "missing.wav"), DurationSec: 60, OutDir: dir}, nil)
	if Code(err) != CodeAudioMissing || IsFallback(err) || IsRequeue(err) {
		t.Errorf("missing audio: %v", err)
	}
	wav := filepath.Join(dir, "audio.wav")
	_ = os.WriteFile(wav, []byte("RIFF"), 0o644)
	b.o.WorkerDir = t.TempDir()
	_, err = b.Transcribe(ctx, Job{ID: "x", AudioPath: wav, DurationSec: 60, OutDir: dir}, nil)
	if Code(err) != CodeWorkerMissing || !IsFallback(err) {
		t.Errorf("worker missing: %v", err)
	}
}

func TestCanDispatchWhileBusyAndTickWithoutSession(t *testing.T) {
	b, kv, clk, _ := newCredBackend(t, nil, "run-1")
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
	ctx := context.Background()
	b.Tick(ctx) // no session: nothing to do, no network
	b.mu.Lock()
	b.settings.Accelerator, b.settings.AcceleratorChosen = "T4", true // one lecture at a time
	// a paid account that may borrow an L4 (colab-obs-core), read just now
	b.ccu = &ccuSnapshot{info: ccuInfo{Balance: 50, EligibleGPUs: []string{"L4", "T4"}, EligibilityKnown: true}, at: clk.Now()}
	b.ccuTry = clk.Now()
	b.jobs = []*jobState{{id: "j", done: make(chan struct{})}}
	b.mu.Unlock()
	if ok, w := b.CanDispatch(ctx, 600); ok || w != "" {
		t.Errorf("busy: %v %q", ok, w)
	}
	// colab-batch: an L4 takes two at once, a third is busy.
	b.mu.Lock()
	b.settings.Accelerator = "L4"
	b.jobs = append(b.jobs, &jobState{id: "k", done: make(chan struct{})})
	b.mu.Unlock()
	if ok, w := b.CanDispatch(ctx, 600); ok || w != "" || b.Slots() != 2 {
		t.Errorf("busy with two on an L4: %v %q (slots %d)", ok, w, b.Slots())
	}
	b.o.WorkerDir = workerDir(t)
	// colabfix: busy is not a hand-back (the lecture may go to Colab again).
	if _, err := b.Transcribe(ctx, Job{ID: "y", AudioPath: writeTemp(t), DurationSec: 60, OutDir: t.TempDir()}, nil); Code(err) != CodeBusy ||
		!IsBusy(err) || IsFallback(err) || IsRequeue(err) {
		t.Errorf("second lecture: %v", err)
	}
}

// colabfix (accept2): a lecture whose Transcribe starts while the runtime is
// being released (QueueDrained, an idle release) gets IsBusy — not a
// hand-back — and the backend neither releases anything for it nor records
// an error; the running release keeps its session.
func TestTranscribeWhileTheRuntimeIsBeingReleased(t *testing.T) {
	b, kv, clk, lb := newCredBackend(t, nil, "run-1")
	storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
	ctx := context.Background()
	b.o.WorkerDir = workerDir(t)
	set := DefaultSettings()
	set.Tier = TierFree // no balance read (no network in this test)
	b.mu.Lock()
	b.settings = set
	stopping := &liveSession{rec: sessionRecord{Version: recordVersion, State: StateStopping, Accelerator: "T4",
		Endpoint: "ep-1", RequestedAt: clk.Now(), RunID: "run-1"}}
	b.sess = stopping
	b.mu.Unlock()
	if ok, w := b.CanDispatch(ctx, 600); ok || w != "" {
		t.Errorf("CanDispatch while stopping: %v %q", ok, w)
	}
	_, err := b.Transcribe(ctx, Job{ID: "b", AudioPath: writeTemp(t), DurationSec: 600, OutDir: t.TempDir()}, nil)
	if Code(err) != CodeBusy || !IsBusy(err) || IsFallback(err) || IsRequeue(err) || WaitingFor(err) != "" {
		t.Fatalf("Transcribe while the runtime is being released: %v", err)
	}
	b.mu.Lock()
	sess, jobs, lastErr, rel := b.sess, len(b.jobs), b.lastErr, b.lastRelease
	b.mu.Unlock()
	if sess != stopping || jobs != 0 || lastErr != nil || rel != nil {
		t.Errorf("after the busy lecture: sess %p (want %p) jobs %d lastErr %+v lastRelease %+v", sess, stopping, jobs, lastErr, rel)
	}
	if strings.Contains(lb.String(), "back to the local devices") || strings.Contains(lb.String(), "runtime released") {
		t.Errorf("log of a busy lecture:\n%s", lb.String())
	}
	if st := b.Status(ctx); st.LastError != nil {
		t.Errorf("status shows an error for a busy lecture: %+v", st.LastError)
	}
}

func writeTemp(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "audio.wav")
	_ = os.WriteFile(p, []byte("RIFF"), 0o644)
	return p
}

func TestPrepareAudio(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "audio.wav")
	_ = os.WriteFile(wav, []byte("RIFF wav data"), 0o644)
	var gotArgs []string
	run := func(_ context.Context, _ string, args []string) error {
		gotArgs = args
		return os.WriteFile(args[len(args)-1], []byte("fLaC data"), 0o644)
	}
	logf := func(string, ...any) {}
	p, err := prepareAudio(context.Background(), run, "ffmpeg", wav, dir, logf)
	if err != nil || p.Name != "audio.flac" || !p.Temp || string(p.Data) != "fLaC data" || p.SHA256 != sha256Hex([]byte("fLaC data")) {
		t.Fatalf("flac: %+v %v", p, err)
	}
	if !slices.Contains(gotArgs, "flac") || !slices.Contains(gotArgs, wav) || gotArgs[len(gotArgs)-2] != "flac" {
		t.Errorf("args %v", gotArgs)
	}
	if _, err := os.Stat(p.Path); err != nil {
		t.Errorf("flac file: %v", err)
	}
	// ffmpeg fails: the WAV goes up as it is.
	fail := func(context.Context, string, []string) error { return errors.New("ffmpeg: not found") }
	p, err = prepareAudio(context.Background(), fail, "ffmpeg", wav, dir, logf)
	if err != nil || p.Name != "audio.wav" || p.Temp || p.SHA256 != sha256Hex([]byte("RIFF wav data")) {
		t.Fatalf("wav: %+v %v", p, err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) > 0 {
		t.Errorf("temporary files left: %v", m)
	}
	// No ffmpeg configured: the WAV.
	if p, _ := prepareAudio(context.Background(), run, "", wav, dir, logf); p.Name != "audio.wav" {
		t.Errorf("no ffmpeg: %+v", p)
	}
}

func TestCellCodeAndNames(t *testing.T) {
	set := DefaultSettings()
	code := cellCode("content/lectureflow", "content/lectureflow/jobs/lecture-1", attachArgs(set, 2, true))
	for _, want := range []string{`"/content/lectureflow"`, `"/content/lectureflow/jobs/lecture-1"`, "colab_remote.py", "stderr.log",
		"stdout=subprocess.PIPE", "stdin=subprocess.DEVNULL", "raise SystemExit(_rc)", "'--attach', '--base', _b, '--job-dir', _j]",
		`["--model","large-v3","--compute-type","float16","--requirements","faster-whisper==1.2.1,av==15.1.0,ctranslate2==4.8.3","--workers","2","--memory"]`} {
		if !strings.Contains(code, want) {
			t.Errorf("cell code lacks %q", want)
		}
	}
	nbh := newNotebookHash()
	if !regexp.MustCompile(`^[0-9a-f]{8}_[0-9a-f]{4}_4[0-9a-f]{3}_[89ab][0-9a-f]{3}_[0-9a-f]{12}\.{8}$`).MatchString(nbh) || len(nbh) != 44 {
		t.Errorf("notebook hash %q", nbh)
	}
	for in, want := range map[string]string{"01JABC-def_1": "01JABC-def_1", "a/b c": "abc", strings.Repeat("x", 80): strings.Repeat("x", 64)} {
		if got := remoteJobID(in); got != want {
			t.Errorf("remoteJobID(%q) = %q", in, got)
		}
	}
	if id := remoteJobID("///"); len(id) != 16 {
		t.Errorf("empty id → %q", id)
	}
}

func TestStatusShape(t *testing.T) {
	b, kv, clk, _ := newCredBackend(t, nil, "run-1")
	storeCredential(t, kv, Lifecycle{Kind: LifecycleDays, Days: 7}, clk.Now(), "")
	ctx := context.Background()
	b.SetQueue([]float64{3000, 3000, 600})
	// colab-obs-core: before the account was read the level is free: a T4.
	if st := b.Status(ctx); st.Accelerator != "T4" || st.EffectiveAccelerator != "T4" || st.Tier != TierFree || st.Slots != 1 ||
		st.Account.Level != LevelFree || st.Account.ReadOK || st.LockedReason != "" || !slices.Equal(st.SelectableAccelerators, []string{"T4"}) {
		t.Errorf("unread account: %+v", st)
	}
	b.mu.Lock()
	b.ccu = &ccuSnapshot{info: ccuInfo{Balance: 297.7909, EligibleGPUs: []string{"G4", "A100", "L4", "T4"}, EligibilityKnown: true,
		IneligibleGPUs: []string{"H100"}}, at: clk.Now()}
	b.mu.Unlock()
	st := b.Status(ctx)
	// colab-batch: the paid tier's default is the L4 (1.54 CU/h measured, two lectures at once)
	if st.Service != "colab" || !st.Configured || st.State != StateNone || st.Planned != 3 || st.PlannedFit != 3 ||
		st.LecturesLeft != 3 || st.WindowTotalSec != 14400 || st.Auth.RemainingSec != 7*24*3600 || st.CUPerHour != 1.54 ||
		st.Accelerator != "L4" || st.Slots != 2 || st.Tier != TierPaid || st.Account.Level != LevelPaid || !st.Account.ReadOK ||
		*st.Account.PaidBalanceCU != 297.79 || !slices.Equal(st.SelectableAccelerators, []string{"T4", "L4", "G4", "A100"}) {
		t.Errorf("status %+v", st)
	}
	b.SetQueue(nil)
	// 4 h with 50-minute lectures at 3.1 s per audio minute + 90 s: (14400 − 60) / 245 = 58.
	if st := b.Status(ctx); st.LecturesLeft != 58 {
		t.Errorf("approx lectures %d", st.LecturesLeft)
	}
	js, _ := json.Marshal(b.Status(ctx))
	assertNoSecrets(t, "status", string(js))
}

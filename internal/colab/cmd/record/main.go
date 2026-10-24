// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command record makes a real, redacted replay sample of the Colab backend
// (SPEC-v3 V14.6: "用官方 CLI 录制的真实交互做回放测试"). It borrows a real
// Colab runtime with the user's gcloud credentials, transcribes one short
// audio file, releases the runtime and writes what was exchanged, with
// every token, secret and transcript text replaced, in the format of
// internal/colab/testdata/replay.
//
// Run it only with the account owner's consent; it costs compute units:
//
//	gcloud auth application-default login --scopes=openid,https://www.googleapis.com/auth/cloud-platform,https://www.googleapis.com/auth/userinfo.email,https://www.googleapis.com/auth/colaboratory
//	go run ./internal/colab/cmd/record -consent -audio sample/short.wav -out internal/colab/testdata/replay/recorded_borrow_run_release.json
//
// The credential is used for this run only (lifecycle "session", kept in
// memory) and is not revoked: revoking would also sign gcloud's own
// application-default login out. Check the written file before committing
// it (it is meant to hold no secret and no personal data).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"lectureflow/internal/colab"
)

func main() {
	consent := flag.Bool("consent", false, "confirm: borrow a real Colab runtime with my Google account (costs compute units)")
	audio := flag.String("audio", "", "a short WAV file to transcribe (use audio without personal content)")
	out := flag.String("out", "", "where to write the recorded sample (.json)")
	accel := flag.String("accelerator", "T4", "accelerator: T4, L4, G4, A100, H100")
	adc := flag.String("adc", "", "gcloud application default credentials file (default: gcloud's location)")
	workerDir := flag.String("worker", "worker", "directory with colab_remote.py, asr.py, asr_common.py")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "ffmpeg for the FLAC upload (\"\" = upload the WAV)")
	duration := flag.Float64("duration", 60, "audio length in seconds (for the estimate)")
	flag.Parse()
	if !*consent || *audio == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/colab/cmd/record -consent -audio FILE.wav -out SAMPLE.json [-accelerator T4]")
		fmt.Fprintln(os.Stderr, "This borrows a real Colab runtime with your Google account, uploads the audio to Google and costs compute units.")
		os.Exit(2)
	}
	if err := run(*audio, *out, *accel, *adc, *workerDir, *ffmpeg, *duration); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func run(audio, out, accel, adc, workerDir, ffmpeg string, duration float64) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rec := colab.NewRecorder(colab.DefaultColabURL, colab.DefaultTokenURL, colab.DefaultRevokeURL)
	kv := colab.NewMemKV()
	b, err := colab.New(colab.Options{
		KV: kv, HTTPClient: &http.Client{Transport: rec.Transport(nil)}, Dial: rec.Dialer(colab.DialWebSocket(nil)),
		ADCPath: adc, WorkerDir: workerDir, FFmpeg: ffmpeg,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		return err
	}
	if err := b.Startup(ctx); err != nil {
		return err
	}
	set := b.Settings()
	set.Accelerator = accel
	set.MaxQueueWaitMin = 10
	if err := b.SetSettings(ctx, set); err != nil {
		return err
	}
	if _, err := b.ImportADC(ctx, colab.Lifecycle{Kind: colab.LifecycleSession}); err != nil {
		return fmt.Errorf("importing the gcloud credential: %w", err)
	}
	workDir, err := os.MkdirTemp("", "lf-colab-record-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	start := time.Now()
	_, terr := b.Transcribe(ctx, colab.Job{ID: "record-1", AudioPath: audio, DurationSec: duration, Language: "en", OutDir: workDir},
		func(ev colab.Event) {
			fmt.Fprintf(os.Stderr, "%s %s %.0f%%\n", ev.Step, ev.Stage, 100*ev.Overall)
		})
	// Always give the runtime back, also after a failure.
	rerr := b.QueueDrained(context.WithoutCancel(ctx))
	_ = b.Close(context.WithoutCancel(ctx))
	st := b.Status(context.WithoutCancel(ctx))
	fmt.Fprintf(os.Stderr, "took %s; session %s; last release %+v\n", time.Since(start).Round(time.Second), st.State, st.LastRelease)
	src := fmt.Sprintf("用 internal/colab/cmd/record 于 %s 录制的真实交互（已脱敏）。", time.Now().UTC().Format("2006-01-02"))
	if terr != nil {
		src += fmt.Sprintf("本次转写失败：%s。", colab.Code(terr))
	}
	f := rec.File(src, colab.ReplayUpstream{Package: "google-colab-cli", Version: "0.7.5",
		Files: []string{"(recorded against the live service; protocol as in google-colab-cli 0.7.5)"}})
	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d steps). Review it before committing.\n", out, len(f.Steps))
	if terr != nil {
		return fmt.Errorf("transcription failed (the recording was written anyway): %w", terr)
	}
	return rerr
}

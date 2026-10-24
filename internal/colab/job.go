// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The run of one lecture on a ready runtime (V14.3 steps 2–3): upload the
// worker scripts (once per runtime) and the audio, run worker/colab_remote.py
// in the kernel, follow its events, download result.json, check it against
// the done event's sha256, delete the remote files.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// jobState is the running lecture (Status.Current, ShutdownNow).
type jobState struct {
	id      string
	started time.Time
	cancel  context.CancelCauseFunc
	done    chan struct{}
	ev      Event
}

// jobProgress reports progress to the caller and into jobState.
type jobProgress struct {
	b   *Backend
	j   *jobState
	out func(Event)
	est float64 // estimated seconds of the whole lecture (-1 unknown)

	transcribeStart time.Time
}

// overall maps a step's fraction to the lecture's: upload 0–5 %, remote
// 5–95 %, download 95–99 %, verification 100 %.
func overall(step string, f float64) float64 {
	f = max(0, min(1, f))
	switch step {
	case StepUploadAudio:
		return 0.05 * f
	case StepRemoteTranscribe:
		return 0.05 + 0.9*f
	case StepDownloadResult:
		return 0.95 + 0.04*f
	case StepVerify:
		return 0.99 + 0.01*f
	}
	return 0
}

func (p *jobProgress) emit(ev Event) {
	b := p.b
	b.mu.Lock()
	if b.sess != nil {
		ev.SessionState = b.sess.rec.State
	} else {
		ev.SessionState = StateQueued
	}
	if p.j != nil {
		p.j.ev = ev
	}
	b.mu.Unlock()
	p.out(ev)
}

func (p *jobProgress) step(step string, fraction float64) {
	ev := Event{Step: step, Fraction: fraction, Overall: overall(step, fraction), ETASec: p.eta(step, fraction)}
	p.emit(ev)
}

func (p *jobProgress) waiting(waitingFor string) {
	p.emit(Event{Step: StepWaitAccelerator, Fraction: -1, WaitingFor: waitingFor, ETASec: p.est})
}

func (p *jobProgress) remote(stage string, fraction float64) {
	ev := Event{Step: StepRemoteTranscribe, Stage: stage, Fraction: fraction, Overall: overall(StepRemoteTranscribe, max(0, fraction)),
		ETASec: p.eta(StepRemoteTranscribe, fraction)}
	p.emit(ev)
}

// eta: from the transcription's own pace once it is under way, else what
// is left of the estimate.
func (p *jobProgress) eta(step string, f float64) float64 {
	if step == StepRemoteTranscribe && f >= 0.02 && !p.transcribeStart.IsZero() {
		el := p.b.now().Sub(p.transcribeStart).Seconds()
		return el*(1-f)/f + 15
	}
	if p.est < 0 {
		return -1
	}
	el := p.b.now().Sub(p.j.started).Seconds()
	return max(0, p.est-el)
}

// remoteJob is job.json of worker/colab_remote.py (paths relative to the
// job directory).
type remoteJob struct {
	Audio         string   `json:"audio"`
	AudioSHA256   string   `json:"audio_sha256"`
	Model         string   `json:"model"`
	ComputeType   string   `json:"compute_type"`
	Language      string   `json:"language"`
	InitialPrompt string   `json:"initial_prompt"`
	Result        string   `json:"result"`
	Requirements  []string `json:"requirements"`
}

// cellCode is the code the kernel runs: the worker as a subprocess whose
// stdout lines are copied to the cell's stdout (and nothing else); its
// stderr goes to stderr.log in the job directory, which is deleted with
// the job. The kernel stays busy while the worker runs, which keeps the
// runtime alive (upstream: "Sessions stay alive as long as the kernel is
// active").
func cellCode(base, jobDir string) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return fmt.Sprintf(`import os, subprocess, sys
_b = %s
_j = %s
with open(os.path.join(_j, 'stderr.log'), 'wb') as _e:
    _p = subprocess.Popen([sys.executable, '-u', os.path.join(_b, 'colab_remote.py'), '--job', os.path.join(_j, 'job.json'), '--log-dir', _b], stdout=subprocess.PIPE, stderr=_e, stdin=subprocess.DEVNULL, cwd=_j)
    for _l in iter(_p.stdout.readline, b''):
        sys.stdout.write(_l.decode('utf-8', 'replace'))
        sys.stdout.flush()
    _rc = _p.wait()
if _rc:
    raise SystemExit(_rc)
`, q("/"+base), q("/"+jobDir))
}

// jobMetrics are the measurements of one finished lecture.
type jobMetrics struct {
	audioSec, transcribeSec, overheadSec, setupSec float64
	// cleanupFailed: a remote file of the job (a fixed name, e.g.
	// result.json) could not be deleted even after the retries; the
	// runtime is released at once (its disk goes with it).
	cleanupFailed string
}

func (b *Backend) runJob(ctx context.Context, s *liveSession, set Settings, job Job, id string,
	files map[string][]byte, hash string, jp *jobProgress) (_ string, m jobMetrics, err error) {
	start := b.now()
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return "", m, err
	}
	base := b.o.RemoteBase
	b.mu.Lock()
	kid, uploaded := s.kernelID, s.workerHash
	b.mu.Unlock()
	if kid == "" {
		if kid, err = rt.startKernel(ctx); err != nil {
			return "", m, err
		}
		b.mu.Lock()
		s.kernelID = kid
		b.mu.Unlock()
	}
	if uploaded != hash {
		if err := rt.mkdir(ctx, base); err != nil {
			return "", m, err
		}
		for _, name := range workerFiles {
			// small files go in one piece (upstream's single "chunk": 1)
			if err := rt.upload(ctx, base+"/"+name, files[name], len(files[name])+1, nil); err != nil {
				return "", m, uploadErr(name, err)
			}
		}
		b.mu.Lock()
		s.workerHash = hash
		b.mu.Unlock()
	}
	jobDir := base + "/jobs/" + id
	if err := rt.mkdir(ctx, base+"/jobs"); err != nil {
		return "", m, err
	}
	if err := rt.mkdir(ctx, jobDir); err != nil {
		return "", m, err
	}
	defer func() {
		// After a success every remote file must go (retried); after a
		// failure one attempt is enough: the runtime is released next. A
		// failure because the lecture was stopped (立即关机, a cancel)
		// skips the cleanup: the release follows at once and removes
		// everything faster. A lecture stopped only after its result was
		// downloaded, checked and written still cleans up (Gate round 2:
		// cleanupRemote runs on a context of its own) and finishJob
		// releases the runtime.
		if err != nil && ctx.Err() != nil {
			return
		}
		if f := b.cleanupRemote(ctx, s, jobDir, err == nil); f != "" && err == nil {
			m.cleanupFailed = f
		}
	}()

	jp.step(StepUploadAudio, 0)
	pa, err := prepareAudio(ctx, b.o.RunFFmpeg, b.o.FFmpeg, job.AudioPath, job.WorkDir, b.log.Info)
	if err != nil {
		return "", m, err
	}
	if pa.Temp {
		defer os.Remove(pa.Path)
	}
	if rt, err = b.runtime(ctx, s); err != nil {
		return "", m, err
	}
	if err := rt.upload(ctx, jobDir+"/"+pa.Name, pa.Data, b.o.ChunkSize, func(done, total int64) {
		jp.step(StepUploadAudio, float64(done)/float64(max(1, total)))
	}); err != nil {
		return "", m, uploadErr(pa.Name, err)
	}
	pa.Data = nil
	rj, _ := json.Marshal(remoteJob{
		Audio: pa.Name, AudioSHA256: pa.SHA256, Model: set.Model, ComputeType: set.ComputeType, Language: job.Language,
		InitialPrompt: job.InitialPrompt, Result: "result.json", Requirements: RemoteRequirements,
	})
	if err := rt.upload(ctx, jobDir+"/job.json", rj, len(rj)+1, nil); err != nil {
		return "", m, uploadErr("job.json", err)
	}

	// Remote transcription.
	jp.remote("", 0)
	var (
		lastStage string
		lastTS    float64
		setupSec  float64
		fraction  float64 // the transcription's progress; stages after it keep it
	)
	stream := &eventStream{onEvent: func(ev remoteEvent) error {
		switch ev.Type {
		case "stage":
			if lastStage == "install" || lastStage == "download_model" {
				setupSec += max(0, ev.TS-lastTS)
			}
			lastStage, lastTS = ev.Stage, ev.TS
			if ev.Stage == "transcribe" {
				jp.transcribeStart = b.now()
			}
			jp.remote(ev.Stage, fraction)
		case "progress":
			fraction = max(fraction, ev.Fraction)
			jp.remote(lastStage, fraction)
		case "done", "error":
			if lastStage == "install" || lastStage == "download_model" {
				setupSec += max(0, ev.TS-lastTS)
			}
		}
		return nil
	}}
	out, err := rt.kernelExec(ctx, kid, cellCode(base, jobDir), stream.feed, b.o.Sleep)
	if err != nil {
		return "", m, err
	}
	final, err := stream.end()
	if err != nil {
		return "", m, err
	}
	if final.Type == "error" {
		return "", m, fallbackErr(CodeRemoteFailed, map[string]any{"reason": final.ErrCode},
			fmt.Errorf("the Colab worker failed: %s (exception %s, retryable %v)", final.ErrCode, orDash(final.Exc), final.Retryable))
	}
	if out.Status != "ok" {
		// A done event and yet the cell failed: the worker's exit code was
		// not 0 after it reported success. Trust nothing.
		return "", m, fallbackErr(CodeWorkerExited, nil, fmt.Errorf("the cell ended with status %s (%s)", safeToken(out.Status), orDash(out.EName)))
	}

	// Download and verify.
	jp.step(StepDownloadResult, 0)
	if rt, err = b.runtime(ctx, s); err != nil {
		return "", m, err
	}
	data, err := rt.download(ctx, jobDir+"/"+final.Result)
	if err != nil {
		if st := statusOf(err); st == 404 {
			return "", m, fallbackErr(CodeDownloadFailed, map[string]any{"file": final.Result}, errors.New("result file missing on the runtime"))
		}
		return "", m, err
	}
	jp.step(StepVerify, 0)
	if int64(len(data)) != final.Bytes || sha256Hex(data) != final.SHA256 {
		return "", m, fallbackErr(CodeHashMismatch, map[string]any{"file": final.Result},
			fmt.Errorf("downloaded %d bytes, the worker reported %d; sha256 differs", len(data), final.Bytes))
	}
	var probe struct {
		Sentences []json.RawMessage `json:"sentences"`
	}
	if json.Unmarshal(data, &probe) != nil || probe.Sentences == nil {
		return "", m, violation("result.json is not a transcription result")
	}
	out2 := filepath.Join(job.OutDir, "result.json")
	if err := writeAtomic(out2, data); err != nil {
		return "", m, newErr(CodeLocalWrite, map[string]any{"file": "result.json"}, kindLocal, "", err)
	}
	jp.step(StepVerify, 1)
	m.audioSec = final.AudioS
	if m.audioSec <= 0 {
		m.audioSec = job.DurationSec
	}
	m.transcribeSec = final.TranscribeS
	m.setupSec = setupSec
	m.overheadSec = max(0, b.now().Sub(start).Seconds()-final.TranscribeS-setupSec)
	return out2, m, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func uploadErr(name string, err error) error {
	if Code(err) != "" || errors.Is(err, context.Canceled) {
		return err
	}
	if st := statusOf(err); st == 401 || st == 403 || st == 404 || st == 410 {
		return fallbackErr(CodeSessionReclaimed, nil, fmt.Errorf("upload of %s: %s", name, err))
	}
	if transient(err) {
		return fallbackErr(CodeUploadFailed, map[string]any{"file": name}, err)
	}
	return err
}

// cleanupWaits are the waits before the retries of a remote deletion
// after a successful lecture (Gate round 1 item 8: three retries, growing
// waits).
var cleanupWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// cleanupTimeout bounds the whole cleanup (the retries included).
const cleanupTimeout = 60 * time.Second

// cleanupStoppedTimeout bounds the cleanup of a lecture that was stopped
// (立即关机, disconnect, a cancel, the credential ending) after its result
// was safely written: one attempt per file, then the runtime is released,
// so the stop still takes effect within seconds.
const cleanupStoppedTimeout = 10 * time.Second

// remoteJobFiles are the files a job may leave in its directory.
var remoteJobFiles = []string{"audio.flac", "audio.wav", "job.json", "result.json", "result.json.tmp", "stderr.log"}

// cleanupRemote deletes the job's files on the runtime. ctx is the
// lecture's context (jctx); the deletions run on a context detached from it
// with a time limit of their own, so a stop that arrives after the result
// was written (or during the cleanup) does not leave the files behind.
//
// With retry (after a successful lecture) each deletion is retried with
// growing waits; the first file that still cannot be deleted ends the
// cleanup and is returned, and the caller releases the runtime at once (its
// disk is reclaimed with it), so neither the uploaded audio nor result.json
// stays on Colab. Without retry (the lecture failed, the runtime is
// released next) every file gets one attempt. Once ctx is cancelled no
// deletion is retried and the first failure ends the cleanup without a
// file: the caller releases the runtime for the stop anyway.
// The log names only the fixed file name and an error code, never content.
func (b *Backend) cleanupRemote(ctx context.Context, s *liveSession, jobDir string, retry bool) (failed string) {
	timeout := cleanupTimeout
	if ctx.Err() != nil {
		timeout = cleanupStoppedTimeout
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// The waits between retries end as soon as the lecture is stopped.
	wctx, wcancel := context.WithCancel(cctx)
	defer wcancel()
	defer context.AfterFunc(ctx, wcancel)()
	for _, name := range remoteJobFiles {
		var err error
		tried := 0
		for a := 0; ; a++ {
			if a > 0 {
				if !retry || a > len(cleanupWaits) || ctx.Err() != nil {
					break
				}
				if b.o.Sleep(wctx, cleanupWaits[a-1]) != nil || ctx.Err() != nil {
					break
				}
			}
			tried++
			var rt *runtimeClient
			if rt, err = b.runtime(cctx, s); err == nil {
				err = rt.remove(cctx, jobDir+"/"+name)
			}
			if err == nil {
				break
			}
		}
		if err == nil {
			continue
		}
		switch {
		case ctx.Err() != nil:
			b.log.Info("colab: deleting a remote file failed after the lecture was stopped (the runtime is released next)",
				"file", name, "attempts", tried, "code", Code(codedHTTP("api/contents", err)))
			return ""
		case !retry:
			b.log.Info("colab: deleting a remote file failed (the runtime is released next)", "file", name,
				"code", Code(codedHTTP("api/contents", err)))
			continue
		}
		b.log.Warn("colab: deleting a remote file failed after retries; releasing the runtime", "file", name,
			"attempts", tried, "code", Code(codedHTTP("api/contents", err)))
		return name
	}
	if rt, err := b.runtime(cctx, s); err == nil {
		_ = rt.remove(cctx, jobDir) // the empty directory: nothing in it to protect
	}
	return ""
}

// finishJob records the measurement and releases the runtime when asked
// to after this lecture. jctx is the lecture's context: when it was
// cancelled after the result had been written (立即关机, disconnect, the
// credential ending, a cancel), the runtime is released now whatever the
// cleanup did — callers such as ShutdownNow rely on it being gone.
func (b *Backend) finishJob(ctx, jctx context.Context, s *liveSession, set Settings, job Job, m jobMetrics) {
	b.mu.Lock()
	s.setupDone = true
	s.lastJobEnd = b.now()
	s.rec.Jobs++
	rec := s.rec
	key := rateKey(rec.Accelerator, set.Model, set.ComputeType)
	b.rates[key] = b.rates[key].Add(Sample{AudioSec: m.audioSec, TranscribeSec: m.transcribeSec, OverheadSec: m.overheadSec})
	rates, _ := json.Marshal(b.rates)
	after, drained := b.afterCurrent, b.drained && len(b.queue) == 0
	b.mu.Unlock()
	b.persist(ctx, rec)
	if err := b.o.KV.SetKV(context.WithoutCancel(ctx), KVRates, string(rates)); err != nil {
		b.log.Warn("colab: saving the measured rate", "err", err)
	}
	b.log.Info("colab: lecture transcribed on Colab", "audio_min", int(m.audioSec/60), "transcribe_s", int(m.transcribeSec),
		"overhead_s", int(m.overheadSec), "setup_s", int(m.setupSec))
	switch {
	case jctx.Err() != nil:
		_ = b.release(ctx, stopReason(jctx))
	case m.cleanupFailed != "":
		// Gate round 1 item 8: a job file (the audio, result.json) stays on
		// the runtime: release it now, its disk goes with it.
		_ = b.releaseWith(ctx, ReleaseCleanupFailed, m.cleanupFailed)
	case after:
		_ = b.release(ctx, ReleaseAfterCurrent)
	case drained:
		_ = b.release(ctx, ReleaseQueueEmpty)
	case set.IdleReleaseMin == 0:
		_ = b.release(ctx, ReleaseIdle) // "0 = right after each lecture"
	}
}

// writeAtomic writes path through path.tmp and a rename.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

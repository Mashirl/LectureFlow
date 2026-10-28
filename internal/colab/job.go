// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The run of one lecture on a ready runtime (V14.3 steps 2–3; colab-batch).
//
// Once per runtime (prepare): the first kernel, the memory probe (memory
// mode), the worker scripts, the jobs directory. Per lecture: its job
// directory, then the cell that attaches to the runtime's resident worker
// (worker/colab_remote.py --attach; the first one starts the worker, which
// installs the environment, downloads and loads the model while the audio
// uploads: colab-prof R1), then the audio (unless it was uploaded ahead of
// its turn, batch.go) and job.json last; the cell's stdout carries the
// lecture's events; result.json is downloaded and checked against the done
// event's sha256 as soon as that event arrives (R3); after a success the
// job's directory is removed in the background (removal.go), after a
// failure its files are deleted in parallel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/remote"
)

// jobState is a running lecture (Status.Jobs, ShutdownNow).
type jobState struct {
	id      string // the remote job directory's name
	key     string // Job.ID
	audio   string // Job.AudioPath
	durSec  float64
	started time.Time
	cancel  context.CancelCauseFunc
	done    chan struct{}
	ev      Event
	// sent: its audio is on the runtime (uploaded, or ahead of its turn);
	// until then no upload ahead starts (a lecture that runs goes first;
	// guarded by Backend.mu).
	sent bool
	// split: the parts of "极速模式" (0 = whole; guarded by Backend.mu)
	split int
}

// jobProgress reports progress to the caller and into jobState.
type jobProgress struct {
	b   *Backend
	j   *jobState
	out func(Event)
	est float64 // estimated seconds of the whole lecture (-1 unknown)

	tsMu            sync.Mutex
	transcribeStart time.Time // the remote transcription began (guarded by tsMu)
}

func (p *jobProgress) setTranscribeStart(t time.Time) {
	p.tsMu.Lock()
	p.transcribeStart = t
	p.tsMu.Unlock()
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
	p.tsMu.Lock()
	ts := p.transcribeStart
	p.tsMu.Unlock()
	if step == StepRemoteTranscribe && f >= 0.02 && !ts.IsZero() {
		el := p.b.now().Sub(ts).Seconds()
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
	// Memory: the job directory is on a RAM disk (memory mode); the worker
	// keeps its packages, the model and temporary files there too. Absent
	// without memory mode, so job.json is byte for byte what it was before.
	Memory bool `json:"memory,omitempty"`
	// Redecode is false when the switch "修复退化的转写窗口" is off
	// (Job.NoRedecode); absent (the worker's default, on) otherwise, so
	// job.json stays byte for byte what it was before.
	Redecode *bool `json:"redecode,omitempty"`
	// Split: "极速模式" (Settings.FastMode on a larger GPU) cuts the lecture
	// into this many parts at silences, transcribed at once; absent = 1.
	Split int `json:"split,omitempty"`
}

// cellCode is the code the kernel runs for one lecture: the attaching
// process as a subprocess whose stdout lines are copied to the cell's
// stdout (and nothing else); its stderr goes to stderr.log in the job
// directory, which is deleted with the job. The kernel stays busy while the
// lecture runs, which keeps the runtime alive (upstream: "Sessions stay
// alive as long as the kernel is active"). args are the resident worker's
// options (attachArgs).
func cellCode(base, jobDir string, args []string) string {
	q := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	return fmt.Sprintf(`import os, subprocess, sys
_b = %s
_j = %s
with open(os.path.join(_j, 'stderr.log'), 'ab') as _e:
    _p = subprocess.Popen([sys.executable, '-u', os.path.join(_b, 'colab_remote.py'), '--attach', '--base', _b, '--job-dir', _j] + %s, stdout=subprocess.PIPE, stderr=_e, stdin=subprocess.DEVNULL, cwd=_j)
    for _l in iter(_p.stdout.readline, b''):
        sys.stdout.write(_l.decode('utf-8', 'replace'))
        sys.stdout.flush()
    _rc = _p.wait()
if _rc:
    raise SystemExit(_rc)
`, q("/"+base), q("/"+jobDir), q(args))
}

// attachArgs are the resident worker's options: model, precision, pins,
// how many lectures (or parts of one) it transcribes at once, memory mode.
func attachArgs(set Settings, workers int, memory bool) []string {
	a := []string{"--model", set.Model, "--compute-type", set.ComputeType,
		"--requirements", strings.Join(RemoteRequirements, ","), "--workers", fmt.Sprint(max(1, workers))}
	if memory {
		a = append(a, "--memory")
	}
	return a
}

// jobMetrics are the measurements of one finished lecture.
type jobMetrics struct {
	audioSec, transcribeSec, overheadSec, setupSec float64
	// memory: the memory probe's decision (nil: memory mode off).
	memory *memoryDecision
	// memPeakMB, memTotalMB: the runtime's peak used memory while the
	// worker ran, and its total (MiB; -1 unknown).
	memPeakMB, memTotalMB int64
	// ahead: the audio had been uploaded ahead of the lecture's turn.
	ahead bool
}

// storage names where the lecture's files were for the log: disk (memory
// mode off), memory, or disk_fallback.
func (m jobMetrics) storage() string {
	switch {
	case m.memory == nil:
		return "disk"
	case m.memory.memory():
		return "memory"
	}
	return "disk_fallback"
}

// ---- once per runtime ----

// sessionPrep is what prepare did on a runtime.
type sessionPrep struct {
	done chan struct{}
	err  error
	base string
	// memory: the memory probe's decision (nil: memory mode off).
	memory *memoryDecision
	// memBudgetMB is the memory for recordings on the RAM disk: what the
	// probe found available minus its estimate for the first lecture plus
	// that lecture's own recording share (memory mode; -1 = the disk, no
	// limit). Lectures uploaded ahead must fit it (batch.go).
	memBudgetMB int64
}

func (p *sessionPrep) memoryOn() bool { return p.memory != nil && p.memory.memory() }

// prepare runs once per runtime: the first kernel, the memory probe (the
// decision holds for the runtime: its resident worker lives on that
// storage), the worker scripts and the jobs directory. A lecture arriving
// meanwhile waits for it; after a failure the next lecture tries again.
func (b *Backend) prepare(ctx context.Context, s *liveSession, set Settings, job Job, files map[string][]byte, hash string) (*sessionPrep, error) {
	for {
		b.mu.Lock()
		p := s.prep
		owner := p == nil
		if owner {
			p = &sessionPrep{done: make(chan struct{}), memBudgetMB: -1}
			s.prep = p
		}
		b.mu.Unlock()
		if owner {
			p.err = b.doPrepare(ctx, s, set, job, files, hash, p)
			if p.err != nil {
				b.mu.Lock()
				if s.prep == p {
					s.prep = nil
				}
				b.mu.Unlock()
			}
			close(p.done)
			if p.err == nil {
				b.kickPlan()
			}
			return p, p.err
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if p.err == nil {
			return p, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (b *Backend) doPrepare(ctx context.Context, s *liveSession, set Settings, job Job, files map[string][]byte, hash string, p *sessionPrep) error {
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return err
	}
	kid, err := rt.startKernel(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	s.kernels = append(s.kernels, kid)
	if s.kernelID == "" {
		s.kernelID = kid
	}
	uploaded := s.workerHash
	b.mu.Unlock()
	base := b.o.RemoteBase
	if b.memoryMode(ctx) == MemoryModeAuto {
		// Memory mode: decide before anything is uploaded (memory.go).
		d, err := b.probeMemory(ctx, rt, kid, set, job, files["colab_remote.py"])
		if err != nil {
			return err
		}
		p.memory = &d
		if d.memory() {
			base = strings.TrimPrefix(d.Base, "/")
			wavMB := int64(0)
			if fi, err := os.Stat(job.AudioPath); err == nil {
				wavMB = (fi.Size() + 1<<20 - 1) >> 20
			}
			p.memBudgetMB = max(0, d.MemAvailableMB-d.NeedMB+2*wavMB)
		}
		if rt, err = b.runtime(ctx, s); err != nil {
			return err
		}
	}
	// The scripts are uploaded once per runtime and directory.
	if uploaded != base+"\x00"+hash {
		if err := rt.mkdir(ctx, base); err != nil {
			return err
		}
		for _, name := range workerFiles {
			// small files go in one piece (upstream's single "chunk": 1)
			if err := rt.upload(ctx, base+"/"+name, files[name], len(files[name])+1, nil); err != nil {
				return uploadErr(name, err)
			}
		}
		b.mu.Lock()
		s.workerHash = base + "\x00" + hash
		b.mu.Unlock()
	}
	if err := rt.mkdir(ctx, base+"/jobs"); err != nil {
		return err
	}
	b.mu.Lock()
	p.base = base
	s.idle = append(s.idle, kid)
	acc := s.rec.Accelerator
	s.workers = workersFor(acc, set.FastMode)
	b.mu.Unlock()
	return nil
}

// takeKernel returns a kernel without a running cell, starting one when
// every kernel is busy (two lectures at once on the larger GPUs).
func (b *Backend) takeKernel(ctx context.Context, s *liveSession) (string, error) {
	b.mu.Lock()
	if n := len(s.idle); n > 0 {
		kid := s.idle[n-1]
		s.idle = s.idle[:n-1]
		b.mu.Unlock()
		return kid, nil
	}
	b.mu.Unlock()
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return "", err
	}
	kid, err := rt.startKernel(ctx)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	s.kernels = append(s.kernels, kid)
	b.mu.Unlock()
	return kid, nil
}

// giveKernel puts a kernel whose cell has ended back. One whose cell may
// still run (the lecture was stopped) is not used again: the attaching
// process in it ends once the job directory is gone, and the release shuts
// every kernel down.
func (b *Backend) giveKernel(s *liveSession, kid string, ended bool) {
	if !ended {
		return
	}
	b.mu.Lock()
	s.idle = append(s.idle, kid)
	b.mu.Unlock()
}

// ---- one lecture ----

// audioFuture is the FLAC encoding started at the beginning of Transcribe
// (colab-prof R3: it overlaps the wait for the accelerator).
type audioFuture struct {
	done chan struct{}
	stop context.CancelFunc // ends the encoding (discard)
	pa   preparedAudio
	err  error
}

func (b *Backend) startAudio(ctx context.Context, job Job) *audioFuture {
	actx, stop := context.WithCancel(ctx)
	f := &audioFuture{done: make(chan struct{}), stop: stop}
	go func() {
		defer close(f.done)
		f.pa, f.err = prepareAudio(actx, b.o.RunFFmpeg, b.o.FFmpeg, job.AudioPath, job.WorkDir, b.log.Info)
	}()
	return f
}

func (f *audioFuture) wait(ctx context.Context) (preparedAudio, error) {
	select {
	case <-f.done:
		return f.pa, f.err
	case <-ctx.Done():
		return preparedAudio{}, ctx.Err()
	}
}

// discard ends the encoding when it still runs (ffmpeg is killed with its
// context: milliseconds) and removes the temporary FLAC before it returns,
// so nothing of the upload is left in the lecture's directory once
// Transcribe has returned. (It used to remove the file in a goroutine: a
// caller looking right after Transcribe, or a test's temporary directory
// cleanup, could still find it — the lead's flaky
// TestReplayBorrowTranscribeRelease, colab-obs-core.)
func (f *audioFuture) discard() {
	if f == nil {
		return
	}
	f.stop()
	<-f.done
	if f.err == nil && f.pa.Temp {
		_ = os.Remove(f.pa.Path)
	}
}

// runOutcome tells the caller what is left to clean up after a failure.
type runOutcome struct {
	jobDir   string // "" before the job directory existed
	cellLive bool   // the cell may still run (its kernel is not reused)
}

func (b *Backend) runJob(ctx context.Context, s *liveSession, set Settings, job Job, j *jobState,
	files map[string][]byte, hash string, af *audioFuture, jp *jobProgress) (_ string, m jobMetrics, out runOutcome, err error) {
	start := b.now()
	prep, err := b.prepare(ctx, s, set, job, files, hash)
	if err != nil {
		return "", m, out, err
	}
	m.memory = prep.memory
	memory := prep.memoryOn()
	base := prep.base

	// A lecture uploaded ahead of its turn (batch.go) keeps its directory.
	ahead, err := b.claimUpload(ctx, s, job)
	if err != nil {
		return "", m, out, err
	}
	if ahead != nil {
		b.mu.Lock()
		j.id = ahead.rid
		b.mu.Unlock()
		m.ahead = true
	}
	jobDir := base + "/jobs/" + j.id
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return "", m, out, err
	}
	if ahead == nil {
		if err := rt.mkdir(ctx, jobDir); err != nil {
			return "", m, out, err
		}
	}
	out.jobDir = jobDir

	// The cell first: the resident worker prepares while the audio uploads.
	kid, err := b.takeKernel(ctx, s)
	if err != nil {
		return "", m, out, err
	}
	b.mu.Lock()
	workers, acc := s.workers, s.rec.Accelerator
	j.split = splitFor(acc, set.FastMode)
	b.mu.Unlock()
	cell := b.startCell(ctx, rt, kid, cellCode(base, jobDir, attachArgs(set, workers, memory)), s, jobDir, jp)
	select {
	case <-cell.started:
	case <-cell.ended:
	}
	out.cellLive = true
	if cell.finished() && cell.err != nil {
		out.cellLive = false
		b.giveKernel(s, kid, false)
		return "", m, out, cell.err
	}

	if err := b.sendLecture(ctx, s, set, job, jobDir, ahead, af, cell, jp, memory, splitFor(acc, set.FastMode)); err != nil {
		cell.abandon()
		return "", m, out, err
	}

	<-cell.ended
	if cell.err != nil {
		// kernelExec stopped following the cell (a cancel, a broken
		// connection, output outside the contract): the cell may still run,
		// its kernel is not used again.
		return "", m, out, cell.err
	}
	out.cellLive = false
	// The kernel is idle: back to the others after a failure; after a
	// success it removes the lecture's directory first (removal.go).
	success := false
	defer func() {
		if !success {
			b.giveKernel(s, kid, true)
		}
	}()
	final, err := cell.stream.end()
	if err != nil {
		return "", m, out, err
	}
	if final.Type == "error" {
		return "", m, out, fallbackErr(CodeRemoteFailed, map[string]any{"reason": final.ErrCode},
			fmt.Errorf("the Colab worker failed: %s (exception %s, retryable %v)", final.ErrCode, orDash(final.Exc), final.Retryable))
	}
	if cell.outcome.Status != "ok" {
		// A done event and yet the cell failed: the worker's exit code was
		// not 0 after it reported success. Trust nothing.
		return "", m, out, fallbackErr(CodeWorkerExited, nil, fmt.Errorf("the cell ended with status %s (%s)", safeToken(cell.outcome.Status), orDash(cell.outcome.EName)))
	}
	data, err := cell.result(ctx)
	if err != nil {
		return "", m, out, err
	}
	out2 := filepath.Join(job.OutDir, "result.json")
	if err := writeAtomic(out2, data); err != nil {
		return "", m, out, newErr(CodeLocalWrite, map[string]any{"file": "result.json"}, kindLocal, "", err)
	}
	jp.step(StepVerify, 1)
	m.audioSec = final.AudioS
	if m.audioSec <= 0 {
		m.audioSec = job.DurationSec
	}
	m.transcribeSec = final.TranscribeS
	m.setupSec = cell.setupSec
	m.memPeakMB, m.memTotalMB = final.MemPeakMB, final.MemTotalMB
	m.overheadSec = max(0, b.now().Sub(start).Seconds()-final.TranscribeS-cell.setupSec)
	// After a success every remote file must go: in the background, the
	// lecture is done (removal.go: a release waits for it; a file that
	// stays releases the runtime once no lecture runs).
	success = true
	b.startRemoval(s, kid, base, jobDir)
	return out2, m, runOutcome{}, nil
}

// sendLecture uploads the audio (unless it went ahead of its turn) and
// job.json, the worker's signal to start (it is written last). When the
// cell has ended already (the worker's setup failed, the connection broke)
// nothing is uploaded: the caller reads the cell's outcome.
func (b *Backend) sendLecture(ctx context.Context, s *liveSession, set Settings, job Job, jobDir string, ahead *preUpload,
	af *audioFuture, cell *lectureCell, jp *jobProgress, memory bool, split int) error {
	jp.step(StepUploadAudio, 0)
	var name, sum string
	defer func() {
		// sent (or given up): the planner may upload ahead again
		b.mu.Lock()
		jp.j.sent = true
		b.mu.Unlock()
		b.kickPlan()
	}()
	if ahead != nil {
		name, sum = ahead.name, ahead.sha
		jp.step(StepUploadAudio, 1)
	} else {
		if af == nil {
			// It was to go ahead but did not (the upload failed, the audio
			// changed since): encode it now.
			af = b.startAudio(ctx, job)
			defer af.discard()
		}
		pa, err := af.wait(ctx)
		if err != nil {
			return err
		}
		if cell.finished() {
			return nil
		}
		rt, err := b.runtime(ctx, s)
		if err != nil {
			return err
		}
		s.upMu.Lock()
		err = rt.upload(ctx, jobDir+"/"+pa.Name, pa.Data, b.o.ChunkSize, func(done, total int64) {
			jp.step(StepUploadAudio, float64(done)/float64(max(1, total)))
		})
		s.upMu.Unlock()
		if err != nil {
			// R1: the caller deletes the job directory, so the attaching
			// process in the cell ends.
			return uploadErr(pa.Name, err)
		}
		name, sum = pa.Name, pa.SHA256
	}
	if cell.finished() {
		return nil
	}
	rjob := remoteJob{
		Audio: name, AudioSHA256: sum, Model: set.Model, ComputeType: set.ComputeType, Language: job.Language,
		InitialPrompt: job.InitialPrompt, Result: "result.json", Requirements: RemoteRequirements, Memory: memory,
		Split: split,
	}
	if job.NoRedecode {
		off := false
		rjob.Redecode = &off
	}
	rj, _ := json.Marshal(rjob)
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return err
	}
	if err := rt.upload(ctx, jobDir+"/job.json", rj, len(rj)+1, nil); err != nil {
		return uploadErr("job.json", err)
	}
	cell.uploaded()
	return nil
}

// lectureCell is the attaching cell of one lecture, running in a
// goroutine: its events, the result fetched as soon as the done event
// arrives, and its outcome.
type lectureCell struct {
	b       *Backend
	s       *liveSession
	jobDir  string
	jp      *jobProgress
	lctx    context.Context // the lecture's: the result fetch outlives the cell
	stream  *eventStream
	started chan struct{}
	ended   chan struct{}
	outcome execOutcome
	err     error
	stop    context.CancelFunc

	mu        sync.Mutex
	up        bool    // job.json is written: remote events go to the caller
	lastStage string  // the remote stage last reported
	fraction  float64 // the transcription's progress; stages after it keep it
	setupSec  float64
	lastTS    float64
	// held: the remote stages / progress that came before job.json was
	// written (at most maxHeld, the latest), reported by uploaded; doneEv: a
	// done event that came before it (uploaded starts its fetch).
	held     []heldState
	doneEv   *remoteEvent
	fetch    chan struct{} // closed when the result was fetched and checked (guarded by mu)
	data     []byte
	fetchErr error
}

func (b *Backend) startCell(ctx context.Context, rt *runtimeClient, kid, code string, s *liveSession, jobDir string, jp *jobProgress) *lectureCell {
	cctx, stop := context.WithCancel(ctx)
	c := &lectureCell{b: b, s: s, jobDir: jobDir, jp: jp, lctx: ctx, started: make(chan struct{}), ended: make(chan struct{}), stop: stop}
	c.stream = &eventStream{onEvent: c.onEvent}
	var once sync.Once
	go func() {
		defer close(c.ended)
		defer stop()
		c.outcome, c.err = rt.kernelExec(cctx, kid, code, c.stream.feed, b.o.Sleep, func() { once.Do(func() { close(c.started) }) })
	}()
	return c
}

func (c *lectureCell) finished() bool {
	select {
	case <-c.ended:
		return true
	default:
		return false
	}
}

// abandon stops following the cell (the lecture failed before its
// job.json): the caller deletes the job directory, which ends it remotely.
func (c *lectureCell) abandon() { c.stop() }

// uploaded: job.json is written; the remote stages and progress that came
// before are reported now, in their order (while the audio uploaded the
// hover card stayed on the upload), and a done event that came before
// starts its result's fetch. The worker starts the moment job.json is on the
// runtime, so a lecture's events (on the kernel channel) may arrive before
// the PUT's answer does (colab-obs-core: they used to be reported only as
// the last stage, and the fetch of a fast lecture reported its download
// before its remote stage — the replay TestReplayBorrowTranscribeRelease
// failed about 2 % of the time).
// Remote events are reported under c.mu, so they never go backwards.
func (c *lectureCell) uploaded() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.up = true
	if len(c.held) == 0 {
		c.jp.remote(c.lastStage, c.fraction)
	}
	for _, h := range c.held {
		c.jp.remote(h.stage, h.fraction)
	}
	c.held = nil
	if c.doneEv != nil {
		c.startFetchLocked(*c.doneEv)
		c.doneEv = nil
	}
}

// heldState is a remote stage and progress held back until job.json is
// written.
type heldState struct {
	stage    string
	fraction float64
}

// maxHeld bounds the held states (a lecture has a dozen events before its
// transcription; the progress after that never comes before job.json).
const maxHeld = 64

// startFetchLocked fetches the result of the done event final in the
// background (R3: result.json is complete and hashed; fetch it now instead
// of after the cell has ended). c.mu held.
func (c *lectureCell) startFetchLocked(final remoteEvent) {
	ch := make(chan struct{})
	c.fetch = ch
	go func() {
		defer close(ch)
		c.data, c.fetchErr = c.b.fetchResult(c.lctx, c.s, c.jobDir, final, c.jp)
	}()
}

func (c *lectureCell) onEvent(ev remoteEvent) error {
	switch ev.Type {
	case remote.EventMachine, remote.EventSample:
		// The runtime's own readings (colab-obs-core): to the session's
		// samples; never progress, never a stage.
		c.b.noteReading(c.s, ev)
		return nil
	}
	c.mu.Lock()
	setupStage := c.lastStage == "install" || c.lastStage == "download_model"
	switch ev.Type {
	case "stage":
		if setupStage {
			c.setupSec += max(0, ev.TS-c.lastTS)
		}
		c.lastStage, c.lastTS = ev.Stage, ev.TS
		if ev.Stage == "transcribe" {
			c.jp.setTranscribeStart(c.b.now())
		}
	case "progress":
		c.fraction = max(c.fraction, ev.Fraction)
	case "memory":
		c.mu.Unlock()
		return violation("a memory event from the transcription")
	case "done", "error":
		if setupStage {
			c.setupSec += max(0, ev.TS-c.lastTS)
		}
	}
	if ev.Type == "stage" || ev.Type == "progress" {
		if c.up {
			c.jp.remote(c.lastStage, c.fraction)
		} else {
			c.held = append(c.held, heldState{c.lastStage, c.fraction})
			if len(c.held) > maxHeld {
				c.held = c.held[len(c.held)-maxHeld:]
			}
		}
	}
	if ev.Type == "done" {
		if c.up {
			c.startFetchLocked(ev)
		} else {
			final := ev
			c.doneEv = &final
		}
	}
	c.mu.Unlock()
	return nil
}

// result waits for the fetch the done event started.
func (c *lectureCell) result(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	fetch := c.fetch
	c.mu.Unlock()
	if fetch == nil {
		return nil, fallbackErr(CodeWorkerExited, nil, errors.New("the Colab worker ended without a done event"))
	}
	select {
	case <-fetch:
		return c.data, c.fetchErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchResult downloads result.json and checks it against the done event.
func (b *Backend) fetchResult(ctx context.Context, s *liveSession, jobDir string, final remoteEvent, jp *jobProgress) ([]byte, error) {
	jp.step(StepDownloadResult, 0)
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return nil, err
	}
	data, err := rt.download(ctx, jobDir+"/"+final.Result)
	if err != nil {
		if st := statusOf(err); st == 404 {
			return nil, fallbackErr(CodeDownloadFailed, map[string]any{"file": final.Result}, errors.New("result file missing on the runtime"))
		}
		return nil, err
	}
	jp.step(StepVerify, 0)
	if int64(len(data)) != final.Bytes || sha256Hex(data) != final.SHA256 {
		return nil, fallbackErr(CodeHashMismatch, map[string]any{"file": final.Result},
			fmt.Errorf("downloaded %d bytes, the worker reported %d; sha256 differs", len(data), final.Bytes))
	}
	var probe struct {
		Sentences []json.RawMessage `json:"sentences"`
	}
	if json.Unmarshal(data, &probe) != nil || probe.Sentences == nil {
		return nil, violation("result.json is not a transcription result")
	}
	return data, nil
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

// cleanupParallel deletions run at once, each attempt within
// removeTimeout: on the real runtime nine at once sometimes left one or two
// stalled for 25–37 s (colab-batch real run; also with four at once while
// an upload ran), a retry on a new request does not stall. A deletion
// normally takes well under a second.
const (
	cleanupParallel = 4
	removeTimeout   = 8 * time.Second
)

// cleanupStoppedTimeout bounds the cleanup of a lecture that was stopped
// (立即关机, disconnect, a cancel, the credential ending) after its result
// was safely written, or whose files go while the runtime stays for other
// lectures: one attempt per file, so the stop still takes effect within
// seconds.
const cleanupStoppedTimeout = 10 * time.Second

// remoteJobFiles are the files a job may leave in its directory (events
// and job.log are written by the resident worker).
var remoteJobFiles = []string{"audio.flac", "audio.wav", "job.json", "result.json", "result.json.tmp", "stderr.log", "events", "job.log"}

// cleanupRemote deletes the job's files on the runtime, all at once
// (colab-prof R3), then the directory. ctx is the lecture's context
// (jctx); the deletions run on a context detached from it with a time
// limit of their own, so a stop that arrives after the result was written
// (or during the cleanup) does not leave the files behind.
//
// With retry (after a successful lecture) each deletion is retried with
// growing waits; a file that still cannot be deleted is returned, and the
// caller releases the runtime at once (its disk is reclaimed with it), so
// neither the uploaded audio nor result.json stays on Colab. Without retry
// (the lecture failed or was stopped) every file gets one attempt. Once
// ctx is cancelled no deletion is retried and a failure returns no file.
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
	errs := make([]error, len(remoteJobFiles))
	tries := make([]int, len(remoteJobFiles))
	var wg sync.WaitGroup
	sem := make(chan struct{}, cleanupParallel)
	for i, name := range remoteJobFiles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var err error
			for a := 0; ; a++ {
				if a > 0 {
					if !retry || a > len(cleanupWaits) || ctx.Err() != nil {
						break
					}
					if b.o.Sleep(wctx, cleanupWaits[a-1]) != nil || ctx.Err() != nil {
						break
					}
				}
				tries[i]++
				var rt *runtimeClient
				if rt, err = b.runtime(cctx, s); err == nil {
					actx, acancel := context.WithTimeout(cctx, removeTimeout)
					err = rt.remove(actx, jobDir+"/"+name)
					acancel()
				}
				if err == nil {
					break
				}
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			continue
		}
		name := remoteJobFiles[i]
		switch {
		case ctx.Err() != nil:
			b.log.Info("colab: deleting a remote file failed after the lecture was stopped",
				"file", name, "attempts", tries[i], "code", Code(codedHTTP("api/contents", err)))
			return ""
		case !retry:
			b.log.Info("colab: deleting a remote file failed", "file", name,
				"code", Code(codedHTTP("api/contents", err)))
			continue
		}
		b.log.Warn("colab: deleting a remote file failed after retries; releasing the runtime", "file", name,
			"attempts", tries[i], "code", Code(codedHTTP("api/contents", err)))
		return name
	}
	if rt, err := b.runtime(cctx, s); err == nil {
		_ = rt.remove(cctx, jobDir) // the empty directory: nothing in it to protect
	}
	return ""
}

// finishJob records the measurement and releases the runtime when asked
// to after this lecture (and no other runs on it). jctx is the lecture's
// context: when it was cancelled after the result had been written by
// 立即关机, disconnect or the credential ending, the runtime is released
// now whatever the cleanup did — callers such as ShutdownNow rely on it
// being gone.
func (b *Backend) finishJob(ctx, jctx context.Context, s *liveSession, set Settings, job Job, j *jobState, m jobMetrics) {
	b.mu.Lock()
	s.setupDone = true
	s.lastJobEnd = b.now()
	s.rec.Jobs++
	rec := s.rec
	key := rateKey(rec.Accelerator, set.Model, set.ComputeType)
	b.rates[key] = b.rates[key].Add(Sample{AudioSec: m.audioSec, TranscribeSec: m.transcribeSec, OverheadSec: m.overheadSec})
	rates, _ := json.Marshal(b.rates)
	after, drained := b.afterCurrent, b.drained && len(b.queue) == 0
	others := b.othersLocked(s, j)
	left := s.leftFile
	mc := m
	b.lastMetrics = &mc
	b.mu.Unlock()
	b.persist(ctx, rec)
	if err := b.o.KV.SetKV(context.WithoutCancel(ctx), KVRates, string(rates)); err != nil {
		b.log.Warn("colab: saving the measured rate", "err", err)
	}
	b.log.Info("colab: lecture transcribed on Colab", "audio_min", int(m.audioSec/60), "transcribe_s", int(m.transcribeSec),
		"overhead_s", int(m.overheadSec), "setup_s", int(m.setupSec), "storage", m.storage(), "mem_peak_mb", m.memPeakMB,
		"uploaded_ahead", m.ahead)
	cause := context.Cause(jctx)
	switch {
	case errors.Is(cause, errShutdownNow), errors.Is(cause, errAuthEnding):
		_ = b.release(ctx, stopReason(jctx))
	case jctx.Err() != nil && others == 0:
		_ = b.release(ctx, stopReason(jctx))
	case left != "" && b.runningOthers(j) == 0:
		_ = b.releaseWith(ctx, ReleaseCleanupFailed, left) // a file of an earlier lecture stayed
	case after && b.runningOthers(j) == 0:
		_ = b.release(ctx, ReleaseAfterCurrent)
	case drained && others == 0:
		_ = b.release(ctx, ReleaseQueueEmpty)
	case set.IdleReleaseMin == 0 && others == 0:
		_ = b.release(ctx, ReleaseIdle) // "0 = right after each lecture"
	}
	b.kickPlan()
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

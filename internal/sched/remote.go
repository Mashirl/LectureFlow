// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// SPEC-v3 V14.3–V14.5: the Colab remote transcription backend inside the
// scheduler (v14-colab-integrate; .claude/v3-brief.md §8; NOTES "决定（v3
// v14-colab-integrate）").
//
//   - colab:remote is a row of the device table while Colab is enabled
//     (kv model.KVColabEnabled) and a credential is stored (Options.OnRemote
//     keeps the registry's presence in step); it is never part of the
//     power-aware policy's input (devices.Place, View, SelfTest,
//     OnPowerChange): it uses neither the machine's power nor its cooling,
//     so the single-active-device rule and multi_device do not apply to it
//     (V14.4) and a run on it is never migrated or suspended.
//   - Automatic dispatch ("overflow"): a queued asr job the local devices
//     cannot take now goes to colab:remote when the job's snapshot is auto,
//     it was not suspended by the power-aware policy (stats.asr_resume: a
//     suspended local CUDA job is never handed to Colab, V14.4), Colab never
//     handed it back before (stats.remote_fallback), the manual device list
//     (if any) names colab:remote, the global pause is off, and — on battery
//     power — battery_prefer is on. Colab.CanDispatch decides last; its
//     waiting_for (model.WaitRemote*) becomes the job's when it says no.
//   - A job pinned with "改用 Colab 处理" (stats.remote_pin, UseRemote) or
//     with a fixed colab:remote snapshot runs on Colab only.
//   - Colab hands a lecture back (colab.IsFallback, colab.IsRequeue): the
//     job is requeued without using up a retry (stats.remote_requeues), its
//     pin is dropped and it goes to the local devices (an auto snapshot is
//     bound to a local device at its next start).
//   - The lectures that wait and could go to Colab are its planned queue
//     (SetQueue); when none is left QueueDrained releases the runtime (now,
//     or after the running lecture).
//   - "处理完当前这节后关机" / "立即关机" (ShutdownRemote) hold the automatic
//     dispatch until no such lecture is left, so the next scan does not
//     borrow a new runtime at once.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
	"lectureflow/internal/wav"
)

// RemoteBackend is the Colab backend as the scheduler and the asr stage see
// it (*colab.Backend implements it; tests fake it).
type RemoteBackend interface {
	// CanDispatch: may a lecture of lectureSec go to Colab now; if not, the
	// waiting_for (model.WaitRemote*, "" = busy or cannot be asked).
	CanDispatch(ctx context.Context, lectureSec float64) (bool, string)
	SetQueue(audioSecs []float64)
	QueueDrained(ctx context.Context) error
	ShutdownAfterCurrent(ctx context.Context) error
	ShutdownNow(ctx context.Context) error
	Settings() colab.Settings
	Credential(ctx context.Context) colab.CredentialInfo
	Status(ctx context.Context) colab.Status
	Transcribe(ctx context.Context, job colab.Job, progress func(colab.Event)) (string, error)
}

// RemotePrefs are the scheduler's Colab switches (kv model.KVColabEnabled,
// model.KVColabBatteryPrefer).
type RemotePrefs struct {
	Enabled       bool `json:"enabled"`
	BatteryPrefer bool `json:"battery_prefer"`
}

// ReadRemotePrefs reads the switches (missing = off).
func ReadRemotePrefs(ctx context.Context, st db.Store) (RemotePrefs, error) {
	var p RemotePrefs
	v, ok, err := st.GetKV(ctx, model.KVColabEnabled)
	if err != nil {
		return p, err
	}
	p.Enabled = model.ParseKVFlag(v, ok)
	v, ok, err = st.GetKV(ctx, model.KVColabBatteryPrefer)
	if err != nil {
		return p, err
	}
	p.BatteryPrefer = model.ParseKVFlag(v, ok)
	return p, nil
}

// WriteRemotePrefs stores the switches.
func WriteRemotePrefs(ctx context.Context, st db.Store, p RemotePrefs) error {
	return st.Tx(ctx, func(tx db.Store) error {
		if err := tx.SetKV(ctx, model.KVColabEnabled, model.KVFlag(p.Enabled)); err != nil {
			return err
		}
		return tx.SetKV(ctx, model.KVColabBatteryPrefer, model.KVFlag(p.BatteryPrefer))
	})
}

// ErrRemoteFallback: the lecture cannot run on Colab after all and goes
// back to the local devices (the asr stage wraps it when it has no backend
// to call; Colab's own errors say so through colab.IsFallback).
var ErrRemoteFallback = errors.New("remote transcription is not available: back to the local devices")

// remoteHandBack reports whether a run's error hands the lecture back to
// the local devices (requeue, not a failure).
func remoteHandBack(err error) bool {
	return err != nil && (errors.Is(err, ErrRemoteFallback) || colab.IsFallback(err) || colab.IsRequeue(err))
}

type remoteKey struct{}

// WithRemote attaches the remote backend to a run's context (the scheduler
// does this for every run; tests of the asr stage do it themselves).
func WithRemote(ctx context.Context, b RemoteBackend) context.Context {
	if b == nil {
		return ctx
	}
	return context.WithValue(ctx, remoteKey{}, b)
}

// RemoteFrom returns the remote backend of a run's context (nil = none).
func RemoteFrom(ctx context.Context) RemoteBackend {
	b, _ := ctx.Value(remoteKey{}).(RemoteBackend)
	return b
}

// RemoteLabel is the label of the colab:remote row.
func RemoteLabel(accelerator string) string {
	if accelerator == "" {
		return "Google Colab"
	}
	return "Google Colab · " + accelerator
}

// RemoteRow is the colab:remote row the scheduler binds and reports: model
// and precision are Colab's settings (the remote worker runs them).
func RemoteRow(set colab.Settings) model.ASRDevice {
	return model.ASRDevice{ID: model.DeviceColab, Backend: model.BackendColab, Label: RemoteLabel(set.Accelerator),
		Enabled: true, Model: set.Model, Precision: set.ComputeType, MaxParallel: 1,
		Priority: model.DefaultDevicePriority(model.DeviceColab)}
}

// remoteScan is the Colab state one scan decides on.
type remoteScan struct {
	configured bool // Options.Remote and Options.Devices are set
	prefs      RemotePrefs
	authOK     bool // a credential is stored and its lifecycle allows it
	rowEnabled bool // the colab:remote row's own switch (no row yet: on)
	set        colab.Settings
}

// present: colab:remote is in the device table.
func (r remoteScan) present() bool { return r.configured && r.prefs.Enabled && r.authOK }

// usable: lectures may be sent to it.
func (r remoteScan) usable() bool { return r.present() && r.rowEnabled }

// remoteState is the scheduler's own Colab bookkeeping (guarded by
// Scheduler.mu).
type remoteState struct {
	hold      bool      // ShutdownRemote: no automatic dispatch until the planned queue is empty
	queue     []float64 // the last SetQueue
	planned   bool      // the last scan had eligible lectures
	runEnded  bool      // a colab run ended since the last scan
	reported  bool      // OnRemote was called once
	lastPres  bool
	lastRow   model.ASRDevice
	draining  bool // a QueueDrained call runs
	durations map[string]audioLen
}

type audioLen struct {
	size int64
	mod  time.Time
	sec  float64
}

// readRemote reads the Colab state of a scan and keeps the registry's
// presence of colab:remote in step (Options.OnRemote). Never with s.mu held.
func (s *Scheduler) readRemote(ctx context.Context) remoteScan {
	var r remoteScan
	if s.remote == nil || s.devs == nil {
		return r
	}
	r.configured = true
	p, err := ReadRemotePrefs(ctx, s.st)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("colab: reading the switches", "err", err)
		}
		return remoteScan{}
	}
	r.prefs = p
	r.authOK = s.remote.Credential(ctx).Present
	r.set = s.remote.Settings()
	r.rowEnabled = true
	if d, ok := s.devs.Get(model.DeviceColab); ok {
		r.rowEnabled = d.Enabled
	}
	s.reportRemote(ctx, r)
	return r
}

// reportRemote calls Options.OnRemote when the presence or the row's
// label, model or precision changed.
func (s *Scheduler) reportRemote(ctx context.Context, r remoteScan) {
	if s.onRemote == nil {
		return
	}
	row := RemoteRow(r.set)
	s.mu.Lock()
	same := s.rem.reported && s.rem.lastPres == r.present() && s.rem.lastRow.Label == row.Label &&
		s.rem.lastRow.Model == row.Model && s.rem.lastRow.Precision == row.Precision
	s.rem.reported, s.rem.lastPres, s.rem.lastRow = true, r.present(), row
	s.mu.Unlock()
	if !same {
		s.onRemote(ctx, r.present(), row)
	}
}

// RefreshRemote re-reads the Colab state now (after the switches or the
// credential changed), updates the device table and kicks a scan.
func (s *Scheduler) RefreshRemote(ctx context.Context) {
	s.readRemote(ctx)
	s.ev.Changed("devices", "")
	s.Kick()
}

// Remote returns Options.Remote (nil = none).
func (s *Scheduler) Remote() RemoteBackend { return s.remote }

// RemotePrefs reads the switches.
func (s *Scheduler) RemotePrefs(ctx context.Context) (RemotePrefs, error) {
	return ReadRemotePrefs(ctx, s.st)
}

// SetRemotePrefs stores the switches; turning Colab on again ends a
// shutdown's hold. The device table follows and a scan runs.
func (s *Scheduler) SetRemotePrefs(ctx context.Context, p RemotePrefs) error {
	old, err := ReadRemotePrefs(ctx, s.st)
	if err != nil {
		return err
	}
	if err := WriteRemotePrefs(ctx, s.st, p); err != nil {
		return err
	}
	if p.Enabled && !old.Enabled {
		s.mu.Lock()
		s.rem.hold = false
		s.mu.Unlock()
	}
	s.RefreshRemote(ctx)
	return nil
}

// RemoteHeld reports whether a shutdown holds the automatic dispatch.
func (s *Scheduler) RemoteHeld() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rem.hold
}

// remoteRunningLocked reports whether a run uses colab:remote (caller holds s.mu).
func (s *Scheduler) remoteRunningLocked() bool {
	return s.pipelineOnLocked(model.DeviceColab) > 0
}

// isRemote: a device id of the remote backend.
func isRemote(id string) bool { return model.DeviceBackend(id) == model.BackendColab }

// remotePinned: the job may run on colab:remote only (a "改用 Colab 处理"
// pin, or a fixed colab:remote snapshot).
func remotePinned(j model.Job) bool {
	if j.Stats.RemotePin {
		return true
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	return ok && !snap.Auto && isRemote(snap.DeviceID)
}

// remoteOnlyAdmit decides a pinned job: Colab or wait.
func (s *Scheduler) remoteOnlyAdmit(ctx context.Context, sc *scanState, r *run) string {
	s.scanSettings(sc)
	rs := sc.remote
	switch {
	case sc.pausedAll:
		return model.WaitGPUPaused
	case !rs.present() && rs.configured && rs.prefs.Enabled && !rs.authOK:
		return model.WaitRemoteAuth
	case !rs.usable():
		return model.WaitNoASRDevice
	case sc.devPaused[model.DeviceColab]:
		return model.WaitDevicePaused(model.DeviceColab)
	}
	sc.remoteQueue = append(sc.remoteQueue, s.audioSec(ctx, r.job))
	sc.remoteEligible++
	if sc.remoteHold {
		return model.WaitGPUSlot
	}
	return s.remoteAdmit(ctx, sc, r, model.WaitGPUSlot)
}

// remoteOverflow is the automatic dispatch of a job the local devices did
// not take (local = their reason): Colab when every condition of the
// package comment holds and Colab can take it, else local (or Colab's
// waiting_for).
func (s *Scheduler) remoteOverflow(ctx context.Context, sc *scanState, r *run, local string) string {
	rs := sc.remote
	if !rs.usable() || local == model.WaitGPUPaused {
		return local
	}
	j := r.job
	if j.Stats.ASRResume != nil || j.Stats.RemoteFallback != nil {
		return local // suspended by the power policy (V14.4), or Colab handed it back before
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if !ok || !snap.Auto {
		return local // a fixed snapshot runs on its own device only
	}
	s.scanSettings(sc)
	if sc.devPaused[model.DeviceColab] {
		return local // its own pause switch
	}
	if sc.asr.DeviceMode == model.ASRDeviceModeManual && !slices.Contains(sc.asr.ManualDevices, model.DeviceColab) {
		return local // "只用某台设备": Colab only when the list names it
	}
	if sc.power.OnBatteryPower() && !rs.prefs.BatteryPrefer {
		return local
	}
	sc.remoteQueue = append(sc.remoteQueue, s.audioSec(ctx, j))
	sc.remoteEligible++
	if sc.remoteHold {
		return local
	}
	return s.remoteAdmit(ctx, sc, r, local)
}

// remoteDispatchTimeout bounds CanDispatch inside a scan (the paid tier
// reads the balance from Colab at most once a minute).
const remoteDispatchTimeout = 10 * time.Second

// remoteAdmit asks Colab and, when it can take the job, binds the run to
// colab:remote (busy: the reason the job keeps).
func (s *Scheduler) remoteAdmit(ctx context.Context, sc *scanState, r *run, busy string) string {
	s.mu.Lock()
	taken := s.remoteRunningLocked()
	s.mu.Unlock()
	if taken || sc.remoteTaken {
		return busy // one lecture at a time
	}
	sec := sc.remoteQueue[len(sc.remoteQueue)-1]
	cctx, cancel := context.WithTimeout(ctx, remoteDispatchTimeout)
	ok, wf := s.remote.CanDispatch(cctx, sec)
	cancel()
	if !ok {
		if wf != "" {
			return wf
		}
		return busy
	}
	// Dispatched: not part of the planned queue any more.
	sc.remoteQueue = sc.remoteQueue[:len(sc.remoteQueue)-1]
	sc.remoteEligible--
	sc.remoteTaken = true
	snap, snapOK := model.ParseASRSnapshot(r.job.ConfigSnapshot)
	r.device, r.devRow = model.DeviceColab, RemoteRow(sc.remote.set)
	r.bind = snapOK && snap.Auto
	r.fixed = !r.bind
	slog.Info("asr job goes to Colab", "job", r.job.ID, "lecture", r.job.ScopeID, "pinned", r.job.Stats.RemotePin)
	return ""
}

// audioSec is the length of the job's lecture audio.wav in seconds (0 when
// unknown), cached by size and modification time.
func (s *Scheduler) audioSec(ctx context.Context, j model.Job) float64 {
	l, err := s.st.GetLecture(ctx, j.ScopeID)
	if err != nil {
		return 0
	}
	path := model.LectureDir(s.dataDir, l.CourseID, l.ID) + string(os.PathSeparator) + model.AudioFile
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	c, ok := s.rem.durations[path]
	s.mu.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.sec
	}
	ms, ok := wav.DurationMS(path)
	if !ok {
		return 0
	}
	sec := float64(ms) / 1000
	s.mu.Lock()
	if s.rem.durations == nil {
		s.rem.durations = map[string]audioLen{}
	}
	s.rem.durations[path] = audioLen{size: fi.Size(), mod: fi.ModTime(), sec: sec}
	s.mu.Unlock()
	return sec
}

// endRemoteScan tells Colab its planned queue (SetQueue) and, when none is
// left, that the queue is drained (once; the runtime is released now or
// after the running lecture). A shutdown's hold ends with the queue.
func (s *Scheduler) endRemoteScan(sc *scanState) {
	if s.remote == nil || !sc.remote.configured {
		return
	}
	q := sc.remoteQueue
	if sc.remoteHold || !sc.remote.usable() {
		q = nil // nothing is planned for Colab while held or unusable
	}
	s.mu.Lock()
	prevPlanned, ended := s.rem.planned, s.rem.runEnded
	s.rem.runEnded = false
	s.rem.planned = len(q) > 0
	changed := !slices.Equal(q, s.rem.queue)
	s.rem.queue = slices.Clone(q)
	if sc.remoteEligible == 0 && s.rem.hold && !s.remoteRunningLocked() {
		s.rem.hold = false
		slog.Info("colab: no lecture is waiting any more; automatic dispatch to Colab allowed again")
	}
	drain := len(q) == 0 && (prevPlanned || ended) && !s.rem.draining
	if drain {
		s.rem.draining = true
	}
	s.mu.Unlock()
	if changed {
		s.remote.SetQueue(q)
	}
	if drain {
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), 2*time.Minute)
			defer cancel()
			if err := s.remote.QueueDrained(ctx); err != nil {
				slog.Info("colab: releasing the runtime after the queue drained failed (retried by the backend)", "code", colab.Code(err))
			}
			s.mu.Lock()
			s.rem.draining = false
			s.mu.Unlock()
		}()
	}
}

// ShutdownRemote is "处理完当前这节后关机" (now false) or "立即关机" (now
// true; the running lecture goes back to the queue): queued lectures are
// no longer pinned to Colab, and the automatic dispatch is held until no
// lecture that could go to Colab is left.
func (s *Scheduler) ShutdownRemote(ctx context.Context, now bool) error {
	if s.remote == nil {
		return ErrRemoteNotConfigured
	}
	s.mu.Lock()
	s.rem.hold = true
	s.mu.Unlock()
	if err := s.unpinQueued(ctx); err != nil {
		slog.Warn("colab: unpinning the queued lectures", "err", err)
	}
	var err error
	if now {
		err = s.remote.ShutdownNow(ctx)
	} else {
		err = s.remote.ShutdownAfterCurrent(ctx)
	}
	s.ev.Changed("devices", "")
	s.Kick()
	return err
}

// unpinQueued drops the "改用 Colab 处理" pin of the queued and pending asr
// jobs (under the scan lock: none starts meanwhile).
func (s *Scheduler) unpinQueued(ctx context.Context) error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	jobs, err := s.st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusQueued)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Stage != model.StageASR || !j.Stats.RemotePin {
			continue
		}
		st := j.Stats
		st.RemotePin = false
		if err := s.st.SetJobStats(ctx, j.ID, st); err != nil {
			return err
		}
		s.emitJob(ctx, j)
	}
	return nil
}

// ErrRemoteNotConfigured: no Colab backend (HTTP 503).
var ErrRemoteNotConfigured = errors.New("the Colab backend is not configured")

// remoteRefusal is UseRemote's refusal (HTTP 409): CodeRemoteDisabled, or
// CodeRemoteUnavailable {waiting_for}.
func remoteRefusal(code string, waitingFor string) error {
	var params map[string]any
	if waitingFor != "" {
		params = map[string]any{"waiting_for": waitingFor}
	}
	return &model.CodedError{Info: model.ErrorInfo{Code: code, Params: params},
		Err: fmt.Errorf("%w: Colab cannot take the lecture (%s)", state.ErrConflict, code+" "+waitingFor)}
}

// UseRemote is V14.4 "改用 Colab 处理" for one lecture: the asr job starts
// over and runs on colab:remote. A queued job (also one the power policy
// suspended) or one running on a local device is cancelled first; a failed
// or cancelled one is retried. Refusals (HTTP 409): Colab is off or its row
// disabled (CodeRemoteDisabled), no usable credential or Colab cannot take
// a lecture now (CodeRemoteUnavailable {waiting_for}: remote_auth,
// remote_window, remote_balance), the job is pending, done or stale
// (state.ErrConflict). A job already pinned or running on Colab: nothing
// to do.
func (s *Scheduler) UseRemote(ctx context.Context, lectureID string) error {
	if s.remote == nil || s.devs == nil {
		return ErrRemoteNotConfigured
	}
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if state.IsDraft(l) {
		return state.ErrDraft
	}
	rs := s.readRemote(ctx)
	switch {
	case !rs.prefs.Enabled || !rs.rowEnabled:
		return remoteRefusal(CodeRemoteDisabled, "")
	case !rs.authOK:
		return remoteRefusal(CodeRemoteUnavailable, model.WaitRemoteAuth)
	}
	j, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR)
	if err != nil {
		return err
	}
	s.mu.Lock()
	cur := s.running[j.ID]
	onRemote := cur != nil && isRemote(cur.device)
	s.mu.Unlock()
	if onRemote || (j.Status == model.StatusQueued && j.Stats.RemotePin) {
		return nil
	}
	switch j.Status {
	case model.StatusQueued, model.StatusRunning, model.StatusFailed, model.StatusCancelled:
	default:
		return fmt.Errorf("%w: asr is %s; 改用 Colab needs a queued, running, failed or cancelled transcription", state.ErrConflict, j.Status)
	}
	if !s.remoteRunning() {
		cctx, cancel := context.WithTimeout(ctx, remoteDispatchTimeout)
		ok, wf := s.remote.CanDispatch(cctx, s.audioSec(ctx, j))
		cancel()
		if !ok && wf != "" {
			return remoteRefusal(CodeRemoteUnavailable, wf)
		}
	}
	if j.Status == model.StatusQueued || j.Status == model.StatusRunning {
		if err := s.Cancel(j.ID); err != nil {
			return err
		}
	}
	lang := s.set.ASR().Language
	if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok {
		lang = snap.Language
	}
	raw := model.MarshalSnapshot(model.ASRDeviceSnapshot(RemoteRow(rs.set), lang, true))
	s.scanMu.Lock()
	err = state.RetryWith(ctx, s.st, s.dataDir, model.ScopeLecture, lectureID, model.StageASR,
		map[string]string{model.StageASR: raw})
	if err == nil {
		var nj model.Job
		if nj, err = s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR); err == nil {
			st := nj.Stats
			// From the start (V14.4): no checkpoint, no power event to show.
			st.RemotePin, st.RemoteFallback, st.ASRResume, st.PowerEvent = true, nil, nil, nil
			err = s.st.SetJobStats(ctx, nj.ID, st)
		}
	}
	if err == nil {
		s.mu.Lock()
		s.rem.hold = false // the user asked for Colab explicitly
		s.mu.Unlock()
	}
	s.scanMu.Unlock()
	if err != nil {
		return err
	}
	slog.Info("asr job pinned to Colab by the user", "lecture", lectureID)
	s.emitScope(ctx, model.ScopeLecture, lectureID)
	s.Kick()
	return nil
}

// remoteRunning reports whether a run uses colab:remote.
func (s *Scheduler) remoteRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remoteRunningLocked()
}

// remoteRequeue is finish's hand-back of a lecture Colab gave back
// (remoteHandBack): requeued like a power interrupt (no retry used up,
// job.error cleared), the pin dropped, and a fixed colab:remote snapshot
// replaced by the current auto snapshot so a local device can take it.
func (s *Scheduler) remoteRequeue(ctx context.Context, r *run, runErr error) {
	id := r.job.ID
	if err := state.PowerRequeue(ctx, s.st, id); err != nil {
		slog.Error("job requeue after Colab handed it back", "job", id, "err", err)
		return
	}
	if snap, ok := model.ParseASRSnapshot(r.job.ConfigSnapshot); ok && !snap.Auto && isRemote(snap.DeviceID) {
		if cur, err := s.currentSnapshots([]string{model.StageASR}, false); err == nil && cur[model.StageASR] != "" {
			if err := s.st.SetJobSnapshot(ctx, id, cur[model.StageASR]); err != nil {
				slog.Warn("replacing the colab snapshot of a lecture Colab handed back", "job", id, "err", err)
			}
		}
	}
	slog.Info("Colab handed the lecture back; it goes to the local devices", "job", id, "lecture", r.scopeID,
		"code", colab.Code(runErr), "requeue", colab.IsRequeue(runErr))
}

// remoteErrorInfo is the stats.remote_fallback of a hand-back.
func remoteErrorInfo(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) && ce.Info.Code != "" {
		info := ce.Info
		info.Detail = ""
		return &info
	}
	return &model.ErrorInfo{Code: colab.CodeUnavailable, Params: map[string]any{"endpoint": "local"}}
}

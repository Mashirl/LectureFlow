// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// The SSH inference servers in the scheduler (ssh-mvp;
// .claude/research/remote-ssh-plan.md §2.3; user decision 2026-10-27,
// plan §10 item 2: the device list's order decides).
//
//   - An ssh:<slug> row is a remote device (model.IsRemoteDevice): outside
//     the power-aware policy and multi_device (isRemote keeps it out of the
//     policy input), never migrated or suspended, and no local worker or
//     self-test runs for it.
//   - Automatic dispatch of an asr job (asrDeviceAdmit): the enabled SSH
//     rows above the highest local device that is enabled and present
//     (the "prefer" group) are tried first; then the local devices
//     (deviceAdmit); then the other SSH rows (the "supplement" group); then
//     Colab's overflow. On battery power every SSH row is tried first (the
//     V14 "电池模式下优先使用 Colab" for remote devices). Only auto
//     snapshots go there, and never a job a remote device handed back
//     (stats.remote_fallback) or that the power policy suspended
//     (stats.asr_resume: its checkpoint is local); "只用某台设备" limits
//     them to the listed rows.
//   - A server's own answer decides (remote.Device.Ready): connected and
//     set up, a free slot; its waiting_for (remote_offline, remote_setup,
//     remote_auth) becomes the job's when no other device takes it.
//   - "改用 <服务器> 处理" (UseSSH) gives the job a fixed snapshot of the
//     server's row: it runs there only (sshOnlyAdmit) until the server
//     hands it back (a fallback: requeued without using up a retry, the
//     snapshot replaced by the current auto one, remoteRequeue).
//   - Without SSH servers (no fleet, or none configured) asrDeviceAdmit is
//     the earlier admission, step for step.

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/state"
)

// SSHFleet is the SSH servers as the scheduler and the asr stage see them
// (*sshremote.Manager).
type SSHFleet interface {
	// Device returns the server of a device id.
	Device(id string) (remote.Device, bool)
}

// SetSSH attaches the SSH servers (cmd, after New; nil = none).
func (s *Scheduler) SetSSH(f SSHFleet) {
	s.mu.Lock()
	s.rem.ssh = f
	s.mu.Unlock()
	s.Kick()
}

// SSH returns the attached SSH servers (nil = none).
func (s *Scheduler) SSH() SSHFleet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rem.ssh
}

type sshKey struct{}

// WithSSH attaches the SSH servers to a run's context.
func WithSSH(ctx context.Context, f SSHFleet) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, sshKey{}, f)
}

// SSHFrom returns the SSH servers of a run's context (nil = none).
func SSHFrom(ctx context.Context) SSHFleet {
	f, _ := ctx.Value(sshKey{}).(SSHFleet)
	return f
}

// withRemotes is the run's context with Colab and the SSH servers.
func (s *Scheduler) withRemotes(ctx context.Context) context.Context {
	return WithSSH(WithRemote(ctx, s.remote), s.SSH())
}

// isColab: the Colab device (colab:*).
func isColab(id string) bool { return model.DeviceBackend(id) == model.BackendColab }

// sshPinned is the SSH device a job's fixed snapshot names ("" = none).
func sshPinned(j model.Job) string {
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if ok && !snap.Auto && model.IsSSHDevice(snap.DeviceID) {
		return snap.DeviceID
	}
	return ""
}

// asrDeviceAdmit is the admission of an asr job with the device registry
// (Options.Devices): the package comment's order. It returns the job's
// waiting_for, "" when r was bound to a device.
func (s *Scheduler) asrDeviceAdmit(ctx context.Context, sc *scanState, r *run) string {
	j := r.job
	fleet := s.SSH()
	if fleet != nil {
		if id := sshPinned(j); id != "" {
			return s.sshOnlyAdmit(ctx, sc, r, fleet, id)
		}
	}
	if s.remote != nil && (remotePinned(j) || sc.remoteKeptJobs[j.ID]) {
		// SPEC-v3 V14.4: "改用 Colab 处理" (or a fixed colab snapshot);
		// colab-batch: a lecture kept for Colab (uploaded ahead, reserved).
		return s.remoteOnlyAdmit(ctx, sc, r)
	}
	var prefer, supplement []model.ASRDevice
	if fleet != nil {
		prefer, supplement = s.sshGroups(sc, j)
	}
	sshReason := ""
	if len(prefer) > 0 {
		if ok, reason := s.sshAdmit(ctx, sc, r, fleet, prefer); ok {
			return ""
		} else if sshReason == "" {
			sshReason = reason
		}
	}
	// SPEC-v2 I8: per-device pools.
	reason := s.deviceAdmit(ctx, sc, r)
	if reason == "" {
		return ""
	}
	if len(supplement) > 0 {
		if ok, sr := s.sshAdmit(ctx, sc, r, fleet, supplement); ok {
			return ""
		} else if sshReason == "" {
			sshReason = sr
		}
	}
	if localUnavailable(reason) && sshFallback(j) {
		// An SSH server handed it back and no local device can take it: it
		// would wait for good (the stage offers a retry or "改用 <服务器>").
		return model.WaitRemoteFallbackSSH
	}
	if sshReason != "" && localUnavailable(reason) {
		// No local device can take it and only SSH servers could: their
		// reason tells the user more (remote_offline, remote_setup, …).
		reason = sshReason
	}
	// SPEC-v3 V14: Colab takes a lecture the local devices cannot take now
	// (without Colab its scan state is not usable: the reason stays).
	return s.remoteOverflow(ctx, sc, r, reason)
}

// sshGroups splits the SSH rows a job may go to automatically into the
// prefer and supplement groups (priority order).
func (s *Scheduler) sshGroups(sc *scanState, j model.Job) (prefer, supplement []model.ASRDevice) {
	if j.Stats.RemoteFallback != nil || j.Stats.ASRResume != nil {
		return nil, nil
	}
	if snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok && !snap.Auto {
		return nil, nil // a fixed snapshot runs on its own device only
	}
	s.scanSettings(sc)
	if sc.pausedAll {
		return nil, nil
	}
	manual := sc.asr.DeviceMode == model.ASRDeviceModeManual
	topLocal := -1 << 31
	var rows []model.ASRDevice
	for _, d := range sc.devList { // priority DESC
		if !d.Enabled {
			continue
		}
		if !model.IsRemoteDevice(d.ID) {
			if sc.present[d.ID] && (!manual || slices.Contains(sc.asr.ManualDevices, d.ID)) {
				topLocal = max(topLocal, d.Priority)
			}
			continue
		}
		if !model.IsSSHDevice(d.ID) || sc.devPaused[d.ID] {
			continue
		}
		if manual && !slices.Contains(sc.asr.ManualDevices, d.ID) {
			continue
		}
		rows = append(rows, d)
	}
	battery := sc.power.OnBatteryPower()
	for _, d := range rows {
		if battery || d.Priority > topLocal {
			prefer = append(prefer, d)
		} else {
			supplement = append(supplement, d)
		}
	}
	return prefer, supplement
}

// sshAdmit binds r to the first of rows that can take it now (ok); else
// reason is the first server's waiting_for ("" = busy).
func (s *Scheduler) sshAdmit(ctx context.Context, sc *scanState, r *run, fleet SSHFleet, rows []model.ASRDevice) (ok bool, reason string) {
	first := ""
	sec := -1.0
	for _, d := range rows {
		dev, ok := fleet.Device(d.ID)
		if !ok {
			continue
		}
		if _, full := sc.devFull[d.ID]; full {
			continue // no slot for an earlier job of this scan (scanState.devFull)
		}
		s.mu.Lock()
		used := s.pipelineOnLocked(d.ID)
		s.mu.Unlock()
		if used >= max(dev.Slots(), 1) {
			sc.noSlot(d.ID, model.WaitGPUSlot)
			continue
		}
		if sec < 0 {
			sec = s.audioSec(ctx, r.job)
		}
		ready, wf := dev.Ready(ctx, sec)
		if ready {
			snap, snapOK := model.ParseASRSnapshot(r.job.ConfigSnapshot)
			r.device, r.devRow = d.ID, d
			r.bind = !snapOK || snap.Auto
			r.fixed = !r.bind
			slog.Info("asr job goes to an SSH server", "job", r.job.ID, "lecture", r.job.ScopeID, "device", d.ID)
			return true, ""
		}
		if first == "" {
			first = wf
		}
	}
	return false, first
}

// sshOnlyAdmit decides a job pinned to one server: there or wait.
func (s *Scheduler) sshOnlyAdmit(ctx context.Context, sc *scanState, r *run, fleet SSHFleet, id string) string {
	s.scanSettings(sc)
	d, ok := sc.row(id)
	switch {
	case sc.pausedAll:
		return model.WaitGPUPaused
	case !ok || !d.Enabled:
		return model.WaitNoASRDevice
	case sc.devPaused[id]:
		return model.WaitDevicePaused(id)
	}
	if _, ok := fleet.Device(id); !ok {
		return model.WaitNoASRDevice
	}
	admitted, reason := s.sshAdmit(ctx, sc, r, fleet, []model.ASRDevice{d})
	switch {
	case admitted:
		return ""
	case reason != "":
		return reason
	}
	return model.WaitGPUSlot // every slot of the server is in use (remote_busy names Colab)
}

// sshFallback reports whether an SSH server handed the job back (its
// stats.remote_fallback has one of the SSH servers' codes).
func sshFallback(j model.Job) bool {
	f := j.Stats.RemoteFallback
	return f != nil && j.Stats.ASRResume == nil && slices.Contains(remote.ErrorCodes, f.Code)
}

// selfTestRemoteCode is the refusal of a remote device's self-test:
// Colab's, or the SSH servers' own wording.
func selfTestRemoteCode(id string) string {
	if model.IsSSHDevice(id) {
		return remote.CodeSelfTestSSH
	}
	return CodeSelfTestRemote
}

// ErrSSHNotConfigured: no SSH servers (HTTP 503).
var ErrSSHNotConfigured = fmt.Errorf("%w: no SSH servers are configured", state.ErrConflict)

// UseSSH is "改用 <服务器> 处理" for one lecture: the asr job starts over
// with a fixed snapshot of the server's row and runs there only. A queued
// or running job is cancelled first; a failed or cancelled one is retried.
// Refusals: the server is unknown or its row disabled
// (CodeRemoteDisabled), the job is pending, done or stale (state.ErrConflict).
func (s *Scheduler) UseSSH(ctx context.Context, lectureID, deviceID string) error {
	fleet := s.SSH()
	if fleet == nil || s.devs == nil {
		return ErrSSHNotConfigured
	}
	if !model.IsSSHDevice(deviceID) {
		return remoteRefusal(CodeRemoteDisabled, "")
	}
	if _, ok := fleet.Device(deviceID); !ok {
		return remoteRefusal(CodeRemoteDisabled, "")
	}
	row, ok := s.devs.Get(deviceID)
	if !ok || !row.Enabled {
		return remoteRefusal(CodeRemoteDisabled, "")
	}
	l, err := s.st.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if state.IsDraft(l) {
		return state.ErrDraft
	}
	j, err := s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR)
	if err != nil {
		return err
	}
	s.mu.Lock()
	cur := s.running[j.ID]
	onIt := cur != nil && cur.device == deviceID
	s.mu.Unlock()
	if onIt || (j.Status == model.StatusQueued && sshPinned(j) == deviceID) {
		return nil
	}
	switch j.Status {
	case model.StatusQueued, model.StatusRunning, model.StatusFailed, model.StatusCancelled:
	default:
		return fmt.Errorf("%w: asr is %s; 改用远程服务器 needs a queued, running, failed or cancelled transcription", state.ErrConflict, j.Status)
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
	raw := model.MarshalSnapshot(model.ASRDeviceSnapshot(row, lang, false))
	s.scanMu.Lock()
	err = state.RetryWith(ctx, s.st, s.dataDir, model.ScopeLecture, lectureID, model.StageASR,
		map[string]string{model.StageASR: raw})
	if err == nil {
		var nj model.Job
		if nj, err = s.st.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR); err == nil {
			st := nj.Stats
			st.RemotePin, st.RemoteFallback, st.ASRResume, st.PowerEvent = false, nil, nil, nil
			st.ASRMode = ""
			err = s.st.SetJobStats(ctx, nj.ID, st)
		}
	}
	s.scanMu.Unlock()
	if err != nil {
		return err
	}
	slog.Info("asr job pinned to an SSH server by the user", "lecture", lectureID, "device", deviceID)
	s.emitScope(ctx, model.ScopeLecture, lectureID)
	s.Kick()
	return nil
}

// sshRequeue is finish's hand-back of a lecture an SSH server gave back
// (remoteHandBack): busy (nothing ran: offline, not set up, every slot
// taken) back to the queue as it was; a fallback back to the queue for the
// local devices, its fixed snapshot of the server replaced by the current
// auto snapshot (stats.remote_fallback keeps it off the remote devices).
func (s *Scheduler) sshRequeue(ctx context.Context, r *run, runErr error) {
	id := r.job.ID
	if err := state.PowerRequeue(ctx, s.st, id); err != nil {
		slog.Error("job requeue after an SSH server handed it back", "job", id, "err", err)
		return
	}
	if remoteBusy(runErr) {
		slog.Info("the SSH server could not take the lecture yet; back to the queue", "job", id, "lecture", r.scopeID,
			"device", r.device, "code", remote.CodeOf(runErr))
		return
	}
	if snap, ok := model.ParseASRSnapshot(r.job.ConfigSnapshot); ok && !snap.Auto && model.IsSSHDevice(snap.DeviceID) {
		if cur, err := s.currentSnapshots([]string{model.StageASR}, false); err == nil && cur[model.StageASR] != "" {
			if err := s.st.SetJobSnapshot(ctx, id, cur[model.StageASR]); err != nil {
				slog.Warn("replacing the SSH snapshot of a lecture the server handed back", "job", id, "err", err)
			}
		}
	}
	slog.Info("the SSH server handed the lecture back; it goes to the local devices", "job", id, "lecture", r.scopeID,
		"device", r.device, "code", remote.CodeOf(runErr))
}

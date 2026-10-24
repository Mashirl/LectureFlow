// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// One borrowed runtime ("一次借卡", V14.3): waiting for the accelerator
// with back-off, boot, the runtime proxy token, release, and the record
// that lets the next start release a session a crash left behind.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"lectureflow/internal/model"
)

// Session states (V14.5: 排队等卡 / 开机中 / 运行中 / 关机中 / 已释放).
const (
	StateNone     = "none"
	StateQueued   = "queued"
	StateBooting  = "booting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateReleased = "released"
)

// Release reasons (Status.LastRelease.Reason; V14.3 step 5).
const (
	ReleaseQueueEmpty   = "queue_empty"   // the scheduler said the queue is empty
	ReleaseIdle         = "idle"          // nothing to do for IdleReleaseMin
	ReleaseWindow       = "window"        // the fair-use window is used up
	ReleaseFailed       = "failed"        // a lecture failed on the runtime
	ReleaseCancelled    = "cancelled"     // the user cancelled the lecture
	ReleaseAfterCurrent = "after_current" // "处理完当前这节后关机"
	ReleaseNow          = "shutdown_now"  // "立即关机"
	ReleaseAuthEnding   = "auth_ending"   // the credential's lifecycle is about to end
	ReleaseDisconnect   = "disconnect"    // "断开连接"
	ReleaseAppExit      = "app_exit"      // the app is closing
	ReleaseLeftover     = "leftover"      // a session of an earlier run, released at start
	ReleaseReclaimed    = "reclaimed"     // Colab took the runtime back
	// ReleaseCleanupFailed: a file of the finished lecture (the uploaded
	// audio, result.json) could not be deleted on the runtime even after
	// retries; releasing the runtime removes its disk (Gate round 1 item 8).
	// ReleaseInfo.File names the file.
	ReleaseCleanupFailed = "cleanup_failed"
)

const (
	recordVersion     = 1
	proxyRefreshAhead = 5 * time.Minute // upstream common.py TOKEN_REFRESH_MARGIN
	releaseTimeout    = 60 * time.Second
)

// sessionRecord is kv colab.session: no token, only what releasing needs.
type sessionRecord struct {
	Version      int       `json:"v"`
	State        string    `json:"state"`
	NotebookHash string    `json:"nbh"`
	Accelerator  string    `json:"accelerator"`
	Shape        string    `json:"shape,omitempty"`
	Endpoint     string    `json:"endpoint,omitempty"`
	RequestedAt  time.Time `json:"requested_at"`
	AssignedAt   time.Time `json:"assigned_at,omitzero"`
	ReadyAt      time.Time `json:"ready_at,omitzero"`
	RunID        string    `json:"run_id"`
	Jobs         int       `json:"jobs"`
}

// liveSession is the runtime of this run (in memory).
type liveSession struct {
	rec        sessionRecord
	rt         *runtimeClient
	proxyExp   time.Time
	kernelID   string
	workerHash string
	setupDone  bool
	lastJobEnd time.Time
}

func (b *Backend) persist(ctx context.Context, rec sessionRecord) {
	raw, _ := json.Marshal(rec)
	if err := b.o.KV.SetKV(context.WithoutCancel(ctx), KVSession, string(raw)); err != nil {
		b.log.Warn("colab: saving the session record", "err", err)
	}
}

func (b *Backend) loadRecord(ctx context.Context) (*sessionRecord, error) {
	v, ok, err := getKV(ctx, b.o.KV, KVSession)
	if err != nil || !ok {
		return nil, err
	}
	var rec sessionRecord
	if json.Unmarshal([]byte(v), &rec) != nil || rec.Version != recordVersion {
		b.log.Warn("colab: unreadable session record dropped")
		return nil, deleteKV(ctx, b.o.KV, KVSession)
	}
	return &rec, nil
}

// newRuntime is the client of an assignment's runtime.
func (b *Backend) newRuntime(p proxyInfo) *runtimeClient {
	return &runtimeClient{hc: b.o.HTTPClient, base: p.URL, token: p.Token, agent: b.o.ClientAgent, dial: b.o.Dial}
}

// acquire returns the running session, borrowing a runtime first when
// there is none: assign with exponential back-off while Colab refuses for
// capacity (412) or fails transiently (waiting_for remote_queue), up to the
// user's limit; then boot until the Jupyter server answers.
func (b *Backend) acquire(ctx context.Context, set Settings, jp *jobProgress) (*liveSession, error) {
	b.mu.Lock()
	if s := b.sess; s != nil {
		b.mu.Unlock()
		if s.rec.State == StateRunning {
			return s, nil
		}
		return nil, fallbackErr(CodeBusy, nil, errors.New("the remote session is busy (starting or stopping)"))
	}
	if now := b.now(); now.Before(b.cooldownUntil) {
		left := b.cooldownUntil.Sub(now)
		b.mu.Unlock()
		return nil, newErr(CodeWindowCooldown, map[string]any{"minutes": int(left.Minutes() + 0.5)},
			kindFallback, model.WaitRemoteWindow, errors.New("the fair-use window of the last session is cooling down"))
	}
	s := &liveSession{rec: sessionRecord{
		Version: recordVersion, State: StateQueued, NotebookHash: newNotebookHash(), Accelerator: set.Accelerator,
		Shape: set.shape(), RequestedAt: b.now(), RunID: b.o.RunID,
	}}
	b.sess = s
	b.mu.Unlock()
	b.persist(ctx, s.rec)
	b.log.Info("colab: requesting a runtime", "accelerator", set.Accelerator)

	wait := b.o.Backoff.Initial
	deadline := s.rec.RequestedAt.Add(time.Duration(set.MaxQueueWaitMin) * time.Minute)
	var a assignment
	for {
		var err error
		a, err = b.tun.assign(ctx, s.rec.NotebookHash, s.rec.Accelerator, s.rec.Shape, b.o.AllowHTTP)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			b.dropUnassigned(ctx, s)
			return nil, ctx.Err()
		}
		st := statusOf(err)
		switch {
		case st == 412 || transient(err):
			if b.now().Add(wait).After(deadline) {
				b.dropUnassigned(ctx, s)
				b.log.Info("colab: no accelerator within the wait limit; back to the local devices",
					"accelerator", set.Accelerator, "minutes", set.MaxQueueWaitMin)
				return nil, fallbackErr(CodeQueueTimeout, map[string]any{"accelerator": set.Accelerator, "minutes": set.MaxQueueWaitMin},
					fmt.Errorf("no %s runtime within %d minutes (last answer: %s)", set.Accelerator, set.MaxQueueWaitMin, err))
			}
			jp.waiting(model.WaitRemoteQueue)
			if err := b.o.Sleep(ctx, wait); err != nil {
				b.dropUnassigned(ctx, s)
				return nil, err
			}
			wait = min(2*wait, b.o.Backoff.Max)
		case st == 400:
			b.dropUnassigned(ctx, s)
			return nil, fallbackErr(CodeAcceleratorUnavailable, map[string]any{"accelerator": set.Accelerator},
				errors.New("Colab refused the accelerator (HTTP 400: no entitlement or quota)"))
		default:
			b.dropUnassigned(ctx, s)
			return nil, codedHTTP("tun/m/assign", err)
		}
	}
	b.mu.Lock()
	s.rec.Endpoint, s.rec.AssignedAt, s.rec.State = a.Endpoint, b.now(), StateBooting
	if a.Accelerator != "" && a.Accelerator != s.rec.Accelerator {
		b.log.Warn("colab: assigned another accelerator than requested", "requested", s.rec.Accelerator, "assigned", a.Accelerator)
		s.rec.Accelerator = a.Accelerator
	}
	s.rt = b.newRuntime(a.Proxy)
	s.proxyExp = b.now().Add(a.Proxy.ExpiresIn)
	rec := s.rec
	b.mu.Unlock()
	b.persist(ctx, rec)
	jp.waiting("")
	b.log.Info("colab: runtime assigned; booting", "accelerator", rec.Accelerator)

	if err := b.boot(ctx, s); err != nil {
		return nil, err
	}
	b.mu.Lock()
	s.rec.State, s.rec.ReadyAt = StateRunning, b.now()
	rec = s.rec
	b.mu.Unlock()
	b.persist(ctx, rec)
	b.log.Info("colab: runtime ready", "accelerator", rec.Accelerator, "waited_s", int(rec.ReadyAt.Sub(rec.RequestedAt).Seconds()))
	return s, nil
}

// boot waits until the runtime's Jupyter server answers (GET /api/kernels).
// Upstream connects right away (with three retries on timeouts); polling
// covers a proxy that is not ready yet.
func (b *Backend) boot(ctx context.Context, s *liveSession) error {
	deadline := b.now().Add(b.o.BootTimeout)
	delay := 3 * time.Second
	refreshed := false
	for {
		rt, err := b.runtime(ctx, s)
		if err == nil {
			_, err = rt.listKernels(ctx)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st := statusOf(err)
		switch {
		case (st == 401 || st == 403) && !refreshed:
			refreshed = true
			b.mu.Lock()
			s.proxyExp = time.Time{} // fetch a fresh proxy token
			b.mu.Unlock()
			continue
		case st == 401 || st == 403:
			return fallbackErr(CodeSessionReclaimed, nil, fmt.Errorf("the runtime refuses its proxy token (HTTP %d)", st))
		case st == 404 || transient(err):
		default:
			return codedHTTP("api/kernels", err)
		}
		if !b.now().Add(delay).Before(deadline) {
			return fallbackErr(CodeBootTimeout, map[string]any{"minutes": int(b.o.BootTimeout.Minutes())},
				fmt.Errorf("the runtime did not answer within %s (last: %s)", b.o.BootTimeout, err))
		}
		if err := b.o.Sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(2*delay, 30*time.Second)
	}
}

// runtime returns the session's runtime client with a proxy token that is
// valid for at least proxyRefreshAhead (upstream State.get_session: refresh
// through list_assignments; an assignment that is gone = reclaimed).
func (b *Backend) runtime(ctx context.Context, s *liveSession) (*runtimeClient, error) {
	b.mu.Lock()
	if s.rt != nil && b.now().Add(proxyRefreshAhead).Before(s.proxyExp) {
		rt := s.rt
		b.mu.Unlock()
		return rt, nil
	}
	endpoint := s.rec.Endpoint
	b.mu.Unlock()
	list, err := b.tun.listAssignments(ctx, b.o.AllowHTTP)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.Endpoint == endpoint {
			b.mu.Lock()
			s.rt = b.newRuntime(a.Proxy)
			s.proxyExp = b.now().Add(a.Proxy.ExpiresIn)
			rt := s.rt
			b.mu.Unlock()
			return rt, nil
		}
	}
	return nil, fallbackErr(CodeSessionReclaimed, nil, errors.New("the runtime is no longer among the account's assignments"))
}

// dropUnassigned forgets a session that never got a runtime. A POST whose
// answer was lost may still have assigned one: one GET with the same
// notebook hash tells (upstream reconciles the same way), and such a
// runtime is released.
func (b *Backend) dropUnassigned(ctx context.Context, s *liveSession) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if a, _, ok, err := b.tun.getAssign(ctx, s.rec.NotebookHash, s.rec.Accelerator, s.rec.Shape, b.o.AllowHTTP); err == nil && ok {
		if err := b.tun.unassign(ctx, a.Endpoint); err != nil {
			b.log.Warn("colab: releasing a runtime assigned after giving up", "code", Code(codedHTTP("tun/m/unassign", err)))
			s.rec.Endpoint, s.rec.State = a.Endpoint, StateStopping
			b.persist(ctx, s.rec)
			return
		}
	}
	b.mu.Lock()
	if b.sess == s {
		b.sess = nil
	}
	b.mu.Unlock()
	if err := deleteKV(ctx, b.o.KV, KVSession); err != nil {
		b.log.Warn("colab: deleting the session record", "err", err)
	}
}

// release gives the runtime back (V14.3 step 5): the kernel is shut down
// (best effort, as upstream's stop), then the assignment released. A
// release that fails keeps the record in state stopping; Tick tries again.
func (b *Backend) release(ctx context.Context, reason string) error {
	return b.releaseWith(ctx, reason, "")
}

// releaseWith is release recording file in ReleaseInfo.File (a remote file
// name, ReleaseCleanupFailed).
func (b *Backend) releaseWith(ctx context.Context, reason, file string) error {
	b.relMu.Lock()
	defer b.relMu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	b.mu.Lock()
	s := b.sess
	if s == nil {
		b.mu.Unlock()
		return nil
	}
	s.rec.State = StateStopping
	rec, rt, kid := s.rec, s.rt, s.kernelID
	b.mu.Unlock()
	b.persist(ctx, rec)
	if rec.Endpoint == "" {
		b.dropUnassigned(ctx, s)
		b.noteRelease(reason, file)
		return nil
	}
	if rt != nil && kid != "" {
		if rt2, err := b.runtime(ctx, s); err == nil {
			rt = rt2
		}
		if err := rt.deleteKernel(ctx, kid); err != nil {
			b.log.Info("colab: shutting down the kernel before release failed (ignored)", "code", Code(codedHTTP("api/kernels", err)))
		}
	}
	err := b.unassignIfListed(ctx, rec.Endpoint)
	if err != nil && WaitingFor(err) == model.WaitRemoteAuth {
		// No usable credential (its lifecycle ended, or Google rejects it):
		// the runtime cannot be released now. Keep the record for the next
		// import or start, stop retrying, and tell the user.
		b.log.Warn("colab: the runtime cannot be released without a credential; kept for later", "reason", reason, "code", Code(err))
		b.mu.Lock()
		if b.sess == s {
			b.sess = nil
		}
		b.leftover = &LeftoverInfo{Accelerator: rec.Accelerator, Since: rec.RequestedAt, Code: CodeLeftoverSession}
		b.lastErr = infoOf(err)
		b.mu.Unlock()
		return err
	}
	if err != nil {
		b.log.Warn("colab: releasing the runtime failed; will retry", "reason", reason, "code", Code(err))
		b.mu.Lock()
		b.lastErr = infoOf(err)
		b.mu.Unlock()
		return err
	}
	b.mu.Lock()
	if b.sess == s {
		b.sess = nil
	}
	if reason == ReleaseWindow && b.settings.WindowCooldownMin > 0 {
		b.cooldownUntil = b.now().Add(time.Duration(b.settings.WindowCooldownMin) * time.Minute)
		_ = b.o.KV.SetKV(ctx, KVCooldown, b.cooldownUntil.UTC().Format(time.RFC3339))
	}
	b.mu.Unlock()
	if err := deleteKV(ctx, b.o.KV, KVSession); err != nil {
		b.log.Warn("colab: deleting the session record", "err", err)
	}
	b.noteRelease(reason, file)
	b.log.Info("colab: runtime released", "reason", reason, "accelerator", rec.Accelerator, "jobs", rec.Jobs,
		"occupied_min", int(b.now().Sub(rec.RequestedAt).Minutes()))
	return nil
}

func (b *Backend) noteRelease(reason, file string) {
	b.mu.Lock()
	b.lastRelease = &ReleaseInfo{Reason: reason, At: b.now(), File: file}
	b.drained, b.afterCurrent = false, false
	b.mu.Unlock()
}

// unassignIfListed releases endpoint when the account still has it.
func (b *Backend) unassignIfListed(ctx context.Context, endpoint string) error {
	list, err := b.tun.listAssignments(ctx, b.o.AllowHTTP)
	if err != nil {
		return codedHTTP("tun/m/assignments", err)
	}
	for _, a := range list {
		if a.Endpoint == endpoint {
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				if lastErr = b.tun.unassign(ctx, endpoint); lastErr == nil {
					return nil
				}
				if !transient(lastErr) {
					break
				}
				if err := b.o.Sleep(ctx, time.Duration(2<<attempt)*time.Second); err != nil {
					break
				}
			}
			return fallbackErr(CodeReleaseFailed, map[string]any{"accelerator": a.Accelerator}, codedHTTP("tun/m/unassign", lastErr))
		}
	}
	return nil // already gone
}

// cleanupLeftover releases the session an earlier run left behind (V14.3
// step 5: "应用启动时检查并清理上次残留的会话"). Only the runtime the
// record names is touched, never other Colab sessions of the account.
// Without a usable credential it cannot: the record stays and Status
// reports it (CodeLeftoverSession).
func (b *Backend) cleanupLeftover(ctx context.Context, rec *sessionRecord) error {
	if _, err := b.auth.remaining(ctx); err != nil {
		b.mu.Lock()
		b.leftover = &LeftoverInfo{Accelerator: rec.Accelerator, Since: rec.RequestedAt, Code: CodeLeftoverSession}
		b.mu.Unlock()
		b.log.Warn("colab: a session of an earlier run may still be running, and there is no usable credential to release it",
			"accelerator", rec.Accelerator)
		return nil
	}
	var err error
	if rec.Endpoint != "" {
		err = b.unassignIfListed(ctx, rec.Endpoint)
	} else if rec.NotebookHash != "" {
		var a assignment
		var ok bool
		a, _, ok, err = b.tun.getAssign(ctx, rec.NotebookHash, rec.Accelerator, rec.Shape, b.o.AllowHTTP)
		if err == nil && ok {
			err = b.unassignIfListed(ctx, a.Endpoint)
		} else if err != nil {
			err = codedHTTP("tun/m/assign", err)
		}
	}
	if err != nil {
		b.mu.Lock()
		b.leftover = &LeftoverInfo{Accelerator: rec.Accelerator, Since: rec.RequestedAt, Code: Code(err)}
		b.mu.Unlock()
		b.log.Warn("colab: releasing the session of an earlier run failed", "code", Code(err))
		return err
	}
	if err := deleteKV(ctx, b.o.KV, KVSession); err != nil {
		return err
	}
	b.mu.Lock()
	b.leftover = nil
	b.mu.Unlock()
	b.noteRelease(ReleaseLeftover, "")
	b.log.Info("colab: the session of an earlier run was released", "accelerator", rec.Accelerator)
	return nil
}

// getAssign is the GET half of assign (upstream _get_assignment): an
// existing assignment of the notebook hash (assigned true), or the XSRF
// token for the POST (the answer was GetAssignmentResponse {acc, nbh,
// token, variant}).
func (t *tunnel) getAssign(ctx context.Context, nbh, accelerator, shape string, allowHTTP bool) (assignment, string, bool, error) {
	const ep = "tun/m/assign"
	o, ok, err := t.do(ctx, "GET", tunPrefix+"/assign", ep, assignQuery(nbh, accelerator, shape), nil, tunnelTimeout)
	if err != nil {
		return assignment{}, "", false, err
	}
	if !ok {
		return assignment{}, "", false, apiChanged(ep, "empty answer")
	}
	if o.has("endpoint") && o.has("runtimeProxyInfo") {
		a := assignment{Variant: -1, Shape: -1}
		if a.Endpoint, err = t.endpointField(o); err != nil {
			return assignment{}, "", false, err
		}
		if a.Proxy, err = t.proxyInfo(o, allowHTTP); err != nil {
			return assignment{}, "", false, err
		}
		return a, "", true, nil
	}
	if _, err := o.str("acc"); err != nil {
		return assignment{}, "", false, err
	}
	if _, err := o.str("nbh"); err != nil {
		return assignment{}, "", false, err
	}
	xsrf, err := o.str("token")
	if err != nil {
		return assignment{}, "", false, err
	}
	if xsrf == "" {
		return assignment{}, "", false, apiChanged(ep, "field token empty")
	}
	if v, err := o.str("variant"); err != nil {
		return assignment{}, "", false, err
	} else if v != "DEFAULT" && v != "GPU" && v != "TPU" {
		return assignment{}, "", false, apiChanged(ep, "unknown variant "+safeToken(v))
	}
	t.secrets.add(xsrf)
	return assignment{}, xsrf, false, nil
}

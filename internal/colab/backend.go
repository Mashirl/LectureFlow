// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// Options configure a Backend. Zero values take the defaults noted.
type Options struct {
	// KV stores the credential, the session record, settings and rates
	// (required; db.Store in the app, MemKV in tests).
	KV KV
	// HTTPClient for the OAuth, tunnel and runtime requests (nil = a client
	// without a global timeout; every request has its own).
	HTTPClient *http.Client
	// Dial opens the kernel channels (nil = DialWebSocket(nil)).
	Dial Dialer
	// ColabURL, TokenURL, RevokeURL: the services (defaults: upstream's
	// production URLs; tests point them at the replay server).
	ColabURL, TokenURL, RevokeURL string
	// ADCPath: where ImportADC reads gcloud's credentials ("" = ADCPath()).
	ADCPath string
	// WorkerDir holds colab_remote.py, asr.py and asr_common.py (required
	// for Transcribe).
	WorkerDir string
	// FFmpeg encodes the upload to FLAC ("" = upload the WAV as it is).
	FFmpeg    string
	RunFFmpeg FFmpegRunner // nil = run the ffmpeg binary
	Logger    *slog.Logger
	Now       func() time.Time
	// Sleep waits d or until ctx ends (tests advance a fake clock).
	Sleep func(ctx context.Context, d time.Duration) error
	// RunID identifies this run of the app ("" = random): a credential with
	// the session lifecycle belongs to one run.
	RunID string
	// ClientAgent is X-Colab-Client-Agent ("" = "lectureflow").
	ClientAgent string
	// AllowHTTP lets the runtime proxy URL be http:// (tests only).
	AllowHTTP bool
	// ChunkSize of uploads (0 = 4 MiB).
	ChunkSize int
	// Backoff while waiting for an accelerator (0 = 30 s doubling to 10 min).
	Backoff Backoff
	// BootTimeout for the runtime to answer after assignment (0 = 10 min).
	BootTimeout time.Duration
	// Defaults of the estimate (zero = DefaultEstimateDefaults()).
	Defaults *EstimateDefaults
	// RemoteBase is the runtime directory LectureFlow uses (Contents API
	// path; "" = "content/lectureflow", i.e. /content/lectureflow).
	RemoteBase string
}

// Backoff is the exponential back-off of the accelerator queue.
type Backoff struct{ Initial, Max time.Duration }

// RemoteRequirements are pip-installed on the runtime: the versions of this
// repository's .venv (faster-whisper 1.2.1 needs PyAV < 19, see
// worker/requirements.txt; ctranslate2 4.x is the CUDA 12 / cuDNN 9 build).
var RemoteRequirements = []string{"faster-whisper==1.2.1", "av==15.1.0", "ctranslate2==4.8.3"}

// workerFiles are uploaded to RemoteBase (colab_remote.py imports asr.py,
// which imports asr_common.py: the same transcription and sentence
// splitting as the local faster-whisper worker).
var workerFiles = []string{"colab_remote.py", "asr.py", "asr_common.py"}

// Steps of one lecture (V14.5 hover card: 等卡 → 上传音频 → 远端转写 →
// 下载结果 → 校验).
const (
	StepWaitAccelerator  = "wait_accelerator"
	StepUploadAudio      = "upload_audio"
	StepRemoteTranscribe = "remote_transcribe"
	StepDownloadResult   = "download_result"
	StepVerify           = "verify"
)

// Event is a progress report of Transcribe.
type Event struct {
	Step string `json:"step"`
	// Stage is the remote worker's stage during remote_transcribe (check,
	// install, download_model, load_model, transcribe, write).
	Stage string `json:"stage,omitempty"`
	// Fraction of the step (0..1; -1 unknown); Overall of the lecture.
	Fraction float64 `json:"fraction"`
	Overall  float64 `json:"overall"`
	// WaitingFor is model.WaitRemoteQueue while Colab has no accelerator.
	WaitingFor   string  `json:"waiting_for,omitempty"`
	ETASec       float64 `json:"eta_sec"` // -1 unknown
	SessionState string  `json:"session_state"`
}

// Job is one lecture to transcribe remotely.
type Job struct {
	// ID names the remote job directory ([A-Za-z0-9_-], ≤ 64; other
	// characters are dropped).
	ID string
	// AudioPath is the lecture's audio.wav.
	AudioPath string
	// DurationSec of the audio (estimate and rate; required > 0).
	DurationSec   float64
	Language      string
	InitialPrompt string
	// OutDir receives result.json (written atomically).
	OutDir string
	// WorkDir for the temporary FLAC ("" = OutDir).
	WorkDir string
}

// ReleaseInfo is the last release.
type ReleaseInfo struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// File: with ReleaseCleanupFailed, the remote file (a fixed name such
	// as result.json) that could not be deleted.
	File string `json:"file,omitempty"`
}

// LeftoverInfo: a session of an earlier run that could not be released.
type LeftoverInfo struct {
	Accelerator string    `json:"accelerator"`
	Since       time.Time `json:"since"`
	Code        string    `json:"code"`
}

// Backend is the Colab remote transcription backend (V14). One lecture at
// a time. See doc.go for the API overview.
type Backend struct {
	o       Options
	auth    *authManager
	tun     *tunnel
	secrets *secretSet
	log     *slog.Logger

	relMu sync.Mutex // one release at a time

	mu            sync.Mutex
	settings      Settings
	sess          *liveSession
	job           *jobState
	queue         []float64
	drained       bool
	afterCurrent  bool
	lastRelease   *ReleaseInfo
	leftover      *LeftoverInfo
	ccu           *ccuSnapshot
	rates         map[string]Rate
	cooldownUntil time.Time
	lastErr       *model.ErrorInfo
}

type ccuSnapshot struct {
	info ccuInfo
	at   time.Time
}

const ccuMaxAge = time.Minute

var errShutdownNow = errors.New("colab: shut down now")
var errAuthEnding = errors.New("colab: the credential is about to end")

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// New builds a Backend. Call Startup once before use.
func New(o Options) (*Backend, error) {
	if o.KV == nil {
		return nil, errors.New("colab: Options.KV is required")
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	if o.Dial == nil {
		o.Dial = DialWebSocket(nil)
	}
	if o.ColabURL == "" {
		o.ColabURL = DefaultColabURL
	}
	if o.TokenURL == "" {
		o.TokenURL = DefaultTokenURL
	}
	if o.RevokeURL == "" {
		o.RevokeURL = DefaultRevokeURL
	}
	if o.ADCPath == "" {
		o.ADCPath = ADCPath()
	}
	if o.RunFFmpeg == nil {
		o.RunFFmpeg = runFFmpeg
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = defaultSleep
	}
	if o.RunID == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		o.RunID = hex.EncodeToString(b[:])
	}
	if o.ClientAgent == "" {
		o.ClientAgent = defaultClientAgent
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = defaultChunkSize
	}
	if o.Backoff.Initial <= 0 {
		o.Backoff.Initial = 30 * time.Second
	}
	if o.Backoff.Max < o.Backoff.Initial {
		o.Backoff.Max = max(o.Backoff.Initial, 10*time.Minute)
	}
	if o.BootTimeout <= 0 {
		o.BootTimeout = 10 * time.Minute
	}
	if o.Defaults == nil {
		d := DefaultEstimateDefaults()
		o.Defaults = &d
	}
	if o.RemoteBase == "" {
		o.RemoteBase = "content/lectureflow"
	}
	b := &Backend{o: o, secrets: &secretSet{}, log: o.Logger, settings: DefaultSettings(), rates: map[string]Rate{}}
	b.auth = &authManager{kv: o.KV, hc: o.HTTPClient, tokenURL: o.TokenURL, revokeURL: o.RevokeURL, now: o.Now,
		runID: o.RunID, secrets: b.secrets, log: o.Logger}
	b.tun = &tunnel{base: o.ColabURL, hc: o.HTTPClient, auth: b.auth, agent: o.ClientAgent, secrets: b.secrets, log: o.Logger}
	return b, nil
}

func (b *Backend) now() time.Time { return b.o.Now() }

// Startup runs once when the app starts: deletes a credential its
// lifecycle no longer allows (a "session" one of an earlier run, an
// expired one), loads settings and measured rates, and releases a session
// an earlier run left behind (V14.3 step 5).
func (b *Backend) Startup(ctx context.Context) error {
	b.auth.startup(ctx)
	set, err := LoadSettings(ctx, b.o.KV)
	if err != nil {
		return err
	}
	rates := map[string]Rate{}
	if v, ok, err := getKV(ctx, b.o.KV, KVRates); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &rates)
	}
	var cooldown time.Time
	if v, ok, err := getKV(ctx, b.o.KV, KVCooldown); err == nil && ok {
		cooldown, _ = time.Parse(time.RFC3339, v)
	}
	b.mu.Lock()
	b.settings, b.rates, b.cooldownUntil = set, rates, cooldown
	b.mu.Unlock()
	rec, err := b.loadRecord(ctx)
	if err != nil {
		return err
	}
	if rec != nil {
		return b.secrets.scrubErr(b.cleanupLeftover(ctx, rec))
	}
	return nil
}

// Settings returns the current settings.
func (b *Backend) Settings() Settings {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settings
}

// SetSettings validates, stores and applies settings (a running session
// keeps its accelerator).
func (b *Backend) SetSettings(ctx context.Context, s Settings) error {
	if err := SaveSettings(ctx, b.o.KV, s); err != nil {
		return err
	}
	b.mu.Lock()
	b.settings = s
	b.mu.Unlock()
	return nil
}

// DetectADC is "检测授权": is gcloud's credential file there and importable?
func (b *Backend) DetectADC() ADCInfo { return DetectADC(b.o.ADCPath) }

// ImportADC imports gcloud's application default credentials with the
// user's lifecycle (V14.2), checking them with one token refresh. A
// leftover session that waited for a credential is released afterwards.
func (b *Backend) ImportADC(ctx context.Context, lc Lifecycle) (CredentialInfo, error) {
	info, err := b.auth.importADC(ctx, b.o.ADCPath, lc)
	if err != nil {
		return info, b.secrets.scrubErr(err)
	}
	if rec, err := b.loadRecord(ctx); err == nil && rec != nil {
		b.mu.Lock()
		live := b.sess != nil
		b.mu.Unlock()
		if !live {
			_ = b.cleanupLeftover(ctx, rec)
		}
	}
	return info, nil
}

// Credential describes the stored credential (no secret).
func (b *Backend) Credential(ctx context.Context) CredentialInfo { return b.auth.info(ctx) }

// DisconnectResult is the outcome of "断开连接".
type DisconnectResult struct {
	// Revoked: Google confirmed the revocation. The local copy is deleted
	// whatever this says.
	Revoked bool `json:"revoked"`
	// Released: a runtime was given back first.
	Released bool              `json:"released"`
	Warnings []model.ErrorInfo `json:"warnings,omitempty"`
}

// Disconnect ends the running lecture (back to the queue), releases the
// runtime, revokes the credential at Google and deletes the local copy
// (V14.2). A failed revocation still deletes the local copy and is
// reported as a warning. Note: gcloud's own ADC file holds the same refresh
// token, so revoking also signs gcloud's application-default login out.
func (b *Backend) Disconnect(ctx context.Context) (DisconnectResult, error) {
	var res DisconnectResult
	b.stopJob(errShutdownNow, 30*time.Second)
	b.mu.Lock()
	had := b.sess != nil
	b.mu.Unlock()
	if had {
		if err := b.release(ctx, ReleaseDisconnect); err != nil {
			res.Warnings = append(res.Warnings, *infoOf(b.secrets.scrubErr(err)))
		} else {
			res.Released = true
		}
	}
	revoked, err := b.auth.revokeAndDelete(ctx)
	res.Revoked = revoked
	if err != nil {
		if Code(err) == "" {
			return res, err // the database failed: the local copy may still be there
		}
		res.Warnings = append(res.Warnings, *infoOf(b.secrets.scrubErr(err)))
	}
	return res, nil
}

// Close is called when the app quits: the runtime is released, and a
// credential with the "session" lifecycle is deleted (its run ends).
func (b *Backend) Close(ctx context.Context) error {
	b.stopJob(errShutdownNow, 10*time.Second)
	err := b.release(ctx, ReleaseAppExit)
	if v, ok, _ := getKV(ctx, b.o.KV, KVCredential); ok {
		var c storedCredential
		if json.Unmarshal([]byte(v), &c) == nil && c.Lifecycle.Kind == LifecycleSession {
			_ = b.auth.deleteLocal(ctx)
		}
	}
	return b.secrets.scrubErr(err)
}

// SetQueue tells the backend the audio lengths (seconds) of the lectures
// planned for it, in order ("本次计划的剩余讲座", "还能再处理约 N 节").
// A non-empty queue clears an earlier QueueDrained.
func (b *Backend) SetQueue(audioSecs []float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queue = append([]float64(nil), audioSecs...)
	if len(audioSecs) > 0 {
		b.drained = false
	}
}

// QueueDrained says no more lectures are coming: the runtime is released
// now, or after the running lecture (V14.3 step 5: "队列清空…立即释放").
func (b *Backend) QueueDrained(ctx context.Context) error {
	b.mu.Lock()
	b.drained, b.queue = true, nil
	busy := b.job != nil
	b.mu.Unlock()
	if busy {
		return nil
	}
	return b.secrets.scrubErr(b.release(ctx, ReleaseQueueEmpty))
}

// ShutdownAfterCurrent is "处理完当前这节后关机": release after the
// running lecture, or now when none runs.
func (b *Backend) ShutdownAfterCurrent(ctx context.Context) error {
	b.mu.Lock()
	b.afterCurrent = true
	busy := b.job != nil
	b.mu.Unlock()
	if busy {
		return nil
	}
	return b.secrets.scrubErr(b.release(ctx, ReleaseAfterCurrent))
}

// ShutdownNow is "立即关机": the running lecture stops (Transcribe returns
// an error with IsRequeue true: it goes back to the queue; a lecture whose
// result was already written succeeds) and the runtime is released.
// Transcribe releases it when it notices the stop, but that is not assumed
// (Gate round 2: it may not have returned within the wait): the release
// here is a no-op when the runtime is already gone.
func (b *Backend) ShutdownNow(ctx context.Context) error {
	b.stopJob(errShutdownNow, 30*time.Second)
	return b.secrets.scrubErr(b.release(ctx, ReleaseNow))
}

// stopJob cancels the running lecture with cause and waits up to wait for
// Transcribe to return; false when no lecture was running.
func (b *Backend) stopJob(cause error, wait time.Duration) bool {
	b.mu.Lock()
	j := b.job
	b.mu.Unlock()
	if j == nil {
		return false
	}
	j.cancel(cause)
	select {
	case <-j.done:
	case <-time.After(wait):
	}
	return true
}

// Run calls Tick every 30 seconds until ctx ends.
func (b *Backend) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Tick(ctx)
		}
	}
}

// Tick does the time-driven work: releases an idle runtime (queue drained,
// after-current, idle limit, window used up, credential about to end),
// stops a lecture whose credential is about to end (back to the queue),
// and retries a failed release.
func (b *Backend) Tick(ctx context.Context) {
	b.mu.Lock()
	s, j := b.sess, b.job
	set := b.settings
	drained, after := b.drained && len(b.queue) == 0, b.afterCurrent
	var state string
	var idleSince, requested time.Time
	if s != nil {
		state, idleSince, requested = s.rec.State, s.lastJobEnd, s.rec.RequestedAt
		if idleSince.IsZero() {
			idleSince = s.rec.ReadyAt
		}
	}
	b.mu.Unlock()
	if s == nil {
		return
	}
	authLeft, authErr := b.auth.remaining(ctx)
	authEnding := authErr != nil || authLeft >= 0 && authLeft < time.Duration(b.o.Defaults.AuthMarginSec)*time.Second
	if j != nil {
		if authEnding {
			b.log.Info("colab: the credential is about to end; stopping the lecture and releasing the runtime")
			j.cancel(errAuthEnding)
		}
		return
	}
	reason := ""
	now := b.now()
	switch {
	case state == StateStopping:
		reason = ReleaseFailed // retry
	case state != StateRunning:
		return
	case authEnding:
		reason = ReleaseAuthEnding
	case after:
		reason = ReleaseAfterCurrent
	case drained:
		reason = ReleaseQueueEmpty
	case now.Sub(requested) >= FairWindow-time.Duration(b.o.Defaults.OverheadSec)*time.Second:
		reason = ReleaseWindow
	case !idleSince.IsZero() && now.Sub(idleSince) >= time.Duration(set.IdleReleaseMin)*time.Minute:
		reason = ReleaseIdle
	}
	if reason != "" {
		_ = b.release(ctx, reason)
	}
}

// ---- dispatch decision ----

func rateKey(accelerator, modelName, computeType string) string {
	return accelerator + "|" + modelName + "|" + computeType
}

// dispatchInput assembles the pure function's input from the backend's
// state. balance is the cached ccu-info (nil = unknown).
func (b *Backend) dispatchInput(audioSec float64, authLeft time.Duration, authOK bool) DispatchInput {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.settings
	in := DispatchInput{
		AudioSec: audioSec, WindowSec: FairWindow.Seconds(), Defaults: *b.o.Defaults, Paid: set.Tier == TierPaid,
		AuthRemainingSec: -1,
	}
	acc := set.Accelerator
	if s := b.sess; s != nil {
		acc = s.rec.Accelerator
		in.UsedSec = max(0, b.now().Sub(s.rec.RequestedAt).Seconds())
		in.SetupDone = s.setupDone
	}
	in.Rate = b.rates[rateKey(acc, set.Model, set.ComputeType)]
	in.CUPerHour = b.o.Defaults.CUPerHour[acc]
	if b.ccu != nil {
		in.BalanceKnown, in.BalanceCU = true, b.ccu.info.Balance
		if b.sess != nil && b.ccu.info.Assignments > 0 && b.ccu.info.RatePerHour > 0 {
			in.CUPerHour = b.ccu.info.RatePerHour // the account's real rate while our runtime runs
		}
	}
	if !authOK {
		in.AuthRemainingSec = 0
	} else if authLeft >= 0 {
		in.AuthRemainingSec = authLeft.Seconds()
	}
	return in
}

// refreshCCU reads ccu-info when the cached value is older than a minute.
func (b *Backend) refreshCCU(ctx context.Context) error {
	b.mu.Lock()
	fresh := b.ccu != nil && b.now().Sub(b.ccu.at) < ccuMaxAge
	b.mu.Unlock()
	if fresh {
		return nil
	}
	info, err := b.tun.ccuInfo(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.ccu = &ccuSnapshot{info: info, at: b.now()}
	b.mu.Unlock()
	return nil
}

// CanDispatch reports whether a lecture of lectureDurationSec should go to
// Colab now, and if not, the waiting_for value (model.WaitRemote*; "" when
// the backend is simply busy or Colab cannot be asked). Paid tier: the
// balance is read from Colab at most once a minute.
func (b *Backend) CanDispatch(ctx context.Context, lectureDurationSec float64) (bool, string) {
	authLeft, err := b.auth.remaining(ctx)
	if err != nil {
		return false, model.WaitRemoteAuth
	}
	b.mu.Lock()
	busy := b.job != nil || b.sess != nil && b.sess.rec.State != StateRunning
	cooling := b.sess == nil && b.now().Before(b.cooldownUntil)
	paid := b.settings.Tier == TierPaid
	b.mu.Unlock()
	if busy {
		return false, ""
	}
	if cooling {
		return false, model.WaitRemoteWindow
	}
	if paid {
		if err := b.refreshCCU(ctx); err != nil {
			b.mu.Lock()
			b.lastErr = infoOf(b.secrets.scrubErr(err))
			b.mu.Unlock()
			if WaitingFor(err) != "" {
				return false, WaitingFor(err)
			}
			return false, ""
		}
	}
	d := Decide(b.dispatchInput(lectureDurationSec, authLeft, true))
	return d.Dispatch, d.WaitingFor
}

// ---- status ----

// Status is what the resource bar and the 工作台 line show (V14.5).
type Status struct {
	Service     string         `json:"service"` // "colab"
	Configured  bool           `json:"configured"`
	Auth        CredentialInfo `json:"auth"`
	Accelerator string         `json:"accelerator"`
	Tier        string         `json:"tier"`
	// State: none, queued, booting, running, stopping, released (the last
	// release is in LastRelease).
	State string `json:"state"`
	// Window: fair-use window of the current session.
	WindowUsedSec  int64 `json:"window_used_sec"`
	WindowTotalSec int64 `json:"window_total_sec"`
	// LecturesLeft: "本次还能再处理约 N 节" (planned queue when known, else
	// lectures of TypicalLectureSec).
	LecturesLeft int `json:"lectures_left"`
	// Planned lectures (SetQueue) and how many of them fit.
	Planned        int      `json:"planned"`
	PlannedFit     int      `json:"planned_fit"`
	Current        *JobInfo `json:"current,omitempty"`
	BalanceCU      *float64 `json:"balance_cu,omitempty"`
	CUPerHour      float64  `json:"cu_per_hour"`
	PlanCostCU     float64  `json:"plan_cost_cu"`     // "本次预计消耗"
	SessionSpentCU float64  `json:"session_spent_cu"` // since assignment, at CUPerHour
	ShutdownAfter  bool     `json:"shutdown_after_current"`
	// CooldownUntil: no new session before this (after a window release).
	CooldownUntil *time.Time       `json:"cooldown_until,omitempty"`
	LastRelease   *ReleaseInfo     `json:"last_release,omitempty"`
	Leftover      *LeftoverInfo    `json:"leftover,omitempty"`
	LastError     *model.ErrorInfo `json:"last_error,omitempty"`
}

// JobInfo is the running lecture.
type JobInfo struct {
	ID        string    `json:"id"`
	Step      string    `json:"step"`
	Stage     string    `json:"stage,omitempty"`
	Fraction  float64   `json:"fraction"`
	Overall   float64   `json:"overall"`
	ETASec    float64   `json:"eta_sec"`
	StartedAt time.Time `json:"started_at"`
}

// Status returns the backend's state without contacting Colab (the balance
// is the last value read).
func (b *Backend) Status(ctx context.Context) Status {
	auth := b.auth.info(ctx)
	authLeft, authErr := b.auth.remaining(ctx)
	in := b.dispatchInput(TypicalLectureSec, authLeft, authErr == nil)
	// Before the balance was read once, "还能再处理约 N 节" counts the window
	// and the credential only (the balance appears when known).
	in.Paid = in.Paid && in.BalanceKnown
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{
		Service: "colab", Configured: auth.Present, Auth: auth, Accelerator: b.settings.Accelerator, Tier: b.settings.Tier,
		State: StateNone, WindowTotalSec: int64(FairWindow.Seconds()), CUPerHour: in.CUPerHour, ShutdownAfter: b.afterCurrent,
		LastRelease: b.lastRelease, Leftover: b.leftover, LastError: b.lastErr, Planned: len(b.queue),
	}
	if b.lastRelease != nil {
		st.State = StateReleased
	}
	if s := b.sess; s != nil {
		st.State, st.Accelerator = s.rec.State, s.rec.Accelerator
		st.WindowUsedSec = int64(in.UsedSec)
		if !s.rec.AssignedAt.IsZero() {
			st.SessionSpentCU = round2(b.now().Sub(s.rec.AssignedAt).Hours() * in.CUPerHour)
		}
	}
	if b.ccu != nil {
		v := round2(b.ccu.info.Balance)
		st.BalanceCU = &v
	}
	if b.now().Before(b.cooldownUntil) {
		t := b.cooldownUntil
		st.CooldownUntil = &t
	}
	if len(b.queue) > 0 {
		p := PlanQueue(in, b.queue)
		st.LecturesLeft, st.PlannedFit, st.PlanCostCU = p.Lectures, p.Lectures, p.CostCU
	} else {
		st.LecturesLeft = ApproxLectures(in, TypicalLectureSec)
	}
	if j := b.job; j != nil {
		st.Current = &JobInfo{ID: j.id, Step: j.ev.Step, Stage: j.ev.Stage, Fraction: j.ev.Fraction, Overall: j.ev.Overall,
			ETASec: j.ev.ETASec, StartedAt: j.started}
	}
	return st
}

func infoOf(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info := ce.Info
		return &info
	}
	return &model.ErrorInfo{Code: CodeUnavailable, Detail: err.Error()}
}

// ---- one lecture ----

var jobIDClean = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func remoteJobID(id string) string {
	id = jobIDClean.ReplaceAllString(id, "")
	if len(id) > 64 {
		id = id[:64]
	}
	if id == "" {
		id = randomHex(8)
	}
	return id
}

// Transcribe runs one lecture on Colab: wait for the accelerator → upload
// the audio → remote transcription → download the result → verify
// (V14.3). It returns the path of result.json in job.OutDir, the same
// structure the local faster-whisper worker writes.
//
// Errors: IsFallback (hand the lecture to the local devices; WaitingFor
// tells why when it is a dispatch condition), IsRequeue ("立即关机": back
// to the queue), a cancelled ctx (the runtime is released, the error wraps
// context.Canceled), or a local problem (missing audio). Any failure on
// the runtime releases it at once (V14.3 step 5).
func (b *Backend) Transcribe(ctx context.Context, job Job, progress func(Event)) (string, error) {
	path, err := b.transcribe(ctx, job, progress)
	return path, b.secrets.scrubErr(err)
}

func (b *Backend) transcribe(ctx context.Context, job Job, progress func(Event)) (string, error) {
	if progress == nil {
		progress = func(Event) {}
	}
	if _, err := os.Stat(job.AudioPath); err != nil {
		return "", newErr(CodeAudioMissing, map[string]any{"file": filepath.Base(job.AudioPath)}, kindLocal, "",
			errors.New("the lecture's audio file is missing"))
	}
	if job.OutDir == "" {
		return "", errors.New("colab: Job.OutDir is required")
	}
	if job.WorkDir == "" {
		job.WorkDir = job.OutDir
	}
	files, hash, err := b.readWorkerFiles()
	if err != nil {
		return "", err
	}
	authLeft, err := b.auth.remaining(ctx)
	if err != nil {
		return "", err
	}

	jctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jp := &jobProgress{b: b, out: progress, est: -1}
	b.mu.Lock()
	if b.job != nil {
		b.mu.Unlock()
		return "", fallbackErr(CodeBusy, nil, errors.New("another lecture is running on the remote runtime"))
	}
	set := b.settings
	j := &jobState{id: remoteJobID(job.ID), started: b.now(), cancel: cancel, done: make(chan struct{})}
	b.job = j
	jp.j = j
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.job = nil
		b.mu.Unlock()
		close(j.done)
	}()

	// Balance before borrowing (paid tier).
	if set.Tier == TierPaid {
		if err := b.refreshCCU(jctx); err != nil {
			return "", b.jobErr(ctx, jctx, err, false)
		}
	}
	if d := Decide(b.dispatchInput(job.DurationSec, authLeft, true)); !d.Dispatch {
		return "", decisionErr(d)
	}
	jp.est = EstimateSec(job.DurationSec, b.currentRate(set), *b.o.Defaults, b.setupDone())
	jp.step(StepWaitAccelerator, -1)
	s, err := b.acquire(jctx, set, jp)
	if err != nil {
		return "", b.jobErr(ctx, jctx, err, false)
	}
	// With the real session: check again (queue wait and boot count).
	if set.Tier == TierPaid {
		if err := b.refreshCCU(jctx); err != nil {
			return "", b.jobErr(ctx, jctx, err, true)
		}
	}
	if authLeft, err = b.auth.remaining(jctx); err != nil {
		return "", b.jobErr(ctx, jctx, err, true)
	}
	if d := Decide(b.dispatchInput(job.DurationSec, authLeft, true)); !d.Dispatch {
		return "", decisionErr(d) // the runtime stays for a lecture that fits; Tick releases it when idle
	}
	path, m, err := b.runJob(jctx, s, set, job, j.id, files, hash, jp)
	if err != nil {
		return "", b.jobErr(ctx, jctx, err, true)
	}
	b.finishJob(ctx, jctx, s, set, job, m)
	return path, nil
}

// decisionErr turns a negative decision into the error Transcribe returns.
func decisionErr(d Decision) error {
	switch d.WaitingFor {
	case model.WaitRemoteAuth:
		return newErr(CodeAuthTooShort, map[string]any{"need_min": int(d.EstimateSec/60 + 0.5)}, kindFallback, d.WaitingFor,
			errors.New("the credential ends before the lecture could finish"))
	case model.WaitRemoteBalance:
		return newErr(CodeBalanceLow, map[string]any{"need": fmt.Sprintf("%.2f", d.CostCU)}, kindFallback, d.WaitingFor,
			errors.New("not enough compute units for the lecture"))
	}
	return newErr(CodeWindowExhausted, map[string]any{"remaining_min": int(d.RemainingSec / 60), "need_min": int(d.EstimateSec/60 + 0.5)},
		kindFallback, model.WaitRemoteWindow, errors.New("the lecture does not fit the remaining fair-use window"))
}

// jobErr classifies a failure of the lecture and releases the runtime when
// one was held (V14.3 step 5: "任务失败、用户取消时立即释放").
func (b *Backend) jobErr(ctx, jctx context.Context, err error, held bool) error {
	cause := context.Cause(jctx)
	switch {
	case errors.Is(cause, errShutdownNow), errors.Is(cause, errAuthEnding):
		_ = b.release(ctx, stopReason(jctx))
		return newErr(CodeShutdownRequeue, nil, kindRequeue, "", errors.New("the remote runtime was shut down; the lecture goes back to the queue"))
	case ctx.Err() != nil:
		_ = b.release(ctx, ReleaseCancelled)
		return newErr(CodeCancelled, nil, kindFallback, "", ctx.Err())
	}
	if st := statusOf(err); Code(err) == "" && (st == 401 || st == 403 || st == 404 || st == 410) && isRuntimeErr(err) {
		err = fallbackErr(CodeSessionReclaimed, nil, err) // the runtime's Jupyter server is gone or refuses us
	}
	if Code(err) == "" {
		err = codedHTTP("colab", err)
		if Code(err) == "" {
			err = fallbackErr(CodeUnavailable, map[string]any{"endpoint": "runtime"}, err)
		}
	}
	reason := ReleaseFailed
	if Code(err) == CodeSessionReclaimed {
		reason = ReleaseReclaimed
	}
	b.mu.Lock()
	b.lastErr = infoOf(b.secrets.scrubErr(err))
	hasSess := b.sess != nil
	b.mu.Unlock()
	if held || hasSess {
		_ = b.release(ctx, reason)
	}
	b.log.Info("colab: lecture failed on Colab; back to the local devices", "code", Code(err))
	return err
}

// stopReason is the release reason of a lecture whose context jctx was
// cancelled: 立即关机 (also disconnect and quitting), the credential about
// to end, else the caller's cancel.
func stopReason(jctx context.Context) string {
	cause := context.Cause(jctx)
	switch {
	case errors.Is(cause, errShutdownNow):
		return ReleaseNow
	case errors.Is(cause, errAuthEnding):
		return ReleaseAuthEnding
	}
	return ReleaseCancelled
}

// isRuntimeErr: an *httpError of the runtime's Jupyter server.
func isRuntimeErr(err error) bool {
	var he *httpError
	return errors.As(err, &he) && strings.HasPrefix(he.endpoint, "api/")
}

func (b *Backend) currentRate(set Settings) Rate {
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := set.Accelerator
	if b.sess != nil {
		acc = b.sess.rec.Accelerator
	}
	return b.rates[rateKey(acc, set.Model, set.ComputeType)]
}

func (b *Backend) setupDone() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sess != nil && b.sess.setupDone
}

// readWorkerFiles reads the scripts to upload and their combined hash.
func (b *Backend) readWorkerFiles() (map[string][]byte, string, error) {
	files := map[string][]byte{}
	var all []byte
	for _, name := range workerFiles {
		data, err := os.ReadFile(filepath.Join(b.o.WorkerDir, name))
		if err != nil {
			return nil, "", newErr(CodeWorkerMissing, map[string]any{"file": name}, kindFallback, "",
				fmt.Errorf("worker script %s not found", name))
		}
		files[name] = data
		all = append(all, []byte(name+"\x00")...)
		all = append(all, data...)
	}
	return files, sha256Hex(all), nil
}

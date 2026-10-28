// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/model"
)

// KV is the part of db.Store the updater uses.
type KV interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error
}

// kv keys (the update channel is the existing app.update_channel).
const (
	KVAutoCheck = "selfupdate.auto_check" // "1" | "0" (default on)
	KVSources   = "selfupdate.sources"    // JSON array of the extra sources
	// JSON {"at": RFC 3339, "result": "up_to_date" | "available" | "error",
	// "failures": failed checks in a row, "error": the last failure}
	KVLastCheck = "selfupdate.last_check"
)

// When automatic checks run (README "Privacy"): a day after a check that
// worked; after a failed one sooner, backing off: 1 h, 3 h, 6 h, then a day
// again. A release without update.json or signature (no_manifest: the
// maintainer has not uploaded the signature yet) is asked again every 2 h
// for about a day, so the update arrives within hours of the upload, then
// every 6 h. A check made by hand counts as well.
const (
	checkDaily      = 24 * time.Hour
	noManifestRetry = 2 * time.Hour
	noManifestLong  = 6 * time.Hour
	noManifestTries = 12 // 2 h apart: about a day
)

var retryLadder = []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour}

// CheckInterval is the time from a check to the next automatic one after
// failures failed checks in a row (0: the last one worked), the last of
// them with code.
func CheckInterval(failures int, code string) time.Duration {
	switch {
	case failures <= 0:
		return checkDaily
	case code == CodeNoManifest && failures <= noManifestTries:
		return noManifestRetry
	case code == CodeNoManifest:
		return noManifestLong
	case failures <= len(retryLadder):
		return retryLadder[failures-1]
	}
	return checkDaily
}

// States of Status.State.
const (
	StateNotConfigured = "not_configured" // Status.Reason says why; nothing is checked
	StateIdle          = "idle"           // not checked since the start
	StateChecking      = "checking"
	StateUpToDate      = "up_to_date"
	StateAvailable     = "available" // Status.Available is newer than this version
	StateDownloading   = "downloading"
	StateImporting     = "importing"
	StatePending       = "pending" // Status.Pending is installed at the next start
	StateError         = "error"   // the last check or download failed (Status.Error)
)

// Reasons of StateNotConfigured.
const (
	ReasonNoPublicKey = "no_public_key"
	ReasonWebMode     = "web_mode"
	ReasonDevBuild    = "dev_build"
	ReasonPlatform    = "unsupported_platform"
	ReasonNoSource    = "no_source"
)

const (
	opCheck    = "check"
	opDownload = "download"
	opImport   = "import"
)

// Options configure an Updater.
type Options struct {
	DataDir string // updates go to {DataDir}/updates
	Version string // the running version
	KV      KV
	// Desktop: the desktop app (Electron installs pending updates). In web
	// mode nothing could install them: not configured.
	Desktop  bool
	Portable bool   // the Windows portable build
	Platform string // "" = this computer's (windows-x64, darwin-arm64)

	// Overrides for tests ("" = the built-in values).
	PublicKey string
	Repo      string
	GitHubWeb string // https://github.com
	GitHubAPI string // https://api.github.com

	Client     *http.Client // checks; nil = NewHTTPClient(60 s)
	Downloader Downloader   // nil = HTTPDownloader
	Now        func() time.Time
	Log        *slog.Logger

	// Automatic checks: the first one this long after Run starts (default
	// 2 min), then Run looks every CheckEvery (default 1 h) whether a day
	// has passed since the last check.
	FirstCheckDelay time.Duration
	CheckEvery      time.Duration
	RequestTimeout  time.Duration // per request of a check (default 30 s)
}

// Status is GET /api/selfupdate.
type Status struct {
	State          string           `json:"state"`
	Reason         string           `json:"reason,omitempty"`
	CurrentVersion string           `json:"current_version"`
	Channel        string           `json:"channel"`
	Platform       string           `json:"platform,omitempty"`
	Kind           string           `json:"kind,omitempty"`
	AutoCheck      bool             `json:"auto_check"`
	LastCheck      string           `json:"last_check,omitempty"`
	NextCheck      string           `json:"next_check,omitempty"` // the next automatic check (auto_check on, after a first check; not before now)
	Available      *Release         `json:"available,omitempty"`
	Progress       *Progress        `json:"progress,omitempty"`
	Pending        *PendingInfo     `json:"pending,omitempty"`
	Rollback       *RollbackInfo    `json:"rollback,omitempty"`
	LastResult     *Result          `json:"last_result,omitempty"`
	Error          *model.ErrorInfo `json:"error,omitempty"`
}

// Release is a newer version found by a check.
type Release struct {
	Version    string `json:"version"`
	Channel    string `json:"channel"`
	ReleasedAt string `json:"released_at"`
	NotesURL   string `json:"notes_url,omitempty"`
	MinVersion string `json:"min_version,omitempty"`
	// Direct is false when this version is older than MinVersion: no
	// download here, the user installs the new version by hand.
	Direct bool   `json:"direct"`
	File   string `json:"file,omitempty"` // this installation's package ("" = none in the release)
	Size   int64  `json:"size,omitempty"`
	Source string `json:"source,omitempty"` // "github", "github-api" or the extra source that answered
}

// Progress of a download.
type Progress struct {
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
}

// PendingInfo summarizes pending.json.
type PendingInfo struct {
	Version    string `json:"version"`
	File       string `json:"file"` // base name
	Size       int64  `json:"size"`
	Origin     string `json:"origin"` // download | file
	VerifiedAt string `json:"verified_at"`
}

// RollbackInfo summarizes previous.json.
type RollbackInfo struct {
	Version string `json:"version"`
	// Package: previous.json names a verified package of that version
	// (otherwise Electron backs up the installation when it installs).
	Package bool `json:"package"`
}

// Settings is GET /api/selfupdate/settings.
type Settings struct {
	AutoCheck bool     `json:"auto_check"`
	Sources   []string `json:"sources"`
}

// SettingsPatch is the body of PATCH /api/selfupdate/settings.
type SettingsPatch struct {
	AutoCheck *bool     `json:"auto_check"`
	Sources   *[]string `json:"sources"`
}

// Updater checks, downloads and verifies application updates.
type Updater struct {
	opt        Options
	log        *slog.Logger
	now        func() time.Time
	reason     string
	pk         Key
	cur        components.Version
	platform   string
	kind       string
	repo       string
	web, api   string
	client     *http.Client
	dl         Downloader
	dir        string
	reqTimeout time.Duration

	root     context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup
	changeMu sync.Mutex
	onChange []func()

	mu        sync.Mutex
	op        string
	cancelDL  context.CancelCauseFunc
	dlDone    chan struct{} // closed when the running download has ended
	avail     *Verified
	checked   bool
	lastCheck time.Time
	failures  int    // failed checks in a row (the automatic checks back off)
	failCode  string // the code of the last failed check
	pending   *Pending
	previous  *Previous
	result    *Result
	progress  *Progress
	lastEmit  time.Time
	lastErr   *model.ErrorInfo
}

// New builds an Updater. It touches nothing on disk; call Startup, then
// run Run in a goroutine, and Close at shutdown.
func New(o Options) *Updater {
	u := &Updater{opt: o, log: o.Log, now: o.Now, client: o.Client, dl: o.Downloader,
		repo: o.Repo, web: o.GitHubWeb, api: o.GitHubAPI, reqTimeout: o.RequestTimeout}
	if u.log == nil {
		u.log = slog.Default()
	}
	if u.now == nil {
		u.now = time.Now
	}
	if u.client == nil {
		u.client = NewHTTPClient(60 * time.Second)
	}
	if u.dl == nil {
		u.dl = &HTTPDownloader{}
	}
	if u.repo == "" {
		u.repo = Repo
	}
	if u.web == "" {
		u.web = "https://github.com"
	}
	if u.api == "" {
		u.api = "https://api.github.com"
	}
	u.web = strings.TrimRight(u.web, "/")
	u.api = strings.TrimRight(u.api, "/")
	if u.reqTimeout <= 0 {
		u.reqTimeout = 30 * time.Second
	}
	if o.DataDir != "" {
		u.dir = filepath.Join(o.DataDir, "updates")
	}
	u.root, u.stop = context.WithCancel(context.Background())

	u.platform = o.Platform
	if u.platform == "" {
		u.platform = currentPlatform()
	}
	switch u.platform {
	case PlatformWindowsX64:
		u.kind = KindInstaller
		if o.Portable {
			u.kind = KindPortable
		}
	case PlatformDarwinArm64:
		u.kind = KindZip
	}

	key := o.PublicKey
	if key == "" {
		key = builtinPublicKey
	}
	var err error
	switch {
	case key == "":
		u.reason = ReasonNoPublicKey
	case !o.Desktop:
		u.reason = ReasonWebMode
	case u.kind == "":
		u.reason = ReasonPlatform
	case u.dir == "" || o.KV == nil:
		u.reason = ReasonNoSource
	}
	if u.reason == "" {
		if u.pk, err = ParsePublicKey(key); err != nil {
			u.log.Error("selfupdate: the built-in public key is unusable; updates are off", "error", err)
			u.reason = ReasonNoPublicKey
		}
	}
	if u.reason == "" {
		if u.cur, err = parseVersion(o.Version); err != nil {
			u.reason = ReasonDevBuild
		}
	}
	return u
}

func currentPlatform() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return PlatformWindowsX64
	case "darwin/arm64":
		return PlatformDarwinArm64
	}
	return ""
}

// importKinds are the package kinds an offline update may bring.
func (u *Updater) importKinds() []string {
	if u.platform == PlatformDarwinArm64 {
		return []string{KindZip, KindDMG}
	}
	return []string{u.kind}
}

// Enabled reports whether updates are configured.
func (u *Updater) Enabled() bool { return u.reason == "" }

// Dir is the updates directory.
func (u *Updater) Dir() string { return u.dir }

// OnChange registers fn, called (outside any lock) whenever the status
// changes; download progress at most once a second.
func (u *Updater) OnChange(fn func()) {
	u.changeMu.Lock()
	u.onChange = append(u.onChange, fn)
	u.changeMu.Unlock()
}

func (u *Updater) changed() {
	u.changeMu.Lock()
	fns := append([]func(){}, u.onChange...)
	u.changeMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Close cancels a running download and waits for it.
func (u *Updater) Close() {
	u.stop()
	u.wg.Wait()
}

// ---- settings ----

func (u *Updater) kvGet(ctx context.Context, key string) (string, bool) {
	if u.opt.KV == nil {
		return "", false
	}
	v, ok, err := u.opt.KV.GetKV(ctx, key)
	if err != nil {
		u.log.Warn("selfupdate: reading a setting", "key", key, "error", err)
		return "", false
	}
	return v, ok
}

// Settings reads the settings (defaults for missing or unreadable values).
func (u *Updater) Settings(ctx context.Context) Settings {
	s := Settings{AutoCheck: true, Sources: []string{}}
	if v, ok := u.kvGet(ctx, KVAutoCheck); ok && v == "0" {
		s.AutoCheck = false
	}
	if v, ok := u.kvGet(ctx, KVSources); ok {
		var list []string
		if json.Unmarshal([]byte(v), &list) == nil {
			// an entry this version no longer accepts (e.g. plain http) is
			// left out, the others stay
			usable := list[:0]
			for _, raw := range list {
				if _, err := parseSource(raw, u.githubLatest()); err == nil {
					usable = append(usable, raw)
				}
			}
			if norm, err := normalizeSources(usable, u.githubLatest()); err == nil {
				s.Sources = norm
			}
		}
	}
	return s
}

// UpdateSettings validates and stores a patch (omitted fields unchanged).
func (u *Updater) UpdateSettings(ctx context.Context, p SettingsPatch) (Settings, error) {
	if u.opt.KV == nil {
		return Settings{}, cerr(CodeNotConfigured, map[string]any{"reason": ReasonNoSource}, errors.New("no settings store"))
	}
	var srcJSON string
	if p.Sources != nil {
		norm, err := normalizeSources(*p.Sources, u.githubLatest())
		if err != nil {
			return u.Settings(ctx), err
		}
		b, _ := json.Marshal(norm)
		srcJSON = string(b)
	}
	if p.AutoCheck != nil {
		v := "0"
		if *p.AutoCheck {
			v = "1"
		}
		if err := u.opt.KV.SetKV(ctx, KVAutoCheck, v); err != nil {
			return u.Settings(ctx), err
		}
	}
	if p.Sources != nil {
		if err := u.opt.KV.SetKV(ctx, KVSources, srcJSON); err != nil {
			return u.Settings(ctx), err
		}
	}
	u.changed()
	return u.Settings(ctx), nil
}

// channel is the user's update channel (app.update_channel).
func (u *Updater) channel(ctx context.Context) string {
	if v, ok := u.kvGet(ctx, model.KVAppUpdateChannel); ok && v == ChannelBeta {
		return ChannelBeta
	}
	return ChannelStable
}

type lastCheckRecord struct {
	At       string           `json:"at"`
	Result   string           `json:"result"`
	Failures int              `json:"failures,omitempty"`
	Error    *model.ErrorInfo `json:"error,omitempty"`
}

// recordCheck keeps the outcome of a check (err nil: it worked) for the
// next start: when it was, the failures in a row and the last failure,
// which the status shows again after a restart.
func (u *Updater) recordCheck(ctx context.Context, at time.Time, result string, err error) {
	rec := lastCheckRecord{At: at.UTC().Format(time.RFC3339), Result: result}
	u.mu.Lock()
	if err == nil {
		u.failures, u.failCode = 0, ""
	} else {
		info := InfoOf(err)
		u.failures++
		u.failCode = info.Code
		rec.Failures, rec.Error = u.failures, &info
	}
	u.mu.Unlock()
	b, _ := json.Marshal(rec)
	if err := u.opt.KV.SetKV(ctx, KVLastCheck, string(b)); err != nil {
		u.log.Warn("selfupdate: saving the check time", "error", err)
	}
}

// lastCheckLocked is the time of the last check, never after now: a check
// recorded "in the future" (the clock was set back since) counts as made
// now, so automatic checks are not held up until that time (u.mu held).
func (u *Updater) lastCheckLocked() time.Time {
	if now := u.now(); u.lastCheck.After(now) {
		u.lastCheck = now
	}
	return u.lastCheck
}

// nextCheckLocked is when the next automatic check is due (zero: no check
// made yet; u.mu held).
func (u *Updater) nextCheckLocked() time.Time {
	last := u.lastCheckLocked()
	if last.IsZero() {
		return time.Time{}
	}
	return last.Add(CheckInterval(u.failures, u.failCode))
}

// ---- status ----

// offered reports whether v may be offered on channel and is newer than
// this version and than the pending update.
func (u *Updater) offeredLocked(v *Verified, channel string) bool {
	if v == nil {
		return false
	}
	m := v.Manifest
	if m.Channel == ChannelBeta && channel != ChannelBeta {
		return false
	}
	if compareVersions(m.Version, u.opt.Version) <= 0 {
		return false
	}
	return u.pending == nil || compareVersions(m.Version, u.pending.Version) > 0
}

func (u *Updater) direct(m *Manifest) bool {
	return m.MinVersion == "" || compareVersions(u.opt.Version, m.MinVersion) >= 0
}

// Status returns the current status.
func (u *Updater) Status(ctx context.Context) Status {
	set := u.Settings(ctx)
	channel := u.channel(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	st := Status{CurrentVersion: u.opt.Version, Channel: channel, Platform: u.platform, Kind: u.kind, AutoCheck: set.AutoCheck}
	if u.reason != "" {
		st.State, st.Reason = StateNotConfigured, u.reason
		return st
	}
	if last := u.lastCheckLocked(); !last.IsZero() {
		st.LastCheck = last.UTC().Format(time.RFC3339)
		if set.AutoCheck {
			next := u.nextCheckLocked()
			if now := u.now(); next.Before(now) {
				next = now
			}
			st.NextCheck = next.UTC().Format(time.RFC3339)
		}
	}
	if u.offeredLocked(u.avail, channel) {
		m := u.avail.Manifest
		r := &Release{Version: m.Version, Channel: m.Channel, ReleasedAt: m.ReleasedAt, NotesURL: m.NotesURL,
			MinVersion: m.MinVersion, Direct: u.direct(m), Source: u.avail.Source}
		if a, ok := m.Asset(u.platform, u.kind); ok {
			r.File, r.Size = a.Name, a.Size
		}
		st.Available = r
	}
	if p := u.pending; p != nil {
		st.Pending = &PendingInfo{Version: p.Version, File: filepath.Base(filepath.FromSlash(p.File)), Size: p.Size,
			Origin: p.Origin, VerifiedAt: p.VerifiedAt}
	}
	if p := u.previous; p != nil {
		st.Rollback = &RollbackInfo{Version: p.Version, Package: p.File != "" || p.Backup != ""}
	}
	if u.result != nil {
		r := *u.result
		st.LastResult = &r
	}
	if u.progress != nil {
		pr := *u.progress
		st.Progress = &pr
	}
	if u.lastErr != nil {
		e := *u.lastErr
		st.Error = &e
	}
	// An update that can be installed comes first: a failed check (the
	// network was not up yet, the latest release has no signature yet)
	// leaves the version found before, or the one downloaded, as it is,
	// with the failure as a note (Status.Error). A failed download of the
	// available version is the state error (download again).
	switch {
	case u.op == opCheck:
		st.State = StateChecking
	case u.op == opDownload:
		st.State = StateDownloading
	case u.op == opImport:
		st.State = StateImporting
	case u.lastErr != nil && IsDownloadError(u.lastErr.Code) && st.Available != nil:
		st.State = StateError
	case u.lastErr != nil && st.Available == nil && st.Pending == nil:
		st.State = StateError
	case st.Available != nil:
		st.State = StateAvailable
	case st.Pending != nil:
		st.State = StatePending
	case u.checked:
		st.State = StateUpToDate
	default:
		st.State = StateIdle
	}
	return st
}

// begin claims the single operation slot.
func (u *Updater) beginLocked(op string) error {
	if u.reason != "" {
		return cerr(CodeNotConfigured, map[string]any{"reason": u.reason}, nil)
	}
	if u.op != "" {
		return cerr(CodeBusy, map[string]any{"op": u.op}, nil)
	}
	if u.root.Err() != nil {
		return cerr(CodeBusy, map[string]any{"op": "shutdown"}, nil)
	}
	u.op = op
	return nil
}

func (u *Updater) begin(op string) error {
	u.mu.Lock()
	err := u.beginLocked(op)
	u.mu.Unlock()
	if err == nil {
		u.changed()
	}
	return err
}

// end releases the slot; err (nil = success, cancellation = no error) is
// what the status shows.
func (u *Updater) end(op string, err error) {
	u.mu.Lock()
	if u.op == op {
		u.op = ""
	}
	u.progress = nil
	if op == opDownload {
		u.cancelDL, u.dlDone = nil, nil
	}
	if err == nil || CodeOf(err) == CodeCancelled || errors.Is(err, context.Canceled) {
		u.lastErr = nil
	} else {
		info := InfoOf(err)
		u.lastErr = &info
	}
	u.mu.Unlock()
	u.changed()
}

// ---- checking ----

// Check asks the sources for update.json now. The outcome is in the
// returned status (an unreachable source or a bad signature is
// Status.Error); the error is only for a check that could not start
// (selfupdate.not_configured, selfupdate.busy).
func (u *Updater) Check(ctx context.Context) (Status, error) {
	if err := u.begin(opCheck); err != nil {
		return u.Status(ctx), err
	}
	err := u.check(ctx)
	u.end(opCheck, err)
	return u.Status(ctx), nil
}

func (u *Updater) check(ctx context.Context) error {
	set := u.Settings(ctx)
	channel := u.channel(ctx)
	now := u.now()
	u.mu.Lock()
	u.lastCheck = now
	u.mu.Unlock()
	srcs := u.sources(set.Sources, channel)
	if len(srcs) == 0 {
		err := cerr(CodeNotConfigured, map[string]any{"reason": ReasonNoSource}, errors.New("no repository and no extra source"))
		u.recordCheck(ctx, now, StateError, err)
		return err
	}
	v, err := u.checkSources(ctx, srcs, channel)
	if err != nil {
		if context.Cause(ctx) != nil {
			return err // stopped (shutdown, the request went away): not a failed check
		}
		u.recordCheck(ctx, now, StateError, err)
		return err
	}
	m := v.Manifest
	newer := compareVersions(m.Version, u.opt.Version) > 0 && (m.Channel == ChannelStable || channel == ChannelBeta)
	u.mu.Lock()
	u.checked = true
	if newer {
		u.avail = v
	} else {
		u.avail = nil
	}
	u.mu.Unlock()
	if newer {
		u.recordCheck(ctx, now, StateAvailable, nil)
		if err := writeFileAtomic(filepath.Join(u.dir, latestFile), v.Raw); err == nil {
			_ = writeFileAtomic(filepath.Join(u.dir, latestSigFile), v.Signature)
		}
		u.log.Info("selfupdate: a newer version is available", "version", m.Version, "channel", m.Channel, "source", redactURL(v.Source))
	} else {
		u.recordCheck(ctx, now, StateUpToDate, nil)
		os.Remove(filepath.Join(u.dir, latestFile))
		os.Remove(filepath.Join(u.dir, latestSigFile))
	}
	return nil
}

// Run checks once a day while automatic checks are on (sooner after a
// failed check, CheckInterval), until ctx ends.
func (u *Updater) Run(ctx context.Context) {
	if u.reason != "" {
		return
	}
	delay := u.opt.FirstCheckDelay
	if delay <= 0 {
		delay = 2 * time.Minute
	}
	every := u.opt.CheckEvery
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.root.Done():
			return
		case <-t.C:
		}
		if u.due(ctx) {
			if _, err := u.Check(ctx); err != nil && CodeOf(err) != CodeBusy {
				u.log.Warn("selfupdate: automatic check", "error", err)
			}
		}
		t.Reset(u.untilDue(every))
	}
}

// due: automatic checks are on, nothing runs, and the interval since the
// last check (also one made by hand) has passed: a day, sooner after a
// failure (CheckInterval).
func (u *Updater) due(ctx context.Context) bool {
	if !u.Settings(ctx).AutoCheck {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	next := u.nextCheckLocked()
	return u.op == "" && (next.IsZero() || !u.now().Before(next))
}

// untilDue is how long Run sleeps: every, or less when the next check is
// due earlier (a retry an hour after a failure is not put off to the tick
// after it).
func (u *Updater) untilDue(every time.Duration) time.Duration {
	u.mu.Lock()
	defer u.mu.Unlock()
	next := u.nextCheckLocked()
	if next.IsZero() {
		return every
	}
	if d := next.Sub(u.now()); d > 0 && d+time.Second < every {
		return d + time.Second
	}
	return every
}

// ---- downloading ----

// Download starts downloading the available version's package (202: the
// status is downloading). Errors: not_configured, busy, not_available,
// too_old, no_package.
func (u *Updater) Download(ctx context.Context) (Status, error) {
	channel := u.channel(ctx)
	if err := u.untrusted(); err != nil {
		return u.Status(ctx), err
	}
	u.mu.Lock()
	err := u.beginLocked(opDownload)
	if err != nil {
		u.mu.Unlock()
		return u.Status(ctx), err
	}
	v := u.avail
	var a Asset
	switch {
	case !u.offeredLocked(v, channel):
		err = cerr(CodeNotAvailable, nil, nil)
	case !u.direct(v.Manifest):
		err = cerr(CodeTooOld, map[string]any{"min_version": v.Manifest.MinVersion, "current": u.opt.Version}, nil)
	default:
		var ok bool
		if a, ok = v.Manifest.Asset(u.platform, u.kind); !ok {
			err = cerr(CodeNoPackage, map[string]any{"platform": u.platform, "kind": u.kind}, nil)
		}
	}
	if err != nil {
		u.op = ""
		u.mu.Unlock()
		return u.Status(ctx), err
	}
	dctx, cancel := context.WithCancelCause(u.root)
	done := make(chan struct{})
	u.cancelDL, u.dlDone = cancel, done
	u.progress = &Progress{BytesTotal: a.Size}
	u.lastErr = nil
	u.wg.Add(1)
	u.mu.Unlock()
	u.changed()
	go func() {
		defer u.wg.Done()
		defer close(done)
		defer cancel(nil)
		err := u.download(dctx, v, a)
		if err != nil && CodeOf(err) != CodeCancelled {
			u.log.Warn("selfupdate: download failed", "version", v.Manifest.Version, "error", err)
		}
		u.end(opDownload, err)
	}()
	return u.Status(ctx), nil
}

// CancelDownload stops a running download (the partial file stays for the
// next attempt) and waits for it to end, at most 10 seconds. It reports
// whether one was running.
func (u *Updater) CancelDownload() bool {
	u.mu.Lock()
	c, done := u.cancelDL, u.dlDone
	u.mu.Unlock()
	if c == nil {
		return false
	}
	c(errCancelledByUser)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		u.log.Warn("selfupdate: the download did not stop within 10 s")
	}
	return true
}

var errCancelledByUser = errors.New("download cancelled")

// checkTrust is CheckTrust (tests replace it).
var checkTrust = CheckTrust

// untrusted returns selfupdate.untrusted_dir when other accounts may
// change the data directory: nothing is downloaded or imported into it
// (the desktop shell would not install it either). A check that fails
// for another reason does not stop anything here; the shell checks again
// (-selfupdate-verify) and refuses then.
func (u *Updater) untrusted() error {
	if u.reason != "" || u.dir == "" {
		return nil
	}
	err := checkTrust(u.dir)
	if CodeOf(err) == CodeUntrustedDir {
		u.log.Warn("selfupdate: the data directory can be changed by other accounts; no update is downloaded or imported", "error", err)
		return err
	}
	if err != nil {
		u.log.Warn("selfupdate: checking the data directory's permissions", "error", err)
	}
	return nil
}

func (u *Updater) download(ctx context.Context, v *Verified, a Asset) error {
	m := v.Manifest
	dir := filepath.Join(u.dir, m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ioErr(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, manifestFile), v.Raw); err != nil {
		return ioErr(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, signatureFile), v.Signature); err != nil {
		return ioErr(err)
	}
	final := filepath.Join(dir, a.Name)
	if !fileMatches(final, a) {
		os.Remove(final)
		part := final + ".part"
		err := u.dl.Download(ctx, DownloadRequest{URLs: u.assetURLs(ctx, v, a), Path: part, Size: a.Size, SHA256: a.SHA256,
			Progress: u.setProgress})
		if c := context.Cause(ctx); c != nil {
			return cerr(CodeCancelled, nil, c)
		}
		if err != nil {
			var le *localError
			if errors.As(err, &le) || CodeOf(err) == CodeIO {
				return ioErr(err)
			}
			return cerr(CodeDownloadFailed, nil, err)
		}
		sum, n, err := hashFile(part)
		if err != nil {
			return ioErr(err)
		}
		if n != a.Size || sum != a.SHA256 {
			os.Remove(part)
			return cerr(CodeChecksum, map[string]any{"file": a.Name},
				fmt.Errorf("%s: %d bytes, SHA-256 %s; update.json says %d bytes, %s", a.Name, n, sum, a.Size, a.SHA256))
		}
		if err := renameRetry(part, final); err != nil {
			return ioErr(err)
		}
	}
	return u.makePending(m, a, OriginDownload)
}

func (u *Updater) setProgress(done int64) {
	now := u.now()
	u.mu.Lock()
	emit := false
	if u.progress != nil {
		u.progress.BytesDone = done
		if now.Sub(u.lastEmit) >= time.Second {
			u.lastEmit = now
			emit = true
		}
	}
	u.mu.Unlock()
	if emit {
		u.changed()
	}
}

// assetURLs are the URLs of a package, best first.
func (u *Updater) assetURLs(ctx context.Context, v *Verified, a Asset) []string {
	var out []string
	add := func(s string) {
		if s == "" || !secureURL(s) { // https, or http to this computer
			return
		}
		for _, x := range out {
			if x == s {
				return
			}
		}
		out = append(out, s)
	}
	if v.source != nil {
		add(v.source.assetURL(a))
	}
	add(a.URL)
	for _, s := range u.sources(u.Settings(ctx).Sources, ChannelStable) {
		if s.kind == srcTemplate || s.kind == srcDir {
			add(s.assetURL(a))
		}
	}
	for _, mu := range v.Manifest.mirrorURLs(a.URL) {
		add(mu)
	}
	return out
}

// ---- pending and previous ----

// makePending records a verified package as the update to install at the
// next start: previous.json first, then pending.json.
func (u *Updater) makePending(m *Manifest, a Asset, origin string) error {
	now := u.now().UTC().Format(time.RFC3339)
	prev := u.previousFor(now)
	if err := writeJSONAtomic(filepath.Join(u.dir, previousFile), prev); err != nil {
		return ioErr(err)
	}
	p := Pending{Schema: fileSchema, Version: m.Version, FromVersion: u.opt.Version, Channel: m.Channel,
		Platform: u.platform, Kind: a.Kind, File: relPath(m.Version, a.Name), Size: a.Size, SHA256: a.SHA256,
		Manifest: relPath(m.Version, manifestFile), Signature: relPath(m.Version, signatureFile),
		KeyID: hex.EncodeToString(keyIDDisplay(u.pk.KeyID)), Origin: origin, VerifiedAt: now}
	p.KeyID = strings.ToUpper(p.KeyID)
	if err := writeJSONAtomic(filepath.Join(u.dir, pendingFile), p); err != nil {
		return ioErr(err)
	}
	u.mu.Lock()
	u.pending = &p
	u.previous = &prev
	u.mu.Unlock()
	u.log.Info("selfupdate: update ready; it is installed at the next start", "version", p.Version, "origin", origin)
	u.cleanup(false)
	return nil
}

// previousFor describes the running version for previous.json: its
// verified package when updates/<version>/ has one (it came through the
// updater), else none (Electron backs up the installation). A backup
// Electron recorded for the same version is kept.
func (u *Updater) previousFor(now string) Previous {
	prev := Previous{Schema: fileSchema, Version: u.opt.Version, Platform: u.platform, Kind: u.kind, CreatedAt: now}
	var old Previous
	if ok, err := readJSON(filepath.Join(u.dir, previousFile), &old); ok && err == nil && old.Version == prev.Version {
		if _, ok := absPath(u.dir, old.Backup); ok {
			prev.Backup = old.Backup
		}
	}
	dir := filepath.Join(u.dir, u.opt.Version)
	raw, err1 := os.ReadFile(filepath.Join(dir, manifestFile))
	sig, err2 := os.ReadFile(filepath.Join(dir, signatureFile))
	if err1 != nil || err2 != nil {
		return prev
	}
	m, err := verifyManifest(u.pk, raw, sig)
	if err != nil || m.Version != u.opt.Version {
		return prev
	}
	for _, k := range append([]string{u.kind}, u.importKinds()...) {
		a, ok := m.Asset(u.platform, k)
		if ok && fileMatches(filepath.Join(dir, a.Name), a) {
			prev.Kind, prev.File, prev.Size, prev.SHA256 = a.Kind, relPath(u.opt.Version, a.Name), a.Size, a.SHA256
			break
		}
	}
	return prev
}

// DiscardPending removes the pending update: pending.json, then the
// downloaded or imported files of that version (updates/<version>/; the
// running version's files and Electron's backups stay). Downloading it
// again starts from the beginning.
func (u *Updater) DiscardPending(ctx context.Context) (Status, error) {
	u.mu.Lock()
	if err := u.beginLocked(opImport); err != nil {
		u.mu.Unlock()
		return u.Status(ctx), err
	}
	p := u.pending
	u.mu.Unlock()
	var err error
	if p != nil {
		if rerr := os.Remove(filepath.Join(u.dir, pendingFile)); rerr != nil && !isNotExist(rerr) {
			err = ioErr(rerr)
		} else {
			u.mu.Lock()
			u.pending = nil
			u.mu.Unlock()
			u.removeVersion(p.Version)
			u.cleanup(false)
			u.log.Info("selfupdate: the pending update was discarded", "version", p.Version)
		}
	}
	u.mu.Lock()
	u.op = ""
	u.mu.Unlock()
	u.changed()
	return u.Status(ctx), err
}

// removeVersion deletes updates/<version>/ unless it belongs to the
// running version or to previous.json (what a failed update returns to).
func (u *Updater) removeVersion(version string) {
	if _, err := parseVersion(version); err != nil || version != filepath.Base(version) || version == u.opt.Version {
		return
	}
	u.mu.Lock()
	keep := u.previous != nil && u.previous.Version == version
	u.mu.Unlock()
	if keep {
		return
	}
	if err := os.RemoveAll(filepath.Join(u.dir, version)); err != nil {
		u.log.Warn("selfupdate: removing the discarded update's files", "version", version, "error", err)
	}
}

// ---- startup and cleanup ----

// Startup reads what earlier runs left: the pending update (dropped once
// this version is not older than it: it was installed), previous.json,
// result.json, the last verified check result; then removes stale files.
func (u *Updater) Startup(ctx context.Context) {
	if u.reason != "" {
		return
	}
	if v, ok := u.kvGet(ctx, KVLastCheck); ok {
		var rec lastCheckRecord
		if json.Unmarshal([]byte(v), &rec) == nil {
			if t, err := time.Parse(time.RFC3339, rec.At); err == nil {
				u.lastCheck = t
				u.checked = rec.Result == StateUpToDate
				if rec.Result == StateError && rec.Failures > 0 && rec.Error != nil && rec.Error.Code != "" {
					// the last check failed: shown again, and the
					// automatic checks keep backing off
					e := *rec.Error
					u.lastErr, u.failures, u.failCode = &e, rec.Failures, e.Code
				}
			}
		}
	}
	u.loadPending()
	var prev Previous
	if ok, err := readJSON(filepath.Join(u.dir, previousFile), &prev); ok && err == nil && prev.Version != "" {
		u.previous = &prev
	}
	var res Result
	if ok, err := readJSON(filepath.Join(u.dir, resultFile), &res); ok && err == nil && res.Version != "" {
		u.result = &res
	}
	raw, err1 := os.ReadFile(filepath.Join(u.dir, latestFile))
	sig, err2 := os.ReadFile(filepath.Join(u.dir, latestSigFile))
	if err1 == nil && err2 == nil {
		if m, err := verifyManifest(u.pk, raw, sig); err == nil && compareVersions(m.Version, u.opt.Version) > 0 {
			u.avail = &Verified{Manifest: m, Raw: raw, Signature: sig}
		} else {
			os.Remove(filepath.Join(u.dir, latestFile))
			os.Remove(filepath.Join(u.dir, latestSigFile))
		}
	}
	u.cleanup(true)
}

func (u *Updater) loadPending() {
	path := filepath.Join(u.dir, pendingFile)
	var p Pending
	ok, err := readJSON(path, &p)
	if !ok {
		return
	}
	drop := func(why string) {
		u.log.Info("selfupdate: removing pending.json", "reason", why, "version", p.Version)
		os.Remove(path)
	}
	if err != nil || p.Schema != fileSchema {
		drop("unreadable")
		return
	}
	if _, err := parseVersion(p.Version); err != nil || p.Platform != u.platform {
		drop("invalid")
		return
	}
	if compareVersions(p.Version, u.opt.Version) <= 0 {
		drop("installed or older than this version")
		return
	}
	f, ok := absPath(u.dir, p.File)
	if fi, err := os.Stat(f); !ok || err != nil || fi.Size() != p.Size {
		drop("the package is missing")
		return
	}
	u.pending = &p
}

// cleanup removes version directories nothing refers to (not this version,
// the pending one, previous.json's, nor the available one being
// downloaded); atStart also removes offline-update staging directories.
// Other names (Electron's backup/) are never touched.
func (u *Updater) cleanup(atStart bool) {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return
	}
	keep := map[string]bool{u.opt.Version: true}
	u.mu.Lock()
	if u.pending != nil {
		keep[u.pending.Version] = true
	}
	if u.previous != nil {
		keep[u.previous.Version] = true
	}
	if u.avail != nil {
		keep[u.avail.Manifest.Version] = true
	}
	u.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		switch {
		case strings.HasPrefix(name, importPrefix):
			if atStart {
				os.RemoveAll(filepath.Join(u.dir, name))
			}
		case !keep[name]:
			if _, err := parseVersion(name); err == nil {
				if err := os.RemoveAll(filepath.Join(u.dir, name)); err != nil {
					u.log.Warn("selfupdate: removing an old update", "version", name, "error", err)
				}
			}
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The in-app update in the mock (SPEC-ext "4. 应用内更新", the Go side in
// internal/selfupdate and internal/api/handlers_selfupdate.go; web-builder,
// ext-step10 M5):
//
//	GET    /api/selfupdate           selfupdate.Status
//	POST   /api/selfupdate/check     200 Status after the check (about suCheckDur,
//	                                 "checking" meanwhile); a failed check is
//	                                 Status.error; 409 not_configured, busy
//	POST   /api/selfupdate/download  202 Status (downloading for about
//	                                 suDownloadDur, then pending); 409
//	                                 not_configured, busy, not_available, too_old,
//	                                 no_package
//	DELETE /api/selfupdate/download  200 Status: cancelled, the downloaded part
//	                                 stays (the next download continues)
//	POST   /api/selfupdate/import    JSON {"paths": [...]} / {"path"} or multipart
//	                                 "file" parts → 200 Status (pending) after
//	                                 about suImportDur ("importing" meanwhile)
//	DELETE /api/selfupdate/pending   200 Status: the pending update is dropped
//	GET    /api/selfupdate/settings  {"auto_check", "sources"}
//	PATCH  /api/selfupdate/settings  {auto_check?, sources?} → 200 settings;
//	                                 400 selfupdate.sources_invalid {index}
//
// The real server answers "not configured" (reason web_mode) unless the
// desktop app runs it. Builds before 3.2.0 had no update key (reason
// no_public_key: the workbench keeps the electron-updater hint of
// v3_update.go); since 2026-10-27 the maintainer's key is built in and the
// desktop app is configured. The mock keeps playing a build without the
// key in desktop mode, so that the update_* scenarios keep their
// electron-updater hint: web_mode, and no_public_key in desktop mode (the
// flag -onboarding desktop, the scenario ob_desktop); only
// the scenarios ?mock=su_* below play a desktop app with the in-app update
// configured (GET /api/app still says web unless the flag is given, so the
// pictures start the mock with -onboarding desktop). The running version is
// suCurrent:
//
//	su_idle       not checked yet; a check finds 3.2.0
//	su_uptodate   checked 3 hours ago, nothing newer
//	su_available  3.2.0 with its notes; the download takes suDownloadDur
//	su_downloading 3.2.0 downloading (from 35 %)
//	su_pending    3.2.0 downloaded and verified, installed at the next start;
//	              the previous version 3.1.0 is kept
//	su_both       3.2.0 pending and 3.2.1 available
//	su_error      the last check reached no source; checks fail until a
//	              mirror is added
//	su_nomanifest the latest release has no update.json signature yet
//	              (selfupdate.no_manifest: not a network problem); checks
//	              find nothing until a mirror is added
//	su_checkfail  3.2.0 was found yesterday; today's check reached neither
//	              GitHub nor the mirror: still available, the failure is a
//	              note (checks fail until another mirror is added)
//	su_nosource   no source at all (no repository built in, no mirror);
//	              checks fail with not_configured {reason: no_source} until a
//	              mirror is added
//	su_dlerror    downloading 3.2.0 failed (the engine's words in detail);
//	              downloading again works
//	su_dlfail     3.2.0 available; the first download fails at 40 % with a
//	              wrong SHA-256, the next one works
//	su_tooold     4.0.0 needs at least 3.5.0: shown, not downloadable
//	su_nopkg      3.2.0 has no package for this installation
//	su_rollback   the update to 3.2.0 did not start; the desktop app went back
//	              to 3.1.0 two hours ago (result.json rolled_back)
//	su_failed     installing 3.2.0 failed, nothing was changed (result.json)
//	su_installed  3.1.0 was installed by the updater this morning
//	su_nokey      a build without the update key (not_configured no_public_key)
//	su_dev        a development build (not_configured dev_build)
//
// Offline updates by file name: a name containing "bad" fails the signature
// check, one containing "old" is not newer (3.0.2); more than three files
// are import_invalid; one .zip alone is a complete bundle; otherwise the
// names must include a .json, a .minisig and a package
// (import_incomplete {missing}). Every change sends SSE changed {kind:
// "selfupdate"} (download progress every suTickEvery). The state is per
// mock instance and scenario; restart the server to get it back.

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/selfupdate"
)

// The mock's timing (tests shorten it).
var (
	suCheckDur    = 1200 * time.Millisecond
	suDownloadDur = 8 * time.Second
	suImportDur   = 1500 * time.Millisecond
	suTickEvery   = time.Second
)

const (
	suCurrent    = "3.1.0"
	suPlatform   = selfupdate.PlatformWindowsX64
	suKind       = selfupdate.KindInstaller
	suMaxSources = 8 // internal/selfupdate maxSources
	suPrefix     = "su_"
)

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		a := m.api
		mux.HandleFunc("GET /api/selfupdate", a(m.suGet))
		mux.HandleFunc("POST /api/selfupdate/check", a(m.suCheck))
		mux.HandleFunc("POST /api/selfupdate/download", a(m.suDownload))
		mux.HandleFunc("DELETE /api/selfupdate/download", a(m.suCancel))
		mux.HandleFunc("POST /api/selfupdate/import", a(m.suImport))
		mux.HandleFunc("DELETE /api/selfupdate/pending", a(m.suDiscard))
		mux.HandleFunc("GET /api/selfupdate/settings", a(m.suGetSettings))
		mux.HandleFunc("PATCH /api/selfupdate/settings", a(m.suPatchSettings))
	})
}

// suState is one scenario's updater.
type suState struct {
	mu        sync.Mutex
	sc        string
	reason    string // not configured: why ("" = configured)
	checked   bool   // the last check found nothing newer
	lastCheck time.Time
	avail     *selfupdate.Release
	pending   *selfupdate.PendingInfo
	rollback  *selfupdate.RollbackInfo
	result    *selfupdate.Result
	err       *model.ErrorInfo
	failures  int    // failed checks in a row (the next automatic check: selfupdate.CheckInterval)
	op        string // check | download | import
	settings  selfupdate.Settings

	dlStart  time.Time
	dlFrom   int64 // bytes there when the download (re)started
	dlFailAt int64 // su_dlfail: the first download fails here (0 = never)
	ticking  bool  // suTick announces the download's progress
}

type suStates struct {
	mu sync.Mutex
	by map[string]*suState
}

var suAll sync.Map // *mock → *suStates

// suReason is why the in-app update is not configured for the request ("" =
// configured: the scenarios su_*).
func suReason(r *http.Request) string {
	switch sc := scenario(r); {
	case sc == "su_nokey":
		return selfupdate.ReasonNoPublicKey
	case sc == "su_dev":
		return selfupdate.ReasonDevBuild
	case strings.HasPrefix(sc, suPrefix):
		return ""
	case obDesktopMode(obMode(r)):
		return selfupdate.ReasonNoPublicKey
	}
	return selfupdate.ReasonWebMode
}

func (m *mock) su(r *http.Request) *suState {
	v, _ := suAll.LoadOrStore(m, &suStates{by: map[string]*suState{}})
	all := v.(*suStates)
	all.mu.Lock()
	defer all.mu.Unlock()
	sc := scenario(r)
	if st := all.by[sc]; st != nil {
		return st
	}
	st := newSUState(sc, suReason(r), time.Now())
	all.by[sc] = st
	return st
}

// suRelease is a sample release: next (3.2.0), newer (3.2.1), major
// (4.0.0, needs 3.5.0) or nopkg (3.2.0 without this installation's package).
func suRelease(which string, now time.Time) *selfupdate.Release {
	r := &selfupdate.Release{Version: "3.2.0", Channel: selfupdate.ChannelStable, ReleasedAt: now.Add(-50 * time.Hour).UTC().Format(time.RFC3339),
		Direct: true, File: "LectureFlow-Setup-3.2.0-x64.exe", Size: 189_123_456, Source: "github"}
	switch which {
	case "newer":
		r.Version, r.File, r.Size = "3.2.1", "LectureFlow-Setup-3.2.1-x64.exe", 189_456_789
		r.ReleasedAt = now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	case "major":
		r.Version, r.File, r.Size, r.MinVersion, r.Direct = "4.0.0", "LectureFlow-Setup-4.0.0-x64.exe", 214_000_000, "3.5.0", false
	case "nopkg":
		r.File, r.Size = "", 0
	}
	r.NotesURL = "https://example.org/lectureflow/releases/v" + r.Version
	return r
}

func newSUState(sc, reason string, now time.Time) *suState {
	st := &suState{sc: sc, reason: reason, settings: selfupdate.Settings{AutoCheck: true, Sources: []string{}}}
	if reason != "" {
		return st
	}
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	pend := func(v string, origin string) {
		r := suRelease("next", now)
		st.pending = &selfupdate.PendingInfo{Version: v, File: r.File, Size: r.Size, Origin: origin, VerifiedAt: at(26 * time.Hour)}
		st.rollback = &selfupdate.RollbackInfo{Version: suCurrent}
	}
	switch sc {
	case "su_uptodate":
		st.checked, st.lastCheck = true, now.Add(-3*time.Hour)
	case "su_available", "su_dlfail":
		st.avail, st.lastCheck = suRelease("next", now), now.Add(-20*time.Minute)
		if sc == "su_dlfail" {
			st.dlFailAt = st.avail.Size * 2 / 5
		}
	case "su_downloading":
		st.avail, st.lastCheck = suRelease("next", now), now.Add(-30*time.Minute)
		st.op, st.dlStart, st.dlFrom = "download", now, st.avail.Size*35/100
	case "su_pending":
		pend("3.2.0", "download")
		st.checked, st.lastCheck = true, now.Add(-26*time.Hour)
	case "su_both":
		pend("3.2.0", "download")
		st.avail, st.lastCheck = suRelease("newer", now), now.Add(-2*time.Hour)
	case "su_error":
		st.lastCheck, st.failures = now.Add(-5*time.Minute), 1
		st.err = &model.ErrorInfo{Code: selfupdate.CodeUnreachable, Detail: suUnreachable}
	case "su_nomanifest":
		st.lastCheck, st.failures = now.Add(-40*time.Minute), 1
		st.err = &model.ErrorInfo{Code: selfupdate.CodeNoManifest, Detail: suNoManifest}
	case "su_checkfail":
		st.avail, st.lastCheck, st.failures = suRelease("next", now), now.Add(-30*time.Minute), 2
		st.settings.Sources = []string{"https://mirror.example.org/lectureflow/"}
		st.err = &model.ErrorInfo{Code: selfupdate.CodeUnreachable, Detail: suUnreachable + "\n" + suMirrorDown}
	case "su_dlerror":
		st.avail, st.lastCheck = suRelease("next", now), now.Add(-40*time.Minute)
		st.err = &model.ErrorInfo{Code: selfupdate.CodeDownloadFailed, Detail: "aria2c: errorCode=1 network problem has occurred. cause: connection reset by peer"}
	case "su_tooold":
		st.avail, st.lastCheck = suRelease("major", now), now.Add(-1*time.Hour)
	case "su_nopkg":
		st.avail, st.lastCheck = suRelease("nopkg", now), now.Add(-1*time.Hour)
	case "su_rollback":
		st.lastCheck = now.Add(-26 * time.Hour)
		st.result = &selfupdate.Result{Version: "3.2.0", From: suCurrent, Outcome: "rolled_back", At: at(2 * time.Hour),
			Error: "3.2.0 did not become healthy after 2 starts (service exited with code 1)"}
		st.rollback = &selfupdate.RollbackInfo{Version: suCurrent, Package: true}
	case "su_failed":
		st.lastCheck = now.Add(-26 * time.Hour)
		st.result = &selfupdate.Result{Version: "3.2.0", From: suCurrent, Outcome: "failed", At: at(3 * time.Hour),
			Error: "installer exited with code 2 (the installation directory is in use)"}
	case "su_installed":
		st.checked, st.lastCheck = true, now.Add(-1*time.Hour)
		st.result = &selfupdate.Result{Version: suCurrent, From: "3.0.2", Outcome: "installed", At: at(5 * time.Hour)}
	}
	return st
}

// The details of the failed checks (what every source answered).
const (
	suUnreachable = "github: selfupdate.unreachable: https://github.com/ShouldBeQuincy/LectureFlow/releases/latest/download/update.json: dial tcp: i/o timeout"
	suNoManifest  = "github: selfupdate.no_manifest: https://github.com/ShouldBeQuincy/LectureFlow/releases/latest/download/update.json.minisig: HTTP status 404"
	suMirrorDown  = "https://mirror.example.org/lectureflow/: selfupdate.unreachable: https://mirror.example.org/lectureflow/update.json: HTTP status 502"
)

// suNewer: version a is newer than b (the sample versions: x.y.z only).
func suNewer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return len(pa) > len(pb)
}

// progress of the running download (st.mu held).
func (st *suState) progress(now time.Time) int64 {
	if st.op != "download" || st.avail == nil {
		return 0
	}
	total := st.avail.Size
	el := now.Sub(st.dlStart).Seconds() / suDownloadDur.Seconds()
	done := st.dlFrom + int64(float64(total-st.dlFrom)*el)
	if done > total {
		done = total
	}
	return done
}

// advance finishes or fails the download with the clock (st.mu held); it
// reports whether the state changed.
func (st *suState) advance(now time.Time) bool {
	if st.op != "download" || st.avail == nil {
		return false
	}
	done := st.progress(now)
	if st.dlFailAt > 0 && done >= st.dlFailAt {
		st.op, st.dlFrom, st.dlFailAt = "", 0, 0
		st.err = &model.ErrorInfo{Code: selfupdate.CodeChecksum, Params: map[string]any{"file": st.avail.File}}
		return true
	}
	if done < st.avail.Size {
		return false
	}
	a := st.avail
	st.op, st.dlFrom = "", 0
	st.pending = &selfupdate.PendingInfo{Version: a.Version, File: a.File, Size: a.Size, Origin: "download", VerifiedAt: now.UTC().Format(time.RFC3339)}
	if st.rollback == nil {
		st.rollback = &selfupdate.RollbackInfo{Version: suCurrent}
	}
	return true
}

// view is the Status at now (st.mu held), as selfupdate.Updater.Status builds it.
func (st *suState) view(now time.Time, channel string) selfupdate.Status {
	st.advance(now)
	out := selfupdate.Status{CurrentVersion: suCurrent, Channel: channel, Platform: suPlatform, Kind: suKind, AutoCheck: st.settings.AutoCheck}
	if st.reason != "" {
		out.State, out.Reason = selfupdate.StateNotConfigured, st.reason
		return out
	}
	if !st.lastCheck.IsZero() {
		out.LastCheck = st.lastCheck.UTC().Format(time.RFC3339)
		if st.settings.AutoCheck {
			code := ""
			if st.err != nil {
				code = st.err.Code
			}
			next := st.lastCheck.Add(selfupdate.CheckInterval(st.failures, code))
			if next.Before(now) {
				next = now
			}
			out.NextCheck = next.UTC().Format(time.RFC3339)
		}
	}
	if a := st.avail; a != nil && suNewer(a.Version, suCurrent) && (st.pending == nil || suNewer(a.Version, st.pending.Version)) {
		r := *a
		out.Available = &r
	}
	if st.pending != nil {
		p := *st.pending
		out.Pending = &p
	}
	if st.rollback != nil {
		rb := *st.rollback
		out.Rollback = &rb
	}
	if st.result != nil {
		res := *st.result
		out.LastResult = &res
	}
	if st.op == "download" && st.avail != nil {
		out.Progress = &selfupdate.Progress{BytesDone: st.progress(now), BytesTotal: st.avail.Size}
		if out.Available == nil {
			r := *st.avail
			out.Available = &r
		}
	}
	if st.err != nil {
		e := *st.err
		out.Error = &e
	}
	switch {
	case st.op == "check":
		out.State = selfupdate.StateChecking
	case st.op == "download":
		out.State = selfupdate.StateDownloading
	case st.op == "import":
		out.State = selfupdate.StateImporting
	case st.err != nil && selfupdate.IsDownloadError(st.err.Code) && out.Available != nil:
		out.State = selfupdate.StateError // a failed download: download again
	case st.err != nil && out.Available == nil && out.Pending == nil:
		out.State = selfupdate.StateError // a failed check with nothing to install
	// (a failed check does not hide an update: available / pending with the note)
	case out.Available != nil:
		out.State = selfupdate.StateAvailable
	case out.Pending != nil:
		out.State = selfupdate.StatePending
	case st.checked:
		out.State = selfupdate.StateUpToDate
	default:
		out.State = selfupdate.StateIdle
	}
	return out
}

// suChannel is the mock's app.update_channel (v3_settings.go).
func (m *mock) suChannel() string {
	set := m.appSet()
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.settings.UpdateChannel == selfupdate.ChannelBeta {
		return selfupdate.ChannelBeta
	}
	return selfupdate.ChannelStable
}

func suChanged(s *store) {
	s.mu.Lock()
	s.notify(api.ChangedEvent{Kind: "selfupdate"})
	s.mu.Unlock()
}

func suError(w http.ResponseWriter, status int, code string, params map[string]any) {
	codedError(w, status, code, params)
}

// begin claims the operation slot (st.mu held); false after writing the error.
func (st *suState) begin(w http.ResponseWriter, op string) bool {
	if st.reason != "" {
		suError(w, http.StatusConflict, selfupdate.CodeNotConfigured, map[string]any{"reason": st.reason})
		return false
	}
	if st.op != "" {
		suError(w, http.StatusConflict, selfupdate.CodeBusy, map[string]any{"op": st.op})
		return false
	}
	st.op = op
	return true
}

func suSleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func (m *mock) suGet(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.su(r)
	channel := m.suChannel()
	st.mu.Lock()
	changed := st.advance(time.Now())
	m.suEnsureTick(st, s) // su_downloading starts downloading
	v := st.view(time.Now(), channel)
	st.mu.Unlock()
	if changed {
		suChanged(s)
	}
	writeJSON(w, http.StatusOK, v)
}

// suCheck: "checking" for suCheckDur, then what the scenario finds.
func (m *mock) suCheck(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.su(r)
	channel := m.suChannel()
	st.mu.Lock()
	st.advance(time.Now())
	if !st.begin(w, "check") {
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	suChanged(s)
	suSleep(r.Context(), suCheckDur)

	now := time.Now()
	st.mu.Lock()
	st.op, st.lastCheck, st.err = "", now, nil
	mirrors := len(st.settings.Sources) > 0
	switch {
	case st.sc == "su_error" && !mirrors:
		st.err = &model.ErrorInfo{Code: selfupdate.CodeUnreachable, Detail: suUnreachable}
	case st.sc == "su_nomanifest" && !mirrors:
		st.err = &model.ErrorInfo{Code: selfupdate.CodeNoManifest, Detail: suNoManifest}
	case st.sc == "su_checkfail" && len(st.settings.Sources) < 2:
		st.err = &model.ErrorInfo{Code: selfupdate.CodeUnreachable, Detail: suUnreachable}
	case st.sc == "su_nosource" && !mirrors:
		st.err = &model.ErrorInfo{Code: selfupdate.CodeNotConfigured, Params: map[string]any{"reason": selfupdate.ReasonNoSource}, Detail: "no repository and no extra source"}
	case st.sc == "su_uptodate" || st.sc == "su_installed":
		st.avail = nil
	case st.sc == "su_both":
		st.avail = suRelease("newer", now)
	case st.sc == "su_tooold":
		st.avail = suRelease("major", now)
	case st.sc == "su_nopkg":
		st.avail = suRelease("nopkg", now)
	default: // 3.2.0; not shown when it is the pending version (su_pending)
		st.avail = suRelease("next", now)
	}
	if st.err == nil {
		st.checked = true // up to date unless something newer is available or pending
		st.failures = 0
	} else {
		st.failures++
	}
	v := st.view(now, channel)
	st.mu.Unlock()
	suChanged(s)
	writeJSON(w, http.StatusOK, v)
}

func (m *mock) suDownload(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.su(r)
	channel := m.suChannel()
	now := time.Now()
	st.mu.Lock()
	st.advance(now)
	cur := st.view(now, channel)
	if !st.begin(w, "download") {
		st.mu.Unlock()
		return
	}
	a := cur.Available
	var fail func()
	switch {
	case a == nil:
		fail = func() { suError(w, http.StatusConflict, selfupdate.CodeNotAvailable, nil) }
	case !a.Direct:
		fail = func() {
			suError(w, http.StatusConflict, selfupdate.CodeTooOld, map[string]any{"min_version": a.MinVersion, "current": suCurrent})
		}
	case a.File == "":
		fail = func() {
			suError(w, http.StatusConflict, selfupdate.CodeNoPackage, map[string]any{"platform": suPlatform, "kind": suKind})
		}
	}
	if fail != nil {
		st.op = ""
		st.mu.Unlock()
		fail()
		return
	}
	st.err = nil
	st.dlStart = now
	m.suEnsureTick(st, s)
	v := st.view(now, channel)
	st.mu.Unlock()
	suChanged(s)
	writeJSON(w, http.StatusAccepted, v)
}

// suEnsureTick starts announcing a running download's progress (st.mu held).
func (m *mock) suEnsureTick(st *suState, s *store) {
	if st.op == "download" && !st.ticking {
		st.ticking = true
		go m.suTick(st, s)
	}
}

// suTick sends changed {kind: selfupdate} every suTickEvery while the
// download runs, and once more when it ends by itself.
func (m *mock) suTick(st *suState, s *store) {
	tk := time.NewTicker(suTickEvery)
	defer tk.Stop()
	for range tk.C {
		st.mu.Lock()
		if st.op != "download" { // cancelled: the cancel announced it
			st.ticking = false
			st.mu.Unlock()
			return
		}
		st.advance(time.Now())
		running := st.op == "download"
		if !running {
			st.ticking = false
		}
		st.mu.Unlock()
		suChanged(s)
		if !running {
			return
		}
	}
}

func (m *mock) suCancel(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.su(r)
	channel := m.suChannel()
	now := time.Now()
	st.mu.Lock()
	st.advance(now)
	changed := false
	if st.op == "download" {
		st.dlFrom = st.progress(now) // the partial file stays
		st.op = ""
		changed = true
	}
	v := st.view(now, channel)
	st.mu.Unlock()
	if changed {
		suChanged(s)
	}
	writeJSON(w, http.StatusOK, v)
}

func (m *mock) suDiscard(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.su(r)
	channel := m.suChannel()
	now := time.Now()
	st.mu.Lock()
	st.advance(now)
	if !st.begin(w, "import") {
		st.mu.Unlock()
		return
	}
	st.op = ""
	st.pending = nil
	v := st.view(now, channel)
	st.mu.Unlock()
	suChanged(s)
	writeJSON(w, http.StatusOK, v)
}

// suImportNames reads the names of the files of an offline update (the
// contents are not looked at); a written error returns ok = false.
func suImportNames(w http.ResponseWriter, r *http.Request) (names []string, ok bool) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		var in struct {
			Path  *string  `json:"path"`
			Paths []string `json:"paths"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeErr(w, err)
			return nil, false
		}
		paths := in.Paths
		if in.Path != nil {
			paths = append([]string{*in.Path}, paths...)
		}
		if len(paths) == 0 {
			suError(w, http.StatusBadRequest, "request.invalid", nil)
			return nil, false
		}
		for _, p := range paths {
			names = append(names, filepath.Base(strings.ReplaceAll(p, `\`, "/")))
		}
		return names, true
	}
	mr, err := r.MultipartReader()
	if err != nil {
		suError(w, http.StatusBadRequest, "upload.not_multipart", nil)
		return nil, false
	}
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		if p.FormName() == "file" && p.FileName() != "" {
			names = append(names, p.FileName())
		}
		p.Close()
	}
	if len(names) == 0 {
		suError(w, http.StatusBadRequest, "upload.no_file", nil)
		return nil, false
	}
	return names, true
}

// suImportCheck decides an offline update by its file names: the error
// code and params, or "" when it is accepted.
func suImportCheck(names []string) (int, string, map[string]any) {
	if len(names) > 3 {
		return http.StatusBadRequest, selfupdate.CodeImportInvalid, map[string]any{"file": names[3]}
	}
	for _, n := range names {
		l := strings.ToLower(n)
		if strings.Contains(l, "bad") {
			return http.StatusBadRequest, selfupdate.CodeSignatureInvalid, nil
		}
		if strings.Contains(l, "old") {
			return http.StatusConflict, selfupdate.CodeNotNewer, map[string]any{"version": "3.0.2", "current": suCurrent}
		}
	}
	if len(names) == 1 && strings.EqualFold(filepath.Ext(names[0]), ".zip") {
		return 0, "", nil
	}
	var manifest, sig, pkg bool
	for _, n := range names {
		switch strings.ToLower(filepath.Ext(n)) {
		case ".json":
			manifest = true
		case ".minisig":
			sig = true
		default:
			pkg = true
		}
	}
	switch {
	case !manifest:
		return http.StatusBadRequest, selfupdate.CodeImportIncomplete, map[string]any{"missing": "manifest"}
	case !sig:
		return http.StatusBadRequest, selfupdate.CodeImportIncomplete, map[string]any{"missing": "signature"}
	case !pkg:
		return http.StatusBadRequest, selfupdate.CodeImportIncomplete, map[string]any{"missing": "package"}
	}
	return 0, "", nil
}

func (m *mock) suImport(w http.ResponseWriter, r *http.Request, s *store) {
	st := m.su(r)
	channel := m.suChannel()
	names, ok := suImportNames(w, r)
	if !ok {
		return
	}
	st.mu.Lock()
	st.advance(time.Now())
	if !st.begin(w, "import") {
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	suChanged(s)
	suSleep(r.Context(), suImportDur)

	status, code, params := suImportCheck(names)
	now := time.Now()
	st.mu.Lock()
	st.op = ""
	if code == "" {
		next := suRelease("next", now)
		st.pending = &selfupdate.PendingInfo{Version: next.Version, File: next.File, Size: next.Size, Origin: "file", VerifiedAt: now.UTC().Format(time.RFC3339)}
		if st.rollback == nil {
			st.rollback = &selfupdate.RollbackInfo{Version: suCurrent}
		}
	}
	v := st.view(now, channel)
	st.mu.Unlock()
	suChanged(s)
	if code != "" {
		suError(w, status, code, params)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (m *mock) suGetSettings(w http.ResponseWriter, r *http.Request, _ *store) {
	st := m.su(r)
	st.mu.Lock()
	defer st.mu.Unlock()
	writeJSON(w, http.StatusOK, st.settings)
}

// suValidSource is the shape check of internal/selfupdate parseSource: an
// http(s) URL with a host and without user info.
func suValidSource(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

func (m *mock) suPatchSettings(w http.ResponseWriter, r *http.Request, s *store) {
	var in selfupdate.SettingsPatch
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.AutoCheck == nil && in.Sources == nil {
		suError(w, http.StatusBadRequest, "request.nothing_to_change", map[string]any{"fields": "auto_check, sources"})
		return
	}
	var sources []string
	if in.Sources != nil {
		list := *in.Sources
		if len(list) > suMaxSources {
			suError(w, http.StatusBadRequest, selfupdate.CodeSourcesInvalid, map[string]any{"index": suMaxSources})
			return
		}
		seen := map[string]bool{}
		sources = []string{}
		for i, raw := range list {
			v := strings.TrimSpace(raw)
			if !suValidSource(v) {
				suError(w, http.StatusBadRequest, selfupdate.CodeSourcesInvalid, map[string]any{"index": i})
				return
			}
			if !seen[v] {
				seen[v] = true
				sources = append(sources, v)
			}
		}
	}
	time.Sleep(200 * time.Millisecond) // the "saving" state is visible
	st := m.su(r)
	st.mu.Lock()
	if in.AutoCheck != nil {
		st.settings.AutoCheck = *in.AutoCheck
	}
	if sources != nil {
		st.settings.Sources = sources
	}
	out := st.settings
	out.Sources = append([]string{}, st.settings.Sources...)
	st.mu.Unlock()
	suChanged(s)
	writeJSON(w, http.StatusOK, out)
}

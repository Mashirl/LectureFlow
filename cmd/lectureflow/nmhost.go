// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
	"lectureflow/internal/nmhost"
)

// The browser extension's side of the app (SPEC-ext "Native Messaging
// 宿主", .claude/ext-brief.md §4): the IPC endpoint the native host dials,
// served in both modes, and the browsers' registration, refreshed at every
// start.

// Downloads is the download queue's side of the bridge (internal/fetch).
type Downloads interface {
	Check(ctx context.Context, req *bridge.CheckRequest) (*bridge.CheckReply, error)
	Enqueue(ctx context.Context, req *bridge.EnqueueRequest) (*bridge.EnqueueReply, error)
	Refresh(ctx context.Context, req *bridge.RefreshRequest) (*bridge.RefreshReply, error)
	// Subscribe delivers status pushes until ctx ends: first one per
	// unfinished task, then each change (bridge.Handler.Subscribe).
	Subscribe(ctx context.Context) <-chan bridge.Status
}

// TaskLocator is optional on Downloads: it answers "open" for a task id
// with the task's lecture (once imported) or course. Without it, an open
// by task id is open.not_found.
type TaskLocator interface {
	LocateTask(ctx context.Context, taskID int64) (courseID, lectureID string, err error)
}

// downloadsFor returns the download queue of the app (App.Downloads, which
// also implements TaskLocator). nil answers check, enqueue and refresh with
// "internal" and pushes nothing. A variable so tests can replace it.
var downloadsFor = func(a *App) Downloads {
	if a == nil || a.Downloads == nil {
		return nil // a nil *fetch.Queue must not become a non-nil interface
	}
	return a.Downloads
}

// OpenLinePrefix starts the stdout line that asks the desktop shell to show
// the window on a route: "LECTUREFLOW_OPEN #/lecture/ID".
const OpenLinePrefix = "LECTUREFLOW_OPEN "

// nmOptions are the inputs of startNM besides the app.
type nmOptions struct {
	Endpoint string // -nm-endpoint ("" = env or default, "off")
	Launcher string // -launcher
	// LauncherBlocked is -launcher-blocked (macOS: the desktop shell found
	// that it runs from a place that disappears).
	LauncherBlocked string
	Portable        bool // -portable
	Desktop         bool
	Port            int
	Version         string
	// BuildVersion is main.version, the version the build was given with
	// -ldflags (never -app-version): "dev" makes this a development build,
	// whose registration admits the unpacked development extension too
	// (nmhost.DevExtension; LECTUREFLOW_DEV_EXTENSION=1 does so in a
	// release build).
	BuildVersion string
	Stdout       io.Writer
	// ResourcesDir is cfg.ResourcesDir (where bin/lectureflow-nmhost is).
	ResourcesDir string
	Getenv       func(string) string
}

// startNM starts the endpoint and the registration; never fatal. The
// returned service is stopped by its Close (before the app shuts down).
func startNM(app *App, o nmOptions) *nmhost.Service {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	explicit := o.Endpoint != "" || o.Getenv(nmhost.EnvEndpoint) != ""
	endpoint, err := nmhost.ResolveEndpoint(o.Endpoint, o.Getenv)
	if err != nil {
		slog.Warn("nm: no endpoint for the browser extension", "err", err)
		endpoint = ""
	}
	h := &nmHandler{
		version:   o.Version,
		desktop:   o.Desktop,
		port:      o.Port,
		store:     app.Store,
		downloads: downloadsFor(app),
		stdout:    o.Stdout,
		openURL:   openInBrowser,
	}
	so := nmhost.ServiceOptions{Endpoint: endpoint, Handler: h}
	// Only the instance on the default endpoint manages the registration:
	// the host program always dials the default endpoint, and an isolated
	// instance (tests, acceptance with its own data dir) must not take the
	// browsers over.
	if !explicit && endpoint != "" {
		if reg := newRegistrar(o); reg != nil {
			so.Registrar = reg
			so.RegisterOnStart = true
		}
	}
	return nmhost.StartService(so)
}

// newRegistrar builds the registration of this build, or nil when there
// is no place for it.
func newRegistrar(o nmOptions) *nmhost.Registrar {
	ro, ok := registrarOptions(o)
	if !ok {
		return nil
	}
	if ro.DevExtension != "" {
		slog.Info("nm: the unpacked development extension may connect", "why", ro.DevExtension)
	}
	return nmhost.NewRegistrar(ro)
}

// registrarOptions are the registration's options for this build; false
// when there is no place for it.
func registrarOptions(o nmOptions) (nmhost.RegistrarOptions, bool) {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	dir, err := nmhost.ConfigDir(o.Getenv)
	if err != nil {
		slog.Warn("nm: no directory for the browser registration", "err", err)
		return nmhost.RegistrarOptions{}, false
	}
	launcher, blocked := launcherOf(o, runtime.GOOS, nmhost.ReadOnlyVolume)
	ro := nmhost.RegistrarOptions{
		Dir:             dir,
		HostSource:      hostProgram(o.ResourcesDir),
		CopyHost:        o.Portable || runtime.GOOS == "darwin",
		Launcher:        launcher,
		LauncherBlocked: blocked,
		DevExtension:    nmhost.DevExtension(o.BuildVersion, o.Getenv),
	}
	if ro.CopyHost {
		if ro.CopyDir, err = nmhost.HostCopyDir(); err != nil {
			slog.Warn("nm: no directory for the host copy", "err", err)
			return nmhost.RegistrarOptions{}, false
		}
	}
	nmhost.PlatformOptions(&ro)
	return ro, true
}

// launcherOf is the launcher launch.json may name, or why there is none
// although this is the desktop app: the shell's -launcher-blocked, or
// what this side finds about -launcher itself (a translocated app or one
// on a read-only volume under /Volumes on macOS, nmhost.UnstableLauncher),
// so that launch.json never names a program that will be gone.
func launcherOf(o nmOptions, goos string, readOnly func(string) bool) (launcher, blocked string) {
	switch {
	case nmhost.ValidLauncherBlocked(o.LauncherBlocked):
		return "", o.LauncherBlocked
	case o.LauncherBlocked != "":
		slog.Warn("nm: unknown -launcher-blocked value ignored", "value", o.LauncherBlocked)
	}
	if why := nmhost.UnstableLauncher(goos, o.Launcher, readOnly); why != "" {
		return "", why
	}
	return o.Launcher, ""
}

// hostProgram is {resources}/bin/lectureflow-nmhost(.exe), else the one
// next to this executable; "" when neither exists (registration is then
// skipped and the status says so).
func hostProgram(resourcesDir string) string {
	var cands []string
	if resourcesDir != "" {
		cands = append(cands, filepath.Join(resourcesDir, "bin", nmhost.HostExe()))
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), nmhost.HostExe()))
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() {
			return c
		}
	}
	return ""
}

// nmHandler is the app's bridge.Handler: hello and open here, the rest
// from the download queue.
type nmHandler struct {
	version   string
	desktop   bool
	port      int
	store     db.Store
	downloads Downloads
	stdout    io.Writer
	openURL   func(url string) error

	outMu sync.Mutex
}

func (h *nmHandler) mode() string {
	if h.desktop {
		return "desktop"
	}
	return "web"
}

func (h *nmHandler) Hello(ctx context.Context, req *bridge.HelloRequest) (*bridge.HelloReply, error) {
	courses, err := h.store.ListCourses(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]bridge.Course, 0, len(courses))
	for _, c := range courses {
		out = append(out, bridge.Course{ID: c.ID, Name: c.Name})
	}
	return &bridge.HelloReply{
		Protocol: bridge.ProtocolVersion,
		App:      bridge.AppInfo{Version: h.version, Running: true, Mode: h.mode()},
		Courses:  out,
	}, nil
}

var errNoQueue = errors.New("the download queue is not available")

func (h *nmHandler) Check(ctx context.Context, req *bridge.CheckRequest) (*bridge.CheckReply, error) {
	if h.downloads == nil {
		return nil, errNoQueue
	}
	return h.downloads.Check(ctx, req)
}

func (h *nmHandler) Enqueue(ctx context.Context, req *bridge.EnqueueRequest) (*bridge.EnqueueReply, error) {
	if h.downloads == nil {
		return nil, errNoQueue
	}
	return h.downloads.Enqueue(ctx, req)
}

func (h *nmHandler) Refresh(ctx context.Context, req *bridge.RefreshRequest) (*bridge.RefreshReply, error) {
	if h.downloads == nil {
		return nil, errNoQueue
	}
	return h.downloads.Refresh(ctx, req)
}

func (h *nmHandler) Subscribe(ctx context.Context) <-chan bridge.Status {
	if h.downloads != nil {
		return h.downloads.Subscribe(ctx)
	}
	ch := make(chan bridge.Status)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

// Open shows a lecture, or the lecture / course of a download task: in
// the desktop app through a stdout line the shell acts on, in web mode in
// the system browser.
func (h *nmHandler) Open(ctx context.Context, req *bridge.OpenRequest) (*bridge.OpenReply, error) {
	route, err := h.openRoute(ctx, req)
	if err != nil {
		return nil, err
	}
	if h.desktop {
		h.outMu.Lock()
		defer h.outMu.Unlock()
		// One Write: the line cannot interleave with a log line.
		if _, err := io.WriteString(h.stdout, OpenLinePrefix+route+"\n"); err != nil {
			return nil, err
		}
		return &bridge.OpenReply{}, nil
	}
	if err := h.openURL(fmt.Sprintf("http://127.0.0.1:%d/%s", h.port, route)); err != nil {
		return nil, err
	}
	return &bridge.OpenReply{}, nil
}

func (h *nmHandler) openRoute(ctx context.Context, req *bridge.OpenRequest) (string, error) {
	switch {
	case req.LectureID != "" && req.TaskID != 0, req.LectureID == "" && req.TaskID == 0:
		return "", bridge.NewError(bridge.CodeBadRequest, map[string]any{"field": "lectureId"})
	case req.LectureID != "":
		return h.lectureRoute(ctx, req.LectureID)
	}
	loc, ok := h.downloads.(TaskLocator)
	if h.downloads == nil || !ok {
		return "", bridge.NewError(bridge.CodeNotFound, nil)
	}
	courseID, lectureID, err := loc.LocateTask(ctx, req.TaskID)
	if err != nil {
		var be *bridge.Error
		if errors.As(err, &be) || errors.Is(err, db.ErrNotFound) {
			return "", bridge.NewError(bridge.CodeNotFound, nil)
		}
		return "", err
	}
	if lectureID != "" {
		if r, err := h.lectureRoute(ctx, lectureID); err == nil {
			return r, nil
		}
	}
	if courseID == "" {
		return "", bridge.NewError(bridge.CodeNotFound, nil)
	}
	if _, err := h.store.GetCourse(ctx, courseID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return "", bridge.NewError(bridge.CodeNotFound, nil)
		}
		return "", err
	}
	return "#/course/" + url.PathEscape(courseID), nil
}

func (h *nmHandler) lectureRoute(ctx context.Context, id string) (string, error) {
	if _, err := h.store.GetLecture(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return "", bridge.NewError(bridge.CodeNotFound, nil)
		}
		return "", err
	}
	return "#/lecture/" + url.PathEscape(id), nil
}

// openInBrowser opens an http://127.0.0.1 URL in the system browser (web
// mode's "open").
func openInBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

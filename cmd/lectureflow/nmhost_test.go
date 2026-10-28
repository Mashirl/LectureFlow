// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/nmhost"
)

// No test of this package serves the per-user endpoint or touches the
// browsers' registration: with the variable set to "off" (inherited by the
// binaries the tests build and run), only an explicit -nm-endpoint is
// served, and an instance with an explicit endpoint never registers.
func init() {
	os.Setenv(nmhost.EnvEndpoint, nmhost.EndpointOff)
}

func testNMEndpoint(t *testing.T) string {
	var b [8]byte
	rand.Read(b[:])
	if runtime.GOOS == "windows" {
		return `\\.\pipe\lectureflow-cmdtest-` + hex.EncodeToString(b[:])
	}
	return filepath.Join(t.TempDir(), "nm.sock")
}

// nmCall sends one request over the endpoint and returns the reply.
func nmCall(t *testing.T, endpoint string, req any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := nmhost.Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, _ := json.Marshal(req)
	if err := bridge.WriteFrame(c, b, bridge.MaxRequest); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	rb, err := bridge.ReadFrame(c, bridge.MaxToBrowser)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(rb, &m)
	return m
}

func errorCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestParseArgsNM(t *testing.T) {
	o, err := parseArgs([]string{"-launcher", `C:\LF\LectureFlow.exe`, "-portable", "-nm-endpoint", "off"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.Launcher != `C:\LF\LectureFlow.exe` || !o.Portable || o.NMEndpoint != "off" {
		t.Fatalf("%+v", o)
	}
	o, _ = parseArgs(nil, io.Discard)
	if o.Launcher != "" || o.Portable || o.NMEndpoint != "" || o.LauncherBlocked != "" {
		t.Fatalf("defaults %+v", o)
	}
	o, err = parseArgs([]string{"-launcher-blocked", "dmg"}, io.Discard)
	if err != nil || o.LauncherBlocked != "dmg" || o.Launcher != "" {
		t.Fatalf("%+v %v", o, err)
	}
}

// launch.json never names a program that will be gone (mac-fix): the
// shell's -launcher-blocked, or what this side finds about -launcher on
// macOS (App Translocation, a read-only volume under /Volumes).
func TestLauncherOf(t *testing.T) {
	const app = "/Applications/LectureFlow.app/Contents/MacOS/LectureFlow"
	const translocated = "/private/var/folders/ab/T/AppTranslocation/1F2E/d/LectureFlow.app/Contents/MacOS/LectureFlow"
	const dmg = "/Volumes/LectureFlow/LectureFlow.app/Contents/MacOS/LectureFlow"
	readOnly := func(dir string) bool { return dir == "/Volumes/LectureFlow" }
	for _, c := range []struct {
		goos, launcher, blockedArg string
		launch, blocked            string
	}{
		{"darwin", app, "", app, ""},
		{"darwin", translocated, "", "", nmhost.LauncherTranslocated},
		{"darwin", dmg, "", "", nmhost.LauncherDiskImage},
		{"darwin", "", nmhost.LauncherDiskImage, "", nmhost.LauncherDiskImage}, // the shell found it
		{"darwin", app, "bogus", app, ""},                                      // an unknown reason is ignored
		{"windows", `C:\LF\LectureFlow.exe`, "", `C:\LF\LectureFlow.exe`, ""},
		{"windows", translocated, "", translocated, ""},
	} {
		launch, blocked := launcherOf(nmOptions{Launcher: c.launcher, LauncherBlocked: c.blockedArg}, c.goos, readOnly)
		if launch != c.launch || blocked != c.blocked {
			t.Errorf("%s %q %q: (%q, %q), want (%q, %q)", c.goos, c.launcher, c.blockedArg, launch, blocked, c.launch, c.blocked)
		}
	}
}

// Security review L4 (nm-harden): the registration admits the unpacked
// development extension only in a development build (main.version left at
// "dev", whatever -app-version says) or with LECTUREFLOW_DEV_EXTENSION=1.
func TestRegistrarOptionsDevExtension(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		build, env, want string
	}{
		{"dev", "", nmhost.DevExtensionBuild},
		{"3.1.0", "", ""},
		{"3.1.0", "1", nmhost.DevExtensionEnv},
		{"3.1.0", "0", ""},
	} {
		getenv := func(k string) string {
			switch k {
			case nmhost.EnvDir:
				return dir
			case nmhost.EnvDevExtension:
				return c.env
			}
			return ""
		}
		ro, ok := registrarOptions(nmOptions{BuildVersion: c.build, Version: "3.1.0", Getenv: getenv})
		if !ok || ro.DevExtension != c.want || ro.Dir != dir {
			t.Errorf("build %q env %q: %v %q (dir %q)", c.build, c.env, ok, ro.DevExtension, ro.Dir)
		}
	}
	// The build's own version: a test binary is a development build.
	if version != nmhost.DevBuildVersion {
		t.Fatalf("main.version = %q in tests", version)
	}
}

// Desktop mode end to end: the extension's endpoint serves hello with the
// course list and open as a stdout line for the shell; the API reports the
// connection; an instance on its own endpoint does not register.
func TestNMDesktopEndToEnd(t *testing.T) {
	cfgPath, dataDir := writeTestConfig(t)
	ep := testNMEndpoint(t)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop", "-app-version", "3.2.0-test",
		"-nm-endpoint", ep, "-launcher", "LectureFlow.exe"}, testToken+"\n", AppOptions{})
	url := p.ready()
	a := <-p.app
	ctx := context.Background()
	course := model.Course{ID: db.NewID(), Name: "CSC1003"}
	if err := a.Store.CreateCourse(ctx, course); err != nil {
		t.Fatal(err)
	}
	lec := model.Lecture{ID: db.NewID(), CourseID: course.ID, Title: "L1", MediaPath: "m.mp4"}
	if err := a.Store.CreateLecture(ctx, lec); err != nil {
		t.Fatal(err)
	}

	rep := nmCall(t, ep, bridge.HelloRequest{Envelope: bridge.Envelope{Type: bridge.TypeHello, ID: "1"}, Protocol: bridge.ProtocolVersion,
		Extension: bridge.ExtensionInfo{Version: "0.1.0", Browser: "edge"}})
	app := rep["app"].(map[string]any)
	if rep["ok"] != true || app["version"] != "3.2.0-test" || app["mode"] != "desktop" || app["running"] != true {
		t.Fatalf("hello: %v", rep)
	}
	courses := rep["courses"].([]any)
	if len(courses) != 1 || courses[0].(map[string]any)["id"] != course.ID || courses[0].(map[string]any)["name"] != "CSC1003" {
		t.Fatalf("courses: %v", courses)
	}

	// The download queue is wired (App.Downloads): an unknown session is new.
	if r := nmCall(t, ep, bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "2"}, Platform: "generic", SessionIDs: []string{"x"}}); r["ok"] != true || len(r["items"].([]any)) != 0 {
		t.Fatalf("check of a new session: %v", r)
	}

	// Credentials in a request never reach the log or the data directory.
	const cookie, sig = "CMDSECRETCOOKIE-91c2", "CMDSECRETSIG-55aa"
	enq := bridge.EnqueueRequest{Envelope: bridge.Envelope{Type: bridge.TypeEnqueue, ID: "e"}, Platform: "generic", SessionID: "s",
		CourseID: course.ID, Referer: "https://example.edu/v?sig=" + sig,
		Tracks:  []bridge.Track{{URL: "https://media.example.edu/a.mp4?sig=" + sig, Kind: bridge.KindAudio, Format: bridge.FormatFile}},
		Cookies: []bridge.Cookie{{Name: "s", Value: cookie, Domain: "media.example.edu"}}}
	if r := nmCall(t, ep, enq); r["ok"] != true || r["taskId"].(float64) < 1 {
		t.Fatalf("enqueue: %v", r)
	}
	// The same session is now known to check (the task may already have
	// failed: there is no aria2c or media server here).
	if r := nmCall(t, ep, bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "2b"}, Platform: "generic", SessionIDs: []string{"s"}}); r["ok"] != true || len(r["items"].([]any)) != 1 {
		t.Fatalf("check after enqueue: %v", r)
	}

	// open: one stdout line the shell acts on.
	if r := nmCall(t, ep, bridge.OpenRequest{Envelope: bridge.Envelope{Type: bridge.TypeOpen, ID: "3"}, LectureID: lec.ID}); r["ok"] != true {
		t.Fatalf("open: %v", r)
	}
	want := OpenLinePrefix + "#/lecture/" + lec.ID
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.stdout(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q on stdout:\n%s", want, p.stdout())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := nmCall(t, ep, bridge.OpenRequest{Envelope: bridge.Envelope{Type: bridge.TypeOpen, ID: "4"}, LectureID: "01NOSUCHLECTURE"}); errorCode(r) != bridge.CodeNotFound {
		t.Fatalf("open unknown: %v", r)
	}
	if r := nmCall(t, ep, bridge.OpenRequest{Envelope: bridge.Envelope{Type: bridge.TypeOpen, ID: "5"}}); errorCode(r) != bridge.CodeBadRequest {
		t.Fatalf("open nothing: %v", r)
	}

	// GET /api/nmhost: listening, the latest connection, not managing the
	// registration (its own endpoint).
	var st nmhost.Status
	getJSON(t, url+"/api/nmhost", &st)
	if !st.Listening || st.Managed || st.LastConnection == nil || st.LastConnection.Browser != "edge" || st.LastConnection.ExtensionVersion != "0.1.0" {
		t.Fatalf("status: %+v", st)
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/api/nmhost/register", nil)
	req.Header.Set(api.TokenHeader, testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("register on an unmanaged instance: %d", resp.StatusCode)
	}

	// The endpoint closes with the app.
	p.stdinW.Close()
	if c := p.wait(10 * time.Second); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	if _, err := nmhost.Dial(context.Background(), ep); !errors.Is(err, nmhost.ErrNotListening) {
		t.Fatalf("endpoint after exit: %v", err)
	}
	for _, out := range []string{p.stdout(), p.stderr.String()} {
		if strings.Contains(out, cookie) || strings.Contains(out, sig) {
			t.Fatalf("credentials in the output: %q", out)
		}
	}
	filepath.Walk(dataDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), cookie) || strings.Contains(string(b), sig) {
			t.Errorf("%s holds a credential", path)
		}
		return nil
	})
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set(api.TokenHeader, testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// The default in tests is off: nothing listens, nothing registers.
func TestNMOffByDefaultInTests(t *testing.T) {
	cfgPath, _ := writeTestConfig(t)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	url := p.ready()
	var st nmhost.Status
	resp, err := http.Get(url + "/api/nmhost")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Listening || st.ListenError != nmhost.ListenOff || st.Managed {
		t.Fatalf("status: %+v", st)
	}
	p.sigc <- os.Interrupt
	p.wait(10 * time.Second)
}

// fakeDownloads is a download queue that can locate its tasks.
type fakeDownloads struct {
	courseID, lectureID string
}

func (f *fakeDownloads) Check(context.Context, *bridge.CheckRequest) (*bridge.CheckReply, error) {
	return &bridge.CheckReply{}, nil
}
func (f *fakeDownloads) Enqueue(context.Context, *bridge.EnqueueRequest) (*bridge.EnqueueReply, error) {
	return &bridge.EnqueueReply{TaskID: 1}, nil
}
func (f *fakeDownloads) Refresh(context.Context, *bridge.RefreshRequest) (*bridge.RefreshReply, error) {
	return &bridge.RefreshReply{}, nil
}
func (f *fakeDownloads) Subscribe(ctx context.Context) <-chan bridge.Status {
	ch := make(chan bridge.Status, 1)
	ch <- bridge.Status{TaskID: 1, State: bridge.StateQueued}
	go func() { <-ctx.Done(); close(ch) }()
	return ch
}
func (f *fakeDownloads) LocateTask(ctx context.Context, id int64) (string, string, error) {
	if id != 1 {
		return "", "", db.ErrNotFound
	}
	return f.courseID, f.lectureID, nil
}

func TestNMHandlerOpenWebModeAndTasks(t *testing.T) {
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	course := model.Course{ID: db.NewID(), Name: "CSC1003"}
	st.CreateCourse(ctx, course)
	lec := model.Lecture{ID: db.NewID(), CourseID: course.ID, Title: "L1", MediaPath: "m.mp4"}
	st.CreateLecture(ctx, lec)

	var opened []string
	dl := &fakeDownloads{courseID: course.ID}
	h := &nmHandler{version: "v", port: 4321, store: st, downloads: dl, openURL: func(u string) error { opened = append(opened, u); return nil }}

	// Web mode: the system browser opens the page.
	if _, err := h.Open(ctx, &bridge.OpenRequest{LectureID: lec.ID}); err != nil {
		t.Fatal(err)
	}
	// A task without a lecture yet: its course page.
	if _, err := h.Open(ctx, &bridge.OpenRequest{TaskID: 1}); err != nil {
		t.Fatal(err)
	}
	// Once imported: the lecture.
	dl.lectureID = lec.ID
	if _, err := h.Open(ctx, &bridge.OpenRequest{TaskID: 1}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"http://127.0.0.1:4321/#/lecture/" + lec.ID,
		"http://127.0.0.1:4321/#/course/" + course.ID,
		"http://127.0.0.1:4321/#/lecture/" + lec.ID,
	}
	if strings.Join(opened, "\n") != strings.Join(want, "\n") {
		t.Fatalf("opened %v, want %v", opened, want)
	}
	var be *bridge.Error
	if _, err := h.Open(ctx, &bridge.OpenRequest{TaskID: 9}); !errors.As(err, &be) || be.Code != bridge.CodeNotFound {
		t.Fatalf("unknown task: %v", err)
	}
	if _, err := h.Open(ctx, &bridge.OpenRequest{TaskID: 1, LectureID: lec.ID}); !errors.As(err, &be) || be.Code != bridge.CodeBadRequest {
		t.Fatalf("both ids: %v", err)
	}
	// A queue without LocateTask: open by task is not found.
	h.downloads = struct{ Downloads }{dl}
	if _, err := h.Open(ctx, &bridge.OpenRequest{TaskID: 1}); !errors.As(err, &be) || be.Code != bridge.CodeNotFound {
		t.Fatalf("no locator: %v", err)
	}

	// The queue's pushes pass through; without a queue the channel just
	// ends with the context.
	h.downloads = dl
	sctx, cancel := context.WithCancel(ctx)
	if s := <-h.Subscribe(sctx); s.TaskID != 1 {
		t.Fatalf("push %+v", s)
	}
	cancel()
	h.downloads = nil
	sctx, cancel = context.WithCancel(ctx)
	ch := h.Subscribe(sctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("a push without a queue")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the channel was not closed")
	}
	if _, err := h.Enqueue(ctx, &bridge.EnqueueRequest{}); err == nil {
		t.Fatal("enqueue without a queue")
	}
}

func TestHostProgramLookup(t *testing.T) {
	res := t.TempDir()
	if got := hostProgram(res); got != "" {
		t.Fatalf("found %q in an empty resources dir", got)
	}
	os.MkdirAll(filepath.Join(res, "bin"), 0o755)
	want := filepath.Join(res, "bin", nmhost.HostExe())
	os.WriteFile(want, []byte("x"), 0o755)
	if got := hostProgram(res); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

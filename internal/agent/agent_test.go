// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/remote"
)

// testDirs points the agent's directories into a temporary HOME / LOCALAPPDATA.
func testDirs(t *testing.T) Dirs {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	d, err := DefaultDirs()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// run calls Main and returns its exit code and stdout lines.
func run(t *testing.T, stdin io.Reader, args ...string) (int, []remote.AgentEvent, string) {
	t.Helper()
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	var out, errb bytes.Buffer
	code := Main(args, stdin, &out, &errb)
	var evs []remote.AgentEvent
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		ev, err := remote.ParseEvent(sc.Bytes())
		if err != nil {
			t.Fatalf("%v: line %q breaks the protocol: %v", args, sc.Text(), err)
		}
		evs = append(evs, ev)
	}
	return code, evs, errb.String()
}

func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestParseFlags(t *testing.T) {
	// Main makes the agent's directories before it looks at the flags: keep
	// them out of the real home (they appeared in ~/Library on a test Mac).
	testDirs(t)
	f := fileFlags["put"]
	if m, err := parseFlags([]string{"--job", "01A", "--kind", "audio", "--offset", "5"}, f); err != nil || m["offset"] != "5" {
		t.Fatal(m, err)
	}
	for _, args := range [][]string{
		{"--job", "../x", "--kind", "audio", "--offset", "0"},
		{"--job", "01A", "--job", "01B"},
		{"--job"},
		{"job", "01A"},
		{"--path", "/etc/passwd"},
		{"--offset", "-1"},
		{"--kind", "result"},
	} {
		if _, err := parseFlags(args, f); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if code, _, _ := run(t, nil, "put", "--job", "01A"); code != 2 {
		t.Fatalf("missing flags: exit %d", code)
	}
	if code, _, _ := run(t, nil, "rm", "-rf", "/"); code != 2 {
		t.Fatalf("unknown command: exit %d", code)
	}
}

func TestResolveDirs(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := t.TempDir() // an absolute path of this system
	d, _ := ResolveDirs("linux", env(map[string]string{"HOME": home}))
	if d.Root != filepath.Join(home, ".local", "share", "lectureflow-agent") || d.Jobs != filepath.Join(home, ".cache", "lectureflow-agent", "jobs") {
		t.Fatalf("linux %+v", d)
	}
	d, _ = ResolveDirs("linux", env(map[string]string{"HOME": home, "XDG_DATA_HOME": "relative"}))
	if !strings.Contains(d.Root, ".local") {
		t.Fatalf("a relative XDG_DATA_HOME is ignored: %+v", d)
	}
	d, _ = ResolveDirs("darwin", env(map[string]string{"HOME": home}))
	if d.Root != filepath.Join(home, "Library", "Application Support", "LectureFlow Agent") ||
		d.Jobs != filepath.Join(home, "Library", "Caches", "LectureFlow Agent", "jobs") {
		t.Fatalf("darwin %+v", d)
	}
	if _, err := ResolveDirs("darwin", env(nil)); err == nil {
		t.Fatal("no HOME accepted")
	}
	if _, err := ResolveDirs("linux", env(map[string]string{"HOME": "relative"})); err == nil {
		t.Fatal("a relative HOME accepted")
	}
}

func TestInsideRefusesEscapes(t *testing.T) {
	base := t.TempDir()
	for _, e := range [][]string{{".."}, {"a/../.."}, {""}, {"a\\b"}, {"x\x00"}} {
		if _, err := inside(base, e...); err == nil {
			t.Errorf("accepted %q", e)
		}
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(base, "link")); err == nil {
		if _, err := inside(base, "link", "x"); err == nil {
			t.Error("followed a symbolic link")
		}
		if err := checkPrivateDir(filepath.Join(base, "link")); err == nil {
			t.Error("a symbolic link passed as a private directory")
		}
	} else {
		t.Logf("symbolic links cannot be created here (%v); the link cases ran on the other platforms", err)
	}
}

func tarGz(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h := h
		body := []byte("data")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write(body)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestUntarRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	for name, h := range map[string]tar.Header{
		"dotdot":   {Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"absolute": {Name: "/evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"link out": {Name: "python/l", Typeflag: tar.TypeSymlink, Linkname: "../../../etc"},
		"link abs": {Name: "python/l", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"hard out": {Name: "python/h", Typeflag: tar.TypeLink, Linkname: "../../x"},
	} {
		p := filepath.Join(dir, name+".tar.gz")
		if err := os.WriteFile(p, tarGz(t, h), 0o600); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, name)
		_ = os.MkdirAll(dst, 0o700)
		if err := untarGz(p, dst); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	// A normal archive unpacks; set-id bits are dropped.
	p := filepath.Join(dir, "ok.tar.gz")
	_ = os.WriteFile(p, tarGz(t, tar.Header{Name: "python/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "python/bin/python3", Typeflag: tar.TypeReg, Mode: 0o4755}), 0o600)
	dst := filepath.Join(dir, "ok")
	_ = os.MkdirAll(dst, 0o700)
	if err := untarGz(p, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "python", "bin", "python3"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Fatal("setuid kept")
	}
}

func TestPutStatGetAndResume(t *testing.T) {
	d := testDirs(t)
	audio := bytes.Repeat([]byte("abcdefgh"), 4096)
	sum := shaHex(audio)
	e := &env{dirs: d, out: &eventWriter{w: io.Discard}, stderr: io.Discard, now: time.Now}
	if err := ensurePrivateDir(d.Jobs); err != nil {
		t.Fatal(err)
	}
	have, resume, err := e.openJob("01JOB", remote.AudioWAV, int64(len(audio)), sum)
	if err != nil || have != 0 || resume {
		t.Fatal(have, resume, err)
	}
	// A first upload that stops halfway.
	code, evs, _ := run(t, bytes.NewReader(audio[:10000]), "put", "--job", "01JOB", "--kind", "audio", "--offset", "0")
	if code != 1 || evs[0].Code != remote.AgentIncompleteUpload {
		t.Fatalf("partial put: %d %+v", code, evs)
	}
	_, evs, _ = run(t, nil, "stat", "--job", "01JOB", "--kind", "audio")
	if evs[0].Have != 10000 || evs[0].Complete {
		t.Fatalf("stat %+v", evs[0])
	}
	// A gap is refused, the rest from "have" completes it.
	if code, _, _ := run(t, bytes.NewReader(audio[20000:]), "put", "--job", "01JOB", "--kind", "audio", "--offset", "20000"); code != 1 {
		t.Fatal("a gap was accepted")
	}
	code, evs, _ = run(t, bytes.NewReader(audio[10000:]), "put", "--job", "01JOB", "--kind", "audio", "--offset", "10000")
	if code != 0 || evs[0].Bytes != int64(len(audio)) {
		t.Fatalf("resumed put: %d %+v", code, evs)
	}
	if _, ok := regularFile(filepath.Join(d.Jobs, "01JOB", "audio.wav")); !ok {
		t.Fatal("audio.wav missing")
	}
	// Reopening the job with the same audio keeps it; with other audio starts over.
	if have, _, _ := e.openJob("01JOB", remote.AudioWAV, int64(len(audio)), sum); have != int64(len(audio)) {
		t.Fatal("reopen lost the audio")
	}
	if have, _, _ := e.openJob("01JOB", remote.AudioWAV, 5, shaHex([]byte("other"))); have != 0 {
		t.Fatal("other audio kept the old file")
	}
	// A wrong hash is refused and the part removed.
	_, _, _ = e.openJob("01JOB", remote.AudioWAV, int64(len(audio)), strings.Repeat("0", 64))
	if code, evs, _ := run(t, bytes.NewReader(audio), "put", "--job", "01JOB", "--kind", "audio", "--offset", "0"); code != 1 || evs[0].Code != remote.AgentAudioMismatch {
		t.Fatalf("hash mismatch: %d %+v", code, evs)
	}
	// get: no result yet → exit 1; a result → its bytes.
	var out, errb bytes.Buffer
	if Main([]string{"get", "--job", "01JOB", "--kind", "result"}, strings.NewReader(""), &out, &errb) != 1 ||
		!strings.Contains(errb.String(), remote.AgentResultMissing) {
		t.Fatalf("get without a result: %q", errb.String())
	}
	_ = os.WriteFile(filepath.Join(d.Jobs, "01JOB", "result.json"), []byte(`{"sentences":[]}`), 0o600)
	out.Reset()
	if Main([]string{"get", "--job", "01JOB", "--kind", "result"}, strings.NewReader(""), &out, &errb) != 0 || out.String() != `{"sentences":[]}` {
		t.Fatalf("get: %q", out.String())
	}
	// An unknown job.
	if code, evs, _ := run(t, nil, "stat", "--job", "01NONE", "--kind", "audio"); code != 1 || evs[0].Code != remote.AgentUnknownJob {
		t.Fatalf("unknown job: %d %+v", code, evs)
	}
}

func TestReap(t *testing.T) {
	d := testDirs(t)
	now := time.Now()
	e := &env{dirs: d, out: &eventWriter{w: io.Discard}, stderr: io.Discard, now: func() time.Time { return now }}
	_ = ensurePrivateDir(d.Jobs)
	mk := func(id string) string {
		p := filepath.Join(d.Jobs, id)
		_ = os.MkdirAll(p, 0o700)
		return p
	}
	expired := mk("01EXPIRED")
	_ = os.WriteFile(filepath.Join(expired, fileExpires), []byte(strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)), 0o600)
	kept := mk("01KEPT")
	_ = os.WriteFile(filepath.Join(kept, fileExpires), []byte(strconv.FormatInt(now.Add(time.Hour).Unix(), 10)), 0o600)
	stale := mk("01STALE")
	_ = os.WriteFile(filepath.Join(stale, fileAlive), nil, 0o600)
	old := now.Add(-3 * time.Hour)
	_ = os.Chtimes(filepath.Join(stale, fileAlive), old, old)
	alive := mk("01ALIVE")
	_ = os.WriteFile(filepath.Join(alive, fileAlive), nil, 0o600)
	_ = os.WriteFile(filepath.Join(d.Jobs, "stray.txt"), []byte("x"), 0o600)
	if n := e.reap(); n != 3 {
		t.Fatalf("reaped %d", n)
	}
	for _, p := range []string{expired, stale, filepath.Join(d.Jobs, "stray.txt")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s left", p)
		}
	}
	for _, p := range []string{kept, alive} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
	// keepJob marks a job for retain.
	e.keepJob("01ALIVE", time.Minute)
	b, _ := os.ReadFile(filepath.Join(alive, fileExpires))
	if at, _ := strconv.ParseInt(string(b), 10, 64); at != now.Add(time.Minute).Unix() {
		t.Fatalf("expires %s", b)
	}
}

func TestDownloadResumesAndChecks(t *testing.T) {
	d := testDirs(t)
	_ = ensurePrivateDir(filepath.Join(d.Root, dirDownloads))
	body := bytes.Repeat([]byte("0123456789"), 50000)
	cut := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cut && r.Header.Get("Range") == "" {
			// The first request stops halfway (a dropped connection).
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body[:len(body)/2])
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				c.Close()
			}
			cut = false
			return
		}
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	e := &env{dirs: d, out: &eventWriter{w: io.Discard}, stderr: io.Discard, now: time.Now}
	f := remote.KitDownload{Name: "f", URL: srv.URL + "/f", SHA256: shaHex(body), Size: int64(len(body))}
	p, err := e.download(t.Context(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
		t.Fatal("content")
	}
	f.SHA256 = strings.Repeat("0", 64)
	f.Name = "g"
	if _, err := e.download(t.Context(), f, nil); codeOf(err) != remote.AgentHashMismatch {
		t.Fatalf("hash mismatch: %v", err)
	}
}

func TestServeSession(t *testing.T) {
	d := testDirs(t)
	_ = ensurePrivateDir(d.Root)
	_ = ensurePrivateDir(d.Jobs)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	e := &env{dirs: d, stdin: inR, out: &eventWriter{w: outW}, stderr: io.Discard, now: time.Now}
	done := make(chan int, 1)
	go func() { done <- e.serve(); outW.Close() }()
	lines := bufio.NewScanner(outR)
	send := func(v any) remote.AgentEvent {
		t.Helper()
		b, _ := json.Marshal(v)
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		for lines.Scan() {
			ev, err := remote.ParseEvent(lines.Bytes())
			if err != nil {
				t.Fatalf("event %q: %v", lines.Text(), err)
			}
			if ev.Type == remote.EventMachine || ev.Type == remote.EventSample {
				continue
			}
			return ev
		}
		t.Fatal("no answer")
		return remote.AgentEvent{}
	}
	if ev := send(map[string]any{"op": "hello", "seq": 1, "retain_s": 60}); ev.Type != remote.EvHello || ev.Seq != 1 {
		t.Fatalf("hello %+v", ev)
	}
	if ev := send(map[string]any{"op": "ping", "seq": 2}); ev.Type != remote.EvPong || ev.Seq != 2 {
		t.Fatalf("ping %+v", ev)
	}
	if ev := send(map[string]any{"op": "rm", "seq": 3}); ev.Type != remote.EvFail || ev.Code != remote.AgentBadRequest {
		t.Fatalf("bad op %+v", ev)
	}
	sum := shaHex([]byte("x"))
	if ev := send(map[string]any{"op": "job", "seq": 4, "job": "01S", "audio_kind": "wav", "audio_size": 1, "audio_sha256": sum}); ev.Type != remote.EvOK || ev.Have != 0 {
		t.Fatalf("job %+v", ev)
	}
	// A run without a kit is refused.
	if ev := send(map[string]any{"op": "run", "seq": 5, "job": "01S", "engine": "mlx", "kit": strings.Repeat("a", 64),
		"params": map[string]any{"model": "m"}}); ev.Type != remote.EvFail || ev.Code != remote.AgentAudioMissing {
		t.Fatalf("run %+v", ev)
	}
	// The session ends with stdin: the job is kept for retain_s.
	inW.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not end")
	}
	b, err := os.ReadFile(filepath.Join(d.Jobs, "01S", fileExpires))
	if err != nil {
		t.Fatal(err)
	}
	if at, _ := strconv.ParseInt(string(b), 10, 64); at < time.Now().Unix()+50 || at > time.Now().Unix()+70 {
		t.Fatalf("expires %s", b)
	}
}

// The agent never links the application's database, HTTP API or scheduler.
func TestImportGuard(t *testing.T) {
	for _, target := range []string{"linux/amd64", "darwin/arm64", "windows/amd64"} {
		goos, goarch, _ := strings.Cut(target, "/")
		cmd := exec.Command("go", "list", "-deps", "lectureflow/cmd/lf-agent")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: go list: %v", target, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.Contains(dep, "sqlite") || strings.HasPrefix(dep, "lectureflow/") && !allowedDep(dep) {
				t.Errorf("%s: lf-agent depends on %s", target, dep)
			}
		}
	}
}

func allowedDep(p string) bool {
	switch p {
	case "lectureflow/cmd/lf-agent", "lectureflow/internal/agent", "lectureflow/internal/remote", "lectureflow/internal/sysprobe",
		"lectureflow/internal/pyenv": // the Python children's environment (standard library only)
		return true
	}
	return false
}

func TestPlatformNames(t *testing.T) {
	if platformOf("linux", "amd64") != remote.PlatformLinuxX64 || platformOf("darwin", "arm64") != remote.PlatformDarwinArm64 ||
		platformOf("linux", "arm64") != "" || platformOf("darwin", "amd64") != "" {
		t.Fatal("platforms")
	}
	if runtime.GOOS == "windows" && Platform() != remote.PlatformWindowsX64 && runtime.GOARCH == "amd64" {
		t.Fatal("this machine")
	}
}

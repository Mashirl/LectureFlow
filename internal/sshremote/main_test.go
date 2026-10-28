// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/remote"
	"lectureflow/internal/secrets"
)

// Built once for every test: the real lf-agent of this machine and the
// fake Python (testdata/fakepython).
var (
	agentBin  string
	fakePyBin string
	buildDir  string
)

func TestMain(m *testing.M) {
	key := make([]byte, secrets.KeySize)
	_, _ = rand.Read(key)
	if err := secrets.SetKey(key); err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "sshremote-test-")
	if err != nil {
		panic(err)
	}
	buildDir = dir
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	agentBin = filepath.Join(dir, "lf-agent"+exe)
	fakePyBin = filepath.Join(dir, "fakepython"+exe)
	for _, b := range [][2]string{{agentBin, "lectureflow/cmd/lf-agent"}, {fakePyBin, "./testdata/fakepython"}} {
		cmd := exec.Command("go", "build", "-o", b[0], b[1])
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n%s", b[1], err, out)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	// Short timings: the heartbeat and keepalive of a lost connection.
	heartbeatEvery = 500 * time.Millisecond
	keepaliveEvery = time.Second
	code := m.Run()
	secrets.ResetForTest()
	os.RemoveAll(dir)
	os.Exit(code)
}

// kitSource serves a fake python archive and model files over HTTP
// (127.0.0.1; remote.KitSpec allows http only to a loopback host).
type kitSource struct {
	srv      *httptest.Server
	archive  []byte
	model    map[string][]byte
	requests int
}

func newKitSource(t *testing.T) *kitSource {
	t.Helper()
	py, err := os.ReadFile(fakePyBin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	name := "python/bin/python3"
	if runtime.GOOS == "windows" {
		name = "python/python.exe"
	}
	_ = tw.WriteHeader(&tar.Header{Name: "python/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(py))})
	_, _ = tw.Write(py)
	_ = tw.WriteHeader(&tar.Header{Name: "python/lib/README", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("ok"))
	tw.Close()
	gz.Close()
	ks := &kitSource{archive: buf.Bytes(), model: map[string][]byte{
		"config.json": []byte(`{"n_mels": 128}`), "weights.safetensors": bytes.Repeat([]byte{7}, 4096)}}
	ks.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ks.requests++
		switch r.URL.Path {
		case "/python.tar.gz":
			http.ServeContent(w, r, "python.tar.gz", time.Time{}, bytes.NewReader(ks.archive))
		default:
			b, ok := ks.model[filepath.Base(r.URL.Path)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(b))
		}
	}))
	t.Cleanup(ks.srv.Close)
	return ks
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// kit is the test kit (KitFor).
func (ks *kitSource) kit(platform, engine, model, precision string) (remote.KitSpec, error) {
	exe := "python/bin/python3"
	if runtime.GOOS == "windows" {
		exe = "python/python.exe"
	}
	lock := "fake-package==1.0 \\\n    --hash=sha256:" + sum([]byte("x")) + "\n"
	k := remote.KitSpec{Version: "0.0.0-dev", Engine: engine,
		Python:  remote.KitArchive{URL: ks.srv.URL + "/python.tar.gz", SHA256: sum(ks.archive), Size: int64(len(ks.archive)), Exe: exe},
		Lock:    remote.KitLock{Name: "asr-test.txt", SHA256: sum([]byte(lock)), Text: lock},
		Imports: []string{"fake_module"},
		Model:   remote.KitModel{ID: "model:" + engine + ":" + model + ":" + precision},
	}
	for _, n := range []string{"config.json", "weights.safetensors"} {
		b := ks.model[n]
		k.Model.Files = append(k.Model.Files, remote.KitDownload{Name: n, URL: ks.srv.URL + "/m/" + n, SHA256: sum(b), Size: int64(len(b))})
	}
	if err := addWorker(&k, filepath.Join("..", "..", "worker")); err != nil {
		return k, err
	}
	return k, nil
}

// testEnv is one manager with its database and device table.
type testEnv struct {
	m    *Manager
	st   db.Store
	devs *devices.Registry
	dir  string
	ks   *kitSource
}

func newTestEnv(t *testing.T, o func(*Options)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	sqlDB, err := db.OpenDB(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	st := db.New(sqlDB)
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ASR.FakeResult = "fake" // test mode: the registry starts no process
	reg := devices.NewRegistry(st, cfg, dir, devices.RegistryOptions{})
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg.EnableSSH() // as cmd does with LECTUREFLOW_EXPERIMENTAL_SSH=1
	agents := filepath.Join(dir, "agents")
	b, err := os.ReadFile(agentBin)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range remote.Platforms {
		_ = os.MkdirAll(filepath.Join(agents, p), 0o755)
		if err := os.WriteFile(filepath.Join(agents, p, "lf-agent"), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ks := newKitSource(t)
	opts := Options{Store: st.(Store), Devices: reg, AgentDir: agents, WorkerDir: filepath.Join("..", "..", "worker"),
		KitFor: ks.kit, ReconnectGrace: 20 * time.Second}
	if o != nil {
		o(&opts)
	}
	m, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return &testEnv{m: m, st: st, devs: reg, dir: dir, ks: ks}
}

// writeWAV writes sec seconds of 16 kHz mono 16-bit silence.
func writeWAV(t *testing.T, path string, sec int) {
	t.Helper()
	n := uint32(sec * 16000 * 2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, 36+n)
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, n)
	b.Write(make([]byte, n))
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

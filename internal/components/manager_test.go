// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fwFloat16 = "model:faster-whisper:tiny:float16"
const fwInt8 = "model:faster-whisper:tiny:int8_float16"

func TestPlanCompletesDependencies(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	p, err := m.Plan([]string{fwFloat16})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range p.Items {
		ids = append(ids, it.ID)
	}
	if strings.Join(ids, ",") != "python,asr-cuda,"+fwFloat16 {
		t.Fatalf("order %v", ids)
	}
	if !p.Items[0].Dependency || !p.Items[1].Dependency || p.Items[2].Dependency {
		t.Errorf("dependency flags: %+v", p.Items)
	}
	man := m.manifest()
	var dl, inst int64
	for _, id := range ids {
		b := man.Component(id).Build(PlatformWindowsX64)
		dl += b.DownloadSize
		inst += b.InstalledSize
	}
	if p.DownloadBytes != dl || p.InstalledBytes != inst {
		t.Errorf("sizes %d/%d, want %d/%d", p.DownloadBytes, p.InstalledBytes, dl, inst)
	}
	if !p.Enough || p.FreeBytes != 1<<40 || p.RequiredBytes <= dl {
		t.Errorf("space: %+v", p)
	}

	// after installing asr-cuda, the plan for the model has nothing else to do
	mustInstall(t, m, Sources{}, IDASRCUDA)
	p, _ = m.Plan([]string{fwFloat16})
	if !p.Items[0].Installed || !p.Items[1].Installed || p.Items[2].Installed {
		t.Errorf("installed flags: %+v", p.Items)
	}
	if b := man.Component(fwFloat16).Build(PlatformWindowsX64); p.DownloadBytes != b.DownloadSize {
		t.Errorf("download %d, want %d", p.DownloadBytes, b.DownloadSize)
	}

	for id, code := range map[string]string{
		"ghost":                    CodeUnknown,
		"model:openvino:huge:fp16": CodeUnavailable,
		IDASRMLX:                   CodeUnsupportedPlatform,
	} {
		if _, err := m.Plan([]string{id}); CodeOf(err) != code {
			t.Errorf("Plan(%s): %v, want %s", id, err, code)
		}
		if _, err := m.Install([]string{id}, Sources{}); CodeOf(err) != code {
			t.Errorf("Install(%s): %v, want %s", id, err, code)
		}
	}
}

func TestDiskSpaceCheck(t *testing.T) {
	e := newTestEnv(t)
	e.setFree(10 << 20)
	m := e.open()
	p, err := m.Plan([]string{IDASRCUDA})
	if err != nil {
		t.Fatal(err)
	}
	if p.Enough || p.FreeBytes != 10<<20 || p.RequiredBytes <= p.FreeBytes {
		t.Fatalf("plan: %+v", p)
	}
	_, err = m.Install([]string{IDASRCUDA}, Sources{})
	if CodeOf(err) != CodeDiskFull {
		t.Fatalf("Install: %v", err)
	}
	info := ErrorInfoOf(err)
	if info.Params["free_mb"] != int64(10) || info.Params["required_mb"] != mb(p.RequiredBytes) {
		t.Errorf("params: %+v", info.Params)
	}
	if len(m.Tasks()) != 0 || e.srv.count("/") != 0 {
		t.Error("nothing may start without space")
	}
	e.setFree(1 << 40)
	mustInstall(t, m, Sources{}, IDASRCUDA)
}

func TestInstallPythonAndVenv(t *testing.T) {
	e := newTestEnv(t)
	log := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("LF_FAKE_UV_LOG", log)
	m := e.open()
	if _, ok := m.Python(BackendBase); ok {
		t.Fatal("python before install")
	}
	var mu sync.Mutex
	var events []Event
	cancel := m.Subscribe(func(ev Event) { mu.Lock(); events = append(events, ev); mu.Unlock() })
	defer cancel()

	st := mustInstall(t, m, Sources{}, fwFloat16)
	if st.Downloaded != st.Total || st.Total == 0 || st.ETASeconds != 0 {
		t.Errorf("final progress %d/%d eta %d", st.Downloaded, st.Total, st.ETASeconds)
	}
	for _, it := range st.Items {
		if it.Phase != PhaseDone {
			t.Errorf("item %s: %s", it.ID, it.Phase)
		}
	}
	base, ok := m.Python(BackendBase)
	if !ok || !strings.HasPrefix(base, filepath.Join(e.data, "components", "python", "1.0.0", "venv")) {
		t.Fatalf("base python %q %v", base, ok)
	}
	fw, ok := m.Python(BackendFasterWhisper)
	if !ok || !strings.HasPrefix(fw, filepath.Join(e.data, "components", "asr-cuda", "1.0.0", "venv")) {
		t.Fatalf("faster-whisper python %q %v", fw, ok)
	}
	// the venv was made with the runtime from the python component
	cfg, _ := os.ReadFile(filepath.Join(e.data, "components", "asr-cuda", "1.0.0", "venv", "pyvenv.cfg"))
	if !strings.Contains(string(cfg), filepath.Join(e.data, "components", "python", "1.0.0", "runtime", "python")) {
		t.Errorf("pyvenv.cfg: %s", cfg)
	}
	dir, ok := m.ModelDir(BackendFasterWhisper, "tiny", "float16")
	if !ok || dir != filepath.Join(e.data, "components", "model__faster-whisper__tiny__float16", "1.0.0", "model") {
		t.Fatalf("model dir %q", dir)
	}
	// faster-whisper: another compute type finds the same files
	if d2, ok := m.ModelDir(BackendFasterWhisper, "tiny", "int8"); !ok || d2 != dir {
		t.Errorf("fallback model dir %q %v", d2, ok)
	}
	if _, ok := m.ModelDir(BackendOpenVINO, "tiny", "fp16"); ok {
		t.Error("openvino has no fallback")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "model.bin"))
	if string(got) != string(e.content["model.bin"]) {
		t.Error("model.bin differs")
	}
	// uv: venv with the runtime, sync with hashes from the default index
	data, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("uv calls: %s", data)
	}
	var args []string
	json.Unmarshal([]byte(lines[3]), &args)
	joined := strings.Join(args, " ")
	for _, want := range []string{"pip sync", "--require-hashes", "--index-url " + e.url("/default/simple"),
		filepath.Join(e.manDir, "locks", "asr-cuda-windows-x64.txt")} {
		if !strings.Contains(joined, want) {
			t.Errorf("uv sync args %q lack %q", joined, want)
		}
	}
	// receipts and state hold relative paths only
	state, _ := os.ReadFile(filepath.Join(e.data, "components", stateFileName))
	if strings.Contains(string(state), filepath.ToSlash(e.data)) || strings.Contains(string(state), e.data) ||
		strings.Contains(string(state), strings.ReplaceAll(e.data, `\`, `\\`)) {
		t.Errorf("state.json has absolute paths: %s", state)
	}
	s, _ := statusOf(m, fwFloat16)
	if !s.Installed || s.InstalledVersion != "1.0.0" || s.DiskUsage <= 0 || s.UpdateAvailable || s.Broken {
		t.Errorf("status %+v", s)
	}
	core, _ := statusOf(m, IDCore)
	if !core.Installed || !core.Bundled || core.Version != "3.0.0" {
		t.Errorf("core %+v", core)
	}
	if _, ok := statusOf(m, IDASRMLX); ok {
		t.Error("asr-mlx has no windows build and must not be listed")
	}
	// the uv cache and the downloads are gone once nothing is active
	for _, f := range listFiles(t, filepath.Join(e.data, "components")) {
		if strings.HasPrefix(f, ".uvc/") || strings.HasPrefix(f, legacyUVCache+"/") || strings.HasPrefix(f, "downloads/") || isStagingPath(f) {
			t.Errorf("left behind: %s", f)
		}
	}
	waitFor(t, "events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if ev.Type == EventTask && ev.Task.State == TaskDone {
				return true
			}
		}
		return false
	})
}

func TestVerifyFailure(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("LF_FAKE_PY_FAIL", "ctranslate2")
	m := e.open()
	id, _ := m.Install([]string{IDASRCUDA}, Sources{})
	st := wait(t, m, id, TaskFailed, TaskDone)
	if st.State != TaskFailed || st.Error.Code != CodeVerifyFailed || !strings.Contains(st.Error.Detail, "No module named 'ctranslate2'") {
		t.Fatalf("got %s %+v", st.State, st.Error)
	}
	if s, _ := statusOf(m, IDPython); !s.Installed {
		t.Error("python passed its own check and stays")
	}
	if s, _ := statusOf(m, IDASRCUDA); s.Installed {
		t.Error("asr-cuda failed its check")
	}
	if dirs, _ := filepath.Glob(filepath.Join(e.data, "components", "asr-cuda", "*")); len(dirs) != 0 {
		t.Errorf("left behind: %v", dirs)
	}
}

func TestUVFailure(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("LF_FAKE_UV_FAIL", "sync")
	m := e.open()
	id, _ := m.Install([]string{IDPython}, Sources{})
	st := wait(t, m, id, TaskFailed, TaskDone)
	if st.State != TaskFailed || st.Error.Code != CodeUVFailed || !strings.Contains(st.Error.Detail, "something unexpected broke") {
		t.Fatalf("got %s %+v", st.State, st.Error)
	}
	if dirs, _ := filepath.Glob(filepath.Join(e.data, "components", "python", "*")); len(dirs) != 0 {
		t.Errorf("a failed python install left %v", dirs)
	}
}

func TestUVMissing(t *testing.T) {
	e := newTestEnv(t)
	opts := e.options()
	opts.UV = filepath.Join(t.TempDir(), "no-uv.exe")
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id, _ := m.Install([]string{IDPython}, Sources{})
	if st := wait(t, m, id, TaskFailed, TaskDone); st.Error == nil || st.Error.Code != CodeUVMissing {
		t.Fatalf("%+v", st)
	}
}

// Files another installed component has are linked, not downloaded again.
func TestReuseInstalledFiles(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, fwFloat16)
	before := e.srv.count("/default/hf/")
	p, _ := m.Plan([]string{fwInt8})
	if p.DownloadBytes != 0 {
		t.Errorf("plan downloads %d bytes for files already on disk", p.DownloadBytes)
	}
	mustInstall(t, m, Sources{}, fwInt8)
	if n := e.srv.count("/default/hf/"); n != before {
		t.Errorf("%d new model requests", n-before)
	}
	d1, _ := m.ModelDir(BackendFasterWhisper, "tiny", "float16")
	d2, _ := m.ModelDir(BackendFasterWhisper, "tiny", "int8_float16")
	if d1 == d2 {
		t.Fatal("two components share a directory")
	}
	b1, _ := os.ReadFile(filepath.Join(d1, "model.bin"))
	b2, _ := os.ReadFile(filepath.Join(d2, "model.bin"))
	if string(b1) != string(b2) || len(b1) == 0 {
		t.Error("reused file differs")
	}
	// removing one keeps the other intact
	if err := m.Uninstall(fwFloat16); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(d2, "model.bin")); string(b) != string(b1) {
		t.Error("uninstalling one copy damaged the other")
	}
	if d, ok := m.ModelDir(BackendFasterWhisper, "tiny", "float16"); !ok || d != d2 {
		t.Errorf("fallback after uninstall: %q %v", d, ok)
	}
}

func TestBinaryUnpack(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, "tool")
	b, err := os.ReadFile(filepath.Join(e.data, "components", "tool", "1.0.0", "bin", "tool.exe"))
	if err != nil || string(b) != "tool binary" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestUninstall(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, fwFloat16)
	if err := m.Uninstall(IDCore); CodeOf(err) != CodeBundled {
		t.Errorf("core: %v", err)
	}
	if err := m.Uninstall(ovModel); CodeOf(err) != CodeNotInstalled {
		t.Errorf("not installed: %v", err)
	}
	err := m.Uninstall(IDPython)
	if CodeOf(err) != CodeInUse || !strings.Contains(fmt.Sprint(ErrorInfoOf(err).Params["dependents"]), IDASRCUDA) {
		t.Errorf("python in use: %v", err)
	}
	if err := m.Uninstall(IDASRCUDA); CodeOf(err) != CodeInUse {
		t.Errorf("asr-cuda is needed by the model: %v", err)
	}
	for _, id := range []string{fwFloat16, IDASRCUDA, IDPython} {
		if err := m.Uninstall(id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	for _, s := range m.List() {
		if s.Installed && !s.Bundled {
			t.Errorf("%s still installed", s.ID)
		}
	}
	if files := listFiles(t, filepath.Join(e.data, "components")); len(files) != 1 || files[0] != stateFileName {
		t.Errorf("left after uninstalling everything: %v", files)
	}
}

func TestUninstallBusy(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, ovModel)
	hit, release := e.srv.gate("/default/hf/tiny/1/model.bin")
	defer release()
	id, err := m.Install([]string{fwFloat16}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	<-hit
	if err := m.Uninstall(IDPython); CodeOf(err) != CodeInUse {
		t.Errorf("python during the model install: %v", err)
	}
	if err := m.Uninstall(IDASRCUDA); CodeOf(err) != CodeBusy {
		t.Errorf("asr-cuda while its model installs: %v", err)
	}
	if err := m.Uninstall(fwFloat16); CodeOf(err) != CodeNotInstalled {
		t.Errorf("the model being installed: %v", err)
	}
	if _, err := m.Update(ovModel); CodeOf(err) != CodeUpToDate {
		t.Errorf("update: %v", err)
	}
	if s, _ := statusOf(m, fwFloat16); s.Task != id {
		t.Errorf("status task %q, want %q", s.Task, id)
	}
	m.Cancel(id)
	wait(t, m, id, TaskCancelled)
}

// A run that died midway leaves directories state.json does not list: the
// next start removes them, and nothing looks installed.
func TestInterruptedInstallIsNotInstalled(t *testing.T) {
	e := newTestEnv(t)
	root := filepath.Join(e.data, "components")
	// a staging directory and a complete-looking version directory without a
	// state entry (crash before state.json was written)
	staging := filepath.Join(root, "asr-cuda", ".s1")
	os.MkdirAll(filepath.Join(staging, "venv", "Scripts"), 0o755)
	os.WriteFile(filepath.Join(staging, "venv", "pyvenv.cfg"), []byte("home = x\n"), 0o644)
	// the staging name before V12.2 and the old uv cache name
	legacy := filepath.Join(root, "asr-cuda", ".staging-1.0.0-t1")
	os.MkdirAll(filepath.Join(legacy, "venv"), 0o755)
	oldCache := filepath.Join(root, legacyUVCache, "archive-v0")
	os.MkdirAll(oldCache, 0o755)
	ver := filepath.Join(root, "model__openvino__tiny__int8", "1.0.0")
	os.MkdirAll(filepath.Join(ver, "model"), 0o755)
	os.WriteFile(filepath.Join(ver, "model", "openvino_encoder_model.xml"), []byte("half"), 0o644)
	os.WriteFile(filepath.Join(root, stateFileName), []byte(`{"schema":1,"active":{}}`), 0o644)
	m := e.open()
	for _, s := range m.List() {
		if s.Installed && !s.Bundled {
			t.Errorf("%s looks installed", s.ID)
		}
	}
	for _, d := range []string{staging, legacy, filepath.Join(root, legacyUVCache)} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s kept", d)
		}
	}
	if _, err := os.Stat(ver); !os.IsNotExist(err) {
		t.Error("unreferenced version directory kept")
	}
	if _, ok := m.ModelDir(BackendOpenVINO, "tiny", "int8"); ok {
		t.Error("ModelDir reports an unreferenced directory")
	}
}

// Partial downloads survive a restart and resume.
func TestResumeAcrossRestart(t *testing.T) {
	e := newTestEnv(t)
	hit, release := e.srv.gate(ovFile)
	m := e.open()
	id, _ := m.Install([]string{ovModel}, Sources{})
	<-hit
	waitFor(t, "progress", func() bool { st, _ := m.Task(id); return st.Downloaded > 0 })
	m.Close()
	release()
	part := filepath.Join(e.data, "components", "downloads", shaHex(ovContent(e))+".part")
	fi, err := os.Stat(part)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("partial file: %v", err)
	}
	m2 := e.open()
	p, _ := m2.Plan([]string{ovModel})
	if p.DownloadBytes != int64(len(ovContent(e)))-fi.Size() {
		t.Errorf("plan download %d, want the rest %d", p.DownloadBytes, int64(len(ovContent(e)))-fi.Size())
	}
	mustInstall(t, m2, Sources{}, ovModel)
	checkModelFile(t, m2, ovContent(e))
	log := e.srv.log()
	if last := log[len(log)-1]; last.Range != fmt.Sprintf("bytes=%d-", fi.Size()) {
		t.Errorf("resume request %+v", last)
	}
}

func TestDamagedStateIsRebuilt(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, fwFloat16, ovModel)
	m.Close()
	p := filepath.Join(e.data, "components", stateFileName)
	os.WriteFile(p, []byte(`{"schema":1,"active":{"python":`), 0o644) // truncated write
	m2 := e.open()
	if !m2.StateRecovered() {
		t.Error("recovery not reported")
	}
	for _, id := range []string{IDPython, IDASRCUDA, fwFloat16, ovModel} {
		if s, _ := statusOf(m2, id); !s.Installed || s.Broken {
			t.Errorf("%s after recovery: %+v", id, s)
		}
	}
	if _, ok := m2.Python(BackendFasterWhisper); !ok {
		t.Error("python lost")
	}
	aside, _ := filepath.Glob(p + ".corrupt-*")
	if len(aside) != 1 {
		t.Errorf("damaged state kept aside: %v", aside)
	}
	data, _ := os.ReadFile(p)
	var st state
	if err := json.Unmarshal(data, &st); err != nil || len(st.Active) != 4 {
		t.Errorf("rewritten state: %s", data)
	}
	// wrong content (valid JSON) is damage too
	m2.Close()
	os.WriteFile(p, []byte(`{"schema":1,"active":{"python":{"id":"other","version":"1.0.0","dir":"../x"}}}`), 0o644)
	m3 := e.open()
	if !m3.StateRecovered() {
		t.Error("inconsistent state accepted")
	}
	if s, _ := statusOf(m3, ovModel); !s.Installed {
		t.Error("rebuild lost a component")
	}
}

// A missing state.json with receipts on disk is rebuilt (crash before the
// first state write); a fresh directory stays empty.
func TestMissingStateRebuilt(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	if _, err := os.Stat(filepath.Join(e.data, "components", stateFileName)); !os.IsNotExist(err) {
		t.Error("a fresh directory needs no state.json")
	}
	mustInstall(t, m, Sources{}, ovModel)
	m.Close()
	os.Remove(filepath.Join(e.data, "components", stateFileName))
	m2 := e.open()
	if s, _ := statusOf(m2, ovModel); !s.Installed {
		t.Error("receipt not used")
	}
	if m2.StateRecovered() {
		t.Error("a missing state.json is not damage")
	}
}

// Many goroutines use the manager while tasks run (go test -race is not
// available on this machine; -count=3 repeats it).
func TestConcurrentUse(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unsub := m.Subscribe(func(Event) { time.Sleep(time.Millisecond) })
			defer unsub()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.List()
				m.Tasks()
				m.Plan([]string{fwFloat16})
				m.Python(BackendFasterWhisper)
				m.ModelDir(BackendFasterWhisper, "tiny", "float16")
				m.Presets()
				m.SuggestSources("cn")
				if i%4 == 0 {
					m.Subscribe(func(Event) {})()
				}
			}
		}(i)
	}
	var ids []TaskID
	for _, set := range [][]string{{fwFloat16}, {ovModel}, {"tool"}, {fwInt8}, {IDASRCUDA}} {
		id, err := m.Install(set, Sources{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// pause and resume one of them while the others run
	m.Pause(ids[1])
	m.Resume(ids[1])
	for _, id := range ids {
		if st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled); st.State != TaskDone {
			t.Errorf("%s: %s %+v", id, st.State, st.Error)
		}
	}
	close(stop)
	wg.Wait()
	for _, id := range []string{IDPython, IDASRCUDA, fwFloat16, fwInt8, ovModel, "tool"} {
		if s, _ := statusOf(m, id); !s.Installed {
			t.Errorf("%s not installed", id)
		}
	}
}

func TestPresetsAndSuggestions(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	if p := m.Presets(); p.Default != "standard" || len(p.Tiers) != 3 || p.Free.Default != "freebie" {
		t.Errorf("presets %+v", p)
	}
	if s := m.SuggestSources("cn"); s.PyPI != "mirror" || s.Models != "hf-mirror" {
		t.Errorf("cn: %+v", s)
	}
	if s := m.SuggestSources(""); s != (Sources{}) {
		t.Errorf("default: %+v", s)
	}
}

func TestTaskNotFound(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	for _, f := range []func(TaskID) error{m.Pause, m.Resume, m.Cancel} {
		if err := f("nope"); CodeOf(err) != CodeTaskNotFound {
			t.Errorf("%v", err)
		}
	}
}

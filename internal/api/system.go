// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Diagnostics (SPEC §17.1). The checks run once at startup (without the CUDA
// self-test) and again on POST /api/system/recheck (with it); GET
// /api/system returns the cached results plus live GPU/API/usage/disk data.

const (
	statusOK    = "ok"
	statusWarn  = "warn"
	statusError = "error"

	cmdTimeout      = 30 * time.Second
	selftestTimeout = 5 * time.Minute // may download the tiny model on first use
	llmCheckTimeout = 30 * time.Second
	diskWarnBytes   = 5 << 30
	diskErrorBytes  = 2 << 30
)

type diagnostics struct {
	s *Server

	mu       sync.Mutex
	checks   []SystemCheck
	cuda     SystemCheck // last CUDA self-test result (kept across rechecks without it)
	ready    chan struct{}
	stop     context.CancelFunc // cancels the startup checks (StopStartupChecks)
	running  sync.Mutex         // serialises check runs
	sizes    map[string]int64
	sizesAt  time.Time
	sizesMu  sync.Mutex
	dataSize int64
	streamMB int64 // MB of the lectures' stream.mp4 (P8.1)

	// pyUse counts the interpreters that diagnostics checks and request
	// handlers run right now, by reason (RunningPythons; v3 Gate round 1
	// item 5, round 2). It lives here because the diagnostics exist for the
	// server's whole life.
	pyMu  sync.Mutex
	pyUse map[RunningPython]int
}

func newDiagnostics(s *Server) *diagnostics {
	return &diagnostics{
		s:     s,
		ready: make(chan struct{}),
		cuda:  checkWith(SystemCheck{Name: "cuda", Status: statusWarn}, "check.not_run", nil),
	}
}

// start runs the startup checks in the background. Until a check finishes
// it is shown as check.checking, so GET /api/system never waits for slow
// checks (python imports, the API request).
func (d *diagnostics) start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.stop = cancel
	go func() {
		d.run(ctx, false)
		close(d.ready)
		d.s.hub.Changed("system", "")
	}()
}

// waitStartup blocks until the startup checks are done (tests).
func (d *diagnostics) waitStartup() { <-d.ready }

type checkSpec struct {
	name string
	fn   func(context.Context) SystemCheck
}

// specs lists the checks in the display order of SPEC §17.1 (disk is added
// live by snapshot).
func (d *diagnostics) specs(withCUDA bool) []checkSpec {
	s := d.s
	const pipHint = "check.hint_pip"
	specs := []checkSpec{
		{"data_dir", s.checkDataDir},
		{"ffmpeg", s.checkFFmpeg},
		{"python", s.checkPython},
		{"faster-whisper", func(ctx context.Context) SystemCheck {
			return s.checkImport(ctx, s.fwPython(), "faster-whisper", "faster_whisper", pipHint)
		}},
		{"pypdf", func(ctx context.Context) SystemCheck {
			return s.checkImport(ctx, s.cfg.PDFPython(), "pypdf", "pypdf", pipHint)
		}},
		{"gpu", s.checkGPU},
		{"cuda", nil}, // only on recheck; otherwise the last result is kept
	}
	if withCUDA {
		specs[len(specs)-1].fn = s.checkCUDA
	}
	providers := s.diagProviders()
	for _, p := range providers {
		specs = append(specs, checkSpec{"api_key:" + p.name, func(context.Context) SystemCheck { return s.checkAPIKey(p) }})
	}
	for _, p := range providers {
		specs = append(specs, checkSpec{"api:" + p.name, func(ctx context.Context) SystemCheck { return s.checkAPI(ctx, p) }})
	}
	return specs
}

// run executes every check in parallel (the CUDA self-test only when
// withCUDA); each result replaces its row as soon as it is known.
func (d *diagnostics) run(ctx context.Context, withCUDA bool) {
	d.running.Lock()
	defer d.running.Unlock()
	specs := d.specs(withCUDA)
	d.mu.Lock()
	if len(d.checks) != len(specs) {
		d.checks = make([]SystemCheck, len(specs))
		for i, sp := range specs {
			d.checks[i] = checkWith(SystemCheck{Name: sp.name, Status: statusWarn}, checkPending, nil)
			if sp.name == "cuda" {
				d.checks[i] = d.cuda
			}
		}
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for i, sp := range specs {
		if sp.fn == nil {
			continue
		}
		wg.Add(1)
		go func(i int, sp checkSpec) {
			defer wg.Done()
			var res SystemCheck
			func() {
				defer func() {
					if p := recover(); p != nil {
						slog.Error("diagnostic check panicked", "check", sp.name, "panic", p)
						res = checkWith(SystemCheck{Name: sp.name, Status: statusError}, "check.crashed", map[string]any{"error": fmt.Sprint(p)})
					}
				}()
				res = sp.fn(ctx)
			}()
			d.mu.Lock()
			d.checks[i] = res
			if sp.name == "cuda" {
				d.cuda = res
			}
			d.mu.Unlock()
		}(i, sp)
	}
	wg.Wait()
}

// snapshot returns the cached checks followed by a live disk check. With
// the device registry (SPEC-v2 I10) the OpenVINO checks follow the CUDA
// one; they are live too, read from the registry's last detection (which
// "重新检查" refreshes) rather than from a process of their own.
func (d *diagnostics) snapshot() []SystemCheck {
	d.mu.Lock()
	cached := append([]SystemCheck(nil), d.checks...)
	d.mu.Unlock()
	ov := d.s.openVINOChecks()
	out := make([]SystemCheck, 0, len(cached)+len(ov)+1)
	at := len(cached)
	for i, c := range cached {
		if c.Name == "cuda" {
			at = i + 1
		}
	}
	out = append(out, cached[:at]...)
	out = append(out, ov...)
	out = append(out, cached[at:]...)
	return append(out, d.s.checkDisk())
}

// courseSizes returns MB per course directory, of the whole data dir and of
// the playback files (stream.mp4 of every lecture, P8.1 "磁盘统计中单独显示
// 播放文件的占用"), cached for a few seconds (the top bar polls GET
// /api/system).
func (d *diagnostics) courseSizes(ctx context.Context) (map[string]int64, int64, int64) {
	d.sizesMu.Lock()
	defer d.sizesMu.Unlock()
	if d.sizes != nil && time.Since(d.sizesAt) < 15*time.Second {
		return d.sizes, d.dataSize, d.streamMB
	}
	sizes := map[string]int64{}
	var stream int64
	if cs, err := d.s.st.ListCourses(ctx); err == nil {
		for _, c := range cs {
			total, st := courseUsage(model.CourseDir(d.s.dataDir, c.ID))
			sizes[c.ID] = total >> 20
			stream += st
		}
	}
	d.sizes, d.dataSize, d.streamMB, d.sizesAt = sizes, dirSize(d.s.dataDir)>>20, stream>>20, time.Now()
	return d.sizes, d.dataSize, d.streamMB
}

// courseUsage sums the regular files under a course directory: all of them,
// and the playback files (model.StreamFile directly inside a lecture
// directory, {course}/lectures/{lecture}/stream.mp4).
func courseUsage(dir string) (total, stream int64) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		if d.Name() == model.StreamFile && filepath.Base(filepath.Dir(filepath.Dir(path))) == "lectures" {
			stream += info.Size()
		}
		return nil
	})
	return total, stream
}

func (d *diagnostics) invalidateSizes() {
	d.sizesMu.Lock()
	d.sizes = nil
	d.sizesMu.Unlock()
}

// ---- individual checks ----

// diagProvider is one provider row of the diagnostics: id for lookups,
// name for the check name ("api_key:{name}").
type diagProvider struct{ id, name string }

// diagProviders lists the providers: the settings' providers in their
// order (v1.1 A5), or the legacy config keys sorted.
func (s *Server) diagProviders() []diagProvider {
	if s.set != nil {
		ps := s.set.Providers()
		out := make([]diagProvider, 0, len(ps))
		for _, p := range ps {
			out = append(out, diagProvider{id: p.ID, name: p.Name})
		}
		return out
	}
	if s.cfg == nil {
		return nil
	}
	out := make([]diagProvider, 0, len(s.cfg.LLM.Providers))
	for name := range s.cfg.LLM.Providers {
		out = append(out, diagProvider{id: name, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (s *Server) cmd(ctx context.Context, timeout time.Duration, name string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.runCmd(ctx, name, args...)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// checkPending is the detail code of a check that is still running.
const checkPending = "check.checking"

// setDetail sets the check's detail as a code of the catalog (area "check",
// P14.3) and the transitional Detail string as its Chinese text.
func (c *SystemCheck) setDetail(code string, params map[string]any) {
	c.DetailInfo = &model.ErrorInfo{Code: code, Params: params}
	c.Detail = zhText(*c.DetailInfo)
}

// setHint is setDetail for the fix hint.
func (c *SystemCheck) setHint(code string, params map[string]any) {
	c.HintInfo = &model.ErrorInfo{Code: code, Params: params}
	c.Hint = zhText(*c.HintInfo)
}

// checkWith returns c with its detail set (setDetail).
func checkWith(c SystemCheck, code string, params map[string]any) SystemCheck {
	c.setDetail(code, params)
	return c
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (s *Server) checkDataDir(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: "data_dir", Status: statusOK}
	err := os.MkdirAll(s.dataDir, 0o755)
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(s.dataDir, "write-check-*.tmp"); err == nil {
			name := f.Name()
			f.Close()
			err = os.Remove(name)
		}
	}
	if err != nil {
		c.Status = statusError
		c.setDetail("check.dir_not_writable", map[string]any{"path": s.dataDir, "error": err.Error()})
		c.setHint("check.hint_data_dir", nil)
		return c
	}
	abs, _ := filepath.Abs(s.dataDir)
	c.setDetail("check.dir_writable", map[string]any{"path": abs})
	return c
}

func (s *Server) checkFFmpeg(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: "ffmpeg", Status: statusOK}
	out, _, err := s.cmd(ctx, cmdTimeout, s.cfg.FFmpeg, "-version")
	if err != nil {
		c.Status = statusError
		c.setDetail("check.run_failed", map[string]any{"program": s.cfg.FFmpeg, "error": err.Error()})
		c.setHint("check.hint_ffmpeg", nil)
		return c
	}
	c.Detail = firstLine(out)
	return c
}

var pyVersionRe = regexp.MustCompile(`Python (\d+)\.(\d+)(?:\.(\d+))?`)

// fwPython is the interpreter of the faster-whisper worker: the asr-cuda
// component's environment when it is installed, else the configured
// python (v3 Gate round 1: the Python, faster-whisper and CUDA checks test
// what the transcription actually runs).
func (s *Server) fwPython() string {
	p, _, _ := s.cfg.WorkerPython(model.BackendFasterWhisper)
	return p
}

// pyCmd is cmd for a check that runs python: while it runs, python is
// reported by RunningPythons with reason self_check.
func (s *Server) pyCmd(ctx context.Context, timeout time.Duration, python string, args ...string) (string, string, error) {
	defer s.trackPython(python, PythonUseSelfCheck)()
	return s.cmd(ctx, timeout, python, args...)
}

// Reasons of RunningPython: the reason keys of components.in_use_by_task.
const (
	PythonUseSelfCheck = "self_check" // a diagnostics check
	PythonUseMaterial  = "material"   // the syllabus PDF extraction of a request
)

// RunningPython is an interpreter the server runs right now and why.
type RunningPython struct {
	Python string
	Reason string // PythonUseSelfCheck | PythonUseMaterial
}

// trackPython registers python as running for reason until the returned
// function is called (components.Options.InUse must not remove its
// environment meanwhile).
func (s *Server) trackPython(python, reason string) (done func()) {
	d := s.diag
	if d == nil || python == "" {
		return func() {}
	}
	k := RunningPython{Python: python, Reason: reason}
	d.pyMu.Lock()
	if d.pyUse == nil {
		d.pyUse = map[RunningPython]int{}
	}
	d.pyUse[k]++
	d.pyMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.pyMu.Lock()
			if d.pyUse[k]--; d.pyUse[k] <= 0 {
				delete(d.pyUse, k)
			}
			d.pyMu.Unlock()
		})
	}
}

// RunningPythons returns the interpreters the server runs right now, with
// the reason, sorted: the diagnostics checks (the Python version, the
// import checks, the CUDA self-test; self_check) and the syllabus PDF
// extraction of a course request (material). The component manager must
// not remove them meanwhile (components.Options.InUse).
func (s *Server) RunningPythons() []RunningPython {
	d := s.diag
	if d == nil {
		return nil
	}
	d.pyMu.Lock()
	out := make([]RunningPython, 0, len(d.pyUse))
	for k := range d.pyUse {
		out = append(out, k)
	}
	d.pyMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Python != out[j].Python {
			return out[i].Python < out[j].Python
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// SelfCheckPythons returns the interpreters the diagnostics checks run
// right now, sorted (RunningPythons with reason self_check).
func (s *Server) SelfCheckPythons() []string {
	var out []string
	for _, r := range s.RunningPythons() {
		if r.Reason == PythonUseSelfCheck && (len(out) == 0 || out[len(out)-1] != r.Python) {
			out = append(out, r.Python)
		}
	}
	return out
}

func (s *Server) checkPython(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: "python", Status: statusOK}
	python := s.fwPython()
	out, errOut, err := s.pyCmd(ctx, cmdTimeout, python, "--version")
	if err != nil {
		c.Status = statusError
		c.setDetail("check.run_failed", map[string]any{"program": python, "error": err.Error()})
		c.setHint("check.hint_python", nil)
		return c
	}
	v := firstLine(out + "\n" + errOut)
	m := pyVersionRe.FindStringSubmatch(v)
	if m == nil {
		c.Status = statusError
		c.setDetail("check.python_version_unknown", map[string]any{"version": v})
		c.setHint("check.hint_python_version", nil)
		return c
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	c.Detail = v // the program's own output
	if major < 3 || (major == 3 && minor < 10) {
		c.Status = statusError
		c.setHint("check.hint_python_version", nil)
	}
	return c
}

// checkImport runs python -c "import module". Warnings on stderr (e.g. a
// DeprecationWarning printed on import) are ignored; the exit code
// decides. hint is the catalog code of the fix hint.
func (s *Server) checkImport(ctx context.Context, python, name, module, hint string) SystemCheck {
	c := SystemCheck{Name: name, Status: statusOK}
	_, errOut, err := s.pyCmd(ctx, cmdTimeout, python, "-c", "import "+module)
	if err != nil {
		detail := lastLine(errOut)
		if detail == "" {
			detail = err.Error()
		}
		c.Status = statusError
		c.setDetail("check.import_failed", map[string]any{"module": module, "error": detail})
		c.setHint(hint, nil)
		return c
	}
	c.setDetail("check.import_ok", map[string]any{"module": module})
	return c
}

// asrSettings is the ASR configuration in effect: the settings (v1.1 A5.3)
// or the legacy [asr] section.
func (s *Server) asrSettings() (device, computeType string) {
	if s.set != nil {
		a := s.set.ASR()
		return a.Device, a.ComputeType
	}
	return s.cfg.ASR.Device, s.cfg.ASR.ComputeType
}

// cudaRule is how the GPU / CUDA checks rate a failure (lead ruling 13):
// error while the CUDA device is used, warn otherwise. With the device
// registry: cuda:0 exists and is enabled; cpuOnly (the "CPU 转写很慢" hint)
// when every enabled device runs on the CPU. Without it: the legacy
// settings' device.
func (s *Server) cudaRule() (failStatus string, cpuOnly bool) {
	if s.dev == nil {
		device, _ := s.asrSettings()
		if device == "cpu" {
			return statusWarn, true
		}
		return statusError, false
	}
	cudaOn, enabled, cpu := false, 0, 0
	for _, d := range s.dev.List() {
		if !d.Enabled {
			continue
		}
		enabled++
		switch model.DeviceKind(d.ID) {
		case model.DeviceKindCUDA:
			cudaOn = cudaOn || d.ID == model.DeviceCUDA0
		case model.DeviceKindCPU:
			cpu++
		}
	}
	if cudaOn {
		return statusError, false
	}
	return statusWarn, enabled > 0 && cpu == enabled
}

// cudaSelftestArgs are the device and compute type of the CUDA self-test:
// the cuda:0 row with the registry (else fw:cpu, else cuda / auto), the
// legacy settings without it.
func (s *Server) cudaSelftestArgs() (device, computeType string) {
	if s.dev == nil {
		return s.asrSettings()
	}
	if d, ok := s.dev.Get(model.DeviceCUDA0); ok {
		return "cuda", d.Precision
	}
	if d, ok := s.dev.Get(model.DeviceFWCPU); ok && d.Enabled {
		return "cpu", d.Precision
	}
	return "cuda", model.PrecisionAuto
}

func (s *Server) checkGPU(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: "gpu", Status: statusOK}
	failStatus, cpu := s.cudaRule()
	if s.sc == nil {
		c.Status = statusWarn
		c.setDetail("check.scheduler_off", nil)
		return c
	}
	g := s.sc.GPUStatus(ctx)
	switch {
	case s.cfg.ASR.FakeResult != "":
		c.Status = statusWarn
		c.setDetail("check.gpu_fake", nil)
	case !g.OK:
		c.setDetail("check.gpu_unreadable", map[string]any{"error": g.Error})
		c.setHint("check.hint_nvidia", nil)
		c.Status = failStatus
	default:
		c.setDetail("check.gpu_vram", map[string]any{"total": g.TotalMB, "free": g.FreeMB})
	}
	if cpu {
		c.Status = statusWarn
		c.setHint("check.hint_cpu", nil)
	}
	return c
}

var selftestRe = regexp.MustCompile(`SELFTEST ok .*`)

func (s *Server) checkCUDA(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: "cuda", Status: statusOK}
	device, computeType := s.cudaSelftestArgs()
	failStatus, _ := s.cudaRule()
	if s.dev == nil && device != "cuda" {
		failStatus = statusWarn
	}
	if s.cfg.ASR.FakeResult != "" {
		c.Status = statusWarn
		c.setDetail("check.cuda_fake", nil)
		return c
	}
	total := 0
	if s.sc != nil {
		total = s.sc.GPUStatus(ctx).TotalMB
	}
	script := filepath.Join(s.cfg.WorkerDir, "asr.py")
	out, errOut, err := s.pyCmd(ctx, selftestTimeout, s.fwPython(), script, "--selftest",
		"--device", device, "--compute-type", config.ResolveComputeType(computeType, device, total))
	if err != nil {
		detail := lastLine(errOut)
		if detail == "" {
			detail = err.Error()
		}
		c.Status = failStatus
		c.setDetail("check.selftest_failed", map[string]any{"error": detail})
		c.setHint("check.hint_selftest", nil)
		return c
	}
	if m := selftestRe.FindString(out); m != "" {
		c.Detail = strings.TrimSpace(m) // the worker's own line
	} else {
		c.setDetail("check.selftest_ok", nil)
	}
	return c
}

func (s *Server) checkAPIKey(dp diagProvider) SystemCheck {
	c := SystemCheck{Name: "api_key:" + dp.name, Status: statusOK}
	if s.set != nil {
		p, ok := s.set.Provider(dp.id)
		switch {
		case !ok:
			c.Status = statusWarn
			c.setDetail("check.provider_deleted", nil)
		case p.APIKey != "":
			c.setDetail("check.key_set", map[string]any{"key": model.KeyHint(p.APIKey)})
		case settings.IsLocal(p.BaseURL):
			c.setDetail("check.key_local_service", nil)
		default:
			c.Status = statusError
			c.setDetail("check.key_missing", nil)
			c.setHint("check.hint_key_settings", map[string]any{"provider": p.Name})
		}
		return c
	}
	p := s.cfg.LLM.Providers[dp.id]
	key := p.Key()
	switch {
	case key != "" && p.APIKey == "":
		c.setDetail("check.key_from_env", map[string]any{"key": config.Mask(key), "env": p.APIKeyEnv})
	case key != "":
		c.setDetail("check.key_from_config", map[string]any{"key": config.Mask(key)})
	case p.Local:
		c.setDetail("check.key_local_model", nil)
	default:
		c.Status = statusError
		if p.APIKeyEnv != "" {
			c.setDetail("check.key_env_unset", map[string]any{"env": p.APIKeyEnv})
			c.setHint("check.hint_key_env", map[string]any{"env": p.APIKeyEnv})
		} else {
			c.setDetail("check.key_not_configured", nil)
			c.setHint("check.hint_key_config", map[string]any{"provider": dp.id})
		}
	}
	return c
}

const apiCheckHint = "check.hint_api"

func (s *Server) checkAPI(ctx context.Context, dp diagProvider) SystemCheck {
	c := SystemCheck{Name: "api:" + dp.name, Status: statusOK}
	ctx, cancel := context.WithTimeout(ctx, llmCheckTimeout)
	defer cancel()
	if s.set != nil {
		p, ok := s.set.Provider(dp.id)
		if !ok {
			c.Status = statusWarn
			c.setDetail("check.api_skipped_deleted", nil)
			return c
		}
		if p.APIKey == "" && !settings.IsLocal(p.BaseURL) {
			c.Status = statusWarn
			c.setDetail("check.api_skipped_no_key", nil)
			return c
		}
		res := s.testConn(ctx, p, s.set.TestModel(ctx, p.ID))
		if !res.OK {
			c.Status = statusWarn
			c.setDetail("check.api_failed", map[string]any{"error": res.Error})
			c.setHint(apiCheckHint, nil)
			return c
		}
		c.setDetail("check.api_ok", map[string]any{"ms": res.Latency.Milliseconds()})
		return c
	}
	p := s.cfg.LLM.Providers[dp.id]
	if p.Key() == "" && !p.Local {
		c.Status = statusWarn
		c.setDetail("check.api_skipped_no_key", nil)
		return c
	}
	if s.llm == nil {
		c.Status = statusWarn
		c.setDetail("check.api_skipped", nil)
		return c
	}
	t0 := time.Now()
	if err := s.llm(ctx, dp.id); err != nil {
		c.Status = statusWarn
		c.setDetail("check.api_failed", map[string]any{"error": err.Error()})
		c.setHint(apiCheckHint, nil)
		return c
	}
	c.setDetail("check.api_ok", map[string]any{"ms": time.Since(t0).Milliseconds()})
	return c
}

func (s *Server) checkDisk() SystemCheck {
	c := SystemCheck{Name: "disk", Status: statusOK}
	free, err := s.freeSpace(s.dataDir)
	if err != nil {
		c.Status = statusWarn
		c.setDetail("check.disk_unreadable", map[string]any{"error": err.Error()})
		return c
	}
	c.setDetail("check.disk_free", map[string]any{"free_mb": free >> 20})
	c.Detail = "剩余 " + humanBytes(free) // transitional text; the front end formats free_mb itself
	switch {
	case free < diskErrorBytes:
		c.Status = statusError
		c.setHint("check.hint_disk_error", nil)
	case free < diskWarnBytes:
		c.Status = statusWarn
		c.setHint("check.hint_disk_warn", nil)
	}
	return c
}

// ---- handlers ----

func (s *Server) systemView(ctx context.Context) System {
	v := System{
		Checks: WithRoleModelChecks(s.diag.snapshot(), s.roleModelChecks(ctx)),
		API:    map[string]APIInfo{},
		Config: s.configView(),
	}
	if s.sc != nil {
		g := s.sc.GPUStatus(ctx)
		v.GPU = GPUInfo{TotalMB: g.TotalMB, FreeMB: g.FreeMB, Paused: g.Paused, SlotsUsed: g.SlotsUsed, SlotsMax: g.SlotsMax, EstimatedJobMB: g.EstimatedJobMB}
		// Keyed by the display name (unique, A5.1); the pool keys by id.
		for id, a := range s.sc.APIStatus() {
			name := a.Name
			if name == "" {
				name = id
			}
			v.API[name] = APIInfo{Inflight: a.Inflight, Max: a.Max, RPMLimit: a.RPM}
		}
		if u, err := s.sc.UsageToday(ctx); err == nil {
			v.UsageToday = UsageStats{PromptTokens: u.PromptTokens, CacheHitTokens: u.CacheHitTokens, CompletionTokens: u.CompletionTokens, EstimatedCost: u.EstimatedCost}
		} else {
			slog.Warn("usage today", "err", err)
		}
		v.BudgetHit = s.sc.BudgetHit(ctx)
	}
	if free, err := s.freeSpace(s.dataDir); err == nil {
		v.Disk.FreeMB = free >> 20
	}
	v.Disk.Courses, v.Disk.DataMB, v.Disk.StreamMB = s.diag.courseSizes(ctx)
	if s.dev != nil && s.dsc != nil { // SPEC-v2 I10: the devices table, power, OpenVINO
		v.Devices, v.Power, v.OpenVINO = s.deviceViews(ctx), s.powerView(), s.openVINOInfo()
	}
	return v
}

// configView is the read-only configuration (SPEC §9.10). v1.1: the
// startup keys of config.toml plus the current ASR and general settings
// (A5.1 moved them into the database; the legacy [asr]/[llm]/[storage]
// sections were imported once and are not shown, they are not in effect).
// Providers and keys live on the settings page (key_hint only).
func (s *Server) configView() any {
	if s.cfg == nil {
		return map[string]any{}
	}
	if s.set == nil {
		return s.legacyConfigView()
	}
	a, g := s.set.ASR(), s.set.General()
	asr := map[string]any{
		"model": a.Model, "compute_type": a.ComputeType, "device": a.Device, "language": a.Language,
		"max_parallel": a.MaxParallel, "vram_reserve_mb": a.VRAMReserveMB, "vram_per_job_mb": a.VRAMPerJobMB,
		// SPEC-v2 I8 (phase 3b)
		"device_mode": a.DeviceMode, "multi_device": a.MultiDevice, "battery_aware": a.BatteryAware, "mem_reserve_mb": a.MemReserveMB,
	}
	if a.DeviceMode == model.ASRDeviceModeManual { // power management: the manual list
		asr["manual_devices"] = a.ManualDevices
	}
	if s.cfg.ASR.FakeResult != "" {
		asr["fake_result"] = s.cfg.ASR.FakeResult
	}
	return map[string]any{
		"listen": s.cfg.Listen, "data_dir": s.cfg.DataDir, "python": s.cfg.Python, "python_openvino": s.cfg.PythonOpenVINO, "ffmpeg": s.cfg.FFmpeg,
		"nvidia_smi": s.cfg.NvidiaSMI, "worker_dir": s.cfg.WorkerDir, "log_level": s.cfg.LogLevel,
		"asr": asr,
		"general": map[string]any{
			"keep_original_media": g.KeepOriginalMedia, "notifications": g.Notifications, "daily_budget": g.DailyBudget,
		},
	}
}

// legacyConfigView is configView without the settings service: config.toml
// with the key names of the file and every API key masked as sk-…last4.
func (s *Server) legacyConfigView() any {
	if s.cfg == nil {
		return map[string]any{}
	}
	red := s.cfg.Redacted()
	var buf bytes.Buffer
	var out map[string]any
	if err := toml.NewEncoder(&buf).Encode(red); err == nil {
		if _, err := toml.Decode(buf.String(), &out); err != nil {
			out = nil
		}
	}
	if out == nil {
		return map[string]any{"error": "无法显示配置"}
	}
	if llm, ok := out["llm"].(map[string]any); ok {
		if ps, ok := llm["providers"].(map[string]any); ok {
			for name, raw := range ps {
				pm, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if k := s.cfg.LLM.Providers[name].Key(); k != "" {
					pm["api_key"] = config.Mask(k)
				} else {
					delete(pm, "api_key")
				}
			}
		}
	}
	return out
}

func (s *Server) getSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.systemView(r.Context()))
}

func (s *Server) recheck(w http.ResponseWriter, r *http.Request) {
	// Not bound to the request: a check that is cut short would cache a
	// misleading failure. The device detection (SPEC-v2 I6 "诊断页“重新检查”")
	// runs alongside; the OpenVINO checks read its result.
	ctx := context.WithoutCancel(r.Context())
	var wg sync.WaitGroup
	if s.dev != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.detectDevices(ctx); err != nil {
				slog.Warn("device detection on recheck", "err", err)
			}
		}()
	}
	s.diag.run(ctx, true)
	wg.Wait()
	s.diag.invalidateSizes()
	s.hub.Changed("system", "")
	writeJSON(w, http.StatusOK, s.systemView(r.Context()))
}

func (s *Server) setGPU(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paused        *bool `json:"paused"`
		CancelRunning bool  `json:"cancel_running"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Paused == nil {
		writeError(w, http.StatusBadRequest, "system.paused_missing", nil)
		return
	}
	if err := s.sc.SetGPUPaused(*req.Paused, *req.Paused && req.CancelRunning); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.systemView(r.Context()))
}

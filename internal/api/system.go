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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/hwdetect"
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
	// goos is the system whose checks run (devices.GOOS when the server
	// was made; mac-opti M3): macOS checks MLX instead of faster-whisper
	// and CUDA. desktop: the server was made in desktop mode (the hints
	// point to the Components page, desktopDiag).
	goos    string
	desktop bool

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

	// ctx is the context of the startup checks and of the reruns after a
	// component change (cancelled by stop); rerun is the pending rerun
	// (mac-opti M3: componentsChanged).
	ctx     context.Context
	rerunMu sync.Mutex
	rerun   *time.Timer
}

func newDiagnostics(s *Server) *diagnostics {
	return &diagnostics{
		s:       s,
		goos:    devices.GOOS,
		desktop: s.desktop != nil,
		ready:   make(chan struct{}),
		cuda:    checkWith(SystemCheck{Name: "cuda", Status: statusWarn}, "check.not_run", nil),
	}
}

// start runs the startup checks in the background. Until a check finishes
// it is shown as check.checking, so GET /api/system never waits for slow
// checks (python imports, the API request).
//
// mac-opti M3: the checks run again (without the CUDA self-test) when the
// installed components change, so the page follows an install from the
// setup guide or the Components page instead of showing the state at
// startup until "重新检查" (on the Mac the Python, pypdf and MLX rows kept
// saying "not installed" after the guide had installed them).
func (d *diagnostics) start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	d.stop = func() {
		cancel()
		d.rerunMu.Lock()
		if d.rerun != nil {
			d.rerun.Stop()
			d.rerun = nil
		}
		d.rerunMu.Unlock()
	}
	go func() {
		d.run(ctx, false)
		close(d.ready)
		d.s.hub.Changed("system", "")
	}()
	if d.s.comps != nil {
		d.s.comps.Subscribe(func(ev components.Event) {
			if ev.Type == components.EventComponents {
				d.componentsChanged()
			}
		})
	}
}

// rerunDelay is how long the checks wait after the last change of the
// installed components (an install of several components sends several
// events).
var rerunDelay = 2 * time.Second

// componentsChanged schedules a run of the checks without the CUDA
// self-test, rerunDelay after the last call, once the startup checks are
// done. Not after stop (shutdown) or once the event hub is closed.
func (d *diagnostics) componentsChanged() {
	d.rerunMu.Lock()
	defer d.rerunMu.Unlock()
	if d.ctx == nil || d.ctx.Err() != nil {
		return
	}
	if d.rerun != nil {
		d.rerun.Reset(rerunDelay)
		return
	}
	d.rerun = time.AfterFunc(rerunDelay, func() {
		d.rerunMu.Lock()
		d.rerun = nil
		d.rerunMu.Unlock()
		select {
		case <-d.ready:
		case <-d.ctx.Done():
			return
		}
		if d.ctx.Err() != nil || hubClosed(d.s.hub) {
			return
		}
		d.run(d.ctx, false)
		d.invalidateSizes()
		d.s.hub.Changed("system", "")
	})
}

// hubClosed reports whether the event hub was closed (shutdown, the end of
// a test).
func hubClosed(h *Hub) bool {
	h.init()
	select {
	case <-h.closed:
		return true
	default:
		return false
	}
}

// waitStartup blocks until the startup checks are done (tests).
func (d *diagnostics) waitStartup() { <-d.ready }

type checkSpec struct {
	name string
	fn   func(context.Context) SystemCheck
}

// specs lists the checks in the display order of SPEC §17.1 (disk is added
// live by snapshot). mac-opti M3: the checks follow the platform and the
// installed components. Windows (and other systems) keep the list of
// before: Python, faster-whisper, pypdf, GPU, CUDA (the GPU and CUDA rows
// are shown only with an NVIDIA GPU, snapshot); macOS checks Python and
// pypdf in the python component's environment and MLX instead of
// faster-whisper and CUDA (mlx_model is live, snapshot).
func (d *diagnostics) specs(withCUDA bool) []checkSpec {
	s := d.s
	specs := []checkSpec{
		{"data_dir", s.checkDataDir},
		{"ffmpeg", s.checkFFmpeg},
	}
	if d.mac() {
		specs = append(specs,
			checkSpec{"python", func(ctx context.Context) SystemCheck { return s.checkPython(ctx, d.versionPython()) }},
			checkSpec{"pypdf", s.checkPyPDF},
			checkSpec{checkMLXPython, func(ctx context.Context) SystemCheck { return s.checkMLXPython(ctx) }},
			checkSpec{checkMLX, s.checkMLXImport},
		)
	} else {
		specs = append(specs,
			checkSpec{"python", func(ctx context.Context) SystemCheck { return s.checkPython(ctx, d.versionPython()) }},
			checkSpec{"faster-whisper", s.checkFasterWhisper},
			checkSpec{"pypdf", s.checkPyPDF},
			checkSpec{"gpu", s.checkGPU},
			checkSpec{"cuda", nil}, // only on recheck; otherwise the last result is kept
		)
		// The CUDA self-test only where its row is shown (an NVIDIA GPU).
		if withCUDA && s.nvidiaChecks() {
			specs[len(specs)-1].fn = s.checkCUDA
		}
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
//
// mac-opti M3: the GPU and CUDA rows are left out without an NVIDIA GPU
// (nvidiaChecks), the OpenVINO rows without an Intel device or OpenVINO
// environment (openVINOVisible); on macOS the live MLX model check follows
// the MLX import check, then the OpenVINO rows (if any).
func (d *diagnostics) snapshot() []SystemCheck {
	d.mu.Lock()
	cached := append([]SystemCheck(nil), d.checks...)
	d.mu.Unlock()
	s := d.s
	var live []SystemCheck
	anchor := "cuda"
	if d.mac() {
		anchor = checkMLX
		live = append(live, s.mlxModelCheck())
	}
	live = append(live, s.openVINOChecks()...)
	at := len(cached)
	for i, c := range cached {
		if c.Name == anchor {
			at = i + 1
		}
	}
	hideGPU := !d.mac() && !s.nvidiaChecks()
	out := make([]SystemCheck, 0, len(cached)+len(live)+1)
	for i, c := range cached {
		if i == at {
			out = append(out, live...)
		}
		if hideGPU && (c.Name == "gpu" || c.Name == "cuda") {
			continue
		}
		out = append(out, c)
	}
	if at == len(cached) {
		out = append(out, live...)
	}
	return append(out, s.checkDisk())
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
		c.setHint(s.devHint("check.hint_data_dir", codeHintDataDirDesktop), nil)
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
		c.setHint(s.devHint("check.hint_ffmpeg", codeHintFFmpegDesktop), nil)
		return c
	}
	c.Detail = firstLine(out)
	return c
}

var pyVersionRe = regexp.MustCompile(`Python (\d+)\.(\d+)(?:\.(\d+))?`)

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

// checkPython is the Python check: python's version (python is
// versionPython; ok false = not installed).
func (s *Server) checkPython(ctx context.Context, python diagPython) SystemCheck {
	c := SystemCheck{Name: "python", Status: statusOK}
	if !python.ok {
		c.Status = statusError
		c.setDetail(codePythonNotInstalled, nil)
		c.setHint(codeHintInstallPython, nil)
		return c
	}
	return s.versionCheck(ctx, c, python.path, statusError, s.devHint("check.hint_python", codeHintInstallPython))
}

// versionCheck runs python --version for the check c: the program's own
// output as the detail; failStatus and runHint when it cannot run, an
// error when the version is unknown or older than 3.10.
func (s *Server) versionCheck(ctx context.Context, c SystemCheck, python, failStatus, runHint string) SystemCheck {
	out, errOut, err := s.pyCmd(ctx, cmdTimeout, python, "--version")
	if err != nil {
		c.Status = failStatus
		c.setDetail("check.run_failed", map[string]any{"program": python, "error": err.Error()})
		c.setHint(runHint, nil)
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

// checkFasterWhisper imports faster_whisper in the faster-whisper
// worker's interpreter (the asr-cuda component's environment).
func (s *Server) checkFasterWhisper(ctx context.Context) SystemCheck {
	python := s.diagInterp(model.BackendFasterWhisper)
	fail := s.backendFail(model.BackendFasterWhisper)
	if !python.ok {
		c := SystemCheck{Name: "faster-whisper", Status: fail}
		c.setDetail(codeFWNotInstalled, nil)
		c.setHint(codeHintInstallFW, nil)
		return c
	}
	c := s.checkImport(ctx, python.path, "faster-whisper", "faster_whisper", s.devHint("check.hint_pip", codeHintInstallFW))
	if c.Status == statusError {
		c.Status = fail
	}
	return c
}

// checkPyPDF imports pypdf in the PDF extraction's interpreter (the
// python component's base environment).
func (s *Server) checkPyPDF(ctx context.Context) SystemCheck {
	python := s.diagInterp(config.ComponentBase)
	if !python.ok {
		c := SystemCheck{Name: "pypdf", Status: statusError}
		c.setDetail(codePythonNotInstalled, nil)
		c.setHint(codeHintInstallPython, nil)
		return c
	}
	return s.checkImport(ctx, python.path, "pypdf", "pypdf", s.devHint("check.hint_pip", codeHintInstallPython))
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
		c.setHint(s.devHint("check.hint_nvidia", codeHintNVIDIADesktop), nil)
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
	python := s.diagInterp(model.BackendFasterWhisper)
	if !python.ok {
		c.Status = failStatus
		c.setDetail(codeFWNotInstalled, nil)
		c.setHint(codeHintInstallFW, nil)
		return c
	}
	total := 0
	if s.sc != nil {
		total = s.sc.GPUStatus(ctx).TotalMB
	}
	script := filepath.Join(s.cfg.WorkerDir, "asr.py")
	out, errOut, err := s.pyCmd(ctx, selftestTimeout, python.path, script, "--selftest",
		"--device", device, "--compute-type", config.ResolveComputeType(computeType, device, total))
	if err != nil {
		detail := lastLine(errOut)
		if detail == "" {
			detail = err.Error()
		}
		c.Status = failStatus
		c.setDetail("check.selftest_failed", map[string]any{"error": detail})
		c.setHint(s.devHint("check.hint_selftest", codeHintSelftestDesktop), nil)
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

// ---- platform and components (mac-opti M3) ----

// Names of the macOS checks (MLX, SPEC-v3 V5).
const (
	checkMLXPython = "mlx_python" // the asr-mlx environment's interpreter runs
	checkMLX       = "mlx"        // mlx and mlx_whisper import, the Metal GPU is there
	checkMLXModel  = "mlx_model"  // the files of the MLX model (live)
)

// Codes of the checks by platform and component (mac-opti M3). They are
// part of the API's catalog (errorCatalog, testdata/api/error_codes.json),
// kept here next to the checks that send them; init adds them. The
// *_not_installed details and the hint_install_* hints are the desktop
// mode's (the interpreters come from the component manager there); web
// mode keeps the developer hints (.venv, README, requirements.txt).
const (
	codePythonNotInstalled   = "check.python_not_installed"
	codeFWNotInstalled       = "check.fw_not_installed"
	codeMLXNotInstalled      = "check.mlx_not_installed"
	codeOpenVINONotInstalled = "check.openvino_not_installed"
	codeMLXModelOK           = "check.mlx_model_ok"
	codeMLXModelMissing      = "check.mlx_model_missing"
	codeHintInstallPython    = "check.hint_install_python"
	codeHintInstallFW        = "check.hint_install_fw"
	codeHintInstallMLX       = "check.hint_install_mlx"
	codeHintInstallOpenVINO  = "check.hint_install_openvino"
	codeHintInstallMLXModel  = "check.hint_install_mlx_model"
	codeHintMLXVenv          = "check.hint_mlx_venv"
	codeHintMLXModelDev      = "check.hint_mlx_model_dev"
	codeHintDataDirDesktop   = "check.hint_data_dir_desktop"
	codeHintFFmpegDesktop    = "check.hint_ffmpeg_desktop"
	codeHintNVIDIADesktop    = "check.hint_nvidia_desktop"
	codeHintSelftestDesktop  = "check.hint_selftest_desktop"
)

var platformCheckCodes = map[string]errDef{
	codePythonNotInstalled:   {"没有安装“Python 运行环境”组件", nil},
	codeFWNotInstalled:       {"没有安装“NVIDIA 显卡转写”组件（faster-whisper）", nil},
	codeMLXNotInstalled:      {"没有安装“Apple 芯片转写（MLX）”组件", nil},
	codeOpenVINONotInstalled: {"没有安装“Intel 转写（OpenVINO）”组件。Intel 核显与 NPU 不可用，不影响其他设备转写", nil},
	codeMLXModelOK:           {"模型文件齐全：Whisper {model}（{precision}）", []string{"model", "precision"}},
	codeMLXModelMissing:      {"没有找到 Whisper {model}（{precision}）的 MLX 模型文件", []string{"model", "precision"}},
	codeHintInstallPython:    {"在设置的“组件”中安装或修复“Python 运行环境”", nil},
	codeHintInstallFW:        {"需要用 NVIDIA 显卡或 CPU 转写时，在设置的“组件”中安装或修复“NVIDIA 显卡转写”", nil},
	codeHintInstallMLX:       {"在设置的“组件”中安装或修复“Apple 芯片转写（MLX）”", nil},
	codeHintInstallOpenVINO:  {"需要用 Intel 核显或 NPU 转写时，在设置的“组件”中安装或修复“Intel 转写（OpenVINO）”，然后点击“重新检查”", nil},
	codeHintInstallMLXModel:  {"在设置的“组件”中安装“Whisper {model} 模型（{precision}）”", []string{"model", "precision"}},
	codeHintMLXVenv:          {"按 worker/requirements-mlx.txt 在项目目录建立 .venv-mlx（或在 config.toml 中设置 python_mlx），然后点击“重新检查”", nil},
	codeHintMLXModelDev:      {"把转换好的 MLX 模型（config.json 与 weights.safetensors）放到 {path}，或安装对应的模型组件", []string{"path"}},
	codeHintDataDirDesktop:   {"检查数据目录所在的磁盘是否已连接、是否可写，或在设置中把数据目录移到别处", nil},
	codeHintFFmpegDesktop:    {"LectureFlow 自带的 ffmpeg 无法运行，请重新安装 LectureFlow", nil},
	codeHintNVIDIADesktop:    {"确认已安装 NVIDIA 显卡驱动（装好后 nvidia-smi 可以运行），然后点击“重新检查”", nil},
	codeHintSelftestDesktop:  {"在设置的“组件”中修复“NVIDIA 显卡转写”，并确认 NVIDIA 显卡驱动是最新的", nil},
}

func init() {
	for code, def := range platformCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
}

// mac reports whether the macOS checks run.
func (d *diagnostics) mac() bool { return d.goos == "darwin" }

// desktopDiag reports whether the checks are the desktop mode's (the
// mode when the server was made: tests switch s.desktop later).
func (s *Server) desktopDiag() bool {
	if s.diag != nil {
		return s.diag.desktop
	}
	return s.desktop != nil
}

// devHint is the fix hint of a check: dev in web mode (the developer's
// .venv, README, config.toml), desktop in desktop mode (the Components
// page, ordinary words).
func (s *Server) devHint(dev, desktop string) string {
	if s.desktopDiag() {
		return desktop
	}
	return dev
}

// diagPython is the interpreter a check runs; ok false = the component is
// not installed (nothing to run).
type diagPython struct {
	path string
	ok   bool
}

// diagInterp is the interpreter of a backend's checks: its component's
// environment when installed (config WorkerPython); else the configured
// path — in web mode always (development: .venv, .venv-ov, .venv-mlx),
// in desktop mode only when that file exists (a python of the user's own
// in config.toml). The default there points into the application's
// resources (Resources/.venv, which never exists): the component is then
// simply not installed. An empty path (3.1.0: config leaves the
// interpreters empty when it has a resources directory) is never run.
func (s *Server) diagInterp(backend string) diagPython {
	p, _, component := s.cfg.WorkerPython(backend)
	if p != "" && (component || !s.desktopDiag() || pythonExists(p)) {
		return diagPython{path: p, ok: true}
	}
	return diagPython{path: p}
}

// versionPython is the interpreter of the Python check: on macOS the
// python component's (pypdf; MLX has its own check), elsewhere the
// faster-whisper worker's as before, in desktop mode falling back to the
// python component's when asr-cuda is not installed (the faster-whisper
// check reports that).
func (d *diagnostics) versionPython() diagPython {
	s := d.s
	if d.mac() {
		return s.diagInterp(config.ComponentBase)
	}
	if p := s.diagInterp(model.BackendFasterWhisper); p.ok || !s.desktopDiag() {
		return p
	}
	return s.diagInterp(config.ComponentBase)
}

// pythonExists: a path names an existing file; a bare name is looked up in
// PATH.
func pythonExists(p string) bool {
	if p == "" {
		return false
	}
	if filepath.Base(p) == p {
		_, err := exec.LookPath(p)
		return err == nil
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// backendFail is the status of a failed check of a backend's environment
// (faster-whisper, MLX): error while that backend is what transcribes (an
// enabled device of it, or no other enabled device that is present: then
// nothing transcribes at all), warn while devices of other backends do
// the work (e.g. Colab on a Mac without MLX, the Intel devices on a PC
// without asr-cuda). Without the registry: error, as before.
func (s *Server) backendFail(backend string) string {
	if s.dev == nil {
		return statusError
	}
	other := false
	for _, d := range s.dev.List() {
		if !d.Enabled {
			continue
		}
		b := d.Backend
		if b == "" {
			b = model.DeviceBackend(d.ID)
		}
		if b == backend {
			return statusError
		}
		other = other || s.dev.Present(d.ID)
	}
	if other {
		return statusWarn
	}
	return statusError
}

// nvidiaChecks reports whether the GPU and CUDA rows are shown (mac-opti
// M3: only with an NVIDIA GPU): never on macOS; without the registry
// always (as before); else when the last detection named an NVIDIA GPU, a
// CUDA device has a row (seen before; also one nvidia-smi does not answer
// for now: the GPU check then tells why), or the hardware detection of
// this run found one (also through the system's device list when
// nvidia-smi fails: a missing driver).
func (s *Server) nvidiaChecks() bool {
	if s.diag != nil && s.diag.mac() {
		return false
	}
	if s.dev == nil {
		return true
	}
	if s.dev.LastDetection().CUDAName != "" {
		return true
	}
	for _, d := range s.dev.List() {
		if model.DeviceKind(d.ID) == model.DeviceKindCUDA {
			return true
		}
	}
	hw, ok := s.cachedHW()
	return ok && len(hw.NVIDIA) > 0
}

// openVINOVisible reports whether the OpenVINO rows are shown (mac-opti
// M3): with an Intel GPU or NPU (found by the last probe, a row of one, or
// the hardware detection of this run) or an OpenVINO environment (the
// asr-openvino component, or python_openvino exists).
func (s *Server) openVINOVisible(det devices.Detection, python string) bool {
	if pythonExists(python) {
		return true
	}
	if det.OV != nil {
		for _, d := range det.OV.Devices {
			if d.Kind == model.DeviceKindIGPU || d.Kind == model.DeviceKindNPU {
				return true
			}
		}
	}
	for _, d := range s.dev.List() {
		if d.ID == model.DeviceOVGPU || d.ID == model.DeviceOVNPU {
			return true
		}
	}
	hw, ok := s.cachedHW()
	return ok && (hw.IntelGPU || hw.IntelNPU)
}

// cachedHW is the hardware detection GET /api/hardware made in this run
// (the setup guide), if any; it never detects. Not while a detection runs.
func (s *Server) cachedHW() (hwdetect.HW, bool) {
	cs := s.compState()
	if !cs.hwMu.TryLock() {
		return hwdetect.HW{}, false
	}
	defer cs.hwMu.Unlock()
	if cs.hw == nil {
		return hwdetect.HW{}, false
	}
	return *cs.hw, true
}

// checkMLXPython runs the MLX worker's interpreter (the asr-mlx
// component's environment; python_mlx in development).
func (s *Server) checkMLXPython(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: checkMLXPython, Status: statusOK}
	python := s.diagInterp(model.BackendMLX)
	fail := s.backendFail(model.BackendMLX)
	if !python.ok {
		c.Status = fail
		c.setDetail(codeMLXNotInstalled, nil)
		c.setHint(codeHintInstallMLX, nil)
		return c
	}
	c = s.versionCheck(ctx, c, python.path, fail, s.devHint(codeHintMLXVenv, codeHintInstallMLX))
	if c.Status == statusError {
		c.Status = fail
	}
	return c
}

// mlxImport is what checkMLXImport runs: the modules worker/asr_mlx.py
// loads and its Metal check (a Mac without a usable GPU fails the run
// with device_unavailable).
const mlxImport = "import mlx.core as mx, mlx_whisper; assert mx.metal.is_available(), 'the Metal GPU is not available'"

// checkMLXImport imports mlx and mlx_whisper in the MLX interpreter; not
// checked while MLX is not installed.
func (s *Server) checkMLXImport(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: checkMLX, Status: statusOK}
	python := s.diagInterp(model.BackendMLX)
	if !python.ok {
		c.Status = statusWarn
		c.setDetail("check.api_skipped", nil) // "未检查"
		return c
	}
	_, errOut, err := s.pyCmd(ctx, cmdTimeout, python.path, "-c", mlxImport)
	if err != nil {
		detail := lastLine(errOut)
		if detail == "" {
			detail = err.Error()
		}
		c.Status = s.backendFail(model.BackendMLX)
		c.setDetail("check.import_failed", map[string]any{"module": "mlx", "error": detail})
		c.setHint(s.devHint(codeHintMLXVenv, codeHintInstallMLX), nil)
		return c
	}
	c.setDetail("check.import_ok", map[string]any{"module": "mlx, mlx_whisper"})
	return c
}

// mlxModelCheck is the live check of the MLX model's files: the model and
// precision of mlx:gpu, else the recommended ones (what the setup guide
// installs), at devices.MLXModelPath (the model component, else
// {data_dir}/models/mlx/{model}).
func (s *Server) mlxModelCheck() SystemCheck {
	c := SystemCheck{Name: checkMLXModel, Status: statusOK}
	cand, ok := devices.Recommended(model.DeviceMLXGPU)
	if !ok {
		cand = devices.Fallback(model.BackendMLX)
	}
	mdl, precision := cand.Model, cand.Precision
	if s.dev != nil {
		if d, ok := s.dev.Get(model.DeviceMLXGPU); ok && d.Model != "" {
			mdl, precision = d.Model, d.Precision
		}
	}
	params := map[string]any{"model": mdl, "precision": precision}
	dir := devices.MLXModelPath(s.cfg, s.absDataDir(), mdl, precision)
	if devices.MLXModelFilesReady(dir) {
		c.setDetail(codeMLXModelOK, params)
		return c
	}
	c.Status = s.backendFail(model.BackendMLX)
	c.setDetail(codeMLXModelMissing, params)
	if s.desktopDiag() {
		c.setHint(codeHintInstallMLXModel, params)
	} else {
		c.setHint(codeHintMLXModelDev, map[string]any{"path": dir})
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
		if v.OpenVINO != nil && !s.openVINOVisible(s.dev.LastDetection(), v.OpenVINO.Python) {
			v.OpenVINO = nil // mac-opti M3: no OpenVINO block where the checks are hidden too
		}
	}
	return v
}

// configView is the read-only configuration (SPEC §9.10). v1.1: the
// startup keys of config.toml plus the current ASR and general settings
// (A5.1 moved them into the database; the legacy [asr]/[llm]/[storage]
// sections were imported once and are not shown, they are not in effect).
// Providers and keys live on the settings page (key_hint only).
//
// 3.1.0 (Mac acceptance): only what is in effect on this computer.
//   - python: the interpreter each backend of this platform runs
//     (configPython), with where it comes from; one that is not installed
//     has no path. It replaces the python / python_openvino keys, which in
//     the desktop application named Resources/.venv that never exists.
//   - nvidia_smi and the VRAM settings (vram_reserve_mb, vram_per_job_mb:
//     CUDA devices only) only with an NVIDIA GPU (nvidiaChecks: the GPU
//     check is shown; never on a Mac).
//   - With the device table (SPEC-v2 I6–I8) the global model,
//     compute_type, device and max_parallel are not in effect (each device
//     has its own; they were only migrated once into cuda:0 / fw:cpu, and
//     are the scheduler's fallback when no device exists): asr_devices
//     lists the devices' backend, model, precision and parallel jobs
//     instead. Without the registry they are what transcribes and stay.
func (s *Server) configView() any {
	if s.cfg == nil {
		return map[string]any{}
	}
	if s.set == nil {
		return s.legacyConfigView()
	}
	a, g := s.set.ASR(), s.set.General()
	nvidia := s.nvidiaChecks()
	asr := map[string]any{
		"language": a.Language,
		// SPEC-v2 I8 (phase 3b)
		"device_mode": a.DeviceMode, "multi_device": a.MultiDevice, "battery_aware": a.BatteryAware, "mem_reserve_mb": a.MemReserveMB,
	}
	if s.dev == nil { // no device table: the legacy fields transcribe
		asr["model"], asr["compute_type"], asr["device"], asr["max_parallel"] = a.Model, a.ComputeType, a.Device, a.MaxParallel
	}
	if nvidia {
		asr["vram_reserve_mb"], asr["vram_per_job_mb"] = a.VRAMReserveMB, a.VRAMPerJobMB
	}
	if a.DeviceMode == model.ASRDeviceModeManual { // power management: the manual list
		asr["manual_devices"] = a.ManualDevices
	}
	if s.cfg.ASR.FakeResult != "" {
		asr["fake_result"] = s.cfg.ASR.FakeResult
	}
	out := map[string]any{
		"listen": s.cfg.Listen, "data_dir": s.cfg.DataDir, "python": s.configPython(), "ffmpeg": s.cfg.FFmpeg,
		"worker_dir": s.cfg.WorkerDir, "log_level": s.cfg.LogLevel,
		"asr": asr,
		"general": map[string]any{
			"keep_original_media": g.KeepOriginalMedia, "notifications": g.Notifications, "daily_budget": g.DailyBudget,
		},
	}
	if nvidia {
		out["nvidia_smi"] = s.cfg.NvidiaSMI
	}
	if s.dev != nil {
		out["asr_devices"] = s.configDevices()
	}
	return out
}

// ConfigPython is one row of the configuration's python list: the
// interpreter config.WorkerPython gives a backend now.
type ConfigPython struct {
	Backend string `json:"backend"` // faster-whisper | openvino | mlx | base (the python component: PDF extraction)
	// Path is "" when nothing is installed (source none).
	Path string `json:"path"`
	// Source: component | config (config.toml) | default (the development
	// .venv) | none (not installed), config.PythonSource.
	Source string `json:"source"`
	// Exists: the file is there (a component's always is).
	Exists bool `json:"exists"`
}

// configBackends are the backends whose interpreter the configuration
// lists on a platform: macOS MLX and the python component; elsewhere
// faster-whisper, OpenVINO and the python component — OpenVINO only with
// the device registry (without it only the legacy faster-whisper path
// transcribes; SPEC-v2 I10: no OpenVINO fields then).
func configBackends(goos string, registry bool) []string {
	switch {
	case goos == "darwin":
		return []string{model.BackendMLX, config.ComponentBase}
	case !registry:
		return []string{model.BackendFasterWhisper, config.ComponentBase}
	}
	return []string{model.BackendFasterWhisper, model.BackendOpenVINO, config.ComponentBase}
}

// configPython lists the interpreters of this platform's backends.
func (s *Server) configPython() []ConfigPython {
	goos := devices.GOOS
	if s.diag != nil {
		goos = s.diag.goos
	}
	out := []ConfigPython{}
	for _, b := range configBackends(goos, s.dev != nil) {
		p, src := s.cfg.PythonSource(b)
		row := ConfigPython{Backend: b, Path: p, Source: src}
		switch src {
		case config.PythonFromComponent:
			row.Exists = true
		case config.PythonNotInstalled:
		default:
			row.Exists = pythonExists(p)
		}
		out = append(out, row)
	}
	return out
}

// ConfigDevice is one row of the configuration's asr_devices: what a
// device of the table runs with.
type ConfigDevice struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Backend     string `json:"backend"`
	Model       string `json:"model"`
	Precision   string `json:"precision"`
	MaxParallel int    `json:"max_parallel"`
	Enabled     bool   `json:"enabled"`
}

// configDevices lists the devices of the table (Registry.List: none that
// is hidden on this platform), highest priority first like the devices
// card.
func (s *Server) configDevices() []ConfigDevice {
	rows := s.dev.List()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Priority > rows[j].Priority })
	out := make([]ConfigDevice, 0, len(rows))
	for _, d := range rows {
		b := d.Backend
		if b == "" {
			b = model.DeviceBackend(d.ID)
		}
		out = append(out, ConfigDevice{ID: d.ID, Label: d.Label, Backend: b, Model: d.Model, Precision: d.Precision,
			MaxParallel: d.MaxParallel, Enabled: d.Enabled})
	}
	return out
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

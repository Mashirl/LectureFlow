// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"lectureflow/internal/api"
	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// componentUse answers components.Options.InUse (v3 Gate round 1 item 5):
// an installed component is in use while local work runs its interpreter
// or its model directory. NOTES "决定（v3 gate-fix-core）". The manager calls
// InUse without its lock, so the paths are resolved the way the stages
// resolve them (cfg.WorkerPython, cfg.ComponentModelDir → the manager's
// Python and ModelDir, the faster-whisper precision fallback included) and
// mapped back to component ids by their directory under the components
// root ({root}/{components.DirName(id)}/{version}/…).
type componentUse struct {
	cfg  *config.Config
	root atomic.Pointer[string] // {data_dir}/components (the manager's Root once it exists)
	// work lists the scheduler's asr runs, device self-tests and material
	// extraction (sched.Scheduler.WorkerUses); set once the scheduler exists.
	work atomic.Pointer[func() []sched.WorkerUse]
	// login reports a running Colab sign-in (colab.Backend.LoginActive):
	// it runs gcloud, and on macOS gcloud runs on the python component
	// (CLOUDSDK_PYTHON; NOTES “决定（3.1.0 gcloud 组件…）” 2).
	login atomic.Pointer[func() bool]

	mu sync.Mutex
	// pythons list interpreters that run outside the scheduler, with the
	// reason: the diagnostics checks and the syllabus PDF extraction
	// (api.Server.RunningPythons), the device detection's probes
	// (devices.Registry.RunningPythons).
	pythons   []func() []runningPython
	seq       int
	selfTests map[int]components.SelfTestRequest // component update self-tests running (V8)
}

// runningPython is an interpreter that runs now and the reason key.
type runningPython struct{ path, reason string }

// Reasons of components.in_use_by_task (the manager's package doc).
const (
	useReasonASR        = "asr"
	useReasonDeviceTest = "device_test"
	useReasonSelfCheck  = "self_check"
	useReasonMaterial   = "material"
	useReasonColabLogin = "colab_login"
)

func newComponentUse(cfg *config.Config) *componentUse {
	u := &componentUse{cfg: cfg, selfTests: map[int]components.SelfTestRequest{}}
	u.setRoot(filepath.Join(cfg.DataDir, "components"))
	return u
}

func (u *componentUse) setRoot(root string) { u.root.Store(&root) }

func (u *componentUse) setWork(f func() []sched.WorkerUse) { u.work.Store(&f) }

// addPythons adds a source of running interpreters.
func (u *componentUse) addPythons(f func() []runningPython) {
	u.mu.Lock()
	u.pythons = append(u.pythons, f)
	u.mu.Unlock()
}

// serverPythons is the source of api.Server.RunningPythons (reasons
// self_check and material).
func serverPythons(f func() []api.RunningPython) func() []runningPython {
	return func() []runningPython {
		rs := f()
		out := make([]runningPython, 0, len(rs))
		for _, r := range rs {
			reason := useReasonSelfCheck
			if r.Reason == api.PythonUseMaterial {
				reason = useReasonMaterial
			}
			out = append(out, runningPython{r.Python, reason})
		}
		return out
	}
}

// probePythons is the source of devices.Registry.RunningPythons (the
// detection's probes, reason self_check).
func probePythons(f func() []string) func() []runningPython {
	return func() []runningPython {
		ps := f()
		out := make([]runningPython, 0, len(ps))
		for _, p := range ps {
			out = append(out, runningPython{p, useReasonSelfCheck})
		}
		return out
	}
}

// wrapSelfTest records an update self-test's interpreter and model while
// it runs (they are the component being updated, or another one it needs:
// the backend's active venv for a model, an installed model for a venv).
func (u *componentUse) wrapSelfTest(f func(context.Context, components.SelfTestRequest) error) func(context.Context, components.SelfTestRequest) error {
	if f == nil {
		return nil
	}
	return func(ctx context.Context, req components.SelfTestRequest) error {
		u.mu.Lock()
		u.seq++
		n := u.seq
		u.selfTests[n] = req
		u.mu.Unlock()
		defer func() {
			u.mu.Lock()
			delete(u.selfTests, n)
			u.mu.Unlock()
		}()
		return f(ctx, req)
	}
}

// setLogin wires the Colab sign-in (nil: no Colab backend).
func (u *componentUse) setLogin(f func() bool) { u.login.Store(&f) }

// gcloudUses: the components a running gcloud uses on this platform.
func gcloudUses(id string) bool {
	return id == "gcloud" || (runtime.GOOS == "darwin" && id == "python")
}

// InUse reports whether id is used by local work now, with the reason key.
func (u *componentUse) InUse(id string) (bool, string) {
	if lp := u.login.Load(); lp != nil && *lp != nil && gcloudUses(id) && (*lp)() {
		return true, useReasonColabLogin
	}
	if wp := u.work.Load(); wp != nil {
		for _, w := range (*wp)() {
			if u.workUses(w, id) {
				reason := useReasonASR
				switch w.What {
				case sched.UseDeviceTest:
					reason = useReasonDeviceTest
				case sched.UseMaterial:
					reason = useReasonMaterial
				}
				return true, reason
			}
		}
	}
	u.mu.Lock()
	sources := slices.Clone(u.pythons)
	u.mu.Unlock()
	for _, src := range sources {
		for _, p := range src() {
			if u.pathUses(p.path, id, true) {
				return true, p.reason
			}
		}
	}
	u.mu.Lock()
	tests := make([]components.SelfTestRequest, 0, len(u.selfTests))
	for _, r := range u.selfTests {
		tests = append(tests, r)
	}
	u.mu.Unlock()
	for _, r := range tests {
		if r.ID == id || (r.Python != "" && u.pathUses(r.Python, id, true)) || (r.ModelDir != "" && u.pathUses(r.ModelDir, id, false)) {
			return true, useReasonSelfCheck
		}
	}
	return false, ""
}

// workUses: does w run id (its backend's interpreter, the python runtime
// under it, or its model)?
func (u *componentUse) workUses(w sched.WorkerUse, id string) bool {
	if py, _, comp := u.cfg.WorkerPython(w.Backend); comp && u.pathUses(py, id, true) {
		return true
	}
	if w.What == sched.UseMaterial {
		return false
	}
	b, name, _, isModel := components.ParseModelID(id)
	if !isModel || b != w.Backend {
		return false
	}
	if w.Model == "" {
		return true // a device self-test: any model of its backend may be next
	}
	if name != w.Model {
		return false
	}
	precisions := []string{w.Precision}
	if w.Precision == "" || w.Precision == model.PrecisionAuto {
		// resolved by the worker from the GPU: either may be loaded
		precisions = []string{model.PrecisionFloat16, model.PrecisionInt8Float16, model.PrecisionFP16, model.PrecisionINT8, model.PrecisionINT4}
	}
	for _, p := range precisions {
		if dir, ok := u.cfg.ComponentModelDir(w.Backend, w.Model, p); ok && u.pathUses(dir, id, false) {
			return true
		}
	}
	return false
}

// pathUses: is path inside id's directory? An interpreter of any venv
// under the root also uses the python runtime ("python": every venv runs
// on it).
func (u *componentUse) pathUses(path, id string, interpreter bool) bool {
	owner := u.componentOf(path)
	if owner == "" {
		return false
	}
	return owner == id || (interpreter && id == components.IDPython)
}

// componentOf is the id of the component whose directory holds path, ""
// when path is not under the components root.
func (u *componentUse) componentOf(path string) string {
	root, err := filepath.Abs(*u.root.Load())
	if err != nil {
		return ""
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ""
	}
	first := strings.Split(filepath.ToSlash(rel), "/")[0]
	return strings.ReplaceAll(first, "__", ":")
}

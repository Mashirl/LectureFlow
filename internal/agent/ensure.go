// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

// "lf-agent ensure": install a kit (remote/kit.go) read from stdin. Each
// part lives in a content-addressed directory and is marked complete with
// a .ok file only after it was checked, so an interrupted install resumes
// where it stopped and a finished part is never redone:
//
//	python/<sha16>/   the python-build-standalone archive, unpacked
//	venv/<key16>/     python -m venv, then pip install --require-hashes --no-deps
//	                  --only-binary :all: -r <lock>, then "import ..." checked
//	worker/<key16>/   the worker scripts (SHA-256 checked)
//	models/<id>/      the model files (downloaded with resume, SHA-256 checked)
//	kits/<kit>.json   written last: the kit is ready
//
// The python-build-standalone runtime brings pip (checked on its
// install_only_stripped builds: lib/python3.12/site-packages/pip and
// ensurepip's bundled wheel), so no other installer is needed; pip's cache
// is kept below downloads/ and removed after a successful install.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"lectureflow/internal/pyenv"
	"lectureflow/internal/remote"
)

const fileOK = ".ok"

func (e *env) ensure() int {
	b, err := io.ReadAll(io.LimitReader(e.stdin, remote.MaxKitSpec+1))
	if err != nil || len(b) > remote.MaxKitSpec {
		e.fail("", remote.AgentBadRequest)
		return 1
	}
	var spec remote.KitSpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil || spec.Validate() != nil {
		e.fail("", remote.AgentBadRequest)
		return 1
	}
	hash := spec.Hash()
	if _, err := e.loadKit(hash); err == nil {
		e.out.emit(map[string]any{"type": remote.EvReady, "kit": hash})
		return 0
	}
	ctx := context.Background()
	if err := e.install(ctx, spec, hash); err != nil {
		// stderr goes to the controller's log only (installer messages,
		// never audio-derived text); events carry the code.
		msg := err.Error()
		if len(msg) > 512 {
			msg = msg[:512]
		}
		fmt.Fprintln(e.stderr, msg)
		e.failErr("", err)
		return 1
	}
	e.out.emit(map[string]any{"type": remote.EvReady, "kit": hash})
	return 0
}

func (e *env) step(name string, f float64) {
	e.out.emit(map[string]any{"type": remote.EvStep, "step": name, "fraction": f})
}

func (e *env) install(ctx context.Context, spec remote.KitSpec, hash string) error {
	for _, d := range []string{dirPython, dirVenv, dirWorker, dirModels, dirKits, dirDownloads} {
		if err := ensurePrivateDir(filepath.Join(e.dirs.Root, d)); err != nil {
			return err
		}
	}
	// Disk check: what is still to download plus the venv's estimate.
	need := int64(0)
	pyDir := filepath.Join(e.dirs.Root, dirPython, spec.PythonKey())
	if !exists(filepath.Join(pyDir, fileOK)) {
		need += spec.Python.Size * 4
	}
	venvDir := filepath.Join(e.dirs.Root, dirVenv, spec.VenvKey())
	if !exists(filepath.Join(venvDir, fileOK)) {
		need += 3 << 30
	}
	modelDir := filepath.Join(e.dirs.Root, dirModels, spec.ModelKey())
	for _, f := range spec.Model.Files {
		if _, ok := regularFile(filepath.Join(modelDir, f.Name)); !ok {
			need += f.Size
		}
	}
	if free, ok := diskFreeMB(e.dirs.Root); ok && need>>20 > free {
		return &codedErr{code: remote.AgentNoSpace, freeMB: free, needMB: need >> 20}
	}

	// 1. the Python runtime
	e.step("python", 0)
	if !exists(filepath.Join(pyDir, fileOK)) {
		archive, err := e.download(ctx, remote.KitDownload{Name: "python.tar.gz", URL: spec.Python.URL,
			SHA256: spec.Python.SHA256, Size: spec.Python.Size}, func(f float64) { e.step("python", f*0.9) })
		if err != nil {
			return err
		}
		_ = removeAllInside(e.dirs.Root, pyDir)
		if err := ensurePrivateDir(pyDir); err != nil {
			return err
		}
		if err := untarGz(archive, pyDir); err != nil {
			_ = removeAllInside(e.dirs.Root, pyDir)
			return coded(remote.AgentUnpackFailed, err)
		}
		if err := os.WriteFile(filepath.Join(pyDir, fileOK), nil, 0o600); err != nil {
			return err
		}
		_ = os.Remove(archive)
	}
	// The interpreter may be a symbolic link inside the runtime (bin/python3 → python3.12).
	pyExe, err := interpreter(pyDir, strings.Split(spec.Python.Exe, "/")...)
	if err != nil {
		return coded(remote.AgentUnpackFailed, err)
	}
	e.step("python", 1)

	// 2. the venv and its packages
	venvPy := venvPython(venvDir)
	if !exists(filepath.Join(venvDir, fileOK)) {
		e.step("venv", 0)
		_ = removeAllInside(e.dirs.Root, venvDir)
		if out, err := e.runQuiet(ctx, 10*time.Minute, pyExe, "-m", "venv", venvDir); err != nil {
			_ = removeAllInside(e.dirs.Root, venvDir)
			return coded(remote.AgentVenvFailed, fmt.Errorf("%v: %s", err, tail(out)))
		}
		e.step("venv", 1)
		lock := filepath.Join(venvDir, "requirements.lock.txt")
		if err := os.WriteFile(lock, []byte(spec.Lock.Text), 0o600); err != nil {
			return err
		}
		e.step("packages", 0)
		args := []string{"-m", "pip", "install", "--no-deps", "--require-hashes", "--only-binary", ":all:",
			"--disable-pip-version-check", "--no-input", "--progress-bar", "off", "-r", lock}
		if spec.IndexURL != "" {
			args = append(args, "--index-url", spec.IndexURL)
		}
		if out, err := e.runQuiet(ctx, 60*time.Minute, venvPy, args...); err != nil {
			return coded(remote.AgentPackagesFailed, fmt.Errorf("%v: %s", err, tail(out)))
		}
		_ = os.RemoveAll(filepath.Join(e.dirs.Root, dirDownloads, "pip"))
		e.step("packages", 1)
		if len(spec.Imports) > 0 {
			e.step("imports", 0)
			if out, err := e.runQuiet(ctx, 5*time.Minute, venvPy, "-c", pyenv.ImportCheck(spec.Imports)); err != nil {
				return coded(remote.AgentImportsFailed, fmt.Errorf("%v: %s", err, tail(out)))
			}
			e.step("imports", 1)
		}
		if err := os.WriteFile(filepath.Join(venvDir, fileOK), nil, 0o600); err != nil {
			return err
		}
	}

	// 3. the worker scripts
	e.step("worker", 0)
	wDir := filepath.Join(e.dirs.Root, dirWorker, spec.WorkerKey())
	if !exists(filepath.Join(wDir, fileOK)) {
		_ = removeAllInside(e.dirs.Root, wDir)
		if err := ensurePrivateDir(wDir); err != nil {
			return err
		}
		for _, f := range spec.Worker {
			p, err := inside(wDir, f.Name)
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, f.Data, 0o600); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(wDir, fileOK), nil, 0o600); err != nil {
			return err
		}
	}
	e.step("worker", 1)

	// 4. the model
	e.step("model", 0)
	if err := ensurePrivateDir(modelDir); err != nil {
		return err
	}
	var total, done int64
	for _, f := range spec.Model.Files {
		total += f.Size
	}
	for _, f := range spec.Model.Files {
		dst, err := inside(modelDir, f.Name)
		if err != nil {
			return err
		}
		if n, ok := regularFile(dst); ok && n == f.Size && exists(dst+".sha256") {
			if b, _ := readSmall(dst+".sha256", 128); strings.TrimSpace(string(b)) == f.SHA256 {
				done += f.Size
				continue
			}
		}
		base := done
		p, err := e.download(ctx, f, func(x float64) {
			if total > 0 {
				e.step("model", float64(base+int64(x*float64(f.Size)))/float64(total))
			}
		})
		if err != nil {
			return err
		}
		if err := os.Rename(p, dst); err != nil {
			return err
		}
		if err := os.WriteFile(dst+".sha256", []byte(f.SHA256), 0o600); err != nil {
			return err
		}
		done += f.Size
	}
	e.step("model", 1)

	// 5. the kit
	rel := func(p string) string {
		r, _ := filepath.Rel(e.dirs.Root, p)
		return filepath.ToSlash(r)
	}
	info := kitInfo{Engine: spec.Engine, Python: rel(venvPy), Script: rel(filepath.Join(wDir, remote.WorkerScript(spec.Engine))),
		ModelDir: rel(modelDir)}
	b, _ := json.Marshal(info)
	kitPath, err := inside(e.dirs.Root, dirKits, hash+".json")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(kitPath, b); err != nil {
		return err
	}
	e.step("done", 1)
	return nil
}

// venvPython is the venv's interpreter.
func venvPython(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "Scripts", "python.exe")
	}
	return filepath.Join(dir, "bin", "python")
}

// runQuiet runs an installer step and returns its combined output (kept
// only to the end of this process; never sent).
func (e *env) runQuiet(ctx context.Context, limit time.Duration, prog string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Dir = e.dirs.Root
	pipCache := filepath.Join(e.dirs.Root, dirDownloads, "pip")
	cmd.Env = append(workerEnv(e.dirs, filepath.Join(e.dirs.Root, dirDownloads)),
		"PIP_CACHE_DIR="+pipCache, "PIP_CONFIG_FILE="+os.DevNull, "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return out.String(), err
}

// tail is the last line of an installer's output (for the local error
// only; it is never part of an event).
func tail(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[len(s)-200:]
	}
	return s
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

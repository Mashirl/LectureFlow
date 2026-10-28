// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

// Running a worker (SPEC §6.0 on the remote): {venv python} {worker
// dir}/{asr.py|asr_mlx.py} --in {job}/job.json --out {job}/result.json.tmp
// in the job directory, with a minimal environment (no network for the
// model: HF_HUB_OFFLINE=1; caches and temporary files inside the agent's
// directories). PROGRESS / STATUS / ERROR_CODE lines become events; other
// stdout and all stderr stay on the remote's pipe and are dropped (plan
// §7: the remote logs codes only). On macOS "caffeinate -i -w <pid>" keeps
// the machine awake while the worker runs.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/pyenv"
	"lectureflow/internal/remote"
)

// kitInfo is kits/<hash>.json: where a kit's files are, relative to Root.
type kitInfo struct {
	Engine   string `json:"engine"`
	Python   string `json:"python"`
	Script   string `json:"script"`
	ModelDir string `json:"model_dir"`
}

// loadKit reads and checks a kit (every path stays below Root).
func (e *env) loadKit(hash string) (kitInfo, error) {
	var k kitInfo
	p, err := inside(e.dirs.Root, dirKits, hash+".json")
	if err != nil {
		return k, err
	}
	b, err := readSmall(p, 16<<10)
	if err != nil {
		return k, coded(remote.AgentKitMissing, err)
	}
	if json.Unmarshal(b, &k) != nil {
		return k, coded(remote.AgentKitMissing, nil)
	}
	py, err := interpreter(e.dirs.Root, strings.Split(k.Python, "/")...)
	if err != nil {
		return k, coded(remote.AgentKitMissing, err)
	}
	k.Python = py
	for _, rel := range []*string{&k.Script, &k.ModelDir} {
		abs, err := inside(e.dirs.Root, strings.Split(*rel, "/")...)
		if err != nil {
			return k, coded(remote.AgentKitMissing, err)
		}
		*rel = abs
	}
	if _, ok := regularFile(k.Script); !ok {
		return k, coded(remote.AgentKitMissing, nil)
	}
	return k, nil
}

// workerRun is one running worker.
type workerRun struct {
	s    *server
	job  string
	cmd  *exec.Cmd
	dir  string
	done chan struct{}

	mu     sync.Mutex
	reason string // why it was stopped ("" = it ended by itself)
	peakMB int
}

func (r *workerRun) pid() int {
	if r.cmd.Process == nil {
		return 0
	}
	return r.cmd.Process.Pid
}

func (r *workerRun) notePeak(mb int) {
	r.mu.Lock()
	r.peakMB = max(r.peakMB, mb)
	r.mu.Unlock()
}

// stop ends the worker: SIGTERM, then SIGKILL after remote.KillGrace.
func (r *workerRun) stop(reason string) {
	r.mu.Lock()
	if r.reason != "" {
		r.mu.Unlock()
		return
	}
	r.reason = reason
	r.mu.Unlock()
	terminate(r.cmd)
	go func() {
		select {
		case <-r.done:
		case <-time.After(killGrace):
			kill(r.cmd)
		}
	}()
}

func (r *workerRun) wait() { <-r.done }

// killGrace is remote.KillGrace (tests shorten it).
var killGrace = remote.KillGrace

func (s *server) startRun(req remote.Request) error {
	s.mu.Lock()
	if _, ok := s.runs[req.Job]; ok {
		s.mu.Unlock()
		return coded(remote.AgentBusy, nil)
	}
	s.mu.Unlock()
	dir, m, err := s.existingJob(req.Job)
	if err != nil {
		return err
	}
	audio := filepath.Join(dir, "audio."+m.AudioKind)
	if n, ok := regularFile(audio); !ok || n != m.AudioSize {
		return coded(remote.AgentAudioMissing, nil)
	}
	if (req.Engine == remote.EngineMLX) != (m.AudioKind == remote.AudioWAV) {
		return coded(remote.AgentBadRequest, errors.New("audio kind does not fit the engine"))
	}
	kit, err := s.loadKit(req.Kit)
	if err != nil {
		return err
	}
	if kit.Engine != req.Engine {
		return coded(remote.AgentKitMissing, nil)
	}
	params, err := remote.CheckParams(req.Engine, req.Params)
	if err != nil {
		return coded(remote.AgentBadRequest, err)
	}
	// The agent's own paths (the only keys it adds).
	params["audio"] = audio
	if req.Engine == remote.EngineMLX {
		params["model_dir"] = kit.ModelDir
	} else {
		params["model"] = kit.ModelDir
	}
	if ck, ok := params["checkpoint"].(map[string]any); ok {
		ck["path"] = filepath.Join(dir, "asr.ckpt.json")
	}
	jb, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return coded(remote.AgentInternal, err)
	}
	jobPath := filepath.Join(dir, fileJob)
	if err := writeFileAtomic(jobPath, jb); err != nil {
		return err
	}
	tmpDir := filepath.Join(dir, dirTmp)
	if err := ensurePrivateDir(tmpDir); err != nil {
		return err
	}
	out := filepath.Join(dir, fileResult+".tmp")
	_ = os.Remove(out)
	_ = os.Remove(filepath.Join(dir, fileResult))
	cmd := exec.Command(kit.Python, kit.Script, "--in", jobPath, "--out", out)
	cmd.Dir = dir
	cmd.Env = workerEnv(s.dirs, tmpDir)
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	setProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return coded(remote.AgentInternal, err)
	}
	if err := cmd.Start(); err != nil {
		return coded(remote.AgentWorkerStart, err)
	}
	r := &workerRun{s: s, job: req.Job, cmd: cmd, dir: dir, done: make(chan struct{})}
	s.mu.Lock()
	s.runs[req.Job] = r
	s.touched[req.Job] = true
	s.mu.Unlock()
	if runtime.GOOS == "darwin" {
		// Keep the Mac awake while the worker runs (plan step 4); caffeinate
		// exits with the worker.
		caf := exec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(cmd.Process.Pid))
		if caf.Start() == nil {
			go func() { _ = caf.Wait() }()
		}
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		r.watch(stdout)
	}()
	return nil
}

// workerEnv is the worker's environment: PATH, HOME, the locale, and the
// switches that keep it offline and inside the agent's directories.
func workerEnv(d Dirs, tmp string) []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "SYSTEMROOT", "WINDIR", "LOCALAPPDATA", "USERPROFILE"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	cache := filepath.Join(d.Root, "cache")
	_ = ensurePrivateDir(cache)
	// pyenv: no usage reporting (HF_HUB_DISABLE_TELEMETRY and the rest).
	return pyenv.Env(env,
		"PYTHONIOENCODING=utf-8", "PYTHONUNBUFFERED=1", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1",
		"HF_HUB_OFFLINE=1", "HF_HOME="+filepath.Join(cache, "hf"),
		"XDG_CACHE_HOME="+cache, "MPLCONFIGDIR="+filepath.Join(cache, "mpl"),
		"TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp,
	)
}

// watch reads the worker's stdout and reports its end.
func (r *workerRun) watch(stdout io.Reader) {
	s := r.s
	var errName string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	lastProgress := time.Time{}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "PROGRESS "):
			v, err := strconv.ParseFloat(strings.TrimSpace(line[len("PROGRESS "):]), 64)
			if err == nil && v >= 0 && v <= 1 && time.Since(lastProgress) >= 250*time.Millisecond {
				lastProgress = time.Now()
				s.out.emit(map[string]any{"type": remote.EvProgress, "job": r.job, "fraction": v})
			}
		case strings.HasPrefix(line, "STATUS "):
			st := strings.TrimSpace(line[len("STATUS "):])
			if slices.Contains(remote.WorkerStatuses, st) {
				s.out.emit(map[string]any{"type": remote.EvStatus, "job": r.job, "status": st})
			}
		case strings.HasPrefix(line, "ERROR_CODE "):
			name, _, _ := strings.Cut(strings.TrimSpace(line[len("ERROR_CODE "):]), " ")
			errName = name
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := r.cmd.Wait()
	close(r.done)
	r.mu.Lock()
	reason, peak := r.reason, r.peakMB
	r.mu.Unlock()
	s.mu.Lock()
	delete(s.runs, r.job)
	s.mu.Unlock()
	tmp := filepath.Join(r.dir, fileResult+".tmp")
	if reason == remote.AgentInterrupted {
		_ = os.Remove(tmp) // the session is gone: nobody to tell
		return
	}
	if reason != "" {
		_ = os.Remove(tmp)
		s.out.emit(map[string]any{"type": remote.EvError, "job": r.job, "code": reason, "retryable": true})
		return
	}
	if werr != nil {
		_ = os.Remove(tmp)
		ev := map[string]any{"type": remote.EvError, "job": r.job, "retryable": true, "code": remote.AgentWorkerFailed}
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code := ee.ExitCode()
			ev["exit"] = max(-1, min(code, 255))
			switch {
			case errName == "device_unavailable":
				ev["code"], ev["retryable"] = remote.AgentDeviceUnusable, false
			case errName == "no_space":
				ev["code"], ev["retryable"] = remote.AgentNoSpace, false
			case killedBySIGKILL(ee):
				ev["code"], ev["retryable"] = remote.AgentOutOfMemory, false
			case code == 2:
				ev["code"], ev["retryable"] = remote.AgentWorkerInput, false
			}
		}
		s.out.emit(ev)
		return
	}
	final := filepath.Join(r.dir, fileResult)
	n, ok := regularFile(tmp)
	switch {
	case !ok:
		s.out.emit(map[string]any{"type": remote.EvError, "job": r.job, "code": remote.AgentResultMissing, "retryable": true})
		return
	case n == 0 || n > remote.MaxResultBytes:
		_ = os.Remove(tmp)
		s.out.emit(map[string]any{"type": remote.EvError, "job": r.job, "code": remote.AgentResultTooLarge, "retryable": false})
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		s.out.emit(map[string]any{"type": remote.EvError, "job": r.job, "code": remote.AgentInternal, "retryable": true})
		return
	}
	sum, err := fileSHA256(final)
	if err != nil {
		s.out.emit(map[string]any{"type": remote.EvError, "job": r.job, "code": remote.AgentInternal, "retryable": true})
		return
	}
	ev := map[string]any{"type": remote.EvDone, "job": r.job, "bytes": n, "sha256": sum, "peak_mb": -1}
	if peak > 0 {
		ev["peak_mb"] = peak
	}
	s.out.emit(ev)
}

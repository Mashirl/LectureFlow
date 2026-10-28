// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

// "lf-agent serve": the control channel of one SSH session
// (remote/protocol.go). Requests come on stdin, events go to stdout. The
// session ends when stdin closes or no request (pings included) arrived
// for remote.HeartbeatTimeout: running workers are stopped (SIGTERM, then
// SIGKILL), their job directories are kept for retain_s with an .expires
// marker (a reconnecting controller continues from the checkpoint), and
// the agent exits.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"lectureflow/internal/remote"
	"lectureflow/internal/sysprobe"
)

type server struct {
	*env
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	runs     map[string]*workerRun
	touched  map[string]bool // jobs this session opened
	retain   time.Duration
	sampling bool
	machine  bool // the machine event was sent

	lastReq atomic.Int64 // unix nanos
	sampler *sysprobe.Sampler
	wg      sync.WaitGroup
}

// heartbeatTimeout and sampleEvery are variables for the tests.
var (
	heartbeatTimeout = remote.HeartbeatTimeout
	sampleEvery      = remote.HistoryStep
	aliveEvery       = 30 * time.Second
)

func (e *env) serve() int {
	ignoreSessionSignals()
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{env: e, ctx: ctx, cancel: cancel, runs: map[string]*workerRun{}, touched: map[string]bool{},
		retain: remote.DefaultRetain, sampler: sysprobe.NewSampler()}
	s.lastReq.Store(time.Now().UnixNano())
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReaderSize(e.stdin, 64<<10)
		for {
			line, err := readLine(r, remote.MaxRequestLine)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	sampleT := time.NewTicker(sampleEvery)
	defer sampleT.Stop()
	aliveT := time.NewTicker(aliveEvery)
	defer aliveT.Stop()
	for {
		select {
		case line := <-lines:
			s.lastReq.Store(time.Now().UnixNano())
			s.handle(line)
		case err := <-readErr:
			if errors.Is(err, errLineTooLong) {
				e.fail("", remote.AgentBadRequest)
			}
			s.end()
			return 0
		case <-tick.C:
			if time.Since(time.Unix(0, s.lastReq.Load())) > heartbeatTimeout {
				s.end()
				return 0
			}
		case <-sampleT.C:
			s.sample()
		case <-aliveT.C:
			s.mu.Lock()
			for id := range s.touched {
				if dir, err := e.jobDir(id); err == nil {
					touch(dir + "/" + fileAlive)
				}
			}
			s.mu.Unlock()
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads one line of at most max bytes (without the newline).
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > max+1 {
			return nil, errLineTooLong
		}
		if err == nil {
			out = out[:len(out)-1]
			if n := len(out); n > 0 && out[n-1] == '\r' {
				out = out[:n-1]
			}
			return out, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(out) > 0 {
			return out, nil
		}
		return nil, err
	}
}

func (s *server) reply(seq int64, ev map[string]any) {
	ev["seq"] = seq
	s.out.emit(ev)
}

func (s *server) failReq(seq int64, job string, err error) {
	ev := map[string]any{"type": remote.EvFail, "seq": seq, "code": codeOf(err)}
	if job != "" {
		ev["job"] = job
	}
	var ce *codedErr
	if errors.As(err, &ce) && ce.code == remote.AgentNoSpace {
		ev["free_mb"], ev["need_mb"] = ce.freeMB, ce.needMB
	}
	s.out.emit(ev)
}

func (s *server) handle(line []byte) {
	req, err := remote.ParseRequest(line)
	if err != nil {
		s.out.emit(map[string]any{"type": remote.EvFail, "code": remote.AgentBadRequest})
		return
	}
	switch req.Op {
	case remote.OpHello:
		if req.RetainS > 0 {
			s.mu.Lock()
			s.retain = time.Duration(req.RetainS) * time.Second
			s.mu.Unlock()
		}
		ev := map[string]any{"type": remote.EvHello, "version": version(), "platform": Platform()}
		if jobs := s.keptJobs(); len(jobs) > 0 {
			ev["jobs"] = joinIDs(jobs)
		}
		s.reply(req.Seq, ev)
	case remote.OpPing:
		s.reply(req.Seq, map[string]any{"type": remote.EvPong})
	case remote.OpJob:
		s.mu.Lock()
		_, running := s.runs[req.Job]
		s.mu.Unlock()
		if running {
			s.failReq(req.Seq, req.Job, coded(remote.AgentBusy, nil))
			return
		}
		have, resume, err := s.openJob(req.Job, req.AudioKind, req.AudioSize, req.AudioSHA256)
		if err != nil {
			s.failReq(req.Seq, req.Job, err)
			return
		}
		s.mu.Lock()
		s.touched[req.Job] = true
		s.mu.Unlock()
		s.reply(req.Seq, map[string]any{"type": remote.EvOK, "job": req.Job, "have": have, "resume": resume})
	case remote.OpRun:
		if err := s.startRun(req); err != nil {
			s.failReq(req.Seq, req.Job, err)
			return
		}
		s.reply(req.Seq, map[string]any{"type": remote.EvOK, "job": req.Job})
	case remote.OpCancel:
		s.mu.Lock()
		r := s.runs[req.Job]
		s.mu.Unlock()
		if r != nil {
			r.stop(remote.AgentCancelled)
		}
		s.reply(req.Seq, map[string]any{"type": remote.EvOK, "job": req.Job})
	case remote.OpDelete:
		s.mu.Lock()
		r := s.runs[req.Job]
		s.mu.Unlock()
		if r != nil {
			r.stop(remote.AgentCancelled)
			r.wait()
		}
		if err := s.deleteJob(req.Job); err != nil {
			s.failReq(req.Seq, req.Job, err)
			return
		}
		s.mu.Lock()
		delete(s.touched, req.Job)
		s.mu.Unlock()
		s.reply(req.Seq, map[string]any{"type": remote.EvOK, "job": req.Job})
	case remote.OpSample:
		s.mu.Lock()
		s.sampling = req.On
		if !req.On {
			s.machine = false
		}
		s.mu.Unlock()
		s.reply(req.Seq, map[string]any{"type": remote.EvOK})
		if req.On {
			s.sample()
		}
	}
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

// end stops the workers, keeps the session's jobs for retain and waits.
func (s *server) end() {
	s.cancel()
	s.mu.Lock()
	runs := make([]*workerRun, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	retain := s.retain
	touched := make([]string, 0, len(s.touched))
	for id := range s.touched {
		touched = append(touched, id)
	}
	s.mu.Unlock()
	for _, r := range runs {
		r.stop(remote.AgentInterrupted)
	}
	for _, r := range runs {
		r.wait()
	}
	for _, id := range touched {
		s.keepJob(id, retain)
	}
	s.wg.Wait()
}

// sample emits the machine event once and a sample while sampling is on
// or a worker runs.
func (s *server) sample() {
	s.mu.Lock()
	on := s.sampling || len(s.runs) > 0
	sendMachine := on && !s.machine
	if sendMachine {
		s.machine = true
	}
	var pids []int
	for _, r := range s.runs {
		if p := r.pid(); p > 0 {
			pids = append(pids, p)
		}
	}
	s.mu.Unlock()
	if !on {
		return
	}
	if sendMachine {
		m := s.sampler.Machine(s.ctx)
		s.out.emit(machineEvent(m))
	}
	u := s.sampler.Sample(s.ctx, pids, s.dirs.Jobs)
	s.mu.Lock()
	for _, r := range s.runs {
		r.notePeak(u.WhisperMB)
	}
	s.mu.Unlock()
	s.out.emit(sampleEvent(u))
}

func machineEvent(m sysprobe.Machine) map[string]any {
	clean := func(s string, ok func(string) bool) string {
		if ok(s) {
			return s
		}
		return ""
	}
	return map[string]any{
		"type": remote.EventMachine, "ts": float64(time.Now().Unix()),
		"gpu_name":    clean(m.GPUName, remote.GPUNamePattern.MatchString),
		"gpu_arch":    clean(m.GPUArch, remote.GPUArchPattern.MatchString),
		"compute_cap": clean(m.ComputeCap, remote.VersionPattern.MatchString),
		"driver":      clean(m.Driver, remote.DriverPattern.MatchString),
		"cuda":        clean(m.CUDA, remote.VersionPattern.MatchString),
		"gpu_count":   m.GPUCount, "vram_total_mb": m.VRAMTotalMB, "vcpus": m.VCPUs, "mem_total_mb": m.MemTotalMB,
		"storage": remote.StorageDisk,
	}
}

func sampleEvent(u sysprobe.Usage) map[string]any {
	return map[string]any{
		"type": remote.EventSample, "ts": float64(time.Now().Unix()),
		"cpu_pct": u.CPUPct, "mem_total_mb": u.MemTotalMB, "mem_available_mb": u.MemAvailableMB,
		"ramdisk_mb": -1, "data_mb": 0, "data_disk_mb": u.DataDiskMB, "whisper_mb": u.WhisperMB, // no memory disk (disk storage)
		"gpu_util_pct": u.GPUUtilPct, "vram_used_mb": u.VRAMUsedMB, "vram_reserved_mb": -1,
		"vram_whisper_mb": u.VRAMWhisperMB, "gpu_temp_c": u.GPUTempC, "gpu_power_w": u.GPUPowerW,
	}
}

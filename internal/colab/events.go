// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The standard-output contract of worker/colab_remote.py (V14.3 step 3):
// every line is one JSON object of type progress, stage, done or error,
// with a timestamp, and nothing that could carry text derived from the
// audio. The parser is strict: anything else is CodeContractViolation and
// the lecture goes back to the local devices. Python side: the EVENT_*
// tables of worker/colab_remote.py, which must list the same keys. The
// memory event is the memory probe's only line (memory.go) and never part
// of a transcription's output. The machine and sample events
// (colab-obs-core: the runtime's own readings, fields in
// internal/remote/sample.go, checked by remote.ReadMachine / ReadSample)
// come from the attaching process: a machine first, then a sample every
// 2 s; they are neither progress nor a stage (job.go lectureCell.onEvent).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"lectureflow/internal/remote"
)

const maxEventLine = 2048

// Remote stages, in the order the worker runs them.
var remoteStages = []string{"check", "install", "download_model", "load_model", "transcribe", "write"}

// Remote error codes (worker/colab_remote.py ERROR_CODES).
var remoteErrorCodes = []string{
	"bad_job", "audio_missing", "audio_hash_mismatch", "audio_unreadable", "install_failed", "no_gpu",
	"model_load_failed", "out_of_memory", "transcribe_failed", "write_failed", "internal",
}

// eventKeys: required and optional keys per event type.
var eventKeys = map[string]struct{ required, optional []string }{
	"progress": {required: []string{"type", "ts", "fraction"}},
	"stage":    {required: []string{"type", "ts", "stage"}},
	"done":     {required: []string{"type", "ts", "result", "sha256", "bytes", "audio_s", "load_s", "transcribe_s", "sentences"}, optional: []string{"mem_peak_mb", "mem_total_mb"}},
	"error":    {required: []string{"type", "ts", "code", "retryable"}, optional: []string{"exc"}},
	"memory":   {required: []string{"type", "ts", "mode", "base", "reason", "mem_total_mb", "mem_available_mb", "need_mb", "free_mb"}},
	// colab-obs-core: every field of remote.MachineEventKeys /
	// SampleEventKeys is required (-1 / "" = unknown)
	remote.EventMachine: {required: append([]string{"type", "ts"}, remote.MachineEventKeys...)},
	remote.EventSample:  {required: append([]string{"type", "ts"}, remote.SampleEventKeys...)},
}

var (
	sha256Re     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	resultNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	excNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,79}$`)
)

// remoteEvent is one parsed line.
type remoteEvent struct {
	Type     string
	TS       float64
	Fraction float64 // progress
	Stage    string  // stage
	// done
	Result      string
	SHA256      string
	Bytes       int64
	AudioS      float64
	LoadS       float64
	TranscribeS float64
	Sentences   int
	// MemPeakMB, MemTotalMB: the runtime's peak used memory while the worker
	// ran and its total (MiB; -1 when the worker could not read them).
	MemPeakMB, MemTotalMB int64
	// memory (the memory probe's only line)
	Memory memoryDecision
	// error
	ErrCode   string
	Retryable bool
	Exc       string
	// machine, sample (colab-obs-core)
	Machine remote.MachineReading
	Sample  remote.SampleReading
}

func violation(reason string) error {
	return fallbackErr(CodeContractViolation, map[string]any{"reason": reason},
		fmt.Errorf("Colab worker output broke the contract: %s", reason))
}

// scalar is one top-level value of an event object.
type scalar struct {
	kind string // "string", "number", "bool"
	str  string
	num  float64
	b    bool
}

// parseEvent checks and decodes one stdout line. The reason of a violation
// never quotes the line (it could be anything, transcript text included).
func parseEvent(line string) (remoteEvent, error) {
	var ev remoteEvent
	if len(line) > maxEventLine {
		return ev, violation("line too long")
	}
	if !utf8.ValidString(line) {
		return ev, violation("line is not UTF-8")
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return ev, violation("line is not a JSON object")
	}
	vals := map[string]scalar{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return ev, violation("line is not a JSON object")
		}
		key, ok := kt.(string)
		if !ok {
			return ev, violation("line is not a JSON object")
		}
		if _, dup := vals[key]; dup {
			return ev, violation("duplicate key")
		}
		vt, err := dec.Token()
		if err != nil {
			return ev, violation("line is not a JSON object")
		}
		switch v := vt.(type) {
		case string:
			vals[key] = scalar{kind: "string", str: v}
		case json.Number:
			f, err := v.Float64()
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return ev, violation("bad number")
			}
			vals[key] = scalar{kind: "number", num: f}
		case bool:
			vals[key] = scalar{kind: "bool", b: v}
		default: // objects, arrays, null: never part of the contract
			return ev, violation("nested or null value")
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return ev, violation("line is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return ev, violation("trailing data after the object")
	}
	typ, ok := vals["type"]
	if !ok || typ.kind != "string" {
		return ev, violation("no event type")
	}
	spec, ok := eventKeys[typ.str]
	if !ok {
		return ev, violation("unknown event type")
	}
	for k := range vals {
		if !slices.Contains(spec.required, k) && !slices.Contains(spec.optional, k) {
			return ev, violation("unknown field in a " + typ.str + " event")
		}
	}
	for _, k := range spec.required {
		if _, ok := vals[k]; !ok {
			return ev, violation("field missing in a " + typ.str + " event")
		}
	}
	ev.Type = typ.str
	num := func(k string, min, max float64) (float64, error) {
		v := vals[k]
		if v.kind != "number" || v.num < min || v.num > max {
			return 0, violation("bad value of " + k)
		}
		return v.num, nil
	}
	integer := func(k string) (int64, error) {
		f, err := num(k, 0, 1<<53)
		if err != nil || f != math.Trunc(f) {
			return 0, violation("bad value of " + k)
		}
		return int64(f), nil
	}
	str := func(k string, ok func(string) bool) (string, error) {
		v := vals[k]
		if v.kind != "string" || !ok(v.str) {
			return "", violation("bad value of " + k)
		}
		return v.str, nil
	}
	if ev.TS, err = num("ts", 0, 1e11); err != nil {
		return ev, err
	}
	switch ev.Type {
	case "progress":
		ev.Fraction, err = num("fraction", 0, 1)
	case "stage":
		ev.Stage, err = str("stage", func(s string) bool { return slices.Contains(remoteStages, s) })
	case "done":
		if ev.Result, err = str("result", resultNameRe.MatchString); err != nil {
			return ev, err
		}
		if ev.SHA256, err = str("sha256", sha256Re.MatchString); err != nil {
			return ev, err
		}
		if ev.Bytes, err = integer("bytes"); err != nil {
			return ev, err
		}
		if ev.AudioS, err = num("audio_s", 0, 1e7); err != nil {
			return ev, err
		}
		if ev.LoadS, err = num("load_s", 0, 1e7); err != nil {
			return ev, err
		}
		if ev.TranscribeS, err = num("transcribe_s", 0, 1e7); err != nil {
			return ev, err
		}
		var n int64
		if n, err = integer("sentences"); err != nil {
			return ev, err
		}
		ev.Sentences = int(n)
		ev.MemPeakMB, ev.MemTotalMB = -1, -1
		if _, ok := vals["mem_peak_mb"]; ok {
			if ev.MemPeakMB, err = integer("mem_peak_mb"); err != nil {
				return ev, err
			}
		}
		if _, ok := vals["mem_total_mb"]; ok {
			ev.MemTotalMB, err = integer("mem_total_mb")
		}
	case "memory":
		d := &ev.Memory
		strOf := func(k string) string { return vals[k].str }
		for _, k := range []string{"mode", "base", "reason"} {
			if vals[k].kind != "string" {
				return ev, violation("bad value of " + k)
			}
		}
		d.Mode, d.Base, d.Reason = strOf("mode"), strOf("base"), strOf("reason")
		if err = checkMemoryEvent(*d); err != nil {
			return ev, err
		}
		for _, f := range []struct {
			k string
			p *int64
		}{{"mem_total_mb", &d.MemTotalMB}, {"mem_available_mb", &d.MemAvailableMB}, {"need_mb", &d.NeedMB}, {"free_mb", &d.FreeMB}} {
			if *f.p, err = integer(f.k); err != nil {
				return ev, err
			}
		}
	case remote.EventMachine, remote.EventSample:
		get := func(k string) (any, bool) {
			v, ok := vals[k]
			switch v.kind {
			case "string":
				return v.str, ok
			case "number":
				return v.num, ok
			}
			return v.b, ok
		}
		var bad string
		if ev.Type == remote.EventMachine {
			ev.Machine, bad = remote.ReadMachine(get)
		} else {
			ev.Sample, bad = remote.ReadSample(get)
		}
		if bad != "" {
			return ev, violation("bad value of " + bad)
		}
	case "error":
		if ev.ErrCode, err = str("code", func(s string) bool { return slices.Contains(remoteErrorCodes, s) }); err != nil {
			return ev, err
		}
		if v := vals["retryable"]; v.kind != "bool" {
			return ev, violation("bad value of retryable")
		} else {
			ev.Retryable = v.b
		}
		if _, ok := vals["exc"]; ok {
			ev.Exc, err = str("exc", excNameRe.MatchString)
		}
	}
	return ev, err
}

// eventStream assembles the kernel's stdout stream chunks (a chunk may
// hold several lines or part of one) into lines and parses them. After a
// done or error event nothing may follow.
type eventStream struct {
	buf      []byte
	final    *remoteEvent
	onEvent  func(remoteEvent) error
	lastTS   float64
	nonEmpty bool
}

// feed adds a chunk of the stdout stream.
func (s *eventStream) feed(chunk string) error {
	s.buf = append(s.buf, chunk...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			if len(s.buf) > maxEventLine {
				return violation("line too long")
			}
			return nil
		}
		line := strings.TrimRight(string(s.buf[:i]), "\r")
		s.buf = s.buf[i+1:]
		if err := s.line(line); err != nil {
			return err
		}
	}
}

func (s *eventStream) line(line string) error {
	if line == "" {
		return violation("empty line")
	}
	if s.final != nil {
		return violation("output after the final event")
	}
	ev, err := parseEvent(line)
	if err != nil {
		return err
	}
	if ev.TS+1 < s.lastTS { // allow 1 s of clock jitter
		return violation("timestamps go backwards")
	}
	s.lastTS = max(s.lastTS, ev.TS)
	s.nonEmpty = true
	if ev.Type == "done" || ev.Type == "error" {
		final := ev
		s.final = &final
	}
	if s.onEvent != nil {
		return s.onEvent(ev)
	}
	return nil
}

// end is called when the execution finished: a partial last line is a
// violation; no final event is CodeWorkerExited.
func (s *eventStream) end() (remoteEvent, error) {
	if len(bytes.TrimSpace(s.buf)) > 0 {
		return remoteEvent{}, violation("unterminated last line")
	}
	if s.final == nil {
		return remoteEvent{}, fallbackErr(CodeWorkerExited, nil, errors.New("the Colab worker ended without a done or error event"))
	}
	return *s.final, nil
}

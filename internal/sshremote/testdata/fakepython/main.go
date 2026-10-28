// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command fakepython stands in for the remote's Python in the SSH tests
// (never a real interpreter, never a real model): it answers
// "-m venv DIR" (copies itself to DIR/bin/python or DIR/Scripts/python.exe),
// "-m pip install ... --require-hashes ... -r LOCK" (checks the flags),
// "-c import ..." (succeeds) and "SCRIPT --in job.json --out OUT" (a worker
// following SPEC §6.0: PROGRESS lines, a checkpoint file, result.json).
// $HOME/fakepython.json sets the worker's behaviour:
// {"mode": "ok"|"fail"|"input"|"device"|"oom"|"bad_result"|"huge",
//
//	"step_ms": 50, "steps": 10, "sentences": 3}.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

type behaviour struct {
	Mode      string `json:"mode"`
	StepMS    int    `json:"step_ms"`
	Steps     int    `json:"steps"`
	Sentences int    `json:"sentences"`
}

func main() {
	args := os.Args[1:]
	switch {
	case len(args) >= 3 && args[0] == "-m" && args[1] == "venv":
		os.Exit(venv(args[2]))
	case len(args) >= 3 && args[0] == "-m" && args[1] == "pip":
		os.Exit(pip(args[2:]))
	case len(args) >= 2 && args[0] == "-c":
		if !strings.HasPrefix(args[1], "import ") {
			os.Exit(1)
		}
		os.Exit(0)
	case len(args) == 5 && args[1] == "--in" && args[3] == "--out":
		os.Exit(worker(args[0], args[2], args[4]))
	}
	fmt.Fprintln(os.Stderr, "fakepython: unexpected arguments", args)
	os.Exit(2)
}

func venv(dir string) int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return 1
	}
	dst := filepath.Join(dir, "bin", "python")
	if runtime.GOOS == "windows" {
		dst = filepath.Join(dir, "Scripts", "python.exe")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 1
	}
	if err := os.WriteFile(dst, b, 0o700); err != nil {
		return 1
	}
	_ = os.WriteFile(filepath.Join(dir, "pyvenv.cfg"), []byte("home = fake\n"), 0o600)
	return 0
}

func pip(args []string) int {
	if args[0] != "install" {
		return 2
	}
	for _, need := range []string{"--require-hashes", "--no-deps", "-r"} {
		if !slices.Contains(args, need) {
			fmt.Fprintln(os.Stderr, "fakepython: pip without", need)
			return 1
		}
	}
	i := slices.Index(args, "-r")
	b, err := os.ReadFile(args[i+1])
	if err != nil || !strings.Contains(string(b), "--hash=sha256:") {
		fmt.Fprintln(os.Stderr, "fakepython: lock without hashes")
		return 1
	}
	return 0
}

func load() behaviour {
	b := behaviour{Mode: "ok", StepMS: 30, Steps: 10, Sentences: 3}
	if raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), "fakepython.json")); err == nil {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// audioMS reads the length of a 16 kHz mono 16-bit WAV (also behind the
// tests' "fLaC" marker; anything else is taken for 60 s).
func audioMS(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	head := make([]byte, 48)
	if _, err := io.ReadFull(f, head); err != nil {
		return 60000
	}
	if string(head[:4]) == "fLaC" { // the tests' "FLAC": the marker, then the WAV
		head = head[4:]
	}
	if string(head[:4]) != "RIFF" {
		return 60000
	}
	n := binary.LittleEndian.Uint32(head[40:44])
	return int64(n) / 32
}

func worker(script, in, out string) int {
	b := load()
	raw, err := os.ReadFile(in)
	if err != nil {
		return 2
	}
	var job map[string]any
	if json.Unmarshal(raw, &job) != nil {
		return 2
	}
	audio, _ := job["audio"].(string)
	if _, err := os.Stat(audio); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: audio missing")
		return 2
	}
	md, _ := job["model_dir"].(string)
	if md == "" {
		md, _ = job["model"].(string)
	}
	if st, err := os.Stat(md); err != nil || !st.IsDir() {
		fmt.Fprintln(os.Stderr, "ERROR: model missing")
		return 2
	}
	switch b.Mode {
	case "fail":
		return 1
	case "input":
		return 2
	case "device":
		fmt.Println("ERROR_CODE device_unavailable")
		return 2
	}
	ms := audioMS(audio)
	start := 0
	ckPath := ""
	if ck, ok := job["checkpoint"].(map[string]any); ok {
		ckPath, _ = ck["path"].(string)
		if c, err := os.ReadFile(ckPath); err == nil {
			var v struct {
				Step int `json:"step"`
			}
			if json.Unmarshal(c, &v) == nil {
				start = v.Step
			}
		}
	}
	fmt.Println("STATUS loading_model")
	for i := start; i < b.Steps; i++ {
		fmt.Printf("PROGRESS %.2f\n", float64(i)/float64(b.Steps))
		time.Sleep(time.Duration(b.StepMS) * time.Millisecond)
		if ckPath != "" {
			_ = os.WriteFile(ckPath, []byte(fmt.Sprintf(`{"step": %d}`, i+1)), 0o600)
		}
		if b.Mode == "oom" && i == b.Steps/2 {
			p, _ := os.FindProcess(os.Getpid())
			_ = p.Kill()
		}
	}
	var sentences []map[string]any
	n := max(b.Sentences, 1)
	for i := 0; i < n; i++ {
		s := int64(i) * ms / int64(n)
		e := s + ms/int64(n)/2
		sentences = append(sentences, map[string]any{"start_ms": s, "end_ms": e, "text": fmt.Sprintf("Sentence %d.", i+1),
			"low_conf_words": []any{}, "words": []any{map[string]any{"w": "Sentence", "s": s, "e": e}},
			"extra_key": "dropped"})
	}
	res := map[string]any{"prompt_terms_used": 0, "sentences": sentences,
		"timing":       map[string]any{"load_s": 0.1, "transcribe_s": 0.2, "audio_s": float64(ms) / 1000},
		"resumed_from": start, "script": filepath.Base(script)}
	if b.Mode == "bad_result" {
		res["sentences"] = []any{map[string]any{"start_ms": ms + 60000, "end_ms": ms + 61000, "text": "x"}}
	}
	if ckPath != "" {
		res["parts"] = []any{map[string]any{"from_ms": 0, "to_ms": ms, "device_id": "ssh:test", "model": "m", "precision": "p"}}
	}
	data, _ := json.Marshal(res)
	if b.Mode == "huge" {
		data = append(data[:len(data)-1], []byte(`,"pad":"`+strings.Repeat("x", 33<<20)+`"}`)...)
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		return 1
	}
	fmt.Println("PROGRESS 1.00")
	return 0
}

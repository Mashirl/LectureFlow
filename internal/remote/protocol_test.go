// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseEventValid(t *testing.T) {
	sha := strings.Repeat("a", 64)
	for _, tc := range []struct {
		line string
		want func(AgentEvent) bool
	}{
		{`{"type":"hello","seq":1,"version":"3.1.0","platform":"darwin-arm64","jobs":"01A,01B"}`,
			func(e AgentEvent) bool { return e.Seq == 1 && e.Platform == PlatformDarwinArm64 && len(e.Jobs) == 2 }},
		{`{"type":"ok","seq":2,"job":"01A","have":10,"resume":true}`, func(e AgentEvent) bool { return e.Have == 10 && e.Resume }},
		{`{"type":"fail","seq":3,"code":"no_space","free_mb":5,"need_mb":900}`,
			func(e AgentEvent) bool { return e.Code == AgentNoSpace && e.FreeMB == 5 && e.NeedMB == 900 }},
		{`{"type":"progress","job":"01A","fraction":0.42,"ts":1700000000}`, func(e AgentEvent) bool { return e.Fraction == 0.42 }},
		{`{"type":"status","job":"01A","status":"loading_model"}`, func(e AgentEvent) bool { return e.Status == "loading_model" }},
		{`{"type":"done","job":"01A","bytes":12,"sha256":"` + sha + `","peak_mb":-1}`,
			func(e AgentEvent) bool { return e.Bytes == 12 && e.SHA256 == sha && e.PeakMB == -1 }},
		{`{"type":"error","job":"01A","code":"out_of_memory","retryable":false,"exit":137}`,
			func(e AgentEvent) bool { return e.Code == AgentOutOfMemory && e.Exit == 137 }},
		{`{"type":"stat","have":0,"complete":false}`, func(e AgentEvent) bool { return !e.Complete }},
		{`{"type":"step","step":"model","fraction":-1}`, func(e AgentEvent) bool { return e.Step == "model" }},
		{`{"type":"ready","kit":"` + sha + `"}`, func(e AgentEvent) bool { return e.Kit == sha }},
		{`{"type":"probe","platform":"linux-x64","cpus":16,"mem_total_mb":64000,"disk_free_mb":-1,"vram_total_mb":24576,"gpu_name":"NVIDIA GeForce RTX 4090","driver":"550.54.14","os_version":"Linux\u0007 6.8","metal":false}`,
			func(e AgentEvent) bool { return e.Probe.VRAMTotalMB == 24576 && e.Probe.OSVersion == "Linux 6.8" }},
		{`{"type":"version","version":"0.0.0-dev","platform":"windows-x64","sha256":"` + sha + `"}`,
			func(e AgentEvent) bool { return e.Version == "0.0.0-dev" }},
	} {
		ev, err := ParseEvent([]byte(tc.line))
		if err != nil {
			t.Errorf("%s: %v", tc.line, err)
			continue
		}
		if !tc.want(ev) {
			t.Errorf("%s: %+v", tc.line, ev)
		}
	}
}

func TestParseEventMachineAndSample(t *testing.T) {
	m := map[string]any{"type": EventMachine, "ts": 1.0}
	for _, k := range MachineEventKeys {
		m[k] = -1
	}
	m["gpu_name"], m["gpu_arch"], m["compute_cap"], m["driver"], m["cuda"], m["storage"] = "Apple M6", "", "", "", "", "disk"
	b, _ := json.Marshal(m)
	ev, err := ParseEvent(b)
	if err != nil || ev.Machine["gpu_name"] != "Apple M6" {
		t.Fatalf("machine: %v %+v", err, ev)
	}
	delete(m, "vcpus")
	b, _ = json.Marshal(m)
	if _, err := ParseEvent(b); err == nil {
		t.Fatal("a machine event without a key was accepted")
	}
	s := map[string]any{"type": EventSample}
	for _, k := range SampleEventKeys {
		s[k] = 10
	}
	s["gpu_power_w"] = 35.5
	b, _ = json.Marshal(s)
	if ev, err := ParseEvent(b); err != nil || ev.Sample["gpu_power_w"] != 35.5 {
		t.Fatalf("sample: %v %+v", err, ev)
	}
	s["cpu_pct"] = 101
	b, _ = json.Marshal(s)
	if _, err := ParseEvent(b); err == nil {
		t.Fatal("cpu_pct 101 was accepted")
	}
}

func TestParseEventRejects(t *testing.T) {
	sha := strings.Repeat("a", 64)
	for _, line := range []string{
		``, `[]`, `"x"`, `{}`, `{"type":"nope"}`,
		`{"type":"pong","seq":1,"seq":2}`,                                        // duplicate key
		`{"type":"pong","seq":1,"extra":1}`,                                      // unknown key
		`{"type":"pong","seq":"1"}`,                                              // wrong type
		`{"type":"pong","seq":1.5}`,                                              // not an integer
		`{"type":"progress","job":"01A","fraction":1.5}`,                         // out of range
		`{"type":"progress","job":"../x","fraction":0.1}`,                        // bad job id
		`{"type":"fail","code":"made_up"}`,                                       // unknown code
		`{"type":"status","job":"01A","status":"shell"}`,                         // unknown status
		`{"type":"done","job":"01A","bytes":99999999999,"sha256":"` + sha + `"}`, // too large
		`{"type":"done","job":"01A","bytes":1,"sha256":"xyz"}`,
		`{"type":"pong","seq":1} trailing`,
		`{"type":"hello","seq":1,"version":"3.1.0","platform":"darwin-arm64","jobs":{"a":1}}`, // not a scalar
		`{"type":"probe","platform":"linux-x64","cpus":1,"mem_total_mb":1,"disk_free_mb":1,"vram_total_mb":1,"gpu_name":"<script>"}`,
		"{\"type\":\"pong\",\"seq\":1}\xff",
		`{"type":"pong","seq":1,` + strings.Repeat(" ", MaxEventLine) + `}`,
	} {
		if _, err := ParseEvent([]byte(line)); err == nil {
			t.Errorf("accepted %q", line)
		} else if !errors.Is(err, ErrContract) {
			t.Errorf("%q: %v is not a contract violation", line, err)
		}
	}
}

func TestParseRequest(t *testing.T) {
	sha := strings.Repeat("b", 64)
	good := []string{
		`{"op":"hello","seq":1,"version":"3.1.0","retain_s":7200}`,
		`{"op":"ping","seq":2}`,
		`{"op":"job","seq":3,"job":"01A","audio_kind":"wav","audio_size":1000,"audio_sha256":"` + sha + `"}`,
		`{"op":"run","seq":4,"job":"01A","engine":"mlx","kit":"` + sha + `","params":{"model":"large-v3-turbo","precision":"fp16","device":"mlx:gpu","language":"en","initial_prompt":"x","word_timestamps":true}}`,
		`{"op":"cancel","seq":5,"job":"01A"}`,
		`{"op":"sample","seq":6,"on":true}`,
	}
	for _, l := range good {
		if _, err := ParseRequest([]byte(l)); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
	bad := []string{
		`{"op":"exec","seq":1}`,
		`{"op":"hello","seq":1,"retain_s":99999}`,
		`{"op":"job","seq":3,"job":"../../etc","audio_kind":"wav","audio_size":1,"audio_sha256":"` + sha + `"}`,
		`{"op":"job","seq":3,"job":"01A","audio_kind":"mp3","audio_size":1,"audio_sha256":"` + sha + `"}`,
		`{"op":"run","seq":4,"job":"01A","engine":"mlx","kit":"` + sha + `","params":{"audio":"/etc/passwd"}}`,
		`{"op":"run","seq":4,"job":"01A","engine":"mlx","kit":"` + sha + `","params":{"model_dir":"/x"}}`,
		`{"op":"run","seq":4,"job":"01A","engine":"faster-whisper","kit":"` + sha + `","params":{"model":"/x"}}`,
		`{"op":"run","seq":4,"job":"01A","engine":"mlx","kit":"` + sha + `","params":{"checkpoint":{"path":"/tmp/x"}}}`,
		`{"op":"run","seq":4,"job":"01A","engine":"mlx","kit":"` + sha + `","params":{"word_timestamps":"yes"}}`,
		`{"op":"ping","seq":1,"unknown":1}`,
		`{"op":"ping","seq":-1}`,
	}
	for _, l := range bad {
		if _, err := ParseRequest([]byte(l)); err == nil {
			t.Errorf("accepted %s", l)
		}
	}
}

func TestStripPathsAndCheck(t *testing.T) {
	job := map[string]any{"audio": "/a/audio.wav", "model_dir": "/m", "model": "large-v3-turbo", "precision": "fp16",
		"device": "mlx:gpu", "language": "en", "initial_prompt": "p", "word_timestamps": true,
		"checkpoint": map[string]any{"path": "/a/ckpt", "job_id": "01A", "input_hash": strings.Repeat("c", 64),
			"device_id": "ssh:mac", "model": "large-v3-turbo", "precision": "fp16", "chunk_s": 600}}
	StripPaths(EngineMLX, job)
	if _, ok := job["audio"]; ok {
		t.Fatal("audio kept")
	}
	if _, ok := job["checkpoint"].(map[string]any)["path"]; ok {
		t.Fatal("checkpoint.path kept")
	}
	b, _ := json.Marshal(job)
	if _, err := CheckParams(EngineMLX, b); err != nil {
		t.Fatal(err)
	}
}

func TestErrorKinds(t *testing.T) {
	e := &Error{Kind: ErrFallback, Code: CodeOffline, Err: errors.New("x")}
	wrapped := errors.Join(errors.New("ctx"), e)
	if !IsFallback(wrapped) || IsBusy(wrapped) || CodeOf(wrapped) != CodeOffline || !IsRemote(wrapped) {
		t.Fatal("kinds")
	}
	if IsFallback(errors.New("plain")) || IsRemote(nil) {
		t.Fatal("plain errors")
	}
}

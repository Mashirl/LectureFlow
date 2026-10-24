// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"strings"
	"testing"
)

func TestParseEventAccepts(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for _, line := range []string{
		`{"type":"progress","ts":1793260800.5,"fraction":0.25}`,
		`{"type":"progress","ts":1793260800,"fraction":1}`,
		`{"ts":1793260800,"type":"stage","stage":"install"}`,
		`{"type":"stage","ts":1,"stage":"download_model"}`,
		`{"type":"done","ts":2,"result":"result.json","sha256":"` + sha + `","bytes":123,"audio_s":600,"load_s":12.5,"transcribe_s":96.25,"sentences":3}`,
		`{"type":"error","ts":3,"code":"no_gpu","retryable":true}`,
		`{"type":"error","ts":3,"code":"internal","retryable":false,"exc":"torch.cuda.OutOfMemoryError"}`,
		` {"type":"progress","ts":1,"fraction":0.5} `,
	} {
		if _, err := parseEvent(line); err != nil {
			t.Errorf("%s: %v", line, err)
		}
	}
}

func TestParseEventRejects(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for name, line := range map[string]string{
		"plain text":            `Welcome to the lecture.`,
		"JSON string":           `"Welcome"`,
		"JSON array":            `[1,2]`,
		"unknown type":          `{"type":"log","ts":1,"message":"hi"}`,
		"text field":            `{"type":"progress","ts":1,"fraction":0.5,"text":"Welcome"}`,
		"segment field":         `{"type":"stage","ts":1,"stage":"transcribe","segment":"Welcome to"}`,
		"unknown stage":         `{"type":"stage","ts":1,"stage":"Welcome to the lecture"}`,
		"fraction > 1":          `{"type":"progress","ts":1,"fraction":1.5}`,
		"fraction string":       `{"type":"progress","ts":1,"fraction":"0.5"}`,
		"missing ts":            `{"type":"progress","fraction":0.5}`,
		"negative ts":           `{"type":"progress","ts":-1,"fraction":0.5}`,
		"nested object":         `{"type":"progress","ts":1,"fraction":0.5,"x":{"text":"Welcome"}}`,
		"null value":            `{"type":"progress","ts":1,"fraction":null}`,
		"duplicate key":         `{"type":"progress","ts":1,"fraction":0.5,"fraction":0.6}`,
		"trailing data":         `{"type":"progress","ts":1,"fraction":0.5} Welcome`,
		"two objects":           `{"type":"progress","ts":1,"fraction":0.5}{"type":"progress","ts":1,"fraction":0.5}`,
		"result path":           `{"type":"done","ts":2,"result":"../result.json","sha256":"` + sha + `","bytes":1,"audio_s":1,"load_s":1,"transcribe_s":1,"sentences":1}`,
		"result with spaces":    `{"type":"done","ts":2,"result":"Welcome to the lecture","sha256":"` + sha + `","bytes":1,"audio_s":1,"load_s":1,"transcribe_s":1,"sentences":1}`,
		"bad sha":               `{"type":"done","ts":2,"result":"result.json","sha256":"XYZ","bytes":1,"audio_s":1,"load_s":1,"transcribe_s":1,"sentences":1}`,
		"fractional bytes":      `{"type":"done","ts":2,"result":"result.json","sha256":"` + sha + `","bytes":1.5,"audio_s":1,"load_s":1,"transcribe_s":1,"sentences":1}`,
		"done missing sha":      `{"type":"done","ts":2,"result":"result.json","bytes":1,"audio_s":1,"load_s":1,"transcribe_s":1,"sentences":1}`,
		"error message":         `{"type":"error","ts":3,"code":"internal","retryable":true,"message":"could not decode Welcome"}`,
		"error exc with spaces": `{"type":"error","ts":3,"code":"internal","retryable":true,"exc":"Welcome to the lecture"}`,
		"unknown error code":    `{"type":"error","ts":3,"code":"Welcome","retryable":true}`,
		"retryable not bool":    `{"type":"error","ts":3,"code":"internal","retryable":"yes"}`,
		"too long":              `{"type":"progress","ts":1,"fraction":0.5,"pad":"` + strings.Repeat("x", 3000) + `"}`,
		"invalid UTF-8":         "{\"type\":\"progress\",\"ts\":1,\"fraction\":0.5}\xff",
		"empty":                 ``,
	} {
		_, err := parseEvent(line)
		if Code(err) != CodeContractViolation || !IsFallback(err) {
			t.Errorf("%s: err %v", name, err)
			continue
		}
		if strings.Contains(err.Error(), "Welcome") {
			t.Errorf("%s: the error quotes the line", name)
		}
	}
}

func TestEventStream(t *testing.T) {
	var got []remoteEvent
	s := &eventStream{onEvent: func(e remoteEvent) error { got = append(got, e); return nil }}
	feed := func(chunks ...string) error {
		for _, c := range chunks {
			if err := s.feed(c); err != nil {
				return err
			}
		}
		return nil
	}
	sha := strings.Repeat("0f", 32)
	err := feed(`{"type":"stage","ts":1,"stage":"check"}`+"\n"+`{"type":"progress","ts":2,"fra`, `ction":0.5}`+"\r\n",
		`{"type":"done","ts":3,"result":"result.json","sha256":"`+sha+`","bytes":10,"audio_s":60,"load_s":1,"transcribe_s":6,"sentences":2}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	final, err := s.end()
	if err != nil || final.Type != "done" || final.Bytes != 10 || len(got) != 3 || got[1].Fraction != 0.5 {
		t.Fatalf("final %+v err %v got %+v", final, err, got)
	}
	cases := map[string][]string{
		"output after done":  {`{"type":"error","ts":3,"code":"internal","retryable":true}` + "\n", `{"type":"progress","ts":4,"fraction":1}` + "\n"},
		"empty line":         {"\n"},
		"partial last line":  {`{"type":"progress","ts":4,"fraction":1}`},
		"time goes back":     {`{"type":"progress","ts":100,"fraction":0.1}` + "\n" + `{"type":"progress","ts":50,"fraction":0.2}` + "\n"},
		"endless line":       {strings.Repeat("x", maxEventLine+10)},
		"text between lines": {`{"type":"stage","ts":1,"stage":"check"}` + "\nWelcome to the lecture.\n"},
	}
	for name, chunks := range cases {
		s := &eventStream{}
		err := feed2(s, chunks)
		if err == nil {
			_, err = s.end()
		}
		if Code(err) != CodeContractViolation {
			t.Errorf("%s: err %v", name, err)
		}
	}
	// No final event: the worker ended without saying anything.
	s2 := &eventStream{}
	_ = s2.feed(`{"type":"stage","ts":1,"stage":"check"}` + "\n")
	if _, err := s2.end(); Code(err) != CodeWorkerExited || !IsFallback(err) {
		t.Errorf("no final event: %v", err)
	}
}

func feed2(s *eventStream, chunks []string) error {
	for _, c := range chunks {
		if err := s.feed(c); err != nil {
			return err
		}
	}
	return nil
}

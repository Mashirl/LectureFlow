// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package qa

import (
	"reflect"
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// The coded failures (SPEC-v2 P14.3) do not change the English reason of
// §7.4 (it is the retry feedback): the sentences are the same as before,
// byte for byte, and ParseReason reads every one of them back.
func TestFailureRoundTrip(t *testing.T) {
	terms := []model.Term{
		{En: "secret key", Zh: "密钥", Enabled: true, Tier: model.TierEnforce},
		{En: `quote "x"; y`, Zh: "引号；分号", Enabled: true, Tier: model.TierEnforce},
	}
	cases := []struct {
		target, zh string
		want       string // the reason, as the sentences have always been written
		codes      []string
	}{
		{"Hello there.", "  ", "the translation is empty", []string{CodeEmpty}},
		{"Use the secret key now.", "现在使用这个秘密的钥匙吧朋友们。", `glossary term "secret key" must be translated as "密钥"`, []string{CodeTermMissing}},
		{`Say quote "x"; y twice.`, "说两次这个东西吧朋友。", `glossary term "quote \"x\"; y" must be translated as "引号；分号"`, []string{CodeTermMissing}},
		{"This is a long sentence about many things here.", "This is mostly English text 中文", "the translation is mostly English (92% Latin letters)", []string{CodeMostlyEnglish}},
		{"One two three four five six seven eight nine ten.", "一二三", "the length ratio is 0.30 characters per English word, outside [0.5, 4]", []string{CodeLengthRatio}},
		{"We need 64 bits here today.", "我们今天在这里需要很多位。", "the number 64 from the source sentence is missing", []string{CodeNumberMissing}},
		{"Values 3 and 1,024 matter now.", "这些值现在很重要啊。", "the numbers 3, 1024 from the source sentence are missing", []string{CodeNumbersMissing}},
		{"Use the secret key with 64 bits and 128 rounds today please.", "Use the key with bits", "",
			[]string{CodeTermMissing, CodeMostlyEnglish, CodeNumbersMissing}},
	}
	for _, c := range cases {
		ok, reason := Check(c.target, c.zh, terms)
		if ok {
			t.Fatalf("%q: passed", c.zh)
		}
		if c.want != "" && reason != c.want {
			t.Errorf("reason %q, want %q", reason, c.want)
		}
		fs, ok := ParseReason(reason)
		var codes []string
		for _, f := range fs {
			codes = append(codes, f.Code)
		}
		if !ok || !slices.Equal(codes, c.codes) {
			t.Errorf("%q: parsed %v (ok %v), want %v", reason, codes, ok, c.codes)
			continue
		}
		if !reflect.DeepEqual(fs, Failures(c.target, c.zh, terms)) {
			t.Errorf("%q: parsed %+v, checked %+v", reason, fs, Failures(c.target, c.zh, terms))
		}
		if Reason(fs) != reason {
			t.Errorf("rewritten %q, stored %q", Reason(fs), reason)
		}
		info := ErrorInfo(reason)
		if info == nil || info.Code != c.codes[0] || info.Detail != reason {
			t.Fatalf("ErrorInfo %+v", info)
		}
		extra, _ := info.Params["extra"].([]model.ErrorInfo)
		if len(extra) != len(c.codes)-1 {
			t.Errorf("%q: extra %+v", reason, extra)
		}
	}

	// A reason this package did not write is not coded.
	for _, legacy := range []string{"数字 64 没有出现在译文中", "the number 64 from the source sentence is missing; and more", ""} {
		if info := ErrorInfo(legacy); info != nil {
			t.Errorf("%q: %+v", legacy, info)
		}
	}
	for _, code := range ErrorCodes {
		if (Failure{Code: code}).Text() == code {
			t.Errorf("%s has no sentence", code)
		}
	}
}

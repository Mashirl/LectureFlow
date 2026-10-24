// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/qa"
)

// SPEC-v2 P14.3 (p2-tail): a sentence's translation-check reason (stored in
// English, the retry feedback of §7) also comes coded as qa_error_info, so
// the transcript page can show it in the interface language.
func TestSentenceQAErrorInfo(t *testing.T) {
	for _, code := range qa.ErrorCodes {
		if _, ok := errorCatalog[code]; !ok {
			t.Errorf("qa code %s is not in the catalog", code)
		}
	}
	terms := []model.Term{{En: "secret key", Zh: "密钥", Enabled: true, Tier: model.TierEnforce}}
	_, reason := qa.Check("Use the secret key with 64 bits and 128 rounds today please.", "Use the key with bits", terms)
	zh := "Use the key with bits"
	v := sentenceView(model.Sentence{Idx: 3, Text: "x", Zh: &zh, ZhStatus: model.ZhQAFailed, QAError: &reason}, nil)
	info := v.QAErrorInfo
	if info == nil || info.Code != qa.CodeTermMissing || info.Detail != reason || v.QAError == nil || *v.QAError != reason {
		t.Fatalf("qa_error_info %+v for %q", info, reason)
	}
	wantCataloged(t, "qa", info)
	extra, _ := info.Params["extra"].([]model.ErrorInfo)
	if len(extra) != 2 || extra[0].Code != qa.CodeMostlyEnglish || extra[1].Code != qa.CodeNumbersMissing || extra[1].Params["numbers"] != "64, 128" {
		t.Fatalf("extra %+v", extra)
	}
	for _, x := range extra {
		wantCataloged(t, "qa extra", &x)
	}

	// A reason qa did not write (an older text) stays text only.
	old := "数字 64 没有出现在译文中"
	if v := sentenceView(model.Sentence{QAError: &old}, nil); v.QAErrorInfo != nil || *v.QAError != old {
		t.Fatalf("legacy reason %+v", v)
	}
	if v := sentenceView(model.Sentence{}, nil); v.QAErrorInfo != nil {
		t.Fatalf("no reason %+v", v.QAErrorInfo)
	}
}

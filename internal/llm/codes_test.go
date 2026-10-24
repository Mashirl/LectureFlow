// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"errors"
	"fmt"
	"testing"

	"lectureflow/internal/model"
)

// infoOf is the ErrorInfo of err's coded form (zero when it has none), with
// Detail = the text of the error it wraps (as state.EncodeJobError stores it).
func infoOf(err error) model.ErrorInfo {
	var ce *model.CodedError
	if !errors.As(err, &ce) {
		return model.ErrorInfo{}
	}
	info := ce.Info
	if ce.Err != nil {
		info.Detail = ce.Err.Error()
	}
	return info
}

// isCode reports whether err is coded code and has the params kv (pairs of
// name and value, compared as printed: 4 matches 4 and 4.0).
func isCode(err error, code string, kv ...any) bool {
	info := infoOf(err)
	if info.Code != code {
		return false
	}
	for i := 0; i+1 < len(kv); i += 2 {
		v, ok := info.Params[kv[i].(string)]
		if !ok || fmt.Sprint(v) != fmt.Sprint(kv[i+1]) {
			return false
		}
	}
	return true
}

// testCode is the code of a failed TestResult ("" when it has none).
func testCode(r TestResult) string {
	if r.ErrorInfo == nil {
		return ""
	}
	return r.ErrorInfo.Code
}

func TestErrorCodesListed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range ErrorCodes {
		if c == "" || seen[c] {
			t.Fatalf("ErrorCodes: %q empty or repeated", c)
		}
		seen[c] = true
	}
	// A probe error keeps its text (the API caches it) and is coded.
	err := probeErr(CodeProbeHTTP, map[string]any{"status": 401}, "HTTP 401：x")
	if err.Error() != "HTTP 401：x" || !isCode(err, CodeProbeHTTP, "status", 401) || infoOf(err).Detail != "HTTP 401：x" {
		t.Fatalf("probe error %v %+v", err, infoOf(err))
	}
}

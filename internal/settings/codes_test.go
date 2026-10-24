// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"errors"
	"reflect"
	"testing"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// codeOf is the code of err's coded form ("" when it has none).
func codeOf(err error) string {
	var ce *model.CodedError
	if !errors.As(err, &ce) {
		return ""
	}
	return ce.Info.Code
}

// hasCode reports whether err's coded form has code and exactly params.
func hasCode(err error, code string, params map[string]any) bool {
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code != code {
		return false
	}
	if len(params) == 0 && len(ce.Info.Params) == 0 {
		return true
	}
	return reflect.DeepEqual(ce.Info.Params, params)
}

// A user error keeps its Chinese text and its kind, and errors.As finds the
// coded form with the text as the wrapped error (the API's detail).
func TestUserErrorIsCoded(t *testing.T) {
	err := userErr(db.ErrDuplicate, CodeProviderNameTaken, map[string]any{"name": "X"}, "已有名为「%s」的提供商，请换一个名称", "X")
	if err.Error() != "已有名为「X」的提供商，请换一个名称" || !errors.Is(err, db.ErrDuplicate) || errors.Is(err, ErrInvalid) {
		t.Fatalf("text/kind: %v", err)
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Info.Code != CodeProviderNameTaken || ce.Info.Params["name"] != "X" || ce.Err == nil || ce.Err.Error() != err.Error() {
		t.Fatalf("coded form: %+v", ce)
	}
	// Wrapped (as the llm client and the stages do): still found.
	wrapped := errors.Join(errors.New("context"), err)
	if codeOf(wrapped) != CodeProviderNameTaken {
		t.Fatalf("wrapped: %q", codeOf(wrapped))
	}
	// Every code is listed once.
	seen := map[string]bool{}
	for _, c := range ErrorCodes {
		if c == "" || seen[c] {
			t.Fatalf("ErrorCodes: %q empty or repeated", c)
		}
		seen[c] = true
	}
}

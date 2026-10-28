// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import "errors"

// NewFallbackError is an error of the kind Transcribe returns when the
// lecture goes back to the local devices (IsFallback; code and params as
// in codes.go). For fakes of the backend in the callers' tests
// (v14-colab-integrate); the backend itself never needs it.
func NewFallbackError(code string, params map[string]any) error {
	return fallbackErr(code, params, errors.New("the lecture goes back to the local devices"))
}

// NewRequeueError is the error Transcribe returns after "立即关机"
// (IsRequeue, CodeShutdownRequeue); for fakes, like NewFallbackError.
func NewRequeueError() error {
	return newErr(CodeShutdownRequeue, nil, kindRequeue, "", errors.New("the remote runtime was shut down; the lecture goes back to the queue"))
}

// NewBusyError is the error Transcribe returns when the runtime cannot take
// the lecture yet (IsBusy, CodeBusy); for fakes, like NewFallbackError.
func NewBusyError() error {
	return busyErr(errors.New("the remote runtime is busy (being released or started)"))
}

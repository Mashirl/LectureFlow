// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
)

// colabfix (accept2 item 3): importing the Colab authorisation, changing the
// Colab settings, a finished in-app sign-in, "检测授权" and disconnecting run
// the diagnostics' checks again, so the Colab rows follow without "重新检查"
// (a credential change is not part of the environment the page compares).
func TestColabChangesRefreshDiagnostics(t *testing.T) {
	old := envRerunDelay
	envRerunDelay = 10 * time.Millisecond
	t.Cleanup(func() { envRerunDelay = old })
	ce := newColabTestEnv(t, false)
	ce.call("PUT", "/api/asr-mode", map[string]any{"mode": "remote"}, http.StatusOK, nil)

	authCode := func() string {
		var sys System
		ce.call("GET", "/api/system", nil, http.StatusOK, &sys)
		if c, ok := checksByName(sys)[checkColabAuth]; ok && c.DetailInfo != nil {
			return c.DetailInfo.Code
		}
		return ""
	}
	waitAuth := func(what, code string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		got := ""
		for time.Now().Before(deadline) {
			if got = authCode(); got == code {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: colab_auth %q, want %q", what, got, code)
	}
	waitRun := func(what string, before int64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if ce.reach.Load() > before {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: the checks did not run again", what)
	}
	waitAuth("not authorised", codeColabAuthMissing)

	ce.call("POST", "/api/colab/auth/import", map[string]any{"lifecycle": colab.LifecycleManual}, http.StatusOK, nil)
	waitAuth("after the import", codeColabAuthOK)

	n := ce.reach.Load()
	ce.call("PUT", "/api/colab/settings", map[string]any{"accelerator": "L4"}, http.StatusOK, nil)
	waitRun("after the settings changed", n)
	n = ce.reach.Load()
	ce.call("PUT", "/api/colab/settings", map[string]any{"enabled": true}, http.StatusOK, nil)
	waitRun("after Colab was switched on", n)
	n = ce.reach.Load()
	ce.call("POST", "/api/colab/auth/check", nil, http.StatusOK, nil)
	waitRun("after 检测授权", n)

	ce.call("DELETE", "/api/colab/auth", nil, http.StatusOK, nil)
	waitAuth("after the disconnect", codeColabAuthMissing)

	// The in-app sign-in imports the credential itself and then reports
	// LoginDone (login.go); here the import is done directly.
	if _, err := ce.b.ImportADC(context.Background(), colab.Lifecycle{Kind: colab.LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	ce.srv.colabLoginChanged(colab.LoginStatus{State: colab.LoginDone})
	waitAuth("after the sign-in", codeColabAuthOK)
}

// colabfix (accept2 item 2): a queued asr stage that waits with
// remote_fallback (Colab handed it back, no local device) or remote_held
// (colabfix2: a shutdown holds the automatic dispatch, no local device)
// offers "改用 Colab 处理"; nothing else does.
func TestWithUseColab(t *testing.T) {
	asr := model.Job{Stage: model.StageASR, Status: model.StatusQueued}
	cancel := []string{model.ActionCancel}
	got := withUseColab(asr, model.WaitRemoteFallback, cancel)
	if !slices.Equal(got, []string{model.ActionCancel, model.ActionUseColab}) {
		t.Fatalf("remote_fallback: %v", got)
	}
	if len(cancel) != 1 {
		t.Fatalf("the input slice was changed: %v", cancel)
	}
	if again := withUseColab(asr, model.WaitRemoteFallback, got); len(again) != 2 {
		t.Fatalf("added twice: %v", again)
	}
	// colabfix2: held by a shutdown while no local device can take it.
	if got := withUseColab(asr, model.WaitRemoteHeld, cancel); !slices.Equal(got, []string{model.ActionCancel, model.ActionUseColab}) {
		t.Fatalf("remote_held: %v", got)
	}
	for _, c := range []struct {
		j    model.Job
		wait string
	}{
		{asr, model.WaitRemoteBusy},
		{asr, model.WaitPowerNoDevice},
		{asr, ""},
		{model.Job{Stage: model.StageSummary, Status: model.StatusQueued}, model.WaitRemoteFallback},
		{model.Job{Stage: model.StageASR, Status: model.StatusRunning}, model.WaitRemoteFallback},
		{model.Job{Stage: model.StageASR, Status: model.StatusFailed}, model.WaitRemoteFallback},
		{model.Job{Stage: model.StageASR, Status: model.StatusRunning}, model.WaitRemoteHeld},
	} {
		if got := withUseColab(c.j, c.wait, cancel); !slices.Equal(got, cancel) {
			t.Errorf("%s %s / %q: %v", c.j.Stage, c.j.Status, c.wait, got)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestErrorParams: ErrorParams lists every code, and the params each code
// is built with in the source are exactly the listed ones (a scan of the
// newErr / fallbackErr / authErr calls).
func TestErrorParams(t *testing.T) {
	for _, c := range ErrorCodes {
		if _, ok := ErrorParams[c]; !ok {
			t.Errorf("%s has no ErrorParams entry", c)
		}
	}
	if len(ErrorParams) != len(ErrorCodes) {
		t.Errorf("ErrorParams has %d entries, ErrorCodes %d", len(ErrorParams), len(ErrorCodes))
	}
	byName := map[string]string{}
	src, _ := os.ReadFile("codes.go")
	for _, m := range regexp.MustCompile(`(Code\w+)\s*=\s*"(colab\.\w+)"`).FindAllStringSubmatch(string(src), -1) {
		byName[m[1]] = m[2]
	}
	call := regexp.MustCompile(`(?:newErr|fallbackErr|authErr)\((Code\w+),\s*(nil|map\[string\]any\{([^}]*)\})`)
	key := regexp.MustCompile(`"(\w+)":`)
	files, _ := filepath.Glob("*.go")
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range call.FindAllStringSubmatch(string(b), -1) {
			code := byName[m[1]]
			if code == "" {
				t.Errorf("%s: unknown constant %s", f, m[1])
				continue
			}
			seen[code] = true
			var got []string
			for _, k := range key.FindAllStringSubmatch(m[3], -1) {
				got = append(got, k[1])
			}
			want := slices.Clone(ErrorParams[code])
			sort.Strings(got)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s: %s built with params %v, ErrorParams says %v", f, code, got, want)
			}
		}
	}
	seen[CodeAPIChanged] = true      // apiChanged(endpoint, detail)
	seen[CodeLeftoverSession] = true // Status.Leftover {Code, Accelerator}
	for _, c := range ErrorCodes {
		if !seen[c] {
			t.Errorf("%s is never used", c)
		}
	}
}

// TestErrorCodesHaveTexts: web/i18n/{zh-CN,en}.json (the lead merged the
// colab-core parts into them) have
// a text for every code, every waiting_for value, state, step, stage,
// release reason and remote error reason, with placeholders among the
// params the code sends.
func TestErrorCodesHaveTexts(t *testing.T) {
	placeholder := regexp.MustCompile(`\{(\w+)\}`)
	for _, loc := range []string{"zh-CN", "en"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "web", "i18n", loc+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
		cat := map[string]string{}
		for k, v := range raw {
			if s, ok := v.(string); ok {
				cat[k] = s
			}
		}
		for _, c := range ErrorCodes {
			txt, ok := cat["errors."+c]
			if !ok {
				t.Errorf("%s: no text for errors.%s", loc, c)
				continue
			}
			for _, m := range placeholder.FindAllStringSubmatch(txt, -1) {
				if !slices.Contains(ErrorParams[c], m[1]) {
					t.Errorf("%s: errors.%s uses {%s}, not a param", loc, c, m[1])
				}
			}
		}
		var keys []string
		for _, w := range []string{"remote_queue", "remote_window", "remote_auth", "remote_balance"} {
			keys = append(keys, "waiting."+w)
		}
		for _, s := range []string{StateNone, StateQueued, StateBooting, StateRunning, StateStopping, StateReleased} {
			keys = append(keys, "colab.state."+s)
		}
		for _, s := range []string{StepWaitAccelerator, StepUploadAudio, StepRemoteTranscribe, StepDownloadResult, StepVerify} {
			keys = append(keys, "colab.step."+s)
		}
		for _, s := range remoteStages {
			keys = append(keys, "colab.stage."+s)
		}
		for _, s := range []string{ReleaseQueueEmpty, ReleaseIdle, ReleaseCleanupFailed, ReleaseWindow, ReleaseFailed, ReleaseCancelled, ReleaseAfterCurrent,
			ReleaseNow, ReleaseAuthEnding, ReleaseDisconnect, ReleaseAppExit, ReleaseLeftover, ReleaseReclaimed} {
			keys = append(keys, "colab.release."+s)
		}
		for _, s := range remoteErrorCodes {
			keys = append(keys, "colab.remote_reason."+s)
		}
		for _, k := range keys {
			if _, ok := cat[k]; !ok {
				t.Errorf("%s: no text for %s", loc, k)
			}
		}
	}
}

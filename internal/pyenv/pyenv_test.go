// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package pyenv

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestEnvReplacesTheSwitches(t *testing.T) {
	base := []string{"PATH=/bin", "HF_HUB_DISABLE_TELEMETRY=0", "DO_NOT_TRACK=", "=C:=C:\\x", "KEEP=1"}
	got := Env(base, "PYTHONIOENCODING=utf-8")
	want := append([]string{"PATH=/bin", "=C:=C:\\x", "KEEP=1"}, TelemetryOff...)
	want = append(want, "PYTHONIOENCODING=utf-8")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Env:\n got %q\nwant %q", got, want)
	}
	if !Has(got) {
		t.Fatalf("Has(%q) = false", got)
	}
	if Has(base) || Has(nil) {
		t.Fatal("Has accepts an environment without the switches")
	}
	// The last entry of a key wins (os/exec): a later "0" turns it back on.
	if Has(append(slices.Clone(got), "HF_HUB_DISABLE_TELEMETRY=0")) {
		t.Fatal("Has ignores a later entry that turns a switch off")
	}
}

func TestEnvCaseOnWindows(t *testing.T) {
	got := Env([]string{"hf_hub_disable_telemetry=0", "Do_Not_Track=0"})
	lower := slices.Contains(got, "hf_hub_disable_telemetry=0")
	if runtime.GOOS == "windows" && lower {
		t.Fatalf("Windows keys are case-insensitive, the old entry stayed: %q", got)
	}
	if runtime.GOOS != "windows" && !lower {
		t.Fatalf("a different key elsewhere was removed: %q", got)
	}
	if !Has(got) {
		t.Fatalf("Has(%q) = false", got)
	}
}

func TestEnvironKeepsTheProcessEnvironment(t *testing.T) {
	t.Setenv("LF_PYENV_TEST", "kept")
	t.Setenv("HF_HUB_DISABLE_TELEMETRY", "0")
	env := Environ("EXTRA=1")
	if !slices.Contains(env, "LF_PYENV_TEST=kept") || env[len(env)-1] != "EXTRA=1" || !Has(env) {
		t.Fatalf("Environ: %q", env)
	}
}

func TestImportCheck(t *testing.T) {
	got := ImportCheck([]string{"openvino", "openvino_genai"})
	want := "import sys; sys.modules['openvino_telemetry'] = None; import openvino, openvino_genai"
	if got != want {
		t.Fatalf("ImportCheck = %q, want %q", got, want)
	}
}

// The worker scripts and the desktop shell carry the same list (they cannot
// import this package): worker/asr_common.py TELEMETRY_OFF_ENV,
// worker/colab_remote.py (runs on Colab without asr_common at first) and
// desktop/src/main/lib/serverutil.js NO_TELEMETRY_ENV.
func TestSameListInTheWorkerAndTheShell(t *testing.T) {
	root := filepath.Join("..", "..")
	pyEntry := regexp.MustCompile(`^\s*"([A-Z_]+)":\s*"([^"]*)",`)
	jsEntry := regexp.MustCompile(`^\s*([A-Z_]+):\s*'([^']*)',`)
	for _, c := range []struct {
		file, start string
		re          *regexp.Regexp
	}{
		{"worker/asr_common.py", "TELEMETRY_OFF_ENV = {", pyEntry},
		{"worker/colab_remote.py", "TELEMETRY_OFF_ENV = {", pyEntry},
		{"desktop/src/main/lib/serverutil.js", "const NO_TELEMETRY_ENV = Object.freeze({", jsEntry},
	} {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(c.file)))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		in := false
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case !in && strings.HasPrefix(line, c.start):
				in = true
			case in && strings.HasPrefix(strings.TrimSpace(line), "}"):
				in = false
			case in:
				if m := c.re.FindStringSubmatch(line); m != nil {
					got = append(got, m[1]+"="+m[2])
				}
			}
		}
		f.Close()
		if !reflect.DeepEqual(got, TelemetryOff) {
			t.Errorf("%s: %q, want %q (internal/pyenv TelemetryOff)", c.file, got, TelemetryOff)
		}
	}
}

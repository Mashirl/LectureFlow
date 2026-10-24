// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "resources", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ip(v int) *int           { return &v }
func fp64(v float64) *float64 { return &v }

func eqInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqF(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameSample(a, b Sample) bool {
	return a.Name == b.Name && eqInt(a.UtilPct, b.UtilPct) && eqInt(a.MemUsedMB, b.MemUsedMB) &&
		eqInt(a.MemTotalMB, b.MemTotalMB) && eqInt(a.TempC, b.TempC) &&
		eqF(a.PowerW, b.PowerW) && eqF(a.PowerLimitW, b.PowerLimitW)
}

func TestParseQueryFixtures(t *testing.T) {
	cases := []struct {
		file string
		want Sample
	}{
		{"nvidia_smi_normal.txt", Sample{Name: "NVIDIA GeForce RTX 4080", UtilPct: ip(37), MemUsedMB: ip(5120),
			MemTotalMB: ip(16376), TempC: ip(63), PowerW: fp64(112.45), PowerLimitW: fp64(320)}},
		// Recorded on the development laptop: power.limit is not reported.
		{"nvidia_smi_laptop.txt", Sample{Name: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", UtilPct: ip(0), MemUsedMB: ip(41),
			MemTotalMB: ip(12227), TempC: ip(45), PowerW: fp64(5.88)}},
		// Several GPUs: the first line wins.
		{"nvidia_smi_multi_gpu.txt", Sample{Name: "NVIDIA GeForce RTX 4090", UtilPct: ip(91), MemUsedMB: ip(20480),
			MemTotalMB: ip(24564), TempC: ip(86), PowerW: fp64(401.2), PowerLimitW: fp64(450)}},
		// [N/A], [Not Supported], [Unknown Error] and bare N/A become null.
		{"nvidia_smi_na_fields.txt", Sample{Name: "NVIDIA GeForce MX450", MemUsedMB: ip(312), MemTotalMB: ip(2048)}},
	}
	for _, c := range cases {
		got, err := ParseQuery(fixture(t, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if !sameSample(got, c.want) {
			t.Fatalf("%s: got %+v, want %+v", c.file, got, c.want)
		}
	}
}

func TestParseQueryErrors(t *testing.T) {
	for _, file := range []string{
		"nvidia_smi_driver_error.txt",  // driver not loaded
		"nvidia_smi_invalid_field.txt", // old driver without a queried field
		"nvidia_smi_garbled.txt",       // localized shell error in another code page
		"nvidia_smi_empty.txt",
	} {
		_, err := ParseQuery(fixture(t, file))
		if err == nil {
			t.Fatalf("%s: no error", file)
		}
		if !strings.HasPrefix(err.Error(), "nvidia-smi: ") || !utf8.ValidString(err.Error()) {
			t.Fatalf("%s: unreadable error %q", file, err)
		}
	}
	for _, out := range []string{
		"",
		"NVIDIA X, 1, 2",                      // too few columns
		"NVIDIA X, 1, 2, 3, 4, 5, 6, 7",       // too many columns
		"NVIDIA X, abc, 1, 2, 3, 4.0, 5.0",    // not a number, not a placeholder
		"NVIDIA X, 1, 2, 3, 4, 5 W, 6",        // units despite nounits
		"NVIDIA X, 1, 2, 3, 4, NaN, 6",        // NaN is not a reading
		strings.Repeat("x,", 500) + "garbage", // long garbage is quoted short
	} {
		_, err := ParseQuery(out)
		if err == nil {
			t.Fatalf("%q: no error", out)
		}
		if len(err.Error()) > 200 {
			t.Fatalf("error not truncated: %d bytes", len(err.Error()))
		}
	}
}

func TestParseQueryTolerance(t *testing.T) {
	// Leading blank lines, LF endings and an integer column printed as a float.
	got, err := ParseQuery("\n\n  NVIDIA T4 , 37.0, 100, 15360, 50, 70.5, 70.00 \n")
	if err != nil {
		t.Fatal(err)
	}
	want := Sample{Name: "NVIDIA T4", UtilPct: ip(37), MemUsedMB: ip(100), MemTotalMB: ip(15360),
		TempC: ip(50), PowerW: fp64(70.5), PowerLimitW: fp64(70)}
	if !sameSample(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestExecRunMissingExecutable(t *testing.T) {
	if _, err := execRun(context.Background(), filepath.Join(t.TempDir(), "no-such-nvidia-smi")); err == nil {
		t.Fatal("expected an error for a missing executable")
	}
}

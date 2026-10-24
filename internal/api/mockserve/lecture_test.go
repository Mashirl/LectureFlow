// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// The "slow" scenario delays audio clips like every other GET, so the A13
// bar's loading state can be previewed; the clip and its X-Clip-* headers
// are unchanged.
func TestAudioSlowScenario(t *testing.T) {
	srv := testServer(t)
	begin := time.Now()
	res, err := http.Get(srv.URL + "/api/lectures/L1/audio?start_ms=5000&end_ms=7400&mock=slow")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if d := time.Since(begin); d < 1400*time.Millisecond {
		t.Errorf("slow clip answered after %v, want ≥ 1.5 s", d)
	}
	if res.StatusCode != http.StatusOK || string(b[:4]) != "RIFF" {
		t.Fatalf("status %d, not a WAV", res.StatusCode)
	}
	if res.Header.Get("X-Clip-Sentence-Start-Ms") != "5000" || res.Header.Get("X-Clip-End-Ms") != "8400" {
		t.Errorf("clip headers %v", res.Header)
	}

	// The default scenario does not wait.
	begin = time.Now()
	res, err = http.Get(srv.URL + "/api/lectures/L1/audio?start_ms=5000&end_ms=7400")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if d := time.Since(begin); d > time.Second {
		t.Errorf("default clip took %v", d)
	}
}

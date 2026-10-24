// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"encoding/json"
	"sync"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/tariff"
)

// The unpriced-request counter (D3, lead ruling at the phase-2 gate): kv
// usage_unpriced:{provider}:{day}, per provider and local day.
func TestUnpricedRequestCounter(t *testing.T) {
	st := openStore(t)
	if n, err := UnpricedRequests(bg(), st, "p1", "2026-10-23"); err != nil || n != 0 {
		t.Fatalf("absent: %d %v", n, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := AddUnpricedRequest(bg(), st, "p1", "2026-10-23"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := AddUnpricedRequest(bg(), st, "p2", "2026-10-23"); err != nil {
		t.Fatal(err)
	}
	if err := AddUnpricedRequest(bg(), st, "p1", "2026-10-24"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		provider, day string
		want          int64
	}{{"p1", "2026-10-23", 20}, {"p2", "2026-10-23", 1}, {"p1", "2026-10-24", 1}, {"p2", "2026-10-24", 0}} {
		if n, err := UnpricedRequests(bg(), st, c.provider, c.day); err != nil || n != c.want {
			t.Errorf("%s %s: %d %v, want %d", c.provider, c.day, n, err, c.want)
		}
	}
	if v, ok, _ := st.GetKV(bg(), "usage_unpriced:p1:2026-10-23"); !ok || v != "20" {
		t.Errorf("kv %q %v", v, ok)
	}
	// an unreadable value counts as 0 and is overwritten by the next request
	st.SetKV(bg(), UnpricedKey("p3", "2026-10-23"), "x")
	if n, _ := UnpricedRequests(bg(), st, "p3", "2026-10-23"); n != 0 {
		t.Errorf("unreadable value: %d", n)
	}
	AddUnpricedRequest(bg(), st, "p3", "2026-10-23")
	if n, _ := UnpricedRequests(bg(), st, "p3", "2026-10-23"); n != 1 {
		t.Errorf("after an unreadable value: %d", n)
	}
}

// DisplayCost: the amount shown for a provider's day of usage (P12.5, D3).
func TestDisplayCost(t *testing.T) {
	in, hit, out := 2.0, 0.5, 8.0
	priced := model.Provider{ID: "p", PriceInput: &in, PriceCacheHit: &hit, PriceOutput: &out}
	preset, err := json.Marshal(tariff.DeepSeekPreset())
	if err != nil {
		t.Fatal(err)
	}
	raw := string(preset)
	modelsOnly := model.Provider{ID: "m", Tariff: &raw} // prices for some models only (D3)
	if !HasAnyPrice(modelsOnly) || HasAnyPrice(model.Provider{ID: "f"}) {
		t.Fatal("HasAnyPrice")
	}
	free := model.Provider{ID: "f"}
	for _, c := range []struct {
		name               string
		p                  *model.Provider
		requests, unpriced int64
		cost               float64
		wantNil            bool
		wantCost           float64
		wantUnpriced       int64
	}{
		{"priced, no requests", &priced, 0, 0, 0, false, 0, 0},
		{"priced, all priced", &priced, 3, 0, 1.5, false, 1.5, 0},
		{"models only, some unpriced", &modelsOnly, 5, 2, 0.7, false, 0.7, 2},
		{"models only, all unpriced", &modelsOnly, 4, 4, 0, true, 0, 4},
		{"count above the requests", &modelsOnly, 3, 5, 0, true, 0, 3},
		{"no price at all", &free, 3, 0, 0, true, 0, 3}, // all unpriced by definition (requests before the counter too)
		{"no price, no requests", &free, 0, 0, 0, true, 0, 0},
		{"deleted provider", nil, 2, 1, 0.3, true, 0, 1},
	} {
		got, n := DisplayCost(c.p, c.requests, c.unpriced, c.cost)
		if (got == nil) != c.wantNil || (got != nil && *got != c.wantCost) || n != c.wantUnpriced {
			t.Errorf("%s: %v %d, want nil=%v %v %d", c.name, got, n, c.wantNil, c.wantCost, c.wantUnpriced)
		}
	}
}

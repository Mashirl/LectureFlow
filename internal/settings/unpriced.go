// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Unpriced requests (appendix D3 "都没有时只统计 token，不显示金额"; lead
// ruling at the phase-2 gate, §0.1 "成本可见"). A request whose model has
// no price in the provider's tariff and whose provider has no price of its
// own is counted with its tokens in usage_daily at cost 0, which on its own
// cannot be told from a free request. The API pool therefore also counts
// such requests per provider and local day, in kv (no migration):
// usage_unpriced:{provider_id}:{day} = the count. The resource bar and
// "今日用量" show the count (cost_unpriced_requests: "部分请求未计价") and no
// amount when every request of the day was unpriced (DisplayCost); the
// daily budget may then be underestimated, which the stages' notes say.

// KVUsageUnpricedPrefix is the kv prefix of the counters.
const KVUsageUnpricedPrefix = "usage_unpriced:"

// UnpricedKey is the kv key of a provider's counter for day (the local
// date "YYYY-MM-DD" of usage_daily).
func UnpricedKey(providerID, day string) string {
	return KVUsageUnpricedPrefix + providerID + ":" + day
}

// unpricedMu serialises the read-modify-write of the counters: kv has no
// atomic increment, and the counters are written by this process only.
// (Not a transaction: upgrading a read transaction to a write one can fail
// with SQLITE_BUSY when another write commits in between.)
var unpricedMu sync.Mutex

// UnpricedRequests reads a provider's counter for day; 0 when absent or
// unreadable.
func UnpricedRequests(ctx context.Context, st db.Store, providerID, day string) (int64, error) {
	v, ok, err := st.GetKV(ctx, UnpricedKey(providerID, day))
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0, nil
	}
	return n, nil
}

// AddUnpricedRequest adds one unpriced request to a provider's counter for
// day.
func AddUnpricedRequest(ctx context.Context, st db.Store, providerID, day string) error {
	unpricedMu.Lock()
	defer unpricedMu.Unlock()
	n, err := UnpricedRequests(ctx, st, providerID, day)
	if err != nil {
		return err
	}
	return st.SetKV(ctx, UnpricedKey(providerID, day), strconv.FormatInt(n+1, 10))
}

// DisplayCost is the estimated cost shown for one provider's usage of a day
// (P12.5, D3): nil — only tokens are shown — when the provider has no price
// at all (HasAnyPrice; also a deleted provider, p nil) or when every one of
// the day's requests was unpriced; otherwise the accumulated estimate cost
// (0 for a priced provider without requests). It also returns the number
// of the day's requests without a price (cost_unpriced_requests): the
// counter, at most requests; for a provider without any price all its
// requests, including those recorded before the counter existed.
func DisplayCost(p *model.Provider, requests, unpriced int64, cost float64) (*float64, int64) {
	unpriced = max(min(unpriced, requests), 0)
	if p != nil && !HasAnyPrice(*p) {
		return nil, max(requests, 0)
	}
	if p == nil || (requests > 0 && unpriced >= requests) {
		return nil, unpriced
	}
	c := cost
	return &c, unpriced
}

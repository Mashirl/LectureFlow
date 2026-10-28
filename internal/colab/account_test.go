// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// colab-obs-core: the account (ccu-info's lenient fields, the level, which
// accelerators unlock, a locked pick, the free quota, when it is read).

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCCUInfoLenientFields(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, raw        string
		eligible, inelig []string
		known            bool
		quota            float64 // -1 none
		refill           int64   // 0 none
		unknownLogged    bool
		wantCode         string
		balance, rate    float64
	}{
		{name: "as read on the paid account (2026-10-27)",
			raw: `{"currentBalance":297.7909,"consumptionRateHourly":0,"assignmentsCount":0,"eligibleGpus":["G4","A100","L4","T4"],` +
				`"ineligibleGpus":["H100"],"eligibleTpus":["V6E1","V5E1"]}`,
			eligible: []string{"G4", "A100", "L4", "T4"}, inelig: []string{"H100"}, known: true, quota: -1, balance: 297.7909},
		{name: "a free account (colab-vscode's schema)",
			raw:      `{"currentBalance":0,"consumptionRateHourly":0.07,"assignmentsCount":0,"eligibleGpus":["T4"],"freeCcuQuotaInfo":{"remainingTokens":"1520","nextRefillTimestampSec":1793300000}}`,
			eligible: []string{"T4"}, inelig: []string{}, known: true, quota: 1.52, refill: 1793300000, rate: 0.07},
		{name: "the newer fields missing", raw: `{"currentBalance":5,"consumptionRateHourly":1.07,"assignmentsCount":1}`,
			quota: -1, balance: 5, rate: 1.07},
		{name: "odd values are unknown, never an API change",
			raw: `{"currentBalance":"5.5","consumptionRateHourly":0,"assignmentsCount":0,"eligibleGpus":"T4","ineligibleGpus":[3,"Z9","h100"],` +
				`"freeCcuQuotaInfo":{"remainingTokens":"lots","nextRefillTimestampSec":-4}}`,
			inelig: []string{"H100"}, quota: -1, unknownLogged: true, balance: 5.5},
		{name: "a TPU in the list is not a GPU", raw: `{"currentBalance":1,"consumptionRateHourly":0,"assignmentsCount":0,"eligibleGpus":["V5E1","t4","NEW9"]}`,
			eligible: []string{"T4"}, known: true, quota: -1, unknownLogged: true, balance: 1},
		{name: "the old fields stay strict", raw: `{"consumptionRateHourly":0,"assignmentsCount":0,"eligibleGpus":["T4"]}`, wantCode: CodeAPIChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeEnv(t, t4())
			e.f.ccuRaw = tc.raw
			info, err := e.b.tun.ccuInfo(ctx)
			if Code(err) != tc.wantCode {
				t.Fatalf("err %v", err)
			}
			if tc.wantCode != "" {
				return
			}
			if info.Balance != tc.balance || info.RatePerHour != tc.rate || info.EligibilityKnown != tc.known ||
				!slices.Equal(info.EligibleGPUs, tc.eligible) || !slices.Equal(info.IneligibleGPUs, tc.inelig) {
				t.Errorf("info %+v", info)
			}
			if tc.quota < 0 && info.FreeQuotaCU != nil || tc.quota >= 0 && (info.FreeQuotaCU == nil || *info.FreeQuotaCU != tc.quota) {
				t.Errorf("quota %v", info.FreeQuotaCU)
			}
			if tc.refill == 0 && info.FreeRefillAt != nil || tc.refill != 0 && (info.FreeRefillAt == nil || info.FreeRefillAt.Unix() != tc.refill) {
				t.Errorf("refill %v", info.FreeRefillAt)
			}
			if logged := strings.Contains(e.log.String(), "does not know; ignored"); logged != tc.unknownLogged {
				t.Errorf("unknown logged %v: %s", logged, e.log.String())
			}
		})
	}
}

func TestAccountSelection(t *testing.T) {
	paid := func(eligible ...string) *ccuSnapshot {
		return &ccuSnapshot{info: ccuInfo{Balance: 50, EligibleGPUs: eligible, EligibilityKnown: eligible != nil}}
	}
	q := 1.5
	pick := func(a string) Settings {
		s := DefaultSettings()
		s.Accelerator, s.AcceleratorChosen = a, true
		return s
	}
	for _, tc := range []struct {
		name                       string
		set                        Settings
		c                          *ccuSnapshot
		level, preferred, eff, why string
		selectable                 []string
	}{
		{"unread, never picked: the free level's T4", DefaultSettings(), nil, LevelFree, "T4", "T4", "", []string{"T4"}},
		{"unread, L4 picked: kept, a T4 borrowed", pick("L4"), nil, LevelFree, "L4", "T4", LockAccountUnknown, []string{"T4"}},
		{"paid, never picked: the L4", DefaultSettings(), paid("G4", "A100", "L4", "T4"), LevelPaid, "L4", "L4", "", []string{"T4", "L4", "G4", "A100"}},
		{"paid, H100 picked but not eligible", pick("H100"), paid("G4", "A100", "L4", "T4"), LevelPaid, "H100", "T4", LockIneligible, []string{"T4", "L4", "G4", "A100"}},
		{"paid, eligibleGpus missing: only T4", pick("A100"), paid(), LevelPaid, "A100", "T4", LockAccountUnknown, []string{"T4"}},
		{"no paid balance: only T4", pick("L4"), &ccuSnapshot{info: ccuInfo{EligibleGPUs: []string{"L4", "T4"}, EligibilityKnown: true}}, LevelFree, "L4", "T4", LockNoBalance, []string{"T4"}},
		{"no paid balance, a free quota: T4 is fine", pick("T4"), &ccuSnapshot{info: ccuInfo{FreeQuotaCU: &q}}, LevelFree, "T4", "T4", "", []string{"T4"}},
		{"T4 always selectable even when not listed", pick("T4"), paid("L4"), LevelPaid, "T4", "T4", "", []string{"T4", "L4"}},
	} {
		sel := selectionOf(tc.set, tc.c)
		if sel.level != tc.level || sel.preferred != tc.preferred || sel.effective != tc.eff || sel.locked != tc.why ||
			!slices.Equal(sel.selectable, tc.selectable) {
			t.Errorf("%s: %+v", tc.name, sel)
		}
	}
}

// A picked L4 locked while the paid balance is 0 (a T4 is borrowed, the pick
// kept), back to the L4 once the balance is there again.
func TestLockedPickBorrowsT4UntilTheBalanceReturns(t *testing.T) {
	set := DefaultSettings()
	set.Accelerator, set.AcceleratorChosen = "L4", true
	e := newFakeEnv(t, set)
	e.f.balance, e.f.freeTokens = 0, "800"
	ctx := context.Background()
	if ok, w := e.b.CanDispatch(ctx, 600); !ok {
		t.Fatalf("CanDispatch on the free quota: %q", w)
	}
	st := e.b.Status(ctx)
	if st.Account.Level != LevelFree || !st.Account.ReadOK || st.Account.FreeQuotaCU == nil || *st.Account.FreeQuotaCU != 0.8 ||
		st.EffectiveAccelerator != "T4" || st.LockedReason != LockNoBalance || st.Accelerator != "T4" || st.Tier != TierFree ||
		st.PreferredAccelerator != "L4" ||
		st.Account.FreeRefillAt == nil || !slices.Equal(st.Account.EligibleGPUs, []string{"G4", "A100", "L4", "T4"}) {
		t.Fatalf("status %+v", st)
	}
	if u := e.b.UserSettings(); u.Accelerator != "L4" || !u.AcceleratorChosen {
		t.Errorf("the pick was not kept: %+v", u)
	}
	if s := e.b.Settings(); s.Accelerator != "T4" || s.Tier != TierFree || SlotsFor(s) != 1 {
		t.Errorf("settings in effect %+v", s)
	}
	a := e.lecture(t, "a")
	if _, err := e.b.Transcribe(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	if e.f.accelerator != "T4" {
		t.Errorf("borrowed %q", e.f.accelerator)
	}
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatal(err)
	}
	// The balance is back: within the minute the reading stays, then the L4.
	e.f.mu.Lock()
	e.f.balance = 20
	e.f.mu.Unlock()
	e.b.CanDispatch(ctx, 600)
	if st := e.b.Status(ctx); st.LockedReason != LockNoBalance {
		t.Errorf("read again within a minute: %+v", st.Account)
	}
	e.clk.Advance(61 * time.Second)
	if ok, w := e.b.CanDispatch(ctx, 600); !ok {
		t.Fatalf("CanDispatch: %q", w)
	}
	if st := e.b.Status(ctx); st.LockedReason != "" || st.EffectiveAccelerator != "L4" || st.Account.Level != LevelPaid || st.Slots != 2 {
		t.Fatalf("after the balance came back: %+v", st)
	}
	b := e.lecture(t, "b")
	if _, err := e.b.Transcribe(ctx, b, nil); err != nil {
		t.Fatal(err)
	}
	if e.f.accelerator != "L4" {
		t.Errorf("borrowed %q after the balance came back", e.f.accelerator)
	}
	_ = e.b.QueueDrained(ctx)
	// The release keeps the account.
	if st := e.b.Status(ctx); st.Account.ReadAt == nil || st.Machine != nil || st.Usage != nil {
		t.Errorf("after the release: %+v", st)
	}
	e.checkSecrets(t)
}

// The free tier, or an account that cannot be read, still borrows a T4
// (best effort); the free quota is what "还能再处理约 N 节" counts.
func TestFreeTierAndUnreadAccountBorrowT4(t *testing.T) {
	ctx := context.Background()
	t.Run("free quota", func(t *testing.T) {
		e := newFakeEnv(t, DefaultSettings())
		e.f.balance, e.f.freeTokens = 0, "10" // 0.01 CU: not even one lecture, still best effort
		if ok, w := e.b.CanDispatch(ctx, 3000); !ok {
			t.Fatalf("CanDispatch %q", w)
		}
		st := e.b.Status(ctx)
		if st.Tier != TierFree || st.EffectiveAccelerator != "T4" || st.LockedReason != "" || st.LecturesLeft != 0 {
			t.Errorf("status %+v", st)
		}
		e.f.mu.Lock()
		e.f.freeTokens = "1500"
		e.f.mu.Unlock()
		e.clk.Advance(2 * time.Minute)
		e.b.CanDispatch(ctx, 3000)
		// 1.5 CU at 1.07 CU/h, a T4's 50-minute lecture: 300 s (+60 s setup once) ≈ 0.09 CU each
		if n := e.b.Status(ctx).LecturesLeft; n < 10 || n > 20 {
			t.Errorf("lectures left on the free quota: %d", n)
		}
		e.f.mu.Lock()
		e.f.freeTokens = ""
		e.f.mu.Unlock()
		e.clk.Advance(2 * time.Minute)
		e.b.CanDispatch(ctx, 3000)
		// no quota reported: the window only (as before)
		if n := e.b.Status(ctx).LecturesLeft; n != 47 {
			t.Errorf("lectures left without a quota: %d", n)
		}
	})
	t.Run("transient failure", func(t *testing.T) {
		e := newFakeEnv(t, DefaultSettings())
		e.f.ccuStatus = 503
		if ok, w := e.b.CanDispatch(ctx, 600); !ok {
			t.Fatalf("CanDispatch with ccu-info failing transiently: %q", w)
		}
		st := e.b.Status(ctx)
		if st.Account.ReadOK || st.Account.ReadError == nil || st.Account.ReadError.Code != CodeAccountUnavailable ||
			st.Account.ReadError.Params["reason"] != "unavailable" || st.Account.Level != LevelFree ||
			st.EffectiveAccelerator != "T4" || st.LastError != nil {
			t.Errorf("status %+v", st)
		}
		// Within the minute: not read again.
		n := e.f.count(func() int { return e.f.ccuReads })
		e.b.CanDispatch(ctx, 600)
		if m := e.f.count(func() int { return e.f.ccuReads }); m != n {
			t.Errorf("read again within a minute: %d → %d", n, m)
		}
		a := e.lecture(t, "a")
		if _, err := e.b.Transcribe(ctx, a, nil); err != nil {
			t.Fatalf("Transcribe: %v", err)
		}
		if e.f.accelerator != "T4" {
			t.Errorf("borrowed %q", e.f.accelerator)
		}
		_ = e.b.QueueDrained(ctx)
	})
	t.Run("the answer changed: loud", func(t *testing.T) {
		e := newFakeEnv(t, DefaultSettings())
		e.f.ccuRaw = `{"balance":3}`
		if ok, w := e.b.CanDispatch(ctx, 600); ok || w != "" {
			t.Fatalf("CanDispatch %v %q", ok, w)
		}
		if st := e.b.Status(ctx); st.LastError == nil || st.LastError.Code != CodeAPIChanged || st.LastErrorAt == nil ||
			st.Account.ReadError == nil || st.Account.ReadError.Code != CodeAccountUnavailable || st.Account.ReadError.Params["reason"] != "api_changed" {
			t.Errorf("status %+v", st)
		}
	})
}

func TestAccountReadingTimes(t *testing.T) {
	ctx := context.Background()
	var changes atomic.Int32
	enabled := atomic.Bool{}
	e := newFakeEnv(t, DefaultSettings(), func(o *Options) {
		o.OnChange = func() { changes.Add(1) }
		o.Enabled = func(context.Context) bool { return enabled.Load() }
	})
	reads := func() int { return e.f.count(func() int { return e.f.ccuReads }) }
	settle := func(want int) {
		t.Helper()
		e.waitFor(t, "the background reading", func() bool {
			e.b.mu.Lock()
			defer e.b.mu.Unlock()
			return !e.b.acctBusy && reads() == want
		})
	}
	// Status never reads.
	e.b.Status(ctx)
	if reads() != 0 {
		t.Fatal("Status read the account")
	}
	// A viewer: never read → in the background, OnChange follows.
	e.b.TouchAccount(ctx)
	settle(1)
	if changes.Load() != 1 || e.b.Status(ctx).Account.Level != LevelPaid {
		t.Errorf("changes %d, %+v", changes.Load(), e.b.Status(ctx).Account)
	}
	// Within five minutes a viewer reads nothing; after, again (the same
	// answer: no change to report).
	e.clk.Advance(4 * time.Minute)
	e.b.TouchAccount(ctx)
	time.Sleep(20 * time.Millisecond)
	settle(1)
	e.clk.Advance(2 * time.Minute)
	e.b.TouchAccount(ctx)
	settle(2)
	if changes.Load() != 1 {
		t.Errorf("an unchanged reading was reported: %d", changes.Load())
	}
	// Run's five-minute reading only while Colab is on.
	e.clk.Advance(6 * time.Minute)
	e.b.accountTick(ctx)
	time.Sleep(20 * time.Millisecond)
	settle(2)
	enabled.Store(true)
	e.b.accountTick(ctx)
	settle(3)
	// An import or sign-in reads at once; "重新查询" synchronously.
	e.f.mu.Lock()
	e.f.balance = 0
	e.f.mu.Unlock()
	e.b.RefreshAccountSoon(ctx)
	settle(4)
	if a := e.b.RefreshAccount(ctx); reads() != 5 || a.Level != LevelFree || !a.ReadOK {
		t.Errorf("refresh: %+v (%d reads)", a, reads())
	}
	if changes.Load() < 2 {
		t.Errorf("the level change was not reported")
	}
	// Without a credential nothing is read.
	_ = e.kv.SetKV(ctx, KVCredential, "")
	e.clk.Advance(10 * time.Minute)
	e.b.TouchAccount(ctx)
	e.b.RefreshAccount(ctx)
	time.Sleep(20 * time.Millisecond)
	if reads() != 5 {
		t.Errorf("read without a credential: %d", reads())
	}
	// The account JSON: level paid or free only, no subscription field.
	js, _ := json.Marshal(e.b.Status(ctx))
	if strings.Contains(string(js), "subscription") || !strings.Contains(string(js), `"account":{"read_at":`) {
		t.Errorf("JSON %s", js)
	}
}

// The tier stored by an older version, or sent by an older front end, is
// ignored: the account decides.
func TestStoredTierIgnored(t *testing.T) {
	ctx := context.Background()
	set := DefaultSettings()
	set.Tier = TierFree
	e := newFakeEnv(t, set)
	e.b.RefreshAccount(ctx) // 95.5 CU paid
	if s := e.b.Settings(); s.Tier != TierPaid || s.Accelerator != "L4" {
		t.Errorf("settings %+v", s)
	}
	// An import forgets the reading (perhaps another Google account).
	e.b.forgetAccount()
	if st := e.b.Status(ctx); st.Account.ReadAt != nil || st.Tier != TierFree {
		t.Errorf("forgotten: %+v", st.Account)
	}
}

// The account's own error code: never a lecture's (which would say the
// lecture went back to the local devices); a credential problem keeps its
// colab.auth_* code.
func TestAccountErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err          error
		code, reason string
	}{
		{fallbackErr(CodeUnavailable, map[string]any{"endpoint": "tun/m/ccu-info"}, nil), CodeAccountUnavailable, "unavailable"},
		{apiChanged("tun/m/ccu-info", "field currentBalance missing"), CodeAccountUnavailable, "api_changed"},
		{context.DeadlineExceeded, CodeAccountUnavailable, "other"},
		{authErr(CodeAuthRejected, map[string]any{"reason": "http_401"}, "x"), CodeAuthRejected, ""},
	} {
		info := infoOf(accountErr(tc.err))
		if info.Code != tc.code || tc.reason != "" && info.Params["reason"] != tc.reason {
			t.Errorf("%v → %+v", tc.err, info)
		}
	}
}

// A free account, recorded with the user's consent (2026-10-27, read-only:
// a token refresh and ccu-info): no paid balance, only T4 eligible, and —
// unlike colab-vscode's older schema — no freeCcuQuotaInfo at all. The level
// is free, only T4 is selectable, a picked L4 is locked (no_balance), and a
// T4 lecture is dispatched on a best-effort basis; with the quota unknown
// "还能再处理约 N 节" counts the window only.
func TestReplayFreeAccount(t *testing.T) {
	e := newEnv(t, "ccu_free_account.json").withCredential(t).startup(t)
	ctx := context.Background()
	a := e.b.RefreshAccount(ctx)
	if a.Level != LevelFree || !a.ReadOK || a.PaidBalanceCU == nil || *a.PaidBalanceCU != 0 || a.FreeQuotaCU != nil ||
		a.FreeRefillAt != nil || !a.EligibilityKnown || !slices.Equal(a.EligibleGPUs, []string{"T4"}) ||
		!slices.Equal(a.IneligibleGPUs, []string{"H100", "G4", "A100", "L4"}) {
		t.Fatalf("account %+v", a)
	}
	st := e.b.Status(ctx)
	if !slices.Equal(st.SelectableAccelerators, []string{"T4"}) || st.EffectiveAccelerator != "T4" || st.LockedReason != "" ||
		st.Tier != TierFree || st.LecturesLeft != 47 {
		t.Errorf("status %+v", st)
	}
	set := e.b.UserSettings()
	set.Accelerator, set.AcceleratorChosen = "L4", true
	if err := e.b.SetSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	st = e.b.Status(ctx)
	if st.PreferredAccelerator != "L4" || st.EffectiveAccelerator != "T4" || st.LockedReason != LockNoBalance {
		t.Errorf("a picked L4 on the free account: %+v", st)
	}
	// Within the minute nothing more is read; a T4 lecture may go.
	if ok, w := e.b.CanDispatch(ctx, 600); !ok {
		t.Errorf("CanDispatch on the free account: %q", w)
	}
	e.rs.finish()
	e.checkClean(t)
}

// GET /tun/m/assign's "p" (Colab wants a reCAPTCHA) is logged; the borrowing
// goes on as Colab decides.
func TestAssignRecaptchaFlagLogged(t *testing.T) {
	e := newFakeEnv(t, t4())
	ctx := context.Background()
	if _, _, _, err := e.b.tun.getAssign(ctx, newNotebookHash(), "T4", "", true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.log.String(), "reCAPTCHA") {
		t.Errorf("logged without p: %s", e.log.String())
	}
	e.f.mu.Lock()
	e.f.assignP = true
	e.f.mu.Unlock()
	if _, _, _, err := e.b.tun.getAssign(ctx, newNotebookHash(), "T4", "", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log.String(), "asks for a reCAPTCHA") {
		t.Errorf("p=true not logged: %s", e.log.String())
	}
}

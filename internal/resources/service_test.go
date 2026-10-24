// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// --- fakes ---

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeSMI answers nvidia-smi calls with out(n) for the n-th call (0-based).
type fakeSMI struct {
	mu    sync.Mutex
	calls int
	name  string
	args  []string
	out   func(n int) (string, error)
}

func (f *fakeSMI) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	n := f.calls
	f.calls++
	f.name, f.args = name, args
	out := f.out
	f.mu.Unlock()
	if out == nil {
		return "NVIDIA Test GPU, 10, 1000, 16376, 50, 30.5, 100.00\n", nil
	}
	return out(n)
}

func (f *fakeSMI) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// manualTicker replaces the real ticker in loop tests.
type manualTicker struct {
	mu      sync.Mutex
	c       chan time.Time
	created int
	stopped int
}

func (m *manualTicker) New(time.Duration) (<-chan time.Time, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created++
	m.c = make(chan time.Time, 1)
	return m.c, func() {
		m.mu.Lock()
		m.stopped++
		m.mu.Unlock()
	}
}

// Fire delivers one tick to the current ticker; false when there is none.
func (m *manualTicker) Fire() bool {
	m.mu.Lock()
	c := m.c
	m.mu.Unlock()
	if c == nil {
		return false
	}
	select {
	case c <- time.Now():
		return true
	default:
		return false
	}
}

func (m *manualTicker) counts() (created, stopped int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.created, m.stopped
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// sampleNow takes one sample as the loop would, synchronously.
func sampleNow(s *Service) {
	s.sampleMu.Lock()
	s.sampleLocked(context.Background())
	s.sampleMu.Unlock()
}

// --- GPU ---

func TestSnapshotGPUFields(t *testing.T) {
	smi := &fakeSMI{out: func(int) (string, error) {
		return "NVIDIA GeForce RTX 4080, 37, 5120, 16376, 85, 112.45, 320.00\r\n", nil
	}}
	clk := newClock()
	s := New(Deps{
		RunCmd: smi.Run, Now: clk.Now, NvidiaSMI: `C:\tools\nvidia-smi.exe`,
		ASR:   func() model.ASRSettings { return model.ASRSettings{Device: "cuda", ComputeType: "auto"} },
		Slots: func() (int, int, bool) { return 1, 2, true },
	})
	snap := s.Snapshot(context.Background())
	g := snap.GPU
	if !g.Available || g.Error != "" || g.Name != "NVIDIA GeForce RTX 4080" {
		t.Fatalf("gpu %+v", g)
	}
	if *g.UtilPct != 37 || *g.MemUsedMB != 5120 || *g.MemTotalMB != 16376 || *g.TempC != 85 ||
		*g.PowerW != 112.45 || *g.PowerLimitW != 320 {
		t.Fatalf("values %+v", g)
	}
	if !g.TempWarn {
		t.Fatal("85 °C must warn")
	}
	if g.Backend != "CUDA · float16" {
		t.Fatalf("backend %q", g.Backend)
	}
	if g.ASRSlotsUsed != 1 || g.ASRSlotsMax != 2 || !g.Paused {
		t.Fatalf("slots %+v", g)
	}
	if snap.SampledAt != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("sampled_at %q", snap.SampledAt)
	}
	if smi.name != `C:\tools\nvidia-smi.exe` || strings.Join(smi.args, " ") != strings.Join(QueryArgs, " ") {
		t.Fatalf("command %q %q", smi.name, smi.args)
	}
	if QueryArgs[0] != "--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit" ||
		QueryArgs[1] != "--format=csv,noheader,nounits" {
		t.Fatalf("query args %q", QueryArgs)
	}
}

func TestSnapshotTempWarnThreshold(t *testing.T) {
	smi := &fakeSMI{out: func(int) (string, error) { return "G, 1, 1, 8192, 84, 1, 1", nil }}
	s := New(Deps{RunCmd: smi.Run, Now: newClock().Now})
	if s.Snapshot(context.Background()).GPU.TempWarn {
		t.Fatal("84 °C must not warn")
	}
}

func TestSnapshotGPUUnavailable(t *testing.T) {
	for name, out := range map[string]func(int) (string, error){
		"exec error": func(int) (string, error) {
			return "", errors.New(`exec: "nvidia-smi": executable file not found in %PATH%`)
		},
		"driver error": func(int) (string, error) {
			return "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.", nil
		},
	} {
		s := New(Deps{RunCmd: (&fakeSMI{out: out}).Run, Now: newClock().Now})
		g := s.Snapshot(context.Background()).GPU
		if g.Available || g.Error == "" || !strings.HasPrefix(g.Error, "nvidia-smi: ") {
			t.Fatalf("%s: %+v", name, g)
		}
		if g.UtilPct != nil || g.MemUsedMB != nil || g.MemTotalMB != nil || g.TempC != nil ||
			g.PowerW != nil || g.PowerLimitW != nil || g.TempWarn {
			t.Fatalf("%s: pointer fields must be null: %+v", name, g)
		}
		if g.Backend != "CUDA · int8_float16" {
			t.Fatalf("%s: backend %q", name, g.Backend)
		}
		b, _ := json.Marshal(g)
		for _, k := range []string{`"util_pct":null`, `"mem_total_mb":null`, `"power_limit_w":null`, `"util_history":[]`, `"temp_history":[]`} {
			if !strings.Contains(string(b), k) {
				t.Fatalf("%s: %s missing in %s", name, k, b)
			}
		}
	}
}

func TestBackend(t *testing.T) {
	cases := []struct {
		asr     model.ASRSettings
		totalMB int
		want    string
	}{
		{model.ASRSettings{Device: "cpu", ComputeType: "auto"}, 16000, "CPU"},
		{model.ASRSettings{Device: "cpu", ComputeType: "float16"}, 0, "CPU"},
		{model.ASRSettings{Device: "cuda", ComputeType: "auto"}, 10 * 1024, "CUDA · float16"},
		{model.ASRSettings{Device: "cuda", ComputeType: "auto"}, 10*1024 - 1, "CUDA · int8_float16"},
		{model.ASRSettings{Device: "cuda", ComputeType: "auto"}, 0, "CUDA · int8_float16"},
		{model.ASRSettings{Device: "cuda", ComputeType: "int8_float16"}, 24000, "CUDA · int8_float16"},
		{model.ASRSettings{Device: "cuda", ComputeType: "float16"}, 4000, "CUDA · float16"},
	}
	for _, c := range cases {
		total := c.totalMB
		smi := &fakeSMI{out: func(int) (string, error) { return fmt.Sprintf("G, 1, 1, %d, 40, 1, 1", total), nil }}
		asr := c.asr
		s := New(Deps{RunCmd: smi.Run, Now: newClock().Now, ASR: func() model.ASRSettings { return asr }})
		if got := s.Snapshot(context.Background()).GPU.Backend; got != c.want {
			t.Fatalf("%+v total %d: got %q want %q", c.asr, c.totalMB, got, c.want)
		}
	}
}

func TestBackendKeepsLastKnownTotal(t *testing.T) {
	// A transient nvidia-smi failure must not flip the auto compute type.
	smi := &fakeSMI{out: func(n int) (string, error) {
		if n == 0 {
			return "G, 1, 1, 16376, 40, 1, 1", nil
		}
		return "", errors.New("exit status 9")
	}}
	clk := newClock()
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now,
		ASR: func() model.ASRSettings { return model.ASRSettings{Device: "cuda", ComputeType: "auto"} }})
	sampleNow(s)
	sampleNow(s)
	g := s.Snapshot(context.Background()).GPU
	if g.Available || g.Backend != "CUDA · float16" {
		t.Fatalf("%+v", g)
	}
}

func TestHistoryWindow(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{out: func(n int) (string, error) {
		return fmt.Sprintf("G, %d, 100, 8192, %d, 10, 20", n, 40+n), nil
	}}
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now})
	for i := 0; i < 40; i++ {
		if i > 0 {
			clk.Advance(2 * time.Second)
		}
		sampleNow(s)
	}
	g := s.build(context.Background()).GPU
	if len(g.UtilHistory) != HistoryMax || len(g.TempHistory) != HistoryMax {
		t.Fatalf("history lengths %d %d", len(g.UtilHistory), len(g.TempHistory))
	}
	for i := 0; i < HistoryMax; i++ { // oldest first: samples 10..39
		if g.UtilHistory[i] != 10+i || g.TempHistory[i] != 50+i {
			t.Fatalf("history[%d] = %d / %d", i, g.UtilHistory[i], g.TempHistory[i])
		}
	}
	// 31 s without samples: only points younger than 60 s remain
	// (sample i was taken at 2i s; now = 78 + 31 = 109 s → i ≥ 25).
	clk.Advance(31 * time.Second)
	g = s.build(context.Background()).GPU
	if len(g.UtilHistory) != 15 || g.UtilHistory[0] != 25 || g.UtilHistory[14] != 39 {
		t.Fatalf("windowed history %v", g.UtilHistory)
	}
	// Two minutes later everything has aged out, and the internal buffer
	// is bounded too.
	clk.Advance(2 * time.Minute)
	sampleNow(s)
	g = s.build(context.Background()).GPU
	if len(g.UtilHistory) != 1 || g.UtilHistory[0] != 40 {
		t.Fatalf("after a gap %v", g.UtilHistory)
	}
	s.mu.Lock()
	n := len(s.hist)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("internal history %d", n)
	}
}

func TestHistoryOmitsMissingSamples(t *testing.T) {
	clk := newClock()
	outs := []string{
		"G, 10, 1, 8192, 50, 1, 1",
		"G, [N/A], 1, 8192, 51, 1, 1", // util missing, temp kept
		"",                            // failure: no point at all
		"G, 13, 1, 8192, [N/A], 1, 1", // temp missing, util kept
	}
	smi := &fakeSMI{out: func(n int) (string, error) {
		if outs[n] == "" {
			return "", errors.New("boom")
		}
		return outs[n], nil
	}}
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now})
	for range outs {
		sampleNow(s)
		clk.Advance(2 * time.Second)
	}
	g := s.build(context.Background()).GPU
	if fmt.Sprint(g.UtilHistory) != "[10 13]" || fmt.Sprint(g.TempHistory) != "[50 51]" {
		t.Fatalf("util %v temp %v", g.UtilHistory, g.TempHistory)
	}
}

func TestSnapshotSamplesOnlyWhenStale(t *testing.T) {
	clk := newClock()
	smi := &fakeSMI{}
	s := New(Deps{RunCmd: smi.Run, Now: clk.Now})
	ctx := context.Background()
	s.Snapshot(ctx) // nothing sampled yet → sample
	s.Snapshot(ctx)
	if smi.Calls() != 1 {
		t.Fatalf("calls %d", smi.Calls())
	}
	clk.Advance(2 * time.Second) // exactly 2 s: still fresh
	s.Snapshot(ctx)
	if smi.Calls() != 1 {
		t.Fatalf("calls %d", smi.Calls())
	}
	clk.Advance(time.Millisecond)
	s.Snapshot(ctx)
	if smi.Calls() != 2 {
		t.Fatalf("calls %d", smi.Calls())
	}
	// Even a failing nvidia-smi counts as a sample (no re-run per request).
	smi.mu.Lock()
	smi.out = func(int) (string, error) { return "", errors.New("exit status 9") }
	smi.mu.Unlock()
	clk.Advance(3 * time.Second)
	s.Snapshot(ctx)
	s.Snapshot(ctx)
	if smi.Calls() != 3 {
		t.Fatalf("calls %d", smi.Calls())
	}
}

func TestSnapshotConcurrentRequestsSampleOnce(t *testing.T) {
	smi := &fakeSMI{}
	s := New(Deps{RunCmd: smi.Run, Now: newClock().Now})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Snapshot(context.Background())
		}()
	}
	wg.Wait()
	if smi.Calls() != 1 {
		t.Fatalf("calls %d", smi.Calls())
	}
}

// --- background loop ---

func TestLoopSamplesOnlyWithClients(t *testing.T) {
	smi := &fakeSMI{}
	tk := &manualTicker{}
	samples := make(chan Snapshot, 16)
	s := New(Deps{RunCmd: smi.Run, Now: newClock().Now,
		OnSample: func(sn Snapshot) { samples <- sn }})
	s.newTicker = tk.New
	s.Start(context.Background())

	next := func() Snapshot {
		t.Helper()
		select {
		case sn := <-samples:
			return sn
		case <-time.After(5 * time.Second):
			t.Fatal("no sample")
			return Snapshot{}
		}
	}
	none := func(what string) {
		t.Helper()
		select {
		case <-samples:
			t.Fatalf("unexpected sample: %s", what)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// No client: no nvidia-smi, no ticker.
	none("no client")
	if c, _ := tk.counts(); c != 0 || smi.Calls() != 0 {
		t.Fatalf("sampled without clients: tickers %d calls %d", c, smi.Calls())
	}

	// First client: one immediate sample, then one per tick.
	s.SetClients(1)
	if sn := next(); !sn.GPU.Available || sn.GPU.Name != "NVIDIA Test GPU" {
		t.Fatalf("snapshot %+v", sn.GPU)
	}
	if smi.Calls() != 1 {
		t.Fatalf("calls %d", smi.Calls())
	}
	waitFor(t, "ticker", func() bool { c, _ := tk.counts(); return c == 1 })
	if !tk.Fire() {
		t.Fatal("no ticker")
	}
	next()
	if smi.Calls() != 2 {
		t.Fatalf("calls %d", smi.Calls())
	}

	// Last client gone: the ticker stops and nothing is sampled.
	s.SetClients(0)
	waitFor(t, "ticker stop", func() bool { _, st := tk.counts(); return st == 1 })
	tk.Fire() // the stopped ticker's channel is no longer read
	none("after the last client left")
	if smi.Calls() != 2 {
		t.Fatalf("calls %d", smi.Calls())
	}

	// A new client samples at once; a further client too; a client leaving
	// while others remain does not.
	s.SetClients(1)
	next()
	s.SetClients(2)
	next()
	s.SetClients(1)
	none("client count fell but stayed > 0")
	if smi.Calls() != 4 {
		t.Fatalf("calls %d", smi.Calls())
	}

	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	if c, st := tk.counts(); c != st {
		t.Fatalf("tickers created %d stopped %d", c, st)
	}
	s.SetClients(3) // after Stop: nothing runs
	none("after Stop")
}

func TestStartContextCancelStopsLoop(t *testing.T) {
	s := New(Deps{RunCmd: (&fakeSMI{}).Run, Now: newClock().Now})
	s.newTicker = (&manualTicker{}).New
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	s.SetClients(1)
	cancel()
	done := make(chan struct{})
	go func() { s.loopWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop still running after the context was cancelled")
	}
	s.Stop()
}

// --- usage ---

func TestUsageFromUsageDaily(t *testing.T) {
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	clk := newClock()
	today := clk.Now().Format("2006-01-02")
	yesterday := clk.Now().AddDate(0, 0, -1).Format("2006-01-02")
	for _, u := range []model.UsageDaily{
		{ProviderID: "ds", Day: yesterday, Requests: 2, PromptTokens: 500, CacheHitTokens: 100, CompletionTokens: 50, EstimatedCost: 0.5},
		// v2 P12.5: the per-request estimates recorded with the usage (here
		// partly off-peak, so less than tokens × the provider prices = 0.003).
		{ProviderID: "ds", Day: today, Requests: 3, PromptTokens: 1000, CacheHitTokens: 400, CompletionTokens: 200, EstimatedCost: 0.0021},
		{ProviderID: "local", Day: yesterday, Requests: 1, PromptTokens: 10, CompletionTokens: 5},
		{ProviderID: "deleted", Day: today, Requests: 9, PromptTokens: 9},
	} {
		if err := st.AddUsageDaily(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	cny := "CNY"
	providers := []model.Provider{
		{ID: "local", Name: "Ollama", BalanceAPI: model.BalanceNone, MaxConcurrency: 1},
		{ID: "ds", Name: "DeepSeek", BalanceAPI: model.BalanceNone, MaxConcurrency: 8,
			PriceInput: fp64(2), PriceCacheHit: fp64(0.5), PriceOutput: fp64(8), Currency: &cny},
		{ID: "anth", Name: "Claude", BalanceAPI: model.BalanceNone, MaxConcurrency: 4},
	}
	s := New(Deps{
		Store: st, Now: clk.Now, RunCmd: (&fakeSMI{}).Run,
		Providers: func() []model.Provider { return providers },
		Inflight: func(id string) (int, int) {
			if id == "ds" {
				return 3, 8
			}
			return 0, 0 // not in the pool yet → provider.max_concurrency
		},
	})
	snap := s.Snapshot(ctx)
	if len(snap.Providers) != 3 || snap.Providers[0].ID != "local" || snap.Providers[1].ID != "ds" || snap.Providers[2].ID != "anth" {
		t.Fatalf("provider order %+v", snap.Providers)
	}
	local, ds, anth := snap.Providers[0], snap.Providers[1], snap.Providers[2]

	if ds.Name != "DeepSeek" || ds.Today != (Tokens{3, 1000, 400, 200}) || ds.Total != (Tokens{5, 1500, 500, 250}) {
		t.Fatalf("ds usage %+v", ds)
	}
	if ds.CacheHitRateToday == nil || *ds.CacheHitRateToday != 0.4 ||
		ds.CacheHitRateTotal == nil || math.Abs(*ds.CacheHitRateTotal-1.0/3) > 1e-12 {
		t.Fatalf("ds hit rates %v %v", ds.CacheHitRateToday, ds.CacheHitRateTotal)
	}
	// v2 P12.5: today's accumulated estimated_cost, not today's tokens × the
	// prices ((1000-400)×2 + 400×0.5 + 200×8 per million = 0.003).
	if ds.CostToday == nil || math.Abs(*ds.CostToday-0.0021) > 1e-12 || ds.Currency == nil || *ds.Currency != "CNY" {
		t.Fatalf("ds cost %v %v", ds.CostToday, ds.Currency)
	}
	if ds.Inflight != 3 || ds.Max != 8 {
		t.Fatalf("ds pool %d/%d", ds.Inflight, ds.Max)
	}
	if ds.Balance != nil || ds.BalanceError != "" {
		t.Fatalf("balance_api none must be null without error: %+v", ds)
	}

	if local.Today != (Tokens{}) || local.Total != (Tokens{1, 10, 0, 5}) || local.CacheHitRateToday != nil ||
		local.CacheHitRateTotal == nil || *local.CacheHitRateTotal != 0 {
		t.Fatalf("local %+v", local)
	}
	if local.CostToday != nil || local.Currency != nil || local.Max != 1 {
		t.Fatalf("local without prices %+v", local)
	}
	if anth.Total != (Tokens{}) || anth.CacheHitRateTotal != nil || anth.Max != 4 {
		t.Fatalf("anth %+v", anth)
	}

	// Usage written after the first snapshot shows up in the next one.
	if err := st.AddUsageDaily(ctx, model.UsageDaily{ProviderID: "anth", Day: today, Requests: 1, PromptTokens: 7, CompletionTokens: 3}); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(ctx).Providers[2].Today; got != (Tokens{1, 7, 0, 3}) {
		t.Fatalf("anth today %+v", got)
	}
}

func TestSnapshotJSONEmpty(t *testing.T) {
	s := New(Deps{RunCmd: (&fakeSMI{out: func(int) (string, error) { return "", errors.New("x") }}).Run, Now: newClock().Now})
	b, err := json.Marshal(s.Snapshot(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"providers":[]`, `"util_history":[]`, `"available":false`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("%s missing in %s", k, b)
		}
	}
}

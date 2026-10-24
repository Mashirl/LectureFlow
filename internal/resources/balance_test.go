// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// Fake keys only (never a real one).
const (
	testKey      = "sk-test-0000000000000001"
	testKeyOther = "sk-test-0000000000000002"
)

// balanceServer is a fake DeepSeek balance API.
type balanceServer struct {
	*httptest.Server
	mu      sync.Mutex
	hits    int
	path    string
	auth    string
	status  int
	body    string
	release chan struct{} // non-nil: requests block until closed (or cancelled)
}

func newBalanceServer(t *testing.T, status int, body string) *balanceServer {
	b := &balanceServer{status: status, body: body}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.hits++
		b.path, b.auth = r.URL.Path, r.Header.Get("Authorization")
		status, body, release := b.status, b.body, b.release
		b.mu.Unlock()
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *balanceServer) Hits() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits
}

func (b *balanceServer) req() (path, auth string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.path, b.auth
}

func (b *balanceServer) set(status int, body string) {
	b.mu.Lock()
	b.status, b.body = status, body
	b.mu.Unlock()
}

// providerList is a mutable Deps.Providers.
type providerList struct {
	mu sync.Mutex
	ps []model.Provider
}

func (l *providerList) Get() []model.Provider {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]model.Provider(nil), l.ps...)
}

func (l *providerList) Set(ps ...model.Provider) {
	l.mu.Lock()
	l.ps = ps
	l.mu.Unlock()
}

func deepseek(baseURL, key string) model.Provider {
	return model.Provider{ID: "ds", Name: "DeepSeek", Type: model.ProviderOpenAI, BaseURL: baseURL,
		APIKey: key, MaxConcurrency: 8, BalanceAPI: model.BalanceDeepSeek}
}

func newBalanceService(clk *fakeClock, pl *providerList) *Service {
	return New(Deps{RunCmd: (&fakeSMI{}).Run, Now: clk.Now, Providers: pl.Get})
}

// sampleAndWait takes one sample and waits for the balance queries it started.
func sampleAndWait(s *Service) {
	sampleNow(s)
	s.fetchWG.Wait()
}

func row(t *testing.T, s *Service, id string) Provider {
	t.Helper()
	for _, p := range s.build(context.Background()).Providers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("provider %s missing", id)
	return Provider{}
}

func TestBalanceDeepSeek(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	clk := newClock()
	pl := &providerList{}
	pl.Set(deepseek(srv.URL+"/", testKey)) // trailing "/" in base_url
	s := newBalanceService(clk, pl)

	before := row(t, s, "ds")
	if before.Balance != nil || before.BalanceError != "" || before.BalanceAt != "" {
		t.Fatalf("before the first query: %+v", before)
	}
	sampleAndWait(s)
	if path, auth := srv.req(); path != "/user/balance" || auth != "Bearer "+testKey {
		t.Fatalf("request %q %q", path, auth)
	}
	r := row(t, s, "ds")
	// The first balance_infos entry is shown.
	if r.Balance == nil || *r.Balance != "110.00" || r.BalanceCurrency == nil || *r.BalanceCurrency != "CNY" || r.BalanceError != "" {
		t.Fatalf("row %+v", r)
	}
	if r.BalanceAt != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("balance_at %q", r.BalanceAt)
	}
}

func TestBalanceNumberTotal(t *testing.T) {
	srv := newBalanceServer(t, 200, `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":12.5}]}`)
	pl := &providerList{}
	pl.Set(deepseek(srv.URL, testKey))
	s := newBalanceService(newClock(), pl)
	sampleAndWait(s)
	if r := row(t, s, "ds"); r.Balance == nil || *r.Balance != "12.5" || *r.BalanceCurrency != "USD" {
		t.Fatalf("row %+v", r)
	}
}

func TestBalanceFailures(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		code       string // v2 P14.3
	}{
		{"401", fixture(t, "deepseek_balance_401.json"), 401, CodeBalanceHTTP},
		{"key echoed", `{"error":{"message":"Authentication Fails, Your api key: ` + testKey + ` is invalid"}}`, 401, CodeBalanceHTTP},
		{"500 empty", "", 500, CodeBalanceHTTP},
		{"not json", "<html>bad gateway</html>", 200, CodeBalanceBadResponse},
		{"no balance_infos", fixture(t, "deepseek_balance_empty.json"), 200, CodeBalanceMissing},
		{"empty total", `{"balance_infos":[{"currency":"CNY","total_balance":""}]}`, 200, CodeBalanceMissing},
	}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	for _, c := range cases {
		srv := newBalanceServer(t, c.status, c.body)
		pl := &providerList{}
		pl.Set(deepseek(srv.URL, testKey))
		s := newBalanceService(newClock(), pl)
		sampleAndWait(s)
		r := row(t, s, "ds")
		if r.Balance != nil || r.BalanceCurrency != nil || r.BalanceAt == "" {
			t.Fatalf("%s: row %+v", c.name, r)
		}
		info := r.BalanceErrorInfo
		if r.BalanceError == "" || info == nil || info.Code != c.code || info.Detail != r.BalanceError ||
			(c.code == CodeBalanceHTTP && info.Params["status"] != c.status) {
			t.Fatalf("%s: error %q %+v", c.name, r.BalanceError, info)
		}
		if strings.Contains(r.BalanceError, testKey) || strings.Contains(info.Detail, testKey) {
			t.Fatalf("%s: key leaked into the error: %q", c.name, r.BalanceError)
		}
	}
	if strings.Contains(logs.String(), testKey) {
		t.Fatalf("key leaked into the log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "balance query failed") {
		t.Fatalf("failure not logged: %s", logs.String())
	}
}

func TestBalanceUnreachable(t *testing.T) {
	srv := newBalanceServer(t, 200, "{}")
	url := srv.URL
	srv.Close()
	pl := &providerList{}
	pl.Set(deepseek(url, testKey))
	s := newBalanceService(newClock(), pl)
	sampleAndWait(s)
	r := row(t, s, "ds")
	if r.Balance != nil || r.BalanceErrorInfo == nil || r.BalanceErrorInfo.Code != CodeBalanceNetwork || strings.Contains(r.BalanceError, testKey) {
		t.Fatalf("row %+v", r)
	}
}

func TestBalanceNoneAndMissingKey(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	none := deepseek(srv.URL, testKey)
	none.ID, none.Name, none.BalanceAPI = "other", "Other", model.BalanceNone
	nokey := deepseek(srv.URL, "")
	pl := &providerList{}
	pl.Set(none, nokey)
	s := newBalanceService(newClock(), pl)
	sampleAndWait(s)
	if srv.Hits() != 0 {
		t.Fatalf("balance API called %d times", srv.Hits())
	}
	if r := row(t, s, "other"); r.Balance != nil || r.BalanceCurrency != nil || r.BalanceError != "" || r.BalanceErrorInfo != nil {
		t.Fatalf("none: %+v", r)
	}
	if r := row(t, s, "ds"); r.Balance != nil || r.BalanceError == "" || r.BalanceErrorInfo == nil || r.BalanceErrorInfo.Code != CodeBalanceNoAPIKey {
		t.Fatalf("no key: %+v", r)
	}
}

func TestBalanceCachedFiveMinutes(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	clk := newClock()
	pl := &providerList{}
	pl.Set(deepseek(srv.URL, testKey))
	s := newBalanceService(clk, pl)
	sampleAndWait(s)
	first := row(t, s, "ds").BalanceAt
	for i := 0; i < 149; i++ { // 2 s samples up to 4 min 58 s
		clk.Advance(2 * time.Second)
		sampleAndWait(s)
	}
	if srv.Hits() != 1 {
		t.Fatalf("hits %d before 5 min", srv.Hits())
	}
	srv.set(200, `{"balance_infos":[{"currency":"CNY","total_balance":"99.50"}]}`)
	clk.Advance(2 * time.Second) // 5 min 0 s
	sampleAndWait(s)
	r := row(t, s, "ds")
	if srv.Hits() != 2 || *r.Balance != "99.50" || r.BalanceAt == first {
		t.Fatalf("hits %d row %+v", srv.Hits(), r)
	}
	// A failed query is also kept for 5 minutes (no hammering a bad key).
	srv.set(401, "{}")
	clk.Advance(BalanceTTL)
	sampleAndWait(s)
	clk.Advance(time.Minute)
	sampleAndWait(s)
	if srv.Hits() != 3 || row(t, s, "ds").BalanceError == "" {
		t.Fatalf("hits %d", srv.Hits())
	}
}

func TestRefreshBalance(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	clk := newClock()
	pl := &providerList{}
	pl.Set(deepseek(srv.URL, testKey))
	s := newBalanceService(clk, pl)
	sampleAndWait(s)
	s.RefreshBalance("ds")
	s.RefreshBalance("ds") // coalesced with the previous call
	s.RefreshBalance("unknown")
	clk.Advance(2 * time.Second)
	sampleAndWait(s)
	clk.Advance(2 * time.Second)
	sampleAndWait(s)
	if srv.Hits() != 2 {
		t.Fatalf("hits %d", srv.Hits())
	}
}

func TestRefreshBalanceDuringQuery(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	srv.release = make(chan struct{})
	clk := newClock()
	pl := &providerList{}
	pl.Set(deepseek(srv.URL, testKey))
	s := newBalanceService(clk, pl)
	sampleNow(s) // query in flight
	waitFor(t, "request", func() bool { return srv.Hits() == 1 })
	s.RefreshBalance("ds") // a job ended while the query ran
	sampleNow(s)           // still in flight: no second query
	close(srv.release)
	s.fetchWG.Wait()
	if srv.Hits() != 1 {
		t.Fatalf("hits %d", srv.Hits())
	}
	sampleAndWait(s) // the refresh request was kept
	if srv.Hits() != 2 {
		t.Fatalf("hits %d", srv.Hits())
	}
}

func TestBalanceProviderChange(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	clk := newClock()
	pl := &providerList{}
	pl.Set(deepseek(srv.URL, testKey))
	s := newBalanceService(clk, pl)
	sampleAndWait(s)
	// A new key: the cached balance belongs to the old key and is hidden
	// until the new query answers.
	pl.Set(deepseek(srv.URL, testKeyOther))
	if r := row(t, s, "ds"); r.Balance != nil || r.BalanceAt != "" {
		t.Fatalf("stale balance shown: %+v", r)
	}
	sampleAndWait(s)
	if _, auth := srv.req(); srv.Hits() != 2 || auth != "Bearer "+testKeyOther || row(t, s, "ds").Balance == nil {
		t.Fatalf("hits %d auth %q", srv.Hits(), auth)
	}
	// Switched to balance_api none, then deleted: no queries, cache dropped.
	p := deepseek(srv.URL, testKeyOther)
	p.BalanceAPI = model.BalanceNone
	pl.Set(p)
	sampleAndWait(s)
	pl.Set()
	sampleAndWait(s)
	s.mu.Lock()
	n := len(s.bal)
	s.mu.Unlock()
	if srv.Hits() != 2 || n != 0 {
		t.Fatalf("hits %d cache %d", srv.Hits(), n)
	}
}

func TestBalanceOnlyWithClients(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	clk := newClock()
	pl := &providerList{}
	pl.Set(deepseek(srv.URL, testKey))
	tk := &manualTicker{}
	samples := make(chan struct{}, 16)
	s := New(Deps{RunCmd: (&fakeSMI{}).Run, Now: clk.Now, Providers: pl.Get,
		OnSample: func(Snapshot) { samples <- struct{}{} }})
	s.newTicker = tk.New
	s.Start(context.Background())
	defer s.Stop()
	next := func() {
		t.Helper()
		select {
		case <-samples:
		case <-time.After(5 * time.Second):
			t.Fatal("no sample")
		}
	}

	time.Sleep(30 * time.Millisecond)
	if srv.Hits() != 0 {
		t.Fatal("balance queried without clients")
	}
	s.SetClients(1)
	next()
	waitFor(t, "first query", func() bool { return srv.Hits() == 1 })

	// After the client left, a job end only marks the balance; the query
	// runs when a front end connects again.
	s.SetClients(0)
	waitFor(t, "ticker stop", func() bool { _, st := tk.counts(); return st == 1 })
	waitFor(t, "first query stored", func() bool { return row(t, s, "ds").Balance != nil })
	s.RefreshBalance("ds")
	clk.Advance(10 * time.Minute) // even an expired balance is not refreshed without clients
	time.Sleep(30 * time.Millisecond)
	if srv.Hits() != 1 {
		t.Fatalf("hits %d without clients", srv.Hits())
	}
	s.SetClients(1)
	next()
	waitFor(t, "query on reconnect", func() bool { return srv.Hits() == 2 })
	waitFor(t, "second query stored", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		e := s.bal["ds"]
		return e != nil && !e.fetching
	})

	// With a client, RefreshBalance is served by the next tick.
	s.RefreshBalance("ds")
	waitFor(t, "ticker", func() bool { c, _ := tk.counts(); return c == 2 })
	tk.Fire()
	next()
	waitFor(t, "query after RefreshBalance", func() bool { return srv.Hits() == 3 })
}

func TestStopCancelsBalanceQuery(t *testing.T) {
	srv := newBalanceServer(t, 200, fixture(t, "deepseek_balance_ok.json"))
	srv.release = make(chan struct{}) // never released
	pl := &providerList{}
	pl.Set(deepseek(srv.URL, testKey))
	s := newBalanceService(newClock(), pl)
	sampleNow(s)
	waitFor(t, "request", func() bool { return srv.Hits() == 1 })
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel the balance query")
	}
	if r := row(t, s, "ds"); r.BalanceError != "" || r.Balance != nil {
		t.Fatalf("cancellation recorded as a result: %+v", r)
	}
	sampleNow(s) // after Stop no new query starts
	if srv.Hits() != 1 {
		t.Fatalf("hits %d", srv.Hits())
	}
}

// DeepSeek lists one entry per currency in no fixed order: the non-zero one
// is shown (real-data check: USD 0.00 came first, CNY 390.44 second).
func TestBalancePicksNonZeroCurrency(t *testing.T) {
	cases := []struct {
		body     string
		bal, cur string
	}{
		{`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"0.00"},{"currency":"CNY","total_balance":"390.44"}]}`, "390.44", "CNY"},
		{`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"390.44"},{"currency":"USD","total_balance":"0.00"}]}`, "390.44", "CNY"},
		{`{"is_available":false,"balance_infos":[{"currency":"USD","total_balance":"0.00"},{"currency":"CNY","total_balance":"0.00"}]}`, "0.00", "USD"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, c.body)
		}))
		p := model.Provider{ID: "ds", Name: "DeepSeek", BaseURL: srv.URL, APIKey: "sk-test-balance-0000", BalanceAPI: model.BalanceDeepSeek}
		res := fetchDeepSeekBalance(context.Background(), srv.Client(), p)
		srv.Close()
		if res.err != nil || res.balance != c.bal || res.currency != c.cur {
			t.Errorf("%s: got %q %q %v, want %q %q", c.body, res.balance, res.currency, res.err, c.bal, c.cur)
		}
	}
}

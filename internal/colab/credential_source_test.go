// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The credential's source (3.1.0): the in-app sign-in stores "app", the
// import of the user's own gcloud ADC file "gcloud"; a 3.0.0 record without
// the field counts as "gcloud". Disconnect reports the source it revoked
// (the API adds remote.gcloud_signed_out only for "gcloud").

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// storedSource is the "source" field of kv colab.credential as stored.
func storedSource(t *testing.T, kv KV) (string, bool) {
	t.Helper()
	v, _, _ := kv.GetKV(context.Background(), KVCredential)
	var raw map[string]any
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		t.Fatalf("stored credential: %v", err)
	}
	s, ok := raw["source"].(string)
	return s, ok
}

func TestCredentialSourceImports(t *testing.T) {
	ctx := context.Background()
	oauth := newFakeOAuth(t, okToken(fullScope))
	b, kv, _, lb := newCredBackend(t, oauth, "run-1")
	adc := writeADC(t, t.TempDir(), "authorized_user")
	b.o.ADCPath = adc

	// The in-app sign-in's import: source app, stored and described.
	info, err := b.ImportADCFile(ctx, adc, Lifecycle{Kind: LifecycleManual}, SourceApp)
	if err != nil || info.Source != SourceApp {
		t.Fatalf("app import: %+v %v", info, err)
	}
	if s, _ := storedSource(t, kv); s != SourceApp {
		t.Errorf("stored source %q, want app", s)
	}
	if got := b.Credential(ctx); got.Source != SourceApp {
		t.Errorf("Credential %+v", got)
	}
	if st := b.Status(ctx); st.Auth.Source != SourceApp {
		t.Errorf("Status.Auth %+v", st.Auth)
	}
	sb, _ := json.Marshal(b.Status(ctx))
	if !strings.Contains(string(sb), `"source":"app"`) {
		t.Errorf("status JSON without the source: %s", sb)
	}

	// "导入授权" of the user's own gcloud replaces it: source gcloud.
	info, err = b.ImportADC(ctx, Lifecycle{Kind: LifecycleDays, Days: 7})
	if err != nil || info.Source != SourceGcloud {
		t.Fatalf("gcloud import: %+v %v", info, err)
	}
	if s, _ := storedSource(t, kv); s != SourceGcloud {
		t.Errorf("stored source %q, want gcloud", s)
	}

	// Anything but app is stored as gcloud (the source that warns).
	for _, src := range []string{"", "unknown"} {
		info, err = b.ImportADCFile(ctx, adc, Lifecycle{Kind: LifecycleManual}, src)
		if err != nil || info.Source != SourceGcloud {
			t.Errorf("import with source %q: %+v %v", src, info, err)
		}
		if s, _ := storedSource(t, kv); s != SourceGcloud {
			t.Errorf("source %q stored as %q", src, s)
		}
	}

	// No credential: no source in the description.
	if _, err := b.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if got := b.Credential(ctx); got.Present || got.Source != "" {
		t.Errorf("Credential without one: %+v", got)
	}
	ib, _ := json.Marshal(b.Credential(ctx))
	if strings.Contains(string(ib), `"source"`) {
		t.Errorf("absent credential carries a source: %s", ib)
	}
	assertNoSecrets(t, "log", lb.String())
}

// A record imported by 3.0.0 has no "source": it counts as gcloud, stays
// usable (not deleted as unreadable) and is not rewritten by reading it.
func TestCredentialSourceOldRecord(t *testing.T) {
	ctx := context.Background()
	oauth := newFakeOAuth(t, okToken(fullScope))
	b, kv, clk, _ := newCredBackend(t, oauth, "run-1")
	storeCredential(t, kv, Lifecycle{Kind: LifecycleDays, Days: 7}, clk.Now().Add(-time.Hour), "")
	before, _, _ := kv.GetKV(ctx, KVCredential)
	if strings.Contains(before, `"source"`) {
		t.Fatalf("the 3.0.0 fixture has a source: %s", before)
	}
	if err := b.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	if info := b.Credential(ctx); !info.Present || info.Source != SourceGcloud || info.Lifecycle != "7d" {
		t.Fatalf("old record: %+v", info)
	}
	if _, err := b.auth.token(ctx); err != nil {
		t.Fatalf("old record not usable: %v", err)
	}
	if after, _, _ := kv.GetKV(ctx, KVCredential); after != before {
		t.Error("reading an old record rewrote it")
	}
	// An unknown source value is read as gcloud too, never deleted.
	var c storedCredential
	_ = json.Unmarshal([]byte(before), &c)
	c.Source = "someday"
	raw, _ := json.Marshal(c)
	_ = kv.SetKV(ctx, KVCredential, string(raw))
	if info := b.Credential(ctx); !info.Present || info.Source != SourceGcloud {
		t.Fatalf("unknown source: %+v", info)
	}
}

// Disconnect reports the revoked credential's source.
func TestDisconnectReportsSource(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, b *Backend, kv KV, clk *fakeClock)
		want  string
	}{
		{"in-app sign-in", func(t *testing.T, b *Backend, _ KV, _ *fakeClock) {
			if _, err := b.ImportADCFile(ctx, writeADC(t, t.TempDir(), "authorized_user"), Lifecycle{Kind: LifecycleManual}, SourceApp); err != nil {
				t.Fatal(err)
			}
		}, SourceApp},
		{"own gcloud", func(t *testing.T, b *Backend, _ KV, _ *fakeClock) {
			b.o.ADCPath = writeADC(t, t.TempDir(), "authorized_user")
			if _, err := b.ImportADC(ctx, Lifecycle{Kind: LifecycleManual}); err != nil {
				t.Fatal(err)
			}
		}, SourceGcloud},
		{"3.0.0 record", func(t *testing.T, _ *Backend, kv KV, clk *fakeClock) {
			storeCredential(t, kv, Lifecycle{Kind: LifecycleManual}, clk.Now(), "")
		}, SourceGcloud},
		{"none", func(*testing.T, *Backend, KV, *fakeClock) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oauth := newFakeOAuth(t, okToken(fullScope))
			b, kv, clk, _ := newCredBackend(t, oauth, "run-1")
			tc.setup(t, b, kv, clk)
			res, err := b.Disconnect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if res.Source != tc.want || res.Revoked != (tc.want != "") {
				t.Errorf("Disconnect = %+v, want source %q", res, tc.want)
			}
			if v, _, _ := kv.GetKV(ctx, KVCredential); v != "" {
				t.Error("the credential survived the disconnect")
			}
		})
	}
	// A failed revocation still says which credential it was.
	oauth := newFakeOAuth(t, func(f url.Values) (int, string) {
		if _, ok := f["grant_type"]; ok {
			return okToken(fullScope)(f)
		}
		return 503, "unavailable"
	})
	b, _, _, _ := newCredBackend(t, oauth, "run-1")
	if _, err := b.ImportADCFile(ctx, writeADC(t, t.TempDir(), "authorized_user"), Lifecycle{Kind: LifecycleManual}, SourceApp); err != nil {
		t.Fatal(err)
	}
	res, err := b.Disconnect(ctx)
	if err != nil || res.Revoked || res.Source != SourceApp || len(res.Warnings) != 1 || res.Warnings[0].Code != CodeRevokeFailed {
		t.Fatalf("failed revocation: %+v %v", res, err)
	}
}

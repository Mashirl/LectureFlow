// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testRefresh = "1//test-refresh-token-6f2c9a51d0"
	testSecret  = "test-client-secret-b81e4d77"
)

func testCredential(refresh string) string {
	return fmt.Sprintf(`{"client_id":"test.apps.example","client_secret":%q,"refresh_token":%q,"lifecycle":"7d"}`, testSecret, refresh)
}

// fileHolds reports which of the database files (the database, its WAL,
// a rollback journal) contain needle as raw bytes.
func fileHolds(t *testing.T, path, needle string) []string {
	t.Helper()
	var out []string
	for _, sfx := range []string{"", "-wal", "-journal"} {
		b, err := os.ReadFile(path + sfx)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(needle)) {
			out = append(out, filepath.Base(path+sfx))
		}
	}
	return out
}

// churn writes kv rows around the secret, rewrites the secret a few times
// (plain SetKV: copies left behind, as the app does with other keys) and
// fills enough pages that the kv b-tree splits.
func churn(t *testing.T, ctx context.Context, st Store, key string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if err := st.SetKV(ctx, key, testCredential(testRefresh)+strings.Repeat(" ", i*50)); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 40; j++ {
			if err := st.SetKV(ctx, fmt.Sprintf("filler.%d.%d", i, j), strings.Repeat("x", 300+j)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestSetSecretKVLeavesNoCopy (v3 Gate round 1 item 9): after the secret is
// deleted, neither the database file nor its WAL holds the refresh token or
// the client secret anywhere (free pages, unused page space, old frames).
func TestSetSecretKVLeavesNoCopy(t *testing.T) {
	ctx := context.Background()

	// Control: a plain delete ("" written) leaves the token in the files,
	// so the scan below can see one.
	ctl := filepath.Join(t.TempDir(), "control.db")
	cst, err := Open(ctl)
	if err != nil {
		t.Fatal(err)
	}
	churn(t, ctx, cst, "colab.credential")
	if err := cst.SetKV(ctx, "colab.credential", ""); err != nil {
		t.Fatal(err)
	}
	if got := fileHolds(t, ctl, testRefresh); len(got) == 0 {
		t.Fatal("control: a plain delete left no copy; the scan proves nothing")
	}
	cst.Close()

	path := filepath.Join(t.TempDir(), "lectureflow.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	churn(t, ctx, st, "colab.credential")
	sk, ok := st.(interface {
		SetSecretKV(ctx context.Context, key, value string) error
	})
	if !ok {
		t.Fatal("the store has no SetSecretKV")
	}
	// Replacing: the new value is stored, the old one leaves no copy.
	const newRefresh = "1//test-refresh-token-second-77aa"
	if err := sk.SetSecretKV(ctx, "colab.credential", testCredential(newRefresh)); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := st.GetKV(ctx, "colab.credential"); !strings.Contains(v, newRefresh) {
		t.Fatalf("replaced value %q", v)
	}
	if got := fileHolds(t, path, testRefresh); len(got) > 0 {
		t.Errorf("the replaced refresh token is still in %v", got)
	}
	// Deleting: the row is gone, and so is every byte of it.
	if err := sk.SetSecretKV(ctx, "colab.credential", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetKV(ctx, "colab.credential"); ok {
		t.Error("the credential row is still there")
	}
	for _, needle := range []string{testRefresh, newRefresh, testSecret} {
		if got := fileHolds(t, path, needle); len(got) > 0 {
			t.Errorf("%q is still in %v", needle, got)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the WAL was not truncated: %d bytes", fi.Size())
	}
	// The other rows are intact, the store keeps working.
	if v, ok, _ := st.GetKV(ctx, "filler.2.39"); !ok || len(v) != 339 {
		t.Errorf("filler row: %d %v", len(v), ok)
	}
	if err := st.SetKV(ctx, "after", "1"); err != nil {
		t.Fatal(err)
	}
	// secure_delete is back to the connection's earlier value.
	var sd int
	if err := st.(*sqlStore).db.QueryRowContext(ctx, "PRAGMA secure_delete").Scan(&sd); err != nil || sd != 0 {
		t.Errorf("secure_delete after the call: %d %v", sd, err)
	}
}

func TestSetSecretKVRefusedInTransaction(t *testing.T) {
	st := openTest(t)
	err := st.Tx(context.Background(), func(tx Store) error {
		return tx.(*sqlStore).SetSecretKV(context.Background(), "colab.credential", "")
	})
	if err == nil {
		t.Fatal("SetSecretKV ran inside a transaction")
	}
}

// TestScrubKV: the relocate backup's credential is removed the same way;
// a database without a kv table is left alone.
func TestScrubKV(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backup.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	churn(t, ctx, st, "colab.credential")
	st.Close()
	d, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ScrubKV(ctx, d, "colab.credential"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	for _, needle := range []string{testRefresh, testSecret} {
		if got := fileHolds(t, path, needle); len(got) > 0 {
			t.Errorf("%q is still in %v", needle, got)
		}
	}
	empty, err := openRaw(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if err := ScrubKV(ctx, empty, "colab.credential"); err != nil {
		t.Fatalf("no kv table: %v", err)
	}
}

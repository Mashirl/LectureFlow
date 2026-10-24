// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

// TestMigration009Upgrade: 009 rebuilds asr_device with the v3 backends
// (SPEC-v3 V3.1 mlx, V14 colab) and keeps every row and column value.
func TestMigration009Upgrade(t *testing.T) {
	ctx := context.Background()
	d, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 8); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO asr_device (id, backend, label, enabled, model, precision, max_parallel, priority, detected_at, last_benchmark)
		VALUES ('cuda:0','faster-whisper','NVIDIA',1,'large-v2','auto',2,300,'2026-10-24T09:43:15Z','{"version":1}')`)
	exec(`INSERT INTO asr_device (id, backend, label, enabled, model, precision, max_parallel, priority, detected_at, last_benchmark)
		VALUES ('ov:CPU','openvino','Intel CPU',0,'large-v3-turbo','fp16',1,0,NULL,NULL)`)
	q := map[string]string{"asr_device": `SELECT id, backend, label, enabled, model, precision, max_parallel, priority, detected_at, last_benchmark FROM asr_device ORDER BY id`}
	before := dumpRows(t, d, q["asr_device"])
	if err := Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v < 9 {
		t.Fatalf("user_version = %d, want >= 9", v)
	}
	if after := dumpRows(t, d, q["asr_device"]); after != before {
		t.Fatalf("rows changed by 009:\n%s\n---\n%s", before, after)
	}
	for _, b := range []string{model.BackendMLX, model.BackendColab} {
		if _, err := d.ExecContext(ctx, `INSERT INTO asr_device (id, backend, label, model, precision) VALUES (?, ?, 'x', 'm', 'p')`, "t:"+b, b); err != nil {
			t.Fatalf("backend %s rejected: %v", b, err)
		}
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO asr_device (id, backend, label, model, precision) VALUES ('rocm:0','rocm','x','m','p')`); err == nil {
		t.Fatal("backend rocm accepted")
	}
}

func dumpRows(t *testing.T, d *sql.DB, query string) string {
	t.Helper()
	rows, err := d.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			out += v.String + "|"
		}
		out += "\n"
	}
	return out
}

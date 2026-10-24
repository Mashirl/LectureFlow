// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"lectureflow/internal/model"
)

// Tests of migration 008 and the asr_device store methods (SPEC-v2 I6,
// phase 3b freeze .claude/v3b-brief.md).

// v7Queries select the columns that exist before migration 008.
var v7Queries = map[string]string{
	"course":   `SELECT id, name, glossary_version, created_at, code, term_start, skip_weeks, filename_rule, numbering FROM course ORDER BY id`,
	"lecture":  `SELECT id, course_id, title, media_path, created_at, launched_at, seq, recorded_at, recorded_at_source, title_zh, title_source, blurb_en, blurb_zh, original_filename, kind, kind_source FROM lecture ORDER BY id`,
	"material": `SELECT id, course_id, lecture_id, filename, path, pages_md, created_at, bind_week FROM material ORDER BY id`,
	"job":      v6Queries["job"],
	"sentence": v6Queries["sentence"],
	"term":     v6Queries["term"],
	"kv":       v6Queries["kv"],
}

// An existing database (migration 007) upgrades to 008 without any change
// to its data: the migration only creates the empty asr_device table (the
// legacy ASR settings become rows in Go, kv-guarded).
func TestMigration008Upgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	d, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(ctx, d, 7); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO course (id, name, glossary_version, created_at, code) VALUES ('C1','Crypto',3,'2026-09-20T10:00:00Z','CSC1003')`)
	exec(`INSERT INTO lecture (id, course_id, title, media_path, created_at, launched_at, seq, kind, kind_source)
		VALUES ('L1','C1','Stream ciphers','m.mp4','2026-10-01T10:00:00Z','2026-10-01T10:00:00Z',1,'lecture','filename')`)
	exec(`INSERT INTO material (id, course_id, lecture_id, filename, path, pages_md, created_at, bind_week)
		VALUES ('M1','C1',NULL,'w1.pdf','p1','["a"]','2026-10-01T11:00:00Z',2)`)
	exec(`INSERT INTO job (id, scope, scope_id, stage, status, input_hash, attempt, error, progress, is_current, stats, created_at,
		config_snapshot) VALUES ('J1','lecture','L1','asr','done','h',1,NULL,1,1,'{"hash_version":2}','2026-10-01T10:00:00Z',
		'{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en"}')`)
	exec(`INSERT INTO kv (key, value) VALUES ('settings.asr','{"model":"large-v2","compute_type":"auto","device":"cuda","language":"en","max_parallel":1}')`)
	exec(`INSERT INTO kv (key, value) VALUES ('gpu_paused','0')`)
	before := dumpTables(t, d, v7Queries)

	if err := migrateUpTo(ctx, d, 8); err != nil {
		t.Fatal(err)
	}
	var v int
	d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != 8 {
		t.Fatalf("user_version = %d, want 8", v)
	}
	if after := dumpTables(t, d, v7Queries); after != before {
		t.Fatalf("data changed by migration 008.\n--- before ---\n%s--- after ---\n%s", before, after)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM asr_device`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("asr_device rows = %d, %v; want an empty table", n, err)
	}
	// The DDL defaults and the backend CHECK of I6.
	exec(`INSERT INTO asr_device (id, backend, label, model, precision) VALUES ('ov:GPU','openvino','Intel(R) Graphics','large-v3-turbo','fp16')`)
	st := New(d)
	defer st.Close()
	got, err := st.GetASRDevice(ctx, model.DeviceOVGPU)
	if err != nil || !got.Enabled || got.MaxParallel != 1 || got.Priority != 0 || got.DetectedAt != nil || got.LastBenchmark != nil {
		t.Fatalf("defaults: %+v, %v", got, err)
	}
	for _, q := range []string{
		`INSERT INTO asr_device (id, backend, label, model, precision) VALUES ('rocm:0','rocm','x','m','p')`,
		`INSERT INTO asr_device (id, backend, model, precision) VALUES ('cuda:0','faster-whisper','m','p')`, // label NOT NULL
		`INSERT INTO asr_device (id, backend, label, precision) VALUES ('cuda:0','faster-whisper','x','p')`, // model NOT NULL
	} {
		if _, err := d.ExecContext(ctx, q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
}

func TestASRDeviceStore(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if ds, err := st.ListASRDevices(ctx); err != nil || len(ds) != 0 {
		t.Fatalf("fresh database: %+v, %v", ds, err)
	}
	if _, err := st.GetASRDevice(ctx, model.DeviceCUDA0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	at := "2026-10-24T08:00:00Z"
	bench := `{"version":1,"status":"done"}`
	rows := []model.ASRDevice{
		{ID: model.DeviceOVNPU, Backend: model.BackendOpenVINO, Label: "Intel(R) AI Boost", Enabled: true,
			Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100, DetectedAt: &at},
		{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", Enabled: true,
			Model: "large-v2", Precision: "auto", MaxParallel: 2, Priority: 300, DetectedAt: &at},
		{ID: model.DeviceOVCPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Core(TM) Ultra 9 386H", Enabled: false,
			Model: "large-v3-turbo", Precision: "int8", MaxParallel: 1, Priority: 0, LastBenchmark: &bench},
		{ID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Label: "Intel(R) Graphics", Enabled: true,
			Model: "large-v3-turbo", Precision: "fp16", MaxParallel: 1, Priority: 100}, // same priority as ov:NPU: id order
	}
	for _, r := range rows {
		if err := st.InsertASRDevice(ctx, r); err != nil {
			t.Fatalf("insert %s: %v", r.ID, err)
		}
	}
	if err := st.InsertASRDevice(ctx, rows[0]); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate insert: %v", err)
	}
	list, err := st.ListASRDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range list {
		ids = append(ids, d.ID)
	}
	if want := []string{model.DeviceCUDA0, model.DeviceOVGPU, model.DeviceOVNPU, model.DeviceOVCPU}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v (priority DESC, id)", ids, want)
	}
	for _, r := range rows {
		got, err := st.GetASRDevice(ctx, r.ID)
		if err != nil || !reflect.DeepEqual(got, r) {
			t.Fatalf("get %s = %+v, %v; want %+v", r.ID, got, err, r)
		}
	}

	// Update writes every column but id and last_benchmark.
	cpu := rows[2]
	cpu.Label, cpu.Enabled, cpu.Model, cpu.Precision, cpu.MaxParallel, cpu.Priority, cpu.DetectedAt = "CPU", true, "small", "fp16", 2, 50, &at
	other := `{"version":1,"status":"failed"}`
	cpu.LastBenchmark = &other // ignored by UpdateASRDevice
	if err := st.UpdateASRDevice(ctx, cpu); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetASRDevice(ctx, model.DeviceOVCPU)
	cpu.LastBenchmark = &bench
	if !reflect.DeepEqual(got, cpu) {
		t.Fatalf("after update %+v, want %+v", got, cpu)
	}
	// detected_at back to NULL
	cpu.DetectedAt = nil
	if err := st.UpdateASRDevice(ctx, cpu); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetASRDevice(ctx, model.DeviceOVCPU); got.DetectedAt != nil {
		t.Fatalf("detected_at = %v, want NULL", *got.DetectedAt)
	}
	if err := st.UpdateASRDevice(ctx, model.ASRDevice{ID: "fw:cpu", Backend: model.BackendFasterWhisper, Label: "x", Model: "m", Precision: "auto"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	// SetASRDeviceBenchmark writes only last_benchmark.
	if err := st.SetASRDeviceBenchmark(ctx, model.DeviceOVNPU, &other); err != nil {
		t.Fatal(err)
	}
	npu, _ := st.GetASRDevice(ctx, model.DeviceOVNPU)
	want := rows[0]
	want.LastBenchmark = &other
	if !reflect.DeepEqual(npu, want) {
		t.Fatalf("after benchmark %+v, want %+v", npu, want)
	}
	if err := st.SetASRDeviceBenchmark(ctx, model.DeviceOVNPU, nil); err != nil {
		t.Fatal(err)
	}
	if npu, _ := st.GetASRDevice(ctx, model.DeviceOVNPU); npu.LastBenchmark != nil {
		t.Fatalf("benchmark not cleared: %v", *npu.LastBenchmark)
	}
	if err := st.SetASRDeviceBenchmark(ctx, "fw:cpu", &other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("benchmark of missing: %v", err)
	}
	// Inside a transaction (the Registry reorders in one).
	err = st.Tx(ctx, func(tx Store) error {
		for i, id := range []string{model.DeviceOVNPU, model.DeviceOVGPU, model.DeviceCUDA0, model.DeviceOVCPU} {
			d, err := tx.GetASRDevice(ctx, id)
			if err != nil {
				return err
			}
			d.Priority = (4 - i) * 100
			if err := tx.UpdateASRDevice(ctx, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListASRDevices(ctx)
	ids = ids[:0]
	for _, d := range list {
		ids = append(ids, d.ID)
	}
	if want := []string{model.DeviceOVNPU, model.DeviceOVGPU, model.DeviceCUDA0, model.DeviceOVCPU}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order after reorder = %v, want %v", ids, want)
	}
}

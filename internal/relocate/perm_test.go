// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/dirperm"
)

// The data directory a move creates admits only this user, and so does
// everything copied into it (sec-update M2: under D:\ every account of the
// computer could otherwise modify it). An existing empty directory the
// user chose is made private too; a failed move gives it its old
// permissions back.
func TestMovedDataDirIsPrivate(t *testing.T) {
	e := newMoveEnv(t)
	if err := MoveDataDir(context.Background(), e.opts()); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, e.dst)
	for _, p := range []string{filepath.Join(e.dst, "courses"), filepath.Join(e.dst, "components", "python")} {
		if ex, err := dirperm.Check(p); err != nil || ex.Exposed() {
			t.Fatalf("%s: %+v %v", p, ex, err)
		}
	}

	e2 := newMoveEnv(t)
	if err := os.MkdirAll(e2.dst, 0o755); err != nil {
		t.Fatal(err)
	}
	loosen(t, e2.dst)
	before := permOf(t, e2.dst)
	o := e2.opts()
	o.FixComponents = func(string, string) error { return errors.New("venv broken") }
	if err := MoveDataDir(context.Background(), o); codeOf(err) != CodeComponentsFailed {
		t.Fatalf("err %v", err)
	}
	if !exists(e2.dst) || permOf(t, e2.dst) != before {
		t.Fatalf("after a failed move: exists %v, permissions %q, were %q", exists(e2.dst), permOf(t, e2.dst), before)
	}
	if ents, _ := os.ReadDir(e2.dst); len(ents) != 0 {
		t.Fatalf("left %v", ents)
	}
	if err := MoveDataDir(context.Background(), e2.opts()); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, e2.dst)
}

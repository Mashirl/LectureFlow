// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package relocate

import (
	"fmt"
	"os"
	"testing"
)

func permOf(t *testing.T, dir string) string {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%o", fi.Mode().Perm())
}

func loosen(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
}

func assertPrivate(t *testing.T, dir string) {
	t.Helper()
	if got := permOf(t, dir); got != "700" {
		t.Fatalf("%s: mode %s", dir, got)
	}
}

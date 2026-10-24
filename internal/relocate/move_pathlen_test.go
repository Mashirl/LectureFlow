// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/model"
)

// pathOfLen is a not yet existing directory whose absolute path has n
// characters.
func pathOfLen(t *testing.T, n int) string {
	base := t.TempDir()
	pad := n - components.PathLen(base) - 1
	if pad < 1 {
		t.Fatalf("temp dir %s is longer than %d", base, n)
	}
	return filepath.Join(base, strings.Repeat("n", pad))
}

// V12.2 acceptance issue 1: a move to a place whose path is too long for
// the installed component environments is refused before anything is
// copied, with data_dir.path_too_long. The need is the deepest file under
// components/; without components there is no limit.
func TestCheckMoveRefusesTooLongTarget(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, DBName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// deep files outside components/ do not count
	writeFiles(t, src, map[string]string{"courses/C1/lectures/L1/export/" + strings.Repeat("x", 150) + ".md": "x"})
	if _, err := checkMove(ctx, src, pathOfLen(t, 200), "", true); err != nil {
		t.Fatalf("no components: %v", err)
	}

	deep := filepath.Join("components", "asr-x", "1.0.0", "venv", "Lib", strings.Repeat("p", 100))
	writeFiles(t, src, map[string]string{filepath.ToSlash(deep): "x"})
	need := components.PathLen(deep)
	fits := components.MaxPathLen - 1 - need
	if _, err := checkMove(ctx, src, pathOfLen(t, fits), "", true); err != nil {
		t.Fatalf("a %d-character target: %v", fits, err)
	}
	dst := pathOfLen(t, fits+1)
	_, err := checkMove(ctx, src, dst, "", true)
	var ce *model.CodedError
	if !asCoded(err, &ce) || ce.Info.Code != CodePathTooLong {
		t.Fatalf("want %s, got %v", CodePathTooLong, err)
	}
	if p := ce.Info.Params; p["path_len"] != components.MaxPathLen+1 || p["max"] != components.MaxPathLen || p["data_dir"] != dst {
		t.Errorf("params %+v", p)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("the check created the target")
	}
	// other platforms: no limit
	if _, err := checkMove(ctx, src, dst, "", false); err != nil {
		t.Errorf("without the limit: %v", err)
	}
}

func asCoded(err error, ce **model.CodedError) bool {
	c, ok := err.(*model.CodedError)
	if ok {
		*ce = c
	}
	return ok
}

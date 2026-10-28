// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"lectureflow/internal/model"
)

// Writing a product through a temporary file and putting it in place
// (SPEC §4.7: every output is written to a .tmp first and renamed), safe
// against files that another program holds open (test-chaos 1 and 3):
//   - tmpFor never hands out a .tmp that is still held (an ffmpeg left
//     running by an earlier LectureFlow that was killed): the remains of the
//     earlier run are never taken for this run's product;
//   - replaceFiles puts a set of new files in place all or nothing: a file
//     that cannot be replaced (Windows: a player or an editor has it open)
//     leaves every old file as it was, and the error names the file only
//     (fileInUseError), never its full path.

// tmpNames is how many temporary names tmpFor tries: out.tmp, out.1.tmp, …
const tmpNames = 10

// tmpFor returns the temporary file to write out through: out+".tmp" after
// removing it, or, when that one cannot be removed (another program still
// has it open), the first of out+".1.tmp" … out+".9.tmp" that is free.
// Every name ends in .tmp, so startup recovery removes the ones left
// behind. An error only when none is free: a *fileInUseError for out's
// name when the names are held, else the first removal error.
func tmpFor(out string) (string, error) {
	var first error
	for i := 0; i < tmpNames; i++ {
		p := out + ".tmp"
		if i > 0 {
			p = fmt.Sprintf("%s.%d.tmp", out, i)
		}
		err := os.Remove(p)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			// Windows: a file removed while another program still has it open
			// (with FILE_SHARE_DELETE) keeps its name until it is closed.
			if _, serr := os.Lstat(p); errors.Is(serr, fs.ErrNotExist) {
				if i > 0 {
					slog.Warn("a temporary file is still held by another program; writing through another name",
						"held", filepath.Base(out)+".tmp", "tmp", filepath.Base(p))
				}
				return p, nil
			}
			err = fmt.Errorf("%s is still in use", filepath.Base(p))
		}
		if first == nil {
			first = err
		}
	}
	if fileInUse(first, out+".tmp") {
		return "", &fileInUseError{name: filepath.Base(out), err: first}
	}
	return "", first
}

// fileInUseError: name could not be removed or replaced because another
// program has it open (Windows). Error() names the file without its path;
// the stages turn it into their coded error (export.file_in_use,
// prep.file_in_use) with params {name}.
type fileInUseError struct {
	name string // the file's base name
	err  error  // the system's error, for the log
}

func (e *fileInUseError) Error() string {
	return fmt.Sprintf("文件“%s”正被其他程序占用，无法替换。请关闭正在使用它的程序后点重试", e.name)
}

// asFileInUse reports err's *fileInUseError.
func asFileInUse(err error) (*fileInUseError, bool) {
	var fe *fileInUseError
	ok := errors.As(err, &fe)
	return fe, ok
}

// inUseCoded turns a *fileInUseError into the stage's coded error: code
// (CodePrepFileInUse, CodeExportFileInUse) with params {name}, retryable
// (the program may close the file before the automatic retry). Other
// errors are returned unchanged.
func inUseCoded(code string, err error) error {
	fe, ok := asFileInUse(err)
	if !ok {
		return err
	}
	slog.Warn("a file is held by another program", "file", fe.name, "err", fe.err)
	return model.Errorf(code, map[string]any{"name": fe.name}, fe)
}

// filePair is a new file (tmp) to put in place of final.
type filePair struct{ tmp, final string }

// replaceFile is replaceFiles for one file.
func replaceFile(tmp, final string) error {
	return replaceFiles([]filePair{{tmp: tmp, final: final}}, nil)
}

// replaceFiles puts every pair's tmp in place of its final and removes the
// files of obsolete, all or nothing:
//  1. every final that exists and every obsolete file is moved aside (to a
//     tmpFor name of final+".old"). Windows refuses to move a file that
//     another program has open without allowing it to be deleted (Go's own
//     os.Open does that, so a download or a player reading it counts), so a
//     held file stops the replacement here, and the files moved aside so far
//     are moved back: nothing changed;
//  2. every tmp is renamed to its final (now free names); a failure moves
//     the new ones back to their tmp names and the old ones back too;
//  3. the files moved aside are removed. A failure there is only logged:
//     their names end in .tmp and startup recovery removes them.
//
// A program that allows deletion (FILE_SHARE_DELETE) does not stop it: it
// keeps reading the old file under its .tmp name. A held file is reported
// as a *fileInUseError naming it; other errors are returned as they are.
// The tmp files are left to the caller (removed on success, since they
// became the finals).
func replaceFiles(pairs []filePair, obsolete []string) error {
	type moved struct{ from, to string }
	var aside []moved
	restore := func() {
		for i := len(aside) - 1; i >= 0; i-- {
			if err := os.Rename(aside[i].to, aside[i].from); err != nil {
				slog.Error("replace files: an old file could not be moved back", "file", aside[i].from, "err", err)
			}
		}
	}
	held := func(err error, path string) error {
		if fileInUse(err, path) {
			return &fileInUseError{name: filepath.Base(path), err: err}
		}
		return err
	}
	olds := make([]string, 0, len(pairs)+len(obsolete))
	for _, p := range pairs {
		olds = append(olds, p.final)
	}
	olds = append(olds, obsolete...)
	for _, old := range olds {
		if _, err := os.Lstat(old); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		to, err := tmpFor(old + ".old")
		if err == nil {
			err = os.Rename(old, to)
		}
		if err != nil {
			restore()
			if fe, ok := asFileInUse(err); ok { // every aside name held: report the file itself
				return &fileInUseError{name: filepath.Base(old), err: fe.err}
			}
			return held(err, old)
		}
		aside = append(aside, moved{from: old, to: to})
	}
	for i, p := range pairs {
		if err := os.Rename(p.tmp, p.final); err != nil {
			for _, q := range pairs[:i] {
				if rerr := os.Rename(q.final, q.tmp); rerr != nil {
					slog.Error("replace files: a new file could not be taken back", "file", q.final, "err", rerr)
				}
			}
			restore()
			return held(err, p.final)
		}
	}
	for _, m := range aside {
		if err := os.Remove(m.to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("replace files: an old file could not be removed (startup recovery removes it)", "file", filepath.Base(m.to), "err", err)
		}
	}
	return nil
}

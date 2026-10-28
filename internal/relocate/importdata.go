// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/secrets"
)

// Import of an existing data directory (SPEC-v3 V1.3 "从开发版迁移", V4.2
// step 1): the database and courses/ are copied into
// {data_dir}/import-staging/, rewritten to the current data directory and
// verified; the READY marker is written last. ApplyStaged swaps them in at
// the next start, before the database is opened. models/, backups/, logs/,
// selftest/ and components/ are not taken (models come from components;
// backups and logs belong to the old install).

// ReadyMarker marks a complete, verified staging area.
const ReadyMarker = "READY"

// importDirs are the directories an import copies.
var importDirs = []string{"courses"}

// requiredTables identify a LectureFlow database.
var requiredTables = []string{"course", "lecture", "job", "kv"}

// carriedKVPrefixes: kv rows of the current (new) install that the staged
// database takes over: the application settings and the onboarding
// progress belong to this install, not to the imported one.
var carriedKVPrefixes = []string{"app.", "onboarding."}

// SourceInfo describes an importable data directory.
type SourceInfo struct {
	Dir      string
	Version  int // PRAGMA user_version
	Courses  int
	Lectures int
	Bytes    int64 // database + courses/
}

// CheckImportSource checks that dir is a LectureFlow data directory: it has
// lectureflow.db, the database has the LectureFlow tables, and its schema
// version is not newer than this program's.
func CheckImportSource(ctx context.Context, dir string) (SourceInfo, error) {
	var info SourceInfo
	d, err := cleanAbs(dir)
	if err != nil {
		return info, err
	}
	info.Dir = d
	notLF := func(err error) error {
		return coded(CodeNotLectureFlow, map[string]any{"path": d}, err)
	}
	p := filepath.Join(d, DBName)
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return info, notLF(err)
	}
	db, err := openDB(p, true)
	if err != nil {
		return info, notLF(err)
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&info.Version); err != nil {
		return info, notLF(err)
	}
	tables, err := userTables(ctx, db)
	if err != nil {
		return info, notLF(err)
	}
	have := map[string]bool{}
	for _, t := range tables {
		have[t] = true
	}
	for _, t := range requiredTables {
		if !have[t] {
			return info, notLF(fmt.Errorf("no table %s", t))
		}
	}
	if info.Version < 1 {
		return info, notLF(errors.New("schema version 0"))
	}
	sup, err := SupportedVersion()
	if err != nil {
		return info, err
	}
	if info.Version > sup {
		return info, coded(CodeTooNew, map[string]any{"version": info.Version, "supported": sup}, nil)
	}
	if info.Courses, info.Lectures, err = libraryCounts(ctx, db); err != nil {
		return info, notLF(err)
	}
	if info.Bytes, err = dbBytes(ctx, db); err != nil {
		return info, notLF(err)
	}
	for _, sub := range importDirs {
		n, err := treeBytes(filepath.Join(d, sub), nil)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return info, coded(CodeCopyFailed, map[string]any{"path": sub}, err)
		}
		info.Bytes += n
	}
	return info, nil
}

func libraryCounts(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (courses, lectures int, err error) {
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM course").Scan(&courses); err != nil {
		return
	}
	err = q.QueryRowContext(ctx, "SELECT count(*) FROM lecture").Scan(&lectures)
	return
}

// CheckFresh: the data directory's database has no course and no lecture
// (a missing database is fresh).
func CheckFresh(ctx context.Context, dataDir string) error {
	p := filepath.Join(dataDir, DBName)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	d, err := openDB(p, true)
	if err != nil {
		return err
	}
	defer d.Close()
	c, l, err := libraryCounts(ctx, d)
	if err != nil {
		return err
	}
	if c > 0 || l > 0 {
		return coded(CodeNotFresh, map[string]any{"courses": c, "lectures": l}, nil)
	}
	return nil
}

// ImportOptions are the inputs of StageImport.
type ImportOptions struct {
	Src     string // the data directory to import (read only)
	DataDir string // the current data directory
	// Fresh checks that the current library is empty; nil = CheckFresh on
	// DataDir's database file.
	Fresh    func(ctx context.Context) error
	Progress func(Progress)
}

// StageImport copies an existing data directory into
// {DataDir}/import-staging/ for ApplyStaged: the database (a read-only
// online backup: the source may be in use by a development instance) and
// courses/, rewritten from Src to DataDir and verified (integrity, row
// counts, file sizes, no old prefix); then the kv rows of carriedKVPrefixes
// are taken over from the current database and READY is written. A failure
// removes the staging area.
func StageImport(ctx context.Context, o ImportOptions) (err error) {
	info, err := CheckImportSource(ctx, o.Src)
	if err != nil {
		return err
	}
	src := info.Dir
	dataDir, err := cleanAbs(o.DataDir)
	if err != nil {
		return err
	}
	if samePath(src, dataDir) || within(dataDir, src) || within(src, dataDir) {
		return coded(CodeImportSameDir, nil, nil)
	}
	fresh := o.Fresh
	if fresh == nil {
		fresh = func(ctx context.Context) error { return CheckFresh(ctx, dataDir) }
	}
	if err := fresh(ctx); err != nil {
		return err
	}
	staging := filepath.Join(dataDir, StagingDir)
	if err := os.RemoveAll(staging); err != nil {
		return coded(CodeTargetNotWritable, map[string]any{"path": staging}, err)
	}
	var free int64
	if err := checkSpace(dataDir, info.Bytes, &free); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(staging)
		}
	}()
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return coded(CodeTargetNotWritable, map[string]any{"path": staging}, err)
	}
	t := &tracker{}

	var files []fileEntry
	var dirs []string
	var fileBytes int64
	for _, sub := range importDirs {
		root := filepath.Join(src, sub)
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		subFiles, ds, n, err := scanTree(root, nil)
		if err != nil {
			return coded(CodeCopyFailed, map[string]any{"path": sub}, err)
		}
		dirs = append(dirs, sub)
		for _, d := range ds {
			dirs = append(dirs, filepath.Join(sub, d))
		}
		for _, f := range subFiles {
			f.rel = filepath.Join(sub, f.rel)
			files = append(files, f)
		}
		fileBytes += n
	}

	s, err := openSource(ctx, filepath.Join(src, DBName))
	if err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	defer s.close()
	pr := &progress{total: fileBytes + s.bytes, fn: o.Progress}
	pr.setPhase(PhaseCopying)
	stagedDB := filepath.Join(staging, DBName)
	if err := s.snapshot(ctx, stagedDB, pr.add); err != nil {
		if ctx.Err() != nil {
			return coded(CodeCancelled, nil, ctx.Err())
		}
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := copyTree(ctx, t, src, staging, dirs, files, pr.add); err != nil {
		return err
	}

	pr.setPhase(PhaseVerifying)
	rw := newRewriter(src, dataDir)
	d, err := openDB(stagedDB, false)
	if err != nil {
		return verifyErr("open", err)
	}
	defer d.Close()
	if _, err := rewriteDB(ctx, d, rw); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := verifyDB(ctx, d, s.counts, rw); err != nil {
		return err
	}
	if err := verifyFiles(staging, files); err != nil {
		return err
	}
	if err := resealImported(ctx, d, src); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	kv, err := carriedKV(ctx, dataDir)
	if err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := setKV(ctx, d, kv); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := finishDB(ctx, d); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := d.Close(); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": DBName}, err)
	}
	if err := ctx.Err(); err != nil {
		return coded(CodeCancelled, nil, err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339) + "\n" + src + "\n"
	if err := atomicWrite(filepath.Join(staging, ReadyMarker), []byte(stamp)); err != nil {
		return coded(CodeCopyFailed, map[string]any{"path": ReadyMarker}, err)
	}
	return nil
}

// carriedKV reads the kv rows of carriedKVPrefixes from the current database.
func carriedKV(ctx context.Context, dataDir string) (map[string]string, error) {
	out := map[string]string{}
	p := filepath.Join(dataDir, DBName)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	d, err := openDB(p, true)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	rows, err := d.QueryContext(ctx, "SELECT key, value FROM kv")
	if err != nil {
		// A database without kv (never migrated) carries nothing.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		for _, p := range carriedKVPrefixes {
			if strings.HasPrefix(k, p) {
				out[k] = v
			}
		}
	}
	return out, rows.Err()
}

// rename is os.Rename; tests make it fail.
var rename = os.Rename

// ApplyError is returned by ApplyStaged. Restored reports that the data
// directory is back as it was (the start can go on with the old database);
// when false, a step could not be undone and the start must stop.
type ApplyError struct {
	Restored bool
	Err      error
}

func (e *ApplyError) Error() string {
	if e.Restored {
		return "import not applied (data directory restored): " + e.Err.Error()
	}
	return "import not applied and the data directory could not be restored: " + e.Err.Error()
}

func (e *ApplyError) Unwrap() error { return e.Err }

// ApplyStaged runs at startup before the database is opened. With a READY
// staging area it moves the current lectureflow.db (and -wal, -shm,
// -journal) to backups/lectureflow-before-import-<time>.db, the current
// courses/ (if any) to backups/courses-before-import-<time>, moves the
// staged database and courses/ into place and deletes the staging area. A
// failed step undoes the earlier ones (no half-switched state) and the READY
// marker is removed, so the next start does not try again; the error is an
// *ApplyError. A staging area without READY (a copy interrupted earlier) is
// deleted. Without a staging area nothing happens. An older imported
// database is upgraded by the normal migration when it is opened next.
func ApplyStaged(ctx context.Context, dataDir string) error {
	staging := filepath.Join(dataDir, StagingDir)
	if _, err := os.Stat(staging); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(staging, ReadyMarker)); err != nil {
		slog.Info("relocate: removing an incomplete import staging area", "dir", staging)
		if err := os.RemoveAll(staging); err != nil {
			slog.Warn("relocate: remove staging area", "dir", staging, "err", err)
		}
		return nil
	}
	stamp := time.Now().Format("20060102-150405")
	backups := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		return &ApplyError{Restored: true, Err: err}
	}
	type move struct{ from, to string }
	var done []move
	undo := func(cause error) error {
		for i := len(done) - 1; i >= 0; i-- {
			if err := rename(done[i].to, done[i].from); err != nil {
				slog.Error("relocate: undo import step", "from", done[i].to, "to", done[i].from, "err", err)
				return &ApplyError{Restored: false, Err: fmt.Errorf("%w; undo %s: %v", cause, done[i].to, err)}
			}
		}
		os.Remove(filepath.Join(staging, ReadyMarker))
		return &ApplyError{Restored: true, Err: cause}
	}
	step := func(from, to string) error {
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err := rename(from, to); err != nil {
			return err
		}
		done = append(done, move{from, to})
		return nil
	}
	cur := filepath.Join(dataDir, DBName)
	bak := filepath.Join(backups, "lectureflow-before-import-"+stamp+".db")
	for _, sfx := range append([]string{""}, dbSidecars...) {
		if err := step(cur+sfx, bak+sfx); err != nil {
			return undo(err)
		}
	}
	for _, sub := range importDirs {
		if err := step(filepath.Join(dataDir, sub), filepath.Join(backups, sub+"-before-import-"+stamp)); err != nil {
			return undo(err)
		}
	}
	if err := step(filepath.Join(staging, DBName), cur); err != nil {
		return undo(err)
	}
	for _, sub := range importDirs {
		if err := step(filepath.Join(staging, sub), filepath.Join(dataDir, sub)); err != nil {
			return undo(err)
		}
	}
	if err := os.RemoveAll(staging); err != nil {
		slog.Warn("relocate: remove staging area after import", "dir", staging, "err", err)
	}
	slog.Info("relocate: imported data applied", "backup", bak)
	scrubBackup(ctx, bak)
	return nil
}

// secretKVKeys are the kv rows the database an import replaced must not
// keep in backups/ (nor the one a move leaves behind): the Colab credential
// (colab.KVCredential: a refresh_token and client_secret; v3 Gate round 1
// item 9) and the other kv rows the Store keeps sealed (db.SealedKVKeys).
// db.ScrubSecrets also empties the providers' API keys (sec2-app L5).
var secretKVKeys = db.SealedKVKeys()

// scrubBackup removes the credentials (secretKVKeys and the providers' API
// keys) from the replaced database kept at path (with its -wal), leaving no
// copy in free pages or the WAL (db.ScrubSecrets: secure_delete, VACUUM,
// TRUNCATE checkpoint; closing the last connection removes the WAL). A
// failure is logged and the import stands: the swap is complete, and the
// backup stays where it is.
func scrubBackup(ctx context.Context, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	d, err := openDB(path, false)
	if err != nil {
		slog.Warn("relocate: opening the replaced database to remove its credentials", "path", path, "err", err)
		return
	}
	defer d.Close()
	if err := db.ScrubSecrets(ctx, d, secretKVKeys...); err != nil {
		slog.Warn("relocate: removing the credentials from the replaced database", "path", path, "err", err)
	}
}

// ImportBackupGlob matches, in a data directory, the databases imports
// replaced (ApplyStaged; with -wal / -shm / -journal beside them).
const ImportBackupGlob = "lectureflow-before-import-*.db"

// ScrubImportBackups removes the credentials from every database an import
// replaced in dataDir/backups (sec3 L1: 3.1 and earlier only removed the
// Colab credential from them, the providers' API keys stayed in the
// clear), the same way as scrubBackup. It returns how many it scrubbed;
// the first failure is returned after trying the others. A missing
// backups/ is nothing to do.
func ScrubImportBackups(ctx context.Context, dataDir string) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "backups", ImportBackupGlob))
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, p := range paths {
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := scrubDB(ctx, p); err != nil {
			if first == nil {
				first = fmt.Errorf("%s: %w", filepath.Base(p), err)
			}
			continue
		}
		n++
	}
	return n, first
}

// scrubDB opens the database at path and removes its credentials
// (db.ScrubSecrets).
func scrubDB(ctx context.Context, path string) error {
	d, err := openDB(path, false)
	if err != nil {
		return err
	}
	if err := db.ScrubSecrets(ctx, d, secretKVKeys...); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// resealImported brings the staged copy's credentials to this
// installation's data key (cred-enc): plain ones (an older install) are
// sealed, ones sealed with the imported directory's key are sealed again
// when that key opens here (secrets.PeekKeyFile: the same account on this
// computer). Credentials no key opens stay as they are: the providers ask
// for their key again, Colab for a new authorisation. Without a data key
// (a failed key setup) nothing changes; the next start seals them.
func resealImported(ctx context.Context, d *sql.DB, src string) error {
	var extra [][]byte
	if k, err := secrets.PeekKeyFile(src); err == nil {
		extra = append(extra, k)
	}
	res, err := db.SealSecrets(ctx, d, extra...)
	if err != nil {
		return err
	}
	if res.Changed() || res.Unreadable > 0 {
		slog.Info("relocate: imported credentials sealed with this installation's key",
			"sealed", res.Sealed, "resealed", res.Resealed, "unreadable", res.Unreadable)
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Before the database is opened for use (test-upgrade F1, F2; NOTES
// "决定（升级前备份与降级保护：core-builder，2026-10-27）"):
//
//   - a database from a newer LectureFlow (user_version above
//     db.SchemaVersion) is refused, untouched (startup.data_dir_newer);
//   - before the migrations of an older one run, it is copied to
//     backups/lectureflow-before-upgrade-schema<N>-to-<version>-<time>.db
//     without any credential (db.SnapshotWithoutSecrets), once per upgrade
//     (kv upgrade.backup), keeping the newest keepUpgradeBackups; without
//     the space for it, or when the copy fails, nothing is migrated and the
//     start fails (startup.upgrade_backup_no_space / _failed);
//   - then the migrations run (step "migrate"). The credentials are sealed
//     after this (setupSecrets): the backup comes before every change.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/relocate"
)

const (
	// upgradeBackupPrefix starts the names of the backups made before an
	// upgrade, in {data_dir}/backups.
	upgradeBackupPrefix = "lectureflow-before-upgrade-"
	// keepUpgradeBackups: the newest ones kept; older ones are removed when
	// a new one is made.
	keepUpgradeBackups = 3
	// upgradeSpaceMargin: free space wanted beyond the copies themselves.
	upgradeSpaceMargin = 64 << 20
)

// upgradeNow is the clock of the backup names (tests replace it).
var upgradeNow = time.Now

// diskFreeFn is diskFree (tests replace it).
var diskFreeFn = diskFree

func dbFile(dataDir string) string { return filepath.Join(dataDir, relocate.DBName) }

func backupsDir(dataDir string) string { return filepath.Join(dataDir, "backups") }

// checkNotNewer refuses a database from a newer LectureFlow before anything
// else touches the data directory (a staged import is not applied either).
// A database that cannot be read here is left to prepareDatabase.
func checkNotNewer(ctx context.Context, dataDir string, prog *progress) error {
	path := dbFile(dataDir)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	info, err := inspect(ctx, path, prog)
	if err != nil {
		return nil
	}
	if known := db.SchemaVersion(); info.Version > known {
		return newerFailure(dataDir, info.Version, known, nil)
	}
	return nil
}

// prepareDatabase runs the steps above on {dataDir}/lectureflow.db; a
// missing database (a new data directory) is created later by the
// application.
func prepareDatabase(ctx context.Context, dataDir, appVersion string, prog *progress) error {
	path := dbFile(dataDir)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	bdir := backupsDir(dataDir)
	removePartialBackups(bdir)
	info, err := inspect(ctx, path, prog)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	known := db.SchemaVersion()
	if info.Version > known {
		return newerFailure(dataDir, info.Version, known, nil)
	}
	if info.Version == known || info.Version == 0 { // nothing to migrate, or nothing in it yet
		return nil
	}
	if name, ok := upgradeBackupDone(info.UpgradeBackup, known, bdir); ok {
		slog.Info("upgrade: the backup made before this upgrade is there already; not made again", "file", filepath.Join(bdir, name))
	} else if err := backupBeforeUpgrade(ctx, path, bdir, info, known, appVersion, prog); err != nil {
		return err
	}
	slog.Info("upgrade: migrating the database", "from", info.Version, "to", known)
	end := prog.step(stepMigrate)
	d, err := db.OpenDB(path)
	end()
	if err != nil {
		var ne *db.NewerSchemaError
		if errors.As(err, &ne) {
			return newerFailure(dataDir, ne.Found, ne.Known, err)
		}
		return fmt.Errorf("open db: %w", err)
	}
	return d.Close()
}

// inspect is db.InspectFile as a step: the first connection after a start
// that was killed in the middle of a VACUUM reads the whole WAL it left
// (as large as the database) before it answers.
func inspect(ctx context.Context, path string, prog *progress) (db.FileInfo, error) {
	end := prog.step(stepCheck)
	defer end()
	return db.InspectFile(ctx, path)
}

// upgradeBackupDone: kv upgrade.backup names a backup made for an upgrade
// to known that is still in bdir (a migration failed or was interrupted
// after it was made).
func upgradeBackupDone(mark string, known int, bdir string) (string, bool) {
	to, name, ok := strings.Cut(mark, "|")
	if !ok || to != strconv.Itoa(known) || !strings.HasPrefix(name, upgradeBackupPrefix) || filepath.Base(name) != name {
		return "", false
	}
	fi, err := os.Lstat(filepath.Join(bdir, name))
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return name, true
}

// backupBeforeUpgrade makes the backup and records it.
func backupBeforeUpgrade(ctx context.Context, path, bdir string, info db.FileInfo, known int, appVersion string, prog *progress) error {
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return backupFailure(bdir, err)
	}
	// the copy; with credentials a second copy without them (the first is
	// removed after); the sealing's VACUUM afterwards needs about the same
	need := info.LiveBytes + upgradeSpaceMargin
	if info.HasSecrets {
		need += info.LiveBytes
	}
	if free, err := diskFreeFn(bdir); err == nil && free < need {
		return spaceFailure(bdir, need, free, nil)
	}
	name := upgradeBackupName(bdir, info.Version, appVersion, upgradeNow())
	dst := filepath.Join(bdir, name)
	slog.Info("upgrade: backing up the database before the upgrade", "file", dst, "schema", info.Version, "to", known)
	end := prog.step(stepUpgradeBackup)
	bctx, cancel := context.WithTimeout(ctx, slowTimeout(2*info.LiveBytes))
	err := db.SnapshotWithoutSecrets(bctx, path, dst)
	cancel()
	end()
	if err != nil {
		if isDiskFull(err) {
			free, _ := diskFreeFn(bdir)
			return spaceFailure(bdir, need, free, err)
		}
		return backupFailure(bdir, err)
	}
	slog.Info("upgrade: database backed up (without the API keys and the Colab authorisation)", "file", dst)
	if err := db.SetUpgradeBackup(ctx, path, strconv.Itoa(known)+"|"+name); err != nil {
		// the next start makes another backup if this upgrade does not finish
		slog.Warn("upgrade: recording the backup", "err", err)
	}
	rotateUpgradeBackups(bdir, name)
	return nil
}

var unsafeVersionChars = regexp.MustCompile(`[^0-9A-Za-z.+-]`)

// upgradeBackupName: lectureflow-before-upgrade-schema<N>-to-<version>-<YYYYMMDD-HHMMSS>.db,
// with -2, -3… when the name is taken.
func upgradeBackupName(bdir string, schema int, appVersion string, now time.Time) string {
	v := unsafeVersionChars.ReplaceAllString(appVersion, "_")
	if v == "" {
		v = "dev"
	}
	base := fmt.Sprintf("%sschema%d-to-%s-%s", upgradeBackupPrefix, schema, v, now.Format("20060102-150405"))
	name := base + ".db"
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(bdir, name)); errors.Is(err, os.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s-%d.db", base, i)
	}
}

// rotateUpgradeBackups removes all but the newest keepUpgradeBackups
// upgrade backups in bdir (by modification time); keep stays.
func rotateUpgradeBackups(bdir, keep string) {
	paths, err := filepath.Glob(filepath.Join(bdir, upgradeBackupPrefix+"*.db"))
	if err != nil {
		return
	}
	type entry struct {
		name string
		mod  time.Time
	}
	var list []entry
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		list = append(list, entry{fi.Name(), fi.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool {
		if ki, kj := list[i].name == keep, list[j].name == keep; ki != kj {
			return ki
		}
		if !list[i].mod.Equal(list[j].mod) {
			return list[i].mod.After(list[j].mod)
		}
		return list[i].name > list[j].name
	})
	for i, e := range list {
		if i < keepUpgradeBackups {
			continue
		}
		if err := db.RemoveDBFiles(filepath.Join(bdir, e.name)); err != nil {
			slog.Warn("upgrade: removing an old upgrade backup", "file", e.name, "err", err)
			continue
		}
		slog.Info("upgrade: removed an old upgrade backup", "file", e.name, "kept", keepUpgradeBackups)
	}
}

// removePartialBackups removes the unfinished copies a start that was
// stopped while backing up left (they may still hold the credentials).
func removePartialBackups(bdir string) {
	paths, _ := filepath.Glob(filepath.Join(bdir, upgradeBackupPrefix+"*"+db.PartialSuffix+"*"))
	for _, p := range paths {
		if err := os.Remove(p); err == nil {
			slog.Info("upgrade: removed an unfinished backup an interrupted start left", "file", filepath.Base(p))
		}
	}
}

// isDiskFull: SQLITE_FULL, or the system's "disk full".
func isDiskFull(err error) bool {
	var ce interface{ Code() int }
	if errors.As(err, &ce) && ce.Code()&0xff == 13 { // SQLITE_FULL
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "database or disk is full") || strings.Contains(s, "no space left") || strings.Contains(s, "not enough space on the disk")
}

// humanBytes: "3.1 GB", "640 MB".
func humanBytes(n int64) string {
	const gb, mb = 1 << 30, 1 << 20
	if n >= gb {
		return strconv.FormatFloat(float64(n)/gb, 'f', 1, 64) + " GB"
	}
	m := (n + mb - 1) / mb
	if m < 1 {
		m = 1
	}
	return strconv.FormatInt(m, 10) + " MB"
}

func newerFailure(dataDir string, found, known int, cause error) *startFailure {
	b := backupsDir(dataDir)
	return &startFailure{
		Code:   failDataDirNewer,
		Params: map[string]any{"found": found, "known": known, "backups": b},
		Err:    cause,
		Msg: fmt.Sprintf("这个数据目录来自更新版本的 LectureFlow（数据库版本 %d，本程序最高支持 %d），为了不损坏数据没有打开它。请安装更新的版本；要回到这个版本，先恢复升级前的备份（%s）。 / "+
			"This data directory comes from a newer LectureFlow (database version %d; this version knows up to %d) and was left untouched. Install the newer version; to go back to this one, first restore the backup made before the upgrade (%s).",
			found, known, b, found, known, b),
	}
}

func spaceFailure(bdir string, need, free int64, cause error) *startFailure {
	return &startFailure{
		Code:   failUpgradeBackupSpace,
		Params: map[string]any{"dir": bdir, "need_bytes": need, "free_bytes": free},
		Err:    cause,
		Msg: fmt.Sprintf("升级前要先备份数据库，需要约 %s 的空闲空间，%s 所在的磁盘只剩 %s；数据库没有升级，也没有改动。请清理出空间后重试。 / "+
			"Backing up the database before the upgrade needs about %s of free space; the disk of %s has %s. Nothing was upgraded or changed. Free some space and try again.",
			humanBytes(need), bdir, humanBytes(free), humanBytes(need), bdir, humanBytes(free)),
	}
}

func backupFailure(bdir string, cause error) *startFailure {
	return &startFailure{
		Code:   failUpgradeBackupFailed,
		Params: map[string]any{"dir": bdir},
		Err:    cause,
		Msg: fmt.Sprintf("升级前备份数据库没有成功，数据库没有升级，也没有改动（备份目录 %s）：%v / "+
			"Backing up the database before the upgrade failed; nothing was upgraded or changed (backup folder %s): %v",
			bdir, cause, bdir, cause),
	}
}

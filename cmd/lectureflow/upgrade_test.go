// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/secrets"
)

// Test values only (never real keys).
const (
	upgKey     = "sk-test-upgrade-cmd-5150"
	upgRefresh = "1//test-refresh-upgrade-cmd-61"
)

// buildOldDB writes a database of schema version upTo at path, with the
// repository's migration files (as an older LectureFlow left it): a
// course, a provider with a plain API key, a plain Colab credential.
func buildOldDB(t *testing.T, path string, upTo int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(path)
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath()+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	dir := filepath.Join("..", "..", "internal", "db", "migrations")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, n := range names {
		v, _ := strconv.Atoi(n[:3])
		if v > upTo {
			break
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO course (id, name, created_at) VALUES ('c1','CSC3631','2026-10-01T00:00:00Z')`,
		`INSERT INTO provider (id, name, type, base_url, api_key, created_at) VALUES ('p1','DeepSeek','openai','https://api.deepseek.com','` + upgKey + `','2026-10-01T00:00:00Z')`,
		`INSERT INTO kv (key, value) VALUES ('colab.credential','{"refresh_token":"` + upgRefresh + `"}')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func schemaOf(t *testing.T, path string) db.FileInfo {
	t.Helper()
	fi, err := db.InspectFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func upgradeBackups(t *testing.T, dataDir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dataDir, "backups", upgradeBackupPrefix+"*"))
	var out []string
	for _, p := range m {
		out = append(out, filepath.Base(p))
	}
	return out
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// failureOf returns the code line and the last line of stderr.
func failureOf(t *testing.T, stderr string) (code string, params map[string]any, last string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	last = lines[len(lines)-1]
	for i, l := range lines {
		if !strings.HasPrefix(l, FailurePrefix) {
			continue
		}
		if i != len(lines)-2 {
			t.Fatalf("the failure line is not just before the ERROR line:\n%s", stderr)
		}
		var f struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, FailurePrefix)), &f); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		return f.Code, f.Params, last
	}
	return "", nil, last
}

// F1: the first start of a new version on an older database copies it to
// backups/ (no credential in the copy, the old schema version and rows)
// before it migrates and seals; a later start makes no other copy; an
// unfinished copy a killed start left is removed.
func TestUpgradeBackupBeforeMigration(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	stale := filepath.Join(dataDir, "backups", upgradeBackupPrefix+"schema9-to-x-20261001-000000.db"+db.PartialSuffix)
	os.MkdirAll(filepath.Dir(stale), 0o755)
	if err := os.WriteFile(stale, []byte("plain "+upgKey), 0o600); err != nil {
		t.Fatal(err)
	}

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-app-version", "3.2.0"}, "", AppOptions{})
	p.ready()
	<-p.app
	baks := upgradeBackups(t, dataDir)
	if len(baks) != 1 || !strings.HasPrefix(baks[0], upgradeBackupPrefix+"schema9-to-3.2.0-") || !strings.HasSuffix(baks[0], ".db") {
		t.Fatalf("backups %v", baks)
	}
	bak := filepath.Join(dataDir, "backups", baks[0])
	for _, needle := range []string{upgKey, upgRefresh, "enc:v1:"} {
		if fileHolds(t, bak, needle) {
			t.Fatalf("the backup holds %q", needle)
		}
	}
	bi := schemaOf(t, bak)
	if bi.Version != 9 || bi.HasSecrets {
		t.Fatalf("backup %+v", bi)
	}
	mi := schemaOf(t, path)
	if mi.Version != db.SchemaVersion() || mi.UpgradeBackup != fmt.Sprintf("%d|%s", db.SchemaVersion(), baks[0]) {
		t.Fatalf("main %+v", mi)
	}
	p.sigc <- os.Interrupt
	if c := p.wait(15 * time.Second); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	if fileHolds(t, path, upgKey) {
		t.Fatal("the database still holds the key in the clear")
	}

	secrets.ResetForTest()
	p = startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-app-version", "3.2.1"}, "", AppOptions{})
	p.ready()
	<-p.app
	if got := upgradeBackups(t, dataDir); len(got) != 1 {
		t.Fatalf("a start with nothing to migrate made a backup: %v", got)
	}
}

// F1: without the space for the backup, nothing is migrated: the start
// fails with startup.upgrade_backup_no_space, the database is unchanged.
func TestUpgradeBackupNoSpace(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	old := diskFreeFn
	diskFreeFn = func(string) (int64, error) { return 10 << 20, nil }
	t.Cleanup(func() { diskFreeFn = old })
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := p.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("exit %d", c)
	}
	code, params, last := failureOf(t, p.stderr.String())
	if code != failUpgradeBackupSpace || params["dir"] != filepath.Join(dataDir, "backups") || params["free_bytes"].(float64) != 10<<20 ||
		params["need_bytes"].(float64) <= 64<<20 {
		t.Fatalf("failure %q %v", code, params)
	}
	if !strings.HasPrefix(last, "ERROR: ") || !strings.Contains(last, "空闲空间") || !strings.Contains(last, "free space") {
		t.Fatalf("ERROR line %q", last)
	}
	if fi := schemaOf(t, path); fi.Version != 9 || fi.UpgradeBackup != "" {
		t.Fatalf("the database changed: %+v", fi)
	}
	if !fileHolds(t, path, upgKey) {
		t.Fatal("the credentials changed (sealed) although nothing should have")
	}
	if b := upgradeBackups(t, dataDir); len(b) != 0 {
		t.Fatalf("files in backups: %v", b)
	}
}

// F1: a backup that cannot be written stops the start before the migration.
func TestUpgradeBackupFailureStopsUpgrade(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	if err := os.WriteFile(filepath.Join(dataDir, "backups"), []byte("not a folder"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := p.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("exit %d", c)
	}
	code, _, last := failureOf(t, p.stderr.String())
	if code != failUpgradeBackupFailed || !strings.Contains(last, "没有升级") {
		t.Fatalf("failure %q / %q", code, last)
	}
	if fi := schemaOf(t, path); fi.Version != 9 {
		t.Fatalf("migrated without a backup: %+v", fi)
	}
}

// F2: a database from a newer LectureFlow: the start is refused, the
// database file is not changed, no staged import is applied, no key file
// is made. Web mode has the reason in Chinese and English, no code line.
func TestNewerDatabaseRefused(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	cfgPath, dataDir := writeTestConfig(t)
	path := filepath.Join(dataDir, "lectureflow.db")
	os.MkdirAll(dataDir, 0o755)
	d, err := db.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	newer := db.SchemaVersion() + 1
	if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", newer)); err != nil {
		t.Fatal(err)
	}
	d.Close()
	before := fileSum(t, path)
	staged := false
	oldStaged := applyStaged
	applyStaged = func(context.Context, string) error { staged = true; return nil }
	t.Cleanup(func() { applyStaged = oldStaged })

	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := p.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("exit %d", c)
	}
	code, params, last := failureOf(t, p.stderr.String())
	if code != failDataDirNewer || params["found"].(float64) != float64(newer) || params["known"].(float64) != float64(db.SchemaVersion()) ||
		params["backups"] != filepath.Join(dataDir, "backups") {
		t.Fatalf("failure %q %v", code, params)
	}
	if !strings.Contains(last, "更新版本") || !strings.Contains(last, "newer LectureFlow") {
		t.Fatalf("ERROR line %q", last)
	}
	if fileSum(t, path) != before {
		t.Fatal("the database file changed")
	}
	if staged {
		t.Fatal("the staged import was applied")
	}
	if _, err := os.Stat(filepath.Join(dataDir, secrets.KeyFileName)); err == nil {
		t.Fatal("a key file was made")
	}

	w := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	if c := w.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("web mode exit %d", c)
	}
	if code, _, last := failureOf(t, w.stderr.String()); code != "" || !strings.Contains(last, "newer LectureFlow") {
		t.Fatalf("web mode: %q %q", code, last)
	}
}

// F1: a migration that failed after the backup was made: the next start
// finds the backup recorded and makes no other one (the oldest state is
// the one worth keeping).
func TestUpgradeBackupOncePerUpgrade(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "lectureflow.db")
	buildOldDB(t, path, 9)
	bdir := filepath.Join(dataDir, "backups")
	os.MkdirAll(bdir, 0o755)
	name := upgradeBackupPrefix + "schema9-to-3.2.0-20261027-100000.db"
	if err := os.WriteFile(filepath.Join(bdir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUpgradeBackup(ctx, path, fmt.Sprintf("%d|%s", db.SchemaVersion(), name)); err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabase(ctx, dataDir, "3.2.0", nil); err != nil {
		t.Fatal(err)
	}
	if got := upgradeBackups(t, dataDir); len(got) != 1 || got[0] != name {
		t.Fatalf("backups %v", got)
	}
	if fi := schemaOf(t, path); fi.Version != db.SchemaVersion() {
		t.Fatalf("not migrated: %+v", fi)
	}

	// a mark for another upgrade, or naming a file that is gone: a new backup
	dataDir2 := t.TempDir()
	path2 := filepath.Join(dataDir2, "lectureflow.db")
	buildOldDB(t, path2, 9)
	if err := db.SetUpgradeBackup(ctx, path2, fmt.Sprintf("%d|%s", db.SchemaVersion(), name)); err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabase(ctx, dataDir2, "3.2.0", nil); err != nil {
		t.Fatal(err)
	}
	if got := upgradeBackups(t, dataDir2); len(got) != 1 || got[0] == name {
		t.Fatalf("backups %v", got)
	}
}

func TestRotateUpgradeBackups(t *testing.T) {
	bdir := t.TempDir()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var names []string
	for i := 0; i < 5; i++ {
		n := fmt.Sprintf("%sschema%d-to-3.2.0-2026100%d-120000.db", upgradeBackupPrefix, 9, i+1)
		p := filepath.Join(bdir, n)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(p+"-wal", []byte("w"), 0o600)
		os.Chtimes(p, base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i)*time.Hour))
		names = append(names, n)
	}
	other := filepath.Join(bdir, "lectureflow-before-import-20261001-120000.db")
	os.WriteFile(other, []byte("x"), 0o600)
	// the one just made is kept even with an older time
	os.Chtimes(filepath.Join(bdir, names[0]), base.Add(-time.Hour), base.Add(-time.Hour))
	rotateUpgradeBackups(bdir, names[0])
	var left []string
	ents, _ := os.ReadDir(bdir)
	for _, e := range ents {
		left = append(left, e.Name())
	}
	want := []string{filepath.Base(other), names[0], names[3], names[3] + "-wal", names[4], names[4] + "-wal", names[0] + "-wal"}
	sort.Strings(want)
	sort.Strings(left)
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Fatalf("left %v\nwant %v", left, want)
	}
}

func TestUpgradeBackupName(t *testing.T) {
	bdir := t.TempDir()
	now := time.Date(2026, 10, 27, 15, 30, 12, 0, time.Local)
	n := upgradeBackupName(bdir, 9, "3.2.0", now)
	if n != "lectureflow-before-upgrade-schema9-to-3.2.0-20261027-153012.db" {
		t.Fatal(n)
	}
	os.WriteFile(filepath.Join(bdir, n), nil, 0o600)
	if n2 := upgradeBackupName(bdir, 9, "3.2.0", now); n2 != "lectureflow-before-upgrade-schema9-to-3.2.0-20261027-153012-2.db" {
		t.Fatal(n2)
	}
	if n3 := upgradeBackupName(bdir, 4, `..\x/y z`, now); n3 != "lectureflow-before-upgrade-schema4-to-.._x_y_z-20261027-153012.db" {
		t.Fatal(n3)
	}
}

// F3: in desktop mode the long steps before the ready line print
// LECTUREFLOW_STARTING lines (the shell waits while they come); none come
// after the ready line, and web mode prints none.
func TestStartingLinesDuringLongSteps(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	oldFirst := heartbeatFirst
	heartbeatFirst = 0 // the first line at once: no race with a quick step
	t.Cleanup(func() { heartbeatFirst = oldFirst })
	cfgPath, dataDir := writeTestConfig(t)
	buildOldDB(t, filepath.Join(dataDir, "lectureflow.db"), 9)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	p.ready()
	<-p.app
	out := p.stdout()
	ready := strings.Index(out, ReadyPrefix)
	for _, step := range []string{stepUpgradeBackup, stepMigrate, stepSeal} {
		i := strings.Index(out, StartingPrefix+step+"\n")
		if i < 0 || i > ready {
			t.Fatalf("no %q line before the ready line:\n%s", step, out)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(p.stdout()[ready:], StartingPrefix) {
		t.Fatal("a starting line after the ready line")
	}

	secrets.ResetForTest()
	cfg2, dataDir2 := writeTestConfig(t)
	buildOldDB(t, filepath.Join(dataDir2, "lectureflow.db"), 9)
	w := startProc(t, []string{"-config", cfg2, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	w.ready()
	<-w.app
	if strings.Contains(w.stdout(), StartingPrefix) {
		t.Fatal("starting lines in web mode")
	}
}

type lineBuf struct {
	mu    sync.Mutex
	lines []string
}

func (b *lineBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, strings.Split(strings.TrimRight(string(p), "\n"), "\n")...)
	return len(p), nil
}

func (b *lineBuf) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.lines)
}

func TestProgressStep(t *testing.T) {
	oldFirst, oldEvery := heartbeatFirst, heartbeatEvery
	heartbeatFirst, heartbeatEvery = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { heartbeatFirst, heartbeatEvery = oldFirst, oldEvery })
	var b lineBuf
	p := &progress{w: &b}
	end := p.step("quick")
	end()
	end() // twice is fine
	if b.count() != 0 {
		t.Fatalf("a step shorter than the first beat printed %v", b.lines)
	}
	end = p.step("long")
	time.Sleep(150 * time.Millisecond)
	end()
	n := b.count()
	if n < 3 {
		t.Fatalf("%d lines", n)
	}
	for _, l := range b.lines {
		if l != StartingPrefix+"long" {
			t.Fatalf("line %q", l)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if b.count() != n {
		t.Fatal("a line after the step ended")
	}
	var nilProg *progress
	nilProg.step("x")() // nil-safe
}

func TestSlowTimeoutGrowsWithSize(t *testing.T) {
	if slowTimeout(0) < 10*time.Minute || slowTimeout(3<<30) < slowTimeout(0)+50*time.Minute {
		t.Fatalf("%v %v", slowTimeout(0), slowTimeout(3<<30))
	}
	if !isDiskFull(errors.New("database or disk is full (13)")) || isDiskFull(errors.New("database is locked (5)")) {
		t.Fatal("isDiskFull")
	}
	if humanBytes(3<<30+(1<<29)) != "3.5 GB" || humanBytes(5<<20) != "5 MB" || humanBytes(1) != "1 MB" {
		t.Fatal(humanBytes(3<<30+(1<<29)), humanBytes(5<<20))
	}
}

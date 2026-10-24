// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The settings page's "App" and "About" sections (SPEC-v3 V11, V7):
//
//	GET  /api/app/storage      the data directory, its size and the free space
//	POST /api/app/open-logs    opens {data_dir}/logs in the system file manager
//	POST /api/app/diagnostics  a zip of recent logs and environment facts
//	GET  /api/app/license      LICENSE (text/plain)
//	GET  /api/app/notices      THIRD_PARTY_NOTICES.md (text/markdown)
//
// The diagnostics never contain lecture content or keys: logs/llm/ (prompts
// and transcript text) is left out, every file is passed through redact()
// (key shapes, Bearer / Authorization / X-LF-Token values, long hex runs,
// the configured provider keys and desktop token, user names in paths) and
// the settings carry no key hint.

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	lectureflow "lectureflow"
	"lectureflow/internal/hwdetect"
)

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/app/storage", s.getAppStorage)
		h("POST /api/app/open-logs", s.postOpenLogs)
		h("POST /api/app/diagnostics", s.postDiagnostics)
		h("GET /api/app/license", s.getLicense)
		h("GET /api/app/notices", s.getNotices)
	})
}

// diagLogBudget caps the log bytes of a diagnostics zip (before redaction).
const diagLogBudget = 5 << 20

// Injectable in tests (no file manager windows, no hardware probes).
var (
	// openFolder shows dir in the system file manager.
	openFolder = openFolderSystem
	// diagHardware is the hardware section of the diagnostics.
	diagHardware = func(ctx context.Context, dataDir string) (any, error) {
		hw, err := hwdetect.Detect(ctx, dataDir)
		if err != nil {
			return nil, err
		}
		return map[string]any{"hw": hw, "recommendation": hwdetect.Recommend(hw)}, nil
	}
)

// DiagSection adds one JSON file to the diagnostics zip; v3 files that own
// a subsystem (components, Colab) register theirs from init. ok = false
// leaves the file out (the subsystem is not available).
type DiagSection struct {
	Name    string // file name inside the zip, e.g. "components.json"
	Collect func(ctx context.Context, s *Server) (v any, ok bool)
}

var diagSections []DiagSection

// ---- GET /api/app/storage ----

// AppStorage is GET /api/app/storage.
type AppStorage struct {
	DataDir   string `json:"data_dir"`   // absolute
	UsedBytes int64  `json:"used_bytes"` // regular files under the data directory
	FreeBytes int64  `json:"free_bytes"` // -1 = unknown
}

// The size walk is cached briefly: the page asks on every render.
var storageCache struct {
	sync.Mutex
	dir  string
	at   time.Time
	used int64
}

func (s *Server) absDataDir() string {
	if s.dataDir == "" {
		return ""
	}
	if a, err := filepath.Abs(s.dataDir); err == nil {
		return a
	}
	return s.dataDir
}

func dirBytes(ctx context.Context, root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries do not count
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (s *Server) getAppStorage(w http.ResponseWriter, r *http.Request) {
	dir := s.absDataDir()
	out := AppStorage{DataDir: dir, FreeBytes: -1}
	if dir != "" {
		storageCache.Lock()
		if storageCache.dir == dir && time.Since(storageCache.at) < 10*time.Second {
			out.UsedBytes = storageCache.used
		} else {
			out.UsedBytes = dirBytes(r.Context(), dir)
			if r.Context().Err() == nil {
				storageCache.dir, storageCache.at, storageCache.used = dir, time.Now(), out.UsedBytes
			}
		}
		storageCache.Unlock()
		if s.freeSpace != nil {
			if free, err := s.freeSpace(dir); err == nil {
				out.FreeBytes = free
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- POST /api/app/open-logs ----

// crossSite refuses a request another site's page sent (a form post needs
// no CORS preflight); the desktop token already covers desktop mode.
func crossSite(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, http.StatusForbidden, "app.cross_site", nil)
		return true
	}
	return false
}

// POST /api/app/open-logs: creates {data_dir}/logs when missing and opens it
// (explorer, open, xdg-open). Works in web mode too: the server only
// listens on this computer. 500 app.open_logs_failed {path}.
func (s *Server) postOpenLogs(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) {
		return
	}
	dir := filepath.Join(s.absDataDir(), "logs")
	if s.dataDir == "" {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "data_dir"})
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.fail(w, r, apiErrDetail(http.StatusInternalServerError, "app.open_logs_failed", map[string]any{"path": dir}, err.Error()))
		return
	}
	if err := openFolder(dir); err != nil {
		s.fail(w, r, apiErrDetail(http.StatusInternalServerError, "app.open_logs_failed", map[string]any{"path": dir}, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

func openFolderSystem(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// explorer.exe exits with status 1 even when it opened the window: only
	// a failure to start counts; reap the process in the background.
	go func() { _ = cmd.Wait() }()
	return nil
}

// ---- GET /api/app/license, /api/app/notices ----

func writeText(w http.ResponseWriter, contentType, text string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}

func (s *Server) getLicense(w http.ResponseWriter, r *http.Request) {
	// The licence and its section 7 additional permission (LICENSE-EXCEPTION.md).
	writeText(w, "text/plain; charset=utf-8", lectureflow.License+"\n\n"+lectureflow.LicenseException)
}

func (s *Server) getNotices(w http.ResponseWriter, r *http.Request) {
	writeText(w, "text/markdown; charset=utf-8", lectureflow.ThirdPartyNotices)
}

// ---- POST /api/app/diagnostics ----

// diagReadme explains the zip to whoever opens it.
const diagReadme = `LectureFlow diagnostics
=======================

Created by "Export diagnostics" on the settings page. You decide whether to
share it. It contains:

  info.json          version, platform, data directory (user name replaced)
  app-settings.json  application settings, providers without keys, roles,
                     transcription and general settings
  checks.json        the last environment checks of the diagnostics page
  hardware.json      the detected hardware and the recommended components
  database.json      the number of rows of each database table
  logs/              the most recent server logs (at most 5 MB)

It never contains lecture content (recordings, transcripts, translations,
course materials), the model request logs (logs/llm/) or API keys. Values
that look like keys or tokens are replaced by <redacted>, user names in
paths by <user>.
`

func (s *Server) postDiagnostics(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) {
		return
	}
	ctx := r.Context()
	now := time.Now()
	red := s.redactor()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte) error {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		_, err = f.Write(red.apply(data))
		return err
	}
	addJSON := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			b, _ = json.Marshal(map[string]string{"error": err.Error()})
		}
		return add(name, append(b, '\n'))
	}
	files := []struct {
		name string
		v    func() any
	}{
		{"info.json", func() any { return s.diagInfo(now) }},
		{"app-settings.json", func() any { return s.diagSettings(ctx) }},
		{"checks.json", func() any {
			if s.diag == nil {
				return []SystemCheck{}
			}
			return s.diag.snapshot()
		}},
		{"hardware.json", func() any {
			hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			v, err := diagHardware(hctx, s.absDataDir())
			if err != nil {
				return map[string]string{"error": err.Error()}
			}
			return v
		}},
		{"database.json", func() any { return s.diagTableCounts(ctx) }},
	}
	err := add("README.txt", []byte(diagReadme))
	for _, f := range files {
		if err == nil {
			err = addJSON(f.name, f.v())
		}
	}
	for _, sec := range diagSections {
		if err != nil {
			break
		}
		if v, ok := sec.Collect(ctx, s); ok {
			err = addJSON(sec.Name, v)
		}
	}
	if err == nil {
		err = s.diagLogs(add)
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := "lectureflow-diagnostics-" + now.Format("2006-01-02") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) diagInfo(now time.Time) map[string]any {
	return map[string]any{
		"version": s.appVersion(), "mode": s.mode(), "platform": runtime.GOOS, "arch": runtime.GOARCH,
		"go_version": runtime.Version(), "cpus": runtime.NumCPU(), "data_dir": s.absDataDir(),
		"created_at": now.Format(time.RFC3339),
	}
}

func (s *Server) diagSettings(ctx context.Context) map[string]any {
	out := map[string]any{"config": s.configView()}
	if a, err := s.appSettings(ctx); err == nil {
		out["app"] = a
	} else {
		out["app_error"] = err.Error()
	}
	if s.set != nil {
		v, err := s.settingsView(ctx)
		if err != nil {
			out["settings_error"] = err.Error()
		} else {
			for i := range v.Providers {
				v.Providers[i].KeyHint = "" // not even the last characters
			}
			out["settings"] = v
		}
	}
	return out
}

// diagTableCounts opens the database read-only and counts the rows of each
// table (no row content).
func (s *Server) diagTableCounts(ctx context.Context) map[string]any {
	path := filepath.Join(s.absDataDir(), "lectureflow.db")
	if _, err := os.Stat(path); err != nil {
		return map[string]any{"error": "database file not found"}
	}
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer d.Close()
	rows, err := d.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	counts := map[string]int64{}
	for _, n := range names {
		var c int64
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(n, `"`, `""`)+`"`).Scan(&c); err == nil {
			counts[n] = c
		}
	}
	var version int64
	_ = d.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	return map[string]any{"schema_version": version, "tables": counts}
}

// diagLogs adds the newest files under {data_dir}/logs (never a directory
// named llm) until diagLogBudget bytes; a file that does not fit whole
// contributes its last lines.
func (s *Server) diagLogs(add func(name string, data []byte) error) error {
	root := filepath.Join(s.absDataDir(), "logs")
	type logFile struct {
		path, rel string
		size      int64
		mod       time.Time
	}
	var list []logFile
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && strings.EqualFold(d.Name(), "llm") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		list = append(list, logFile{path: p, rel: filepath.ToSlash(rel), size: fi.Size(), mod: fi.ModTime()})
		return nil
	})
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	left := int64(diagLogBudget)
	for _, f := range list {
		if left <= 0 {
			break
		}
		data, err := readTail(f.path, f.size, left)
		if err != nil {
			continue
		}
		left -= int64(len(data))
		if int64(len(data)) < f.size { // cut to fit: the budget is spent
			left = 0
		}
		if err := add("logs/"+f.rel, data); err != nil {
			return err
		}
	}
	return nil
}

// readTail reads the whole file, or its last max bytes starting at a line.
func readTail(path string, size, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if size <= max {
		return io.ReadAll(io.LimitReader(f, max))
	}
	b := make([]byte, max)
	n, _ := f.ReadAt(b, size-max)
	b = b[:n]
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
		b = b[i+1:]
	}
	return append([]byte("[earlier lines omitted]\n"), b...), nil
}

// ---- redaction ----

type redactor struct {
	secrets []string // exact values, longest first
	user    *regexp.Regexp
}

var redactPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// header-like values first: the whole value up to the end of the field
	{regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)[^"'\r\n,}]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(x-lf-token["']?\s*[:=]\s*["']?)[^\s"',}]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=\-]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|access[_-]?token|secret|password|token)["']?\s*[:=]\s*["']?)[^\s"',}&]+`), "${1}<redacted>"},
	// key shapes: sk-…, sk-ant-…, and long hex runs (tokens, hashes)
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`), "sk-<redacted>"},
	{regexp.MustCompile(`\b[0-9A-Fa-f]{32,}\b`), "<redacted>"},
	// user names in home paths (C:\Users\x, C:\\Users\\x in JSON, /Users/x, /home/x)
	{regexp.MustCompile(`(?i)([A-Za-z]:(?:\\\\|\\|/)(?:Users|Documents and Settings)(?:\\\\|\\|/))[^\\/\s"'<>:]+`), "${1}<user>"},
	{regexp.MustCompile(`(/(?:Users|home)/)[^/\s"'<>:]+`), "${1}<user>"},
}

// redactor collects the secrets known to this server: provider keys, the
// desktop token and the values of *_API_KEY / *_TOKEN / *_SECRET variables.
func (s *Server) redactor() *redactor {
	var sec []string
	addSecret := func(v string) {
		if v = strings.TrimSpace(v); len(v) >= 8 {
			sec = append(sec, v)
		}
	}
	if s.set != nil {
		for _, p := range s.set.Providers() {
			addSecret(p.APIKey)
		}
	}
	if s.desktop != nil {
		addSecret(s.desktop.Token)
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		k = strings.ToUpper(k)
		if strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_TOKEN") || strings.HasSuffix(k, "_SECRET") {
			addSecret(v)
		}
	}
	sort.Slice(sec, func(i, j int) bool { return len(sec[i]) > len(sec[j]) })
	r := &redactor{secrets: sec}
	if name := currentUserName(); len(name) >= 3 {
		r.user = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
	}
	return r
}

func currentUserName() string {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if i := strings.LastIndexAny(name, `\/`); i >= 0 { // DOMAIN\name
		name = name[i+1:]
	}
	if name == "" {
		name = os.Getenv("USERNAME")
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	return name
}

func (r *redactor) apply(b []byte) []byte {
	s := string(b)
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "<redacted>")
	}
	for _, p := range redactPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	if r.user != nil {
		s = r.user.ReplaceAllString(s, "<user>")
	}
	return []byte(s)
}

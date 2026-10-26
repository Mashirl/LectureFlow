// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The in-app Google sign-in (3.1.0, .claude/v31-brief.md §2.2). Instead of
// asking the user to run GcloudLoginCommand in a terminal, the backend runs
//
//	gcloud auth application-default login --scopes=<GcloudLoginScopes> --disable-quota-project
//
// itself, with CLOUDSDK_CONFIG set to a configuration directory of its own
// ({data_dir}/colab/gcloud): the user's own gcloud configuration and ADC
// file are neither read nor changed. gcloud opens the system browser; the
// sign-in link it prints is kept (LoginStatus.URL) so the page can offer it
// when no browser window appeared. When gcloud exits successfully, the ADC
// file it wrote into that directory is imported exactly like "导入授权"
// (ImportADCFile: one token refresh, the scopes checked, the user's
// lifecycle), recorded as SourceApp (its disconnect does not touch the
// user's own gcloud), and the directory is then wiped. Cancelling, the
// timeout and the app quitting end gcloud's whole process tree
// (login_windows.go, login_unix.go) — but never a browser gcloud started.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lectureflow/internal/model"
)

// Login states (LoginStatus.State).
const (
	LoginIdle      = "idle"      // no sign-in since the app started
	LoginWaiting   = "waiting"   // gcloud runs: the user signs in in the browser
	LoginImporting = "importing" // gcloud succeeded: its credential is being imported
	LoginDone      = "done"      // imported; the credential is stored
	LoginFailed    = "failed"    // LoginStatus.Error says why
	LoginCancelled = "cancelled" // the user cancelled (or the app quit)
)

// DefaultLoginTimeout bounds the sign-in in the browser (gcloud is stopped
// afterwards: CodeLoginTimeout).
const DefaultLoginTimeout = 10 * time.Minute

const (
	// loginWaitDelay: after gcloud ended (or was stopped), how long its
	// output pipes may stay open (a process that inherited them).
	loginWaitDelay = 2 * time.Second
	// loginLineMax caps one line of gcloud's output (the sign-in link is
	// about 600 characters).
	loginLineMax = 16 << 10
	// loginDetailMax caps the ERROR line kept as the error's detail.
	loginDetailMax = 500
)

// LoginStatus is the state of the in-app sign-in (the body of the
// /api/colab/auth/login endpoints). It never holds a credential.
type LoginStatus struct {
	State string `json:"state"`
	// URL is the Google sign-in link gcloud printed ("" until then, or when
	// it printed none); only https://accounts.google.com links are kept.
	URL       string           `json:"url"`
	StartedAt *time.Time       `json:"started_at"`
	Error     *model.ErrorInfo `json:"error"`
}

// LoginOptions configure the sign-in (Backend.Login).
type LoginOptions struct {
	// ConfigDir is gcloud's configuration directory during the sign-in
	// (CLOUDSDK_CONFIG; the app uses {data_dir}/colab/gcloud). It must be
	// an absolute path whose last element is "gcloud": the whole directory
	// is removed before and after every sign-in.
	ConfigDir string
	// Gcloud returns the gcloud executable to run (the gcloud component's,
	// else one on PATH); false = none (CodeGcloudMissing). Called at every
	// start.
	Gcloud func() (string, bool)
	// OnChange is called after every change of the status (state, URL),
	// outside the Login's lock.
	OnChange func(LoginStatus)
	// Timeout of the sign-in in the browser (0 = DefaultLoginTimeout).
	Timeout time.Duration
	// Env returns the environment gcloud starts from (nil = os.Environ());
	// called at every start. The app passes config.Config.GcloudEnv (the
	// gcloud component's variables, e.g. CLOUDSDK_PYTHON on macOS). The
	// variables of the user's own gcloud configuration are removed from it
	// (loginEnv).
	Env    func() []string
	Logger *slog.Logger
	Now    func() time.Time

	// importADC replaces Backend.ImportADCFile (tests).
	importADC func(ctx context.Context, path string, lc Lifecycle) error
}

// Login runs the in-app sign-in, one at a time.
type Login struct {
	o   LoginOptions
	log *slog.Logger

	mu     sync.Mutex
	st     LoginStatus
	run    *loginRun // the sign-in in progress (nil = none)
	closed bool
}

// loginRun is one sign-in in progress.
type loginRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

var (
	errLoginCancelled = errors.New("colab: the sign-in was cancelled")
	errLoginClosed    = errors.New("colab: the app is closing")
	errLoginTimeout   = errors.New("colab: the sign-in timed out")
)

// Login returns the backend's in-app sign-in. The first call creates it
// with o (and removes what an interrupted earlier sign-in left in
// o.ConfigDir); later calls return the same one and ignore o.
func (b *Backend) Login(o LoginOptions) *Login {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	if b.login == nil {
		if o.importADC == nil {
			o.importADC = func(ctx context.Context, path string, lc Lifecycle) error {
				_, err := b.ImportADCFile(ctx, path, lc, SourceApp)
				return err
			}
		}
		if o.Logger == nil {
			o.Logger = b.log
		}
		if o.Now == nil {
			o.Now = b.o.Now
		}
		b.login = newLogin(o)
	}
	return b.login
}

// LoginActive reports whether an in-app sign-in runs now (gcloud waits for
// the browser, or its credential is being imported): the component manager
// must not remove gcloud meanwhile (cmd componentUse).
func (b *Backend) LoginActive() bool {
	b.loginMu.Lock()
	l := b.login
	b.loginMu.Unlock()
	if l == nil {
		return false
	}
	st := l.Status().State
	return st == LoginWaiting || st == LoginImporting
}

// closeLogin cancels a running sign-in (Backend.Close).
func (b *Backend) closeLogin() {
	b.loginMu.Lock()
	l := b.login
	b.loginMu.Unlock()
	if l != nil {
		l.Close()
	}
}

func newLogin(o LoginOptions) *Login {
	if o.Timeout <= 0 {
		o.Timeout = DefaultLoginTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Gcloud == nil {
		o.Gcloud = func() (string, bool) { return "", false }
	}
	l := &Login{o: o, log: o.Logger, st: LoginStatus{State: LoginIdle}}
	if err := l.wipeConfigDir(); err != nil {
		l.log.Warn("colab: cleaning the sign-in directory", "err", err)
	}
	return l
}

// Status returns the current status.
func (l *Login) Status() LoginStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st.clone()
}

func (s LoginStatus) clone() LoginStatus {
	if s.StartedAt != nil {
		t := *s.StartedAt
		s.StartedAt = &t
	}
	if s.Error != nil {
		e := *s.Error
		s.Error = &e
	}
	return s
}

// update changes the status of run (ignored once run is no longer the
// running sign-in) and reports it.
func (l *Login) update(run *loginRun, fn func(*LoginStatus)) {
	l.mu.Lock()
	if l.run != run {
		l.mu.Unlock()
		return
	}
	before := l.st.State + "\x00" + l.st.URL
	fn(&l.st)
	changed := l.st.State+"\x00"+l.st.URL != before
	st := l.st.clone()
	l.mu.Unlock()
	if changed {
		l.notify(st)
	}
}

func (l *Login) notify(st LoginStatus) {
	if l.o.OnChange != nil {
		l.o.OnChange(st)
	}
}

// Start begins a sign-in whose credential is imported with lc. Errors:
// CodeLifecycleInvalid, CodeLoginRunning (one is waiting or importing),
// CodeGcloudMissing. When gcloud cannot be started the returned status is
// failed (CodeGcloudStartFailed) and the error nil: the page shows the
// status's error like any other outcome.
func (l *Login) Start(lc Lifecycle) (LoginStatus, error) {
	if !lc.valid() {
		return l.Status(), newErr(CodeLifecycleInvalid, map[string]any{"value": lc.String()}, kindLocal, "",
			errors.New("unknown credential lifecycle"))
	}
	if err := validConfigDir(l.o.ConfigDir); err != nil {
		return l.Status(), err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return l.Status(), errLoginClosed
	}
	if l.run != nil {
		st := l.st.clone()
		l.mu.Unlock()
		return st, newErr(CodeLoginRunning, nil, kindLocal, "", errors.New("a Google sign-in is already running"))
	}
	gcloud, ok := l.o.Gcloud()
	if !ok || strings.TrimSpace(gcloud) == "" {
		st := l.st.clone()
		l.mu.Unlock()
		return st, newErr(CodeGcloudMissing, nil, kindLocal, "", errors.New("gcloud is not installed"))
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	run := &loginRun{cancel: cancel, done: make(chan struct{})}
	now := l.o.Now()
	l.run = run
	l.st = LoginStatus{State: LoginWaiting, StartedAt: &now}
	l.mu.Unlock()

	p, err := l.launch(ctx, run, gcloud)
	if err != nil {
		l.log.Warn("colab: gcloud could not be started", "err", err)
		l.finish(run, nil, LoginFailed, loginInfo(newErr(CodeGcloudStartFailed, nil, kindLocal, "", err)))
		cancel(nil)
		close(run.done)
		return l.Status(), nil
	}
	l.log.Info("colab: Google sign-in started", "timeout", l.o.Timeout.String())
	st := l.Status()
	l.notify(st)
	go l.supervise(ctx, run, p, lc)
	return st, nil
}

// Cancel ends a running sign-in (gcloud's process tree is stopped) and
// waits up to wait for it to end; the status is then cancelled (or done,
// when the import finished first). Without a running sign-in it only
// returns the status.
func (l *Login) Cancel(wait time.Duration) LoginStatus {
	l.mu.Lock()
	run := l.run
	l.mu.Unlock()
	if run == nil {
		return l.Status()
	}
	run.cancel(errLoginCancelled)
	select {
	case <-run.done:
	case <-time.After(wait):
		l.log.Warn("colab: the cancelled sign-in did not end in time")
	}
	return l.Status()
}

// Close cancels a running sign-in and refuses new ones (the app quits).
func (l *Login) Close() {
	l.mu.Lock()
	l.closed = true
	run := l.run
	l.mu.Unlock()
	if run == nil {
		return
	}
	run.cancel(errLoginClosed)
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		l.log.Warn("colab: the sign-in did not end in time")
	}
}

// finish wipes the configuration directory, sets the final status and
// frees the Login for the next sign-in (in this order: a new sign-in never
// finds the old one's files, and the old one never removes the new one's).
func (l *Login) finish(run *loginRun, p *loginProc, state string, info *model.ErrorInfo) {
	if p != nil {
		p.tree.kill() // whatever gcloud left running
		p.tree.close()
		p.cancelWait()
	}
	if err := l.wipeConfigDir(); err != nil {
		l.log.Warn("colab: cleaning the sign-in directory", "err", err)
	}
	l.mu.Lock()
	if l.run != run {
		l.mu.Unlock()
		return
	}
	l.st.State, l.st.Error = state, info
	l.run = nil
	st := l.st.clone()
	l.mu.Unlock()
	if info != nil {
		l.log.Info("colab: Google sign-in ended", "state", state, "code", info.Code)
	} else {
		l.log.Info("colab: Google sign-in ended", "state", state)
	}
	l.notify(st)
}

// supervise waits for gcloud, imports its credential and ends the run.
func (l *Login) supervise(ctx context.Context, run *loginRun, p *loginProc, lc Lifecycle) {
	defer close(run.done)
	err := p.cmd.Wait()
	p.out.flush()
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errLoginCancelled), errors.Is(cause, errLoginClosed):
		l.finish(run, p, LoginCancelled, nil)
		return
	case errors.Is(context.Cause(p.waitCtx), errLoginTimeout):
		minutes := int(l.o.Timeout.Round(time.Minute) / time.Minute)
		l.finish(run, p, LoginFailed, loginInfo(newErr(CodeLoginTimeout, map[string]any{"minutes": minutes}, kindLocal, "",
			fmt.Errorf("the sign-in did not finish within %s", l.o.Timeout))))
		return
	case err != nil:
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		detail := p.out.lastError()
		if detail == "" {
			detail = fmt.Sprintf("gcloud exited with code %d", code)
		}
		l.finish(run, p, LoginFailed, loginInfo(newErr(CodeLoginFailed, map[string]any{"exit_code": code}, kindLocal, "",
			errors.New(detail))))
		return
	}
	p.cancelWait() // the timeout covers the browser part only
	l.update(run, func(s *LoginStatus) { s.State = LoginImporting })
	ierr := l.o.importADC(ctx, filepath.Join(l.o.ConfigDir, adcFileName), lc)
	switch {
	case ierr == nil:
		l.finish(run, p, LoginDone, nil)
	case ctx.Err() != nil:
		l.finish(run, p, LoginCancelled, nil)
	default:
		l.finish(run, p, LoginFailed, loginInfo(ierr))
	}
}

// loginInfo is an error's ErrorInfo with the wrapped error's text as the
// detail ("查看详情"; built from fixed strings and gcloud's ERROR line,
// never from a credential: ImportADCFile scrubs its errors).
func loginInfo(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info := ce.Info
		if info.Detail == "" && ce.Err != nil {
			info.Detail = ce.Err.Error()
		}
		return &info
	}
	return &model.ErrorInfo{Code: CodeLoginFailed, Params: map[string]any{"exit_code": 0}, Detail: err.Error()}
}

// ---- gcloud ----

// LoginArgs are gcloud's arguments (exported for the documentation and
// the tests): the scopes of GcloudLoginCommand; no quota project (the
// sign-in's configuration has no project, and Colab needs none).
func LoginArgs() []string {
	return []string{"auth", "application-default", "login", "--scopes=" + strings.Join(GcloudLoginScopes, ","), "--disable-quota-project"}
}

// loginEnv is gcloud's environment: base without the variables that would
// point gcloud at the user's own configuration, credentials or projects,
// plus the sign-in's configuration directory, no prompts (a question would
// wait forever: there is no terminal), no usage reporting and no update
// check.
func loginEnv(base []string, configDir string) []string {
	own := map[string]bool{
		"CLOUDSDK_CONFIG": true, "CLOUDSDK_ACTIVE_CONFIG_NAME": true, "CLOUDSDK_CORE_PROJECT": true, "CLOUDSDK_CORE_ACCOUNT": true,
		"CLOUDSDK_BILLING_QUOTA_PROJECT": true, "GOOGLE_APPLICATION_CREDENTIALS": true, "GOOGLE_CLOUD_PROJECT": true,
		"CLOUDSDK_CORE_DISABLE_PROMPTS": true, "CLOUDSDK_CORE_DISABLE_USAGE_REPORTING": true,
		"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK": true, "CLOUDSDK_SURVEY_DISABLE_PROMPTS": true,
	}
	out := make([]string, 0, len(base)+5)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		uk := strings.ToUpper(k)
		if own[uk] || strings.HasPrefix(uk, "CLOUDSDK_AUTH_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"CLOUDSDK_CONFIG="+configDir,
		"CLOUDSDK_CORE_DISABLE_PROMPTS=1",
		"CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true",
		"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true",
		"CLOUDSDK_SURVEY_DISABLE_PROMPTS=true",
	)
}

// loginProc is a started gcloud.
type loginProc struct {
	cmd        *exec.Cmd
	tree       *procTree
	out        *loginOutput
	waitCtx    context.Context // ends at the timeout (or with the run)
	cancelWait context.CancelFunc
}

// launch prepares the configuration directory and starts gcloud; the
// sign-in link it prints goes to run's status.
func (l *Login) launch(ctx context.Context, run *loginRun, gcloud string) (*loginProc, error) {
	if err := l.wipeConfigDir(); err != nil {
		// Only a credential left behind matters (it would be imported).
		if _, serr := os.Lstat(filepath.Join(l.o.ConfigDir, adcFileName)); serr == nil || !errors.Is(serr, os.ErrNotExist) {
			return nil, err
		}
		l.log.Warn("colab: cleaning the sign-in directory", "err", err)
	}
	if err := os.MkdirAll(l.o.ConfigDir, 0o700); err != nil {
		return nil, err
	}
	env := os.Environ()
	if l.o.Env != nil {
		env = l.o.Env()
	}
	waitCtx, cancelWait := context.WithTimeoutCause(ctx, l.o.Timeout, errLoginTimeout)
	out := &loginOutput{}
	out.onLine = func(line string) {
		if u := signInURL(line); u != "" {
			l.update(run, func(s *LoginStatus) {
				if s.URL == "" {
					s.URL = u
				}
			})
		}
	}
	tree := newProcTree(gcloud)
	cmd := exec.CommandContext(waitCtx, gcloud, LoginArgs()...)
	cmd.Env = loginEnv(env, l.o.ConfigDir)
	cmd.Dir = l.o.ConfigDir
	cmd.Stdin = nil // reads see end of file: a prompt fails instead of waiting
	cmd.Stdout = out.writer()
	cmd.Stderr = out.writer()
	cmd.WaitDelay = loginWaitDelay
	cmd.Cancel = func() error {
		tree.kill()
		return nil
	}
	tree.prepare(cmd)
	if err := cmd.Start(); err != nil {
		cancelWait()
		tree.close()
		return nil, err
	}
	if err := tree.started(cmd); err != nil {
		tree.kill()
		_ = cmd.Wait()
		cancelWait()
		tree.close()
		return nil, err
	}
	return &loginProc{cmd: cmd, tree: tree, out: out, waitCtx: waitCtx, cancelWait: cancelWait}, nil
}

// signInURLRe finds the sign-in link in a line of gcloud's output ("Your
// browser has been opened to visit:" followed by the link on its own line;
// with no browser "Go to the following link in your browser").
var signInURLRe = regexp.MustCompile(`https://accounts\.google\.com/[^\s"'<>]+`)

// signInURL returns the Google sign-in link of line, "" when there is none.
func signInURL(line string) string {
	m := signInURLRe.FindString(line)
	if m == "" {
		return ""
	}
	u, err := url.Parse(m)
	if err != nil || u.Scheme != "https" || u.Host != "accounts.google.com" || u.User != nil {
		return ""
	}
	return u.String()
}

// validConfigDir: LoginOptions.ConfigDir is an absolute .../gcloud path
// (the directory is removed as a whole).
func validConfigDir(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Base(dir) != "gcloud" {
		return fmt.Errorf("colab: the sign-in directory %q is not an absolute .../gcloud path", dir)
	}
	return nil
}

// wipeConfigDir removes the sign-in's configuration directory; the ADC file
// in it is overwritten first (it holds a refresh token).
func (l *Login) wipeConfigDir() error {
	dir := l.o.ConfigDir
	if err := validConfigDir(dir); err != nil {
		return err
	}
	wipeFile(filepath.Join(dir, adcFileName))
	var err error
	for i := 0; i < 20; i++ { // Windows: a stopped process may still hold a file for a moment
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// wipeFile overwrites a regular file with zeros and removes it (best
// effort: a copy-on-write or flash filesystem may keep the old blocks).
func wipeFile(path string) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		_, _ = f.Write(make([]byte, fi.Size()))
		_ = f.Sync()
		_ = f.Close()
	}
	_ = os.Remove(path)
}

// ---- gcloud's output ----

// loginOutput splits gcloud's stdout and stderr into lines: onLine sees
// each one; the last "ERROR:" line is kept for the failure's detail.
type loginOutput struct {
	onLine func(string)

	mu      sync.Mutex
	errLine string
	sinks   []*lineSink
}

func (o *loginOutput) writer() *lineSink {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := &lineSink{o: o}
	o.sinks = append(o.sinks, s)
	return s
}

func (o *loginOutput) line(b []byte) {
	line := strings.TrimSpace(strings.ToValidUTF8(string(b), "?"))
	if line == "" {
		return
	}
	if strings.HasPrefix(line, "ERROR:") {
		d := signInURLRe.ReplaceAllString(line, "[link]")
		if len(d) > loginDetailMax {
			d = d[:loginDetailMax]
			for !utf8.ValidString(d) {
				d = d[:len(d)-1]
			}
			d += "…"
		}
		o.mu.Lock()
		o.errLine = d
		o.mu.Unlock()
	}
	if o.onLine != nil {
		o.onLine(line)
	}
}

func (o *loginOutput) lastError() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.errLine
}

// flush passes on the unterminated last lines (after gcloud ended).
func (o *loginOutput) flush() {
	o.mu.Lock()
	sinks := append([]*lineSink(nil), o.sinks...)
	o.mu.Unlock()
	for _, s := range sinks {
		s.mu.Lock()
		rest := s.buf
		s.buf = nil
		s.mu.Unlock()
		if len(rest) > 0 {
			s.o.line(rest)
		}
	}
}

// lineSink is one output stream of gcloud (an io.Writer for exec.Cmd).
type lineSink struct {
	o *loginOutput

	mu   sync.Mutex
	buf  []byte
	long bool // the current line exceeded loginLineMax; the rest is dropped
}

func (s *lineSink) Write(p []byte) (int, error) {
	n := len(p)
	s.mu.Lock()
	var lines [][]byte
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !s.long {
				s.buf = append(s.buf, p...)
				if len(s.buf) > loginLineMax {
					s.buf, s.long = s.buf[:loginLineMax], true
				}
			}
			break
		}
		if !s.long {
			s.buf = append(s.buf, p[:i]...)
			if len(s.buf) > loginLineMax {
				s.buf = s.buf[:loginLineMax]
			}
		}
		lines = append(lines, s.buf)
		s.buf, s.long = nil, false
		p = p[i+1:]
	}
	s.mu.Unlock()
	for _, l := range lines {
		s.o.line(l)
	}
	return n, nil
}

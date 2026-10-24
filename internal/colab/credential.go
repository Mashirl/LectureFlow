// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// Google OAuth endpoints (google-auth's defaults; the ADC file names the
// token endpoint implicitly through its type "authorized_user").
const (
	DefaultTokenURL  = "https://oauth2.googleapis.com/token"
	DefaultRevokeURL = "https://oauth2.googleapis.com/revoke"
)

// Scopes (google-colab-cli 0.7.5 colab_cli/auth.py, _get_adc_credentials):
// the Colab session backend needs userinfo.email (else 401), the Colab
// APIs need colaboratory; openid and cloud-platform are only there because
// gcloud refuses a login without them.
const (
	scopeOpenID   = "openid"
	scopeCloud    = "https://www.googleapis.com/auth/cloud-platform"
	scopeEmail    = "https://www.googleapis.com/auth/userinfo.email"
	scopeColab    = "https://www.googleapis.com/auth/colaboratory"
	adcFileName   = "application_default_credentials.json"
	adcMaxBytes   = 64 << 10
	accessSkew    = time.Minute // an access token closer than this to its end is refreshed
	tokenTimeout  = 30 * time.Second
	adcTypeUser   = "authorized_user"
	credVersion   = 1
	maxLifeDays   = 90
	defaultLifeDy = 7
)

// GcloudLoginScopes are the scopes the user logs in with (the guide shows
// GcloudLoginCommand); the minimum gcloud accepts that still covers Colab.
var GcloudLoginScopes = []string{scopeOpenID, scopeCloud, scopeEmail, scopeColab}

// requiredScopes must have been granted (checked when importing).
var requiredScopes = []string{scopeEmail, scopeColab}

// refreshScopes are requested on every refresh: the access tokens the
// backend holds are narrowed to what Colab needs (no cloud-platform), so a
// leaked access token cannot reach the user's Google Cloud projects. Google
// returns the intersection with what the refresh token was granted.
var refreshScopes = []string{scopeOpenID, scopeEmail, scopeColab}

// GcloudLoginCommand is the command the guide shows (V14.2). The scope
// list is quoted: PowerShell splits an unquoted a,b,c into separate
// arguments, and gcloud then sees only the first scope. The quotes work the
// same in cmd, bash and zsh.
func GcloudLoginCommand() string {
	return `gcloud auth application-default login --scopes="` + strings.Join(GcloudLoginScopes, ",") + `"`
}

// ---- lifecycle ----

// Lifecycle kinds (V14.2): "7 天后失效" (days), "仅本次会话" (session: this
// run of the app only), "直到我手动断开" (manual).
const (
	LifecycleDays    = "days"
	LifecycleSession = "session"
	LifecycleManual  = "manual"
)

// Lifecycle is how long an imported credential may be used.
type Lifecycle struct {
	Kind string `json:"kind"`
	Days int    `json:"days,omitempty"` // LifecycleDays only: 1..90
}

// ParseLifecycle reads "7d" (any 1..90 days), "session" or "manual".
func ParseLifecycle(s string) (Lifecycle, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	bad := func() (Lifecycle, error) {
		return Lifecycle{}, newErr(CodeLifecycleInvalid, map[string]any{"value": s}, kindLocal, "",
			fmt.Errorf("unknown credential lifecycle"))
	}
	switch s {
	case LifecycleSession, LifecycleManual:
		return Lifecycle{Kind: s}, nil
	case "":
		return Lifecycle{Kind: LifecycleDays, Days: defaultLifeDy}, nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if !strings.HasSuffix(s, "d") || err != nil || n < 1 || n > maxLifeDays {
		return bad()
	}
	return Lifecycle{Kind: LifecycleDays, Days: n}, nil
}

// String is the ParseLifecycle form.
func (l Lifecycle) String() string {
	if l.Kind == LifecycleDays {
		return strconv.Itoa(l.Days) + "d"
	}
	return l.Kind
}

func (l Lifecycle) valid() bool {
	switch l.Kind {
	case LifecycleSession, LifecycleManual:
		return l.Days == 0
	case LifecycleDays:
		return l.Days >= 1 && l.Days <= maxLifeDays
	}
	return false
}

// ---- stored credential ----

// storedCredential is kv colab.credential. It holds secrets: it is never
// returned by an exported function, and String / LogValue print no field.
type storedCredential struct {
	Version      int       `json:"v"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
	RefreshToken string    `json:"refresh_token"`
	QuotaProject string    `json:"quota_project_id,omitempty"`
	Lifecycle    Lifecycle `json:"lifecycle"`
	ImportedAt   time.Time `json:"imported_at"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"` // LifecycleDays: ImportedAt + Days
	RunID        string    `json:"run_id,omitempty"`    // LifecycleSession: the run that imported it
	Scopes       []string  `json:"scopes,omitempty"`    // as Google reported them when importing
}

// String, GoString, LogValue and Format keep every formatting of the
// credential (value or pointer, %v, %+v, %#v, %s, slog) free of secrets.
func (c storedCredential) String() string       { return "[colab credential]" }
func (c storedCredential) GoString() string     { return "[colab credential]" }
func (c storedCredential) LogValue() slog.Value { return slog.StringValue("[colab credential]") }
func (c storedCredential) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "[colab credential]")
}

// usable reports whether the user's lifecycle still allows the credential
// at now in the run run (V14.2: checked strictly before every use).
func (c *storedCredential) usable(now time.Time, run string) bool {
	switch c.Lifecycle.Kind {
	case LifecycleDays:
		return !c.ExpiresAt.IsZero() && now.Before(c.ExpiresAt)
	case LifecycleSession:
		return c.RunID != "" && c.RunID == run
	case LifecycleManual:
		return true
	}
	return false
}

// deadline is the end of a days lifecycle (ok false for session / manual).
func (c *storedCredential) deadline() (time.Time, bool) {
	if c.Lifecycle.Kind == LifecycleDays {
		return c.ExpiresAt, true
	}
	return time.Time{}, false
}

// CredentialInfo describes the stored credential without any secret
// (settings page, resource bar: "授权剩余有效期").
type CredentialInfo struct {
	Present    bool       `json:"present"`
	Lifecycle  string     `json:"lifecycle,omitempty"` // "7d", "session", "manual"
	ImportedAt *time.Time `json:"imported_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // days lifecycle only
	// RemainingSec until ExpiresAt; -1 = no time limit (session, manual).
	RemainingSec int64    `json:"remaining_sec"`
	Scopes       []string `json:"scopes,omitempty"`
}

// ADCInfo is what "检测授权" shows before importing.
type ADCInfo struct {
	Path  string `json:"path"`
	Found bool   `json:"found"`
	Type  string `json:"type,omitempty"`
	// Problem is an error code (CodeADC*) when the file cannot be imported.
	Problem string `json:"problem,omitempty"`
	// LoginCommand is GcloudLoginCommand (shown by the guide).
	LoginCommand string `json:"login_command"`
}

// adcFile is the part of gcloud's application_default_credentials.json
// the backend reads.
type adcFile struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	QuotaProject string `json:"quota_project_id"`
}

// ADCPath is where gcloud keeps the application default credentials:
// GOOGLE_APPLICATION_CREDENTIALS when it names a file (the ADC discovery
// order upstream inherits from google-auth), else {CLOUDSDK_CONFIG or the
// gcloud config dir}/application_default_credentials.json (Windows
// %APPDATA%\gcloud, macOS and Linux ~/.config/gcloud).
func ADCPath() string {
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if d := os.Getenv("CLOUDSDK_CONFIG"); d != "" {
		return filepath.Join(d, adcFileName)
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "gcloud", adcFileName)
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gcloud", adcFileName)
}

// readADC reads and checks an ADC file; the error is CodeADC* (kindLocal).
func readADC(path string) (adcFile, error) {
	var f adcFile
	b, err := readLimited(path, adcMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return f, newErr(CodeADCNotFound, map[string]any{"path": path}, kindLocal, model.WaitRemoteAuth,
			errors.New("gcloud application default credentials not found"))
	}
	if err != nil {
		return f, newErr(CodeADCInvalid, map[string]any{"field": "file"}, kindLocal, model.WaitRemoteAuth,
			errors.New("cannot read the application default credentials file"))
	}
	if json.Unmarshal(b, &f) != nil {
		return f, newErr(CodeADCInvalid, map[string]any{"field": "json"}, kindLocal, model.WaitRemoteAuth,
			errors.New("the application default credentials file is not JSON"))
	}
	if f.Type != adcTypeUser {
		// Service accounts, external accounts, impersonation: not what the
		// gcloud login of the guide produces, and Colab runtimes belong to
		// a user.
		return f, newErr(CodeADCUnsupported, map[string]any{"type": f.Type}, kindLocal, model.WaitRemoteAuth,
			errors.New("unsupported credential type"))
	}
	for _, kv := range [][2]string{{"client_id", f.ClientID}, {"client_secret", f.ClientSecret}, {"refresh_token", f.RefreshToken}} {
		if strings.TrimSpace(kv[1]) == "" {
			return f, newErr(CodeADCInvalid, map[string]any{"field": kv[0]}, kindLocal, model.WaitRemoteAuth,
				fmt.Errorf("field %s is missing", kv[0]))
		}
	}
	return f, nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file too large")
	}
	return b, nil
}

// DetectADC reports whether the ADC file exists and can be imported,
// without importing it or contacting Google ("检测授权").
func DetectADC(path string) ADCInfo {
	if path == "" {
		path = ADCPath()
	}
	info := ADCInfo{Path: path, LoginCommand: GcloudLoginCommand()}
	f, err := readADC(path)
	info.Found = Code(err) != CodeADCNotFound
	info.Type = f.Type
	if err != nil {
		info.Problem = Code(err)
	}
	return info
}

// ---- the auth manager ----

// authManager owns the credential: lifecycle checks, access tokens,
// refresh, revocation. Access tokens live in memory only.
type authManager struct {
	kv        KV
	hc        *http.Client
	tokenURL  string
	revokeURL string
	now       func() time.Time
	runID     string
	secrets   *secretSet
	log       *slog.Logger

	mu        sync.Mutex
	access    string
	accessExp time.Time
}

// tokenResponse is Google's token endpoint answer (RFC 6749 §5.1).
type tokenResponse struct {
	AccessToken *string  `json:"access_token"`
	ExpiresIn   *float64 `json:"expires_in"`
	TokenType   string   `json:"token_type"`
	Scope       *string  `json:"scope"`
	Error       string   `json:"error"`
}

func authErr(code string, params map[string]any, cause string) error {
	return newErr(code, params, kindFallback, model.WaitRemoteAuth, errors.New(cause))
}

// load returns the stored credential when the lifecycle allows its use;
// a credential the lifecycle no longer allows (or that cannot be read) is
// deleted first (V14.2: "已过期即删除本地凭据").
func (a *authManager) load(ctx context.Context) (*storedCredential, error) {
	v, ok, err := getKV(ctx, a.kv, KVCredential)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, authErr(CodeAuthMissing, nil, "no Google credential imported")
	}
	var c storedCredential
	if json.Unmarshal([]byte(v), &c) != nil || c.Version != credVersion || c.RefreshToken == "" || !c.Lifecycle.valid() {
		a.log.Warn("colab: stored credential unreadable; deleted")
		if err := a.deleteLocal(ctx); err != nil {
			return nil, err
		}
		return nil, authErr(CodeAuthMissing, nil, "stored credential unreadable")
	}
	a.secrets.add(c.RefreshToken, c.ClientSecret)
	if !c.usable(a.now(), a.runID) {
		a.log.Info("colab: credential lifecycle ended; local credential deleted", "lifecycle", c.Lifecycle.String())
		if err := a.deleteLocal(ctx); err != nil {
			return nil, err
		}
		return nil, authErr(CodeAuthExpired, nil, "the credential's lifecycle ended")
	}
	return &c, nil
}

// deleteLocal removes the stored credential and the in-memory token.
func (a *authManager) deleteLocal(ctx context.Context) error {
	a.mu.Lock()
	a.access, a.accessExp = "", time.Time{}
	a.mu.Unlock()
	return setSecret(context.WithoutCancel(ctx), a.kv, KVCredential, "")
}

// startup deletes a credential the lifecycle no longer allows: a session
// credential of an earlier run, an expired days credential (V14.2).
func (a *authManager) startup(ctx context.Context) {
	if _, err := a.load(ctx); err != nil && Code(err) == "" {
		a.log.Warn("colab: reading the stored credential", "err", a.secrets.scrub(err.Error()))
	}
}

// info describes the credential (after the lifecycle check).
func (a *authManager) info(ctx context.Context) CredentialInfo {
	c, err := a.load(ctx)
	if err != nil {
		return CredentialInfo{RemainingSec: -1}
	}
	return describe(c, a.now())
}

func describe(c *storedCredential, now time.Time) CredentialInfo {
	imp := c.ImportedAt
	info := CredentialInfo{Present: true, Lifecycle: c.Lifecycle.String(), ImportedAt: &imp, RemainingSec: -1,
		Scopes: slices.Clone(c.Scopes)}
	if d, ok := c.deadline(); ok {
		info.ExpiresAt = &d
		info.RemainingSec = max(0, int64(d.Sub(now)/time.Second))
	}
	return info
}

// remaining is how long the credential may still be used (-1 = no time
// limit); the error says why there is no usable credential (load).
func (a *authManager) remaining(ctx context.Context) (time.Duration, error) {
	c, err := a.load(ctx)
	if err != nil {
		return 0, err
	}
	if d, ok := c.deadline(); ok {
		return d.Sub(a.now()), nil
	}
	return -1, nil
}

// token returns an access token for the Colab tunnel API, refreshing it
// when Google's expiry is near. Neither the access token nor a refresh is
// ever used past the lifecycle's end (V14.2: "不得超出用户设定的期限"):
// load deletes the credential at that moment, and the cached token is
// only handed out before it.
func (a *authManager) token(ctx context.Context) (string, error) {
	c, err := a.load(ctx)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if d, ok := c.deadline(); ok && !now.Before(d) {
		return "", authErr(CodeAuthExpired, nil, "the credential's lifecycle ended")
	}
	if a.access != "" && now.Add(accessSkew).Before(a.accessExp) {
		return a.access, nil
	}
	tok, exp, _, err := a.refresh(ctx, c, true)
	if err != nil {
		return "", err
	}
	a.access, a.accessExp = tok, exp
	return tok, nil
}

// forget drops the in-memory access token (after a 401 from Colab).
func (a *authManager) forget() {
	a.mu.Lock()
	a.access, a.accessExp = "", time.Time{}
	a.mu.Unlock()
}

// refresh exchanges the refresh token for an access token (RFC 6749 §6).
// A refresh token Google no longer accepts (invalid_grant: revoked,
// expired, password change) deletes the local credential. Returns the
// token, its expiry and the granted scopes (nil when Google did not say).
// stored: c is the stored credential (a rejection deletes it); false
// while importing (a bad new file leaves the stored one alone). Called with
// a.mu held.
func (a *authManager) refresh(ctx context.Context, c *storedCredential, stored bool) (string, time.Time, []string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"refresh_token": {c.RefreshToken},
		"scope":         {strings.Join(refreshScopes, " ")},
	}
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
			return "", time.Time{}, nil, ctx.Err()
		}
		return "", time.Time{}, nil, newErr(CodeAuthEndpoint, map[string]any{"status": "network"}, kindFallback, "",
			fmt.Errorf("token refresh: %s", netErrText(err)))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var tr tokenResponse
	jsonErr := json.Unmarshal(body, &tr)
	switch {
	case resp.StatusCode == http.StatusOK:
	case (resp.StatusCode == 400 || resp.StatusCode == 401) && jsonErr == nil && tr.Error != "":
		switch tr.Error {
		case "invalid_grant", "invalid_client", "unauthorized_client":
			if stored {
				a.log.Info("colab: Google rejected the refresh token; local credential deleted", "error", tr.Error)
				a.access, a.accessExp = "", time.Time{}
				if err := setSecret(context.WithoutCancel(ctx), a.kv, KVCredential, ""); err != nil {
					return "", time.Time{}, nil, err
				}
			}
			return "", time.Time{}, nil, authErr(CodeAuthRejected, map[string]any{"reason": tr.Error}, "token refresh rejected: "+tr.Error)
		case "invalid_scope":
			return "", time.Time{}, nil, authErr(CodeAuthRejected, map[string]any{"reason": tr.Error}, "token refresh rejected: invalid_scope")
		}
		return "", time.Time{}, nil, apiChanged("oauth2/token", "error "+safeToken(tr.Error))
	case resp.StatusCode >= 500 || resp.StatusCode == 429:
		return "", time.Time{}, nil, newErr(CodeAuthEndpoint, map[string]any{"status": strconv.Itoa(resp.StatusCode)}, kindFallback, "",
			fmt.Errorf("token refresh: HTTP %d", resp.StatusCode))
	default:
		return "", time.Time{}, nil, apiChanged("oauth2/token", fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	if jsonErr != nil {
		return "", time.Time{}, nil, apiChanged("oauth2/token", "response is not JSON")
	}
	if tr.AccessToken == nil || *tr.AccessToken == "" {
		return "", time.Time{}, nil, apiChanged("oauth2/token", "access_token missing")
	}
	if tr.ExpiresIn == nil || *tr.ExpiresIn <= 0 {
		return "", time.Time{}, nil, apiChanged("oauth2/token", "expires_in missing")
	}
	a.secrets.add(*tr.AccessToken)
	var scopes []string
	if tr.Scope != nil {
		scopes = strings.Fields(*tr.Scope)
		slices.Sort(scopes)
	}
	exp := a.now().Add(time.Duration(*tr.ExpiresIn * float64(time.Second)))
	return *tr.AccessToken, exp, scopes, nil
}

// importADC reads the ADC file at path, checks it with one refresh
// (scopes included) and stores it with the lifecycle.
func (a *authManager) importADC(ctx context.Context, path string, lc Lifecycle) (CredentialInfo, error) {
	if !lc.valid() {
		return CredentialInfo{RemainingSec: -1}, newErr(CodeLifecycleInvalid, map[string]any{"value": lc.String()}, kindLocal, "",
			errors.New("unknown credential lifecycle"))
	}
	f, err := readADC(path)
	if err != nil {
		return CredentialInfo{RemainingSec: -1}, err
	}
	a.secrets.add(f.RefreshToken, f.ClientSecret)
	now := a.now()
	c := &storedCredential{
		Version: credVersion, ClientID: f.ClientID, ClientSecret: f.ClientSecret, RefreshToken: f.RefreshToken,
		QuotaProject: f.QuotaProject, Lifecycle: lc, ImportedAt: now,
	}
	switch lc.Kind {
	case LifecycleDays:
		c.ExpiresAt = now.Add(time.Duration(lc.Days) * 24 * time.Hour)
	case LifecycleSession:
		c.RunID = a.runID
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tok, exp, scopes, err := a.refresh(ctx, c, false)
	if err != nil {
		return CredentialInfo{RemainingSec: -1}, err
	}
	if scopes != nil {
		var missing []string
		for _, s := range requiredScopes {
			if !slices.Contains(scopes, s) {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			return CredentialInfo{RemainingSec: -1}, newErr(CodeADCMissingScope, map[string]any{"scopes": strings.Join(missing, " ")},
				kindLocal, model.WaitRemoteAuth, errors.New("the credential lacks scopes Colab needs"))
		}
	} else {
		a.log.Info("colab: the token endpoint did not report scopes; not checked")
	}
	c.Scopes = scopes
	b, err := json.Marshal(c)
	if err != nil {
		return CredentialInfo{RemainingSec: -1}, err
	}
	// A credential it replaces leaves no copy (SecretKV).
	if err := setSecret(ctx, a.kv, KVCredential, string(b)); err != nil {
		return CredentialInfo{RemainingSec: -1}, err
	}
	a.access, a.accessExp = tok, exp
	a.log.Info("colab: credential imported", "lifecycle", lc.String())
	return describe(c, now), nil
}

// revokeAndDelete revokes the refresh token at Google and deletes the local
// copy whatever the revocation's outcome ("断开连接", V14.2). The error is
// CodeRevokeFailed when Google could not be told; the local copy is gone
// either way. Without a stored credential it only clears the memory.
func (a *authManager) revokeAndDelete(ctx context.Context) (bool, error) {
	v, ok, err := getKV(ctx, a.kv, KVCredential)
	if err != nil {
		return false, err
	}
	var c storedCredential
	if !ok || json.Unmarshal([]byte(v), &c) != nil || c.RefreshToken == "" {
		return false, a.deleteLocal(ctx)
	}
	a.secrets.add(c.RefreshToken, c.ClientSecret)
	revokeErr := a.revoke(ctx, c.RefreshToken)
	if err := a.deleteLocal(ctx); err != nil {
		return false, err
	}
	if revokeErr != nil {
		a.log.Warn("colab: revoking the credential failed; the local copy was deleted", "code", Code(revokeErr))
		return false, revokeErr
	}
	a.log.Info("colab: credential revoked and deleted")
	return true, nil
}

// revoke calls Google's revocation endpoint (RFC 7009). A token Google
// already considers invalid counts as revoked.
func (a *authManager) revoke(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.revokeURL, strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.hc.Do(req)
	if err != nil {
		return newErr(CodeRevokeFailed, map[string]any{"status": "network"}, kindLocal, "",
			fmt.Errorf("revoke: %s", netErrText(err)))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var tr tokenResponse
	if resp.StatusCode == 400 && json.Unmarshal(body, &tr) == nil && tr.Error == "invalid_token" {
		return nil
	}
	return newErr(CodeRevokeFailed, map[string]any{"status": strconv.Itoa(resp.StatusCode)}, kindLocal, "",
		fmt.Errorf("revoke: HTTP %d", resp.StatusCode))
}

// netErrText is a transport error without the request URL (whose query may
// carry a runtime proxy token).
func netErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return err.Error()
}

// safeToken keeps a short identifier-like string (an OAuth error name) for
// a message; anything else becomes "?".
func safeToken(s string) string {
	if len(s) > 40 {
		return "?"
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return "?"
		}
	}
	return s
}

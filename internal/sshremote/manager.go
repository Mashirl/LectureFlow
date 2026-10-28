// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package sshremote is the controller side of the SSH inference servers
// (ssh-mvp; .claude/research/remote-ssh-plan.md): the servers the user
// added (remote_server rows), their connections, lf-agent and kit, and one
// remote.Device per server for the scheduler and the asr stage. The agent
// is internal/agent; the protocol internal/remote.
package sshremote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// Store is what the manager needs from the database (db's sqlStore).
type Store interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error
	InsertRemoteServer(ctx context.Context, r db.RemoteServer) error
	UpdateRemoteServer(ctx context.Context, r db.RemoteServer) error
	GetRemoteServer(ctx context.Context, id string) (db.RemoteServer, error)
	ListRemoteServers(ctx context.Context) ([]db.RemoteServer, error)
	DeleteRemoteServer(ctx context.Context, id, keyRef string) error
}

// DeviceTable is the device registry's SSH part (devices.Registry).
type DeviceTable interface {
	Get(id string) (model.ASRDevice, bool)
	List() []model.ASRDevice
	UpsertSSH(ctx context.Context, row model.ASRDevice) error
	RemoveSSH(ctx context.Context, id string) error
	SetSSHPresent(id string, present bool)
}

// Options configure the manager.
type Options struct {
	Store   Store
	Devices DeviceTable
	// AgentDir holds the lf-agent builds: AgentDir/<platform>/lf-agent.
	AgentDir string
	// Manifest is the component manifest (components.json); WorkerDir the
	// worker scripts; FFmpeg encodes the FLAC uploads.
	Manifest  string
	WorkerDir string
	FFmpeg    string
	// AgentVersion is the application version ("" = 0.0.0-dev).
	AgentVersion string
	// ReconnectGrace: how long a lecture waits for a lost server
	// (default 10 minutes); IdleTimeout: an unused connection is closed
	// after it (5 minutes); Retain: how long the agent keeps a job's files
	// after a lost connection (2 hours).
	ReconnectGrace time.Duration
	IdleTimeout    time.Duration
	Retain         time.Duration
	// OnChange is called when a server's state or card changed (the
	// scheduler kicks a scan, the API sends "devices").
	OnChange func()
	// Tests: Dial replaces net.Dialer; KitFor replaces the manifest kit;
	// Now the clock.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// EncodeFLAC replaces the ffmpeg encoding of the FLAC upload (tests).
	EncodeFLAC func(ctx context.Context, in, out string) error
	KitFor     func(platform, engine, model, precision string) (remote.KitSpec, error)
	Now        func() time.Time
}

// Manager holds the servers.
type Manager struct {
	o      Options
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	servers map[string]*Server
	builds  map[string]agentBuild
	sess    map[string]*sync.Mutex
	hooks   []func()
	done    chan struct{}
}

// New loads the servers and starts the idle watcher.
func New(ctx context.Context, o Options) (*Manager, error) {
	if o.Store == nil {
		return nil, errors.New("sshremote: Store is required")
	}
	if o.ReconnectGrace <= 0 {
		o.ReconnectGrace = 10 * time.Minute
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 5 * time.Minute
	}
	if o.Retain <= 0 || o.Retain > remote.MaxRetain {
		o.Retain = remote.DefaultRetain
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AgentVersion == "" || !remote.VersionStringPattern.MatchString(o.AgentVersion) {
		o.AgentVersion = "0.0.0-dev"
	}
	m := &Manager{o: o, servers: map[string]*Server{}, builds: map[string]agentBuild{}, sess: map[string]*sync.Mutex{},
		done: make(chan struct{})}
	m.ctx, m.cancel = context.WithCancel(context.WithoutCancel(ctx))
	recs, err := o.Store.ListRemoteServers(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		m.servers[r.ID] = newServer(m, r)
	}
	go m.idleLoop()
	return m, nil
}

func (m *Manager) now() time.Time { return m.o.Now() }

func (m *Manager) closed() bool { return m.ctx.Err() != nil }

func (m *Manager) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if m.o.Dial != nil {
		return m.o.Dial(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (m *Manager) kitFor(platform, engine, modelName, precision string) (remote.KitSpec, error) {
	if m.o.KitFor != nil {
		k, err := m.o.KitFor(platform, engine, modelName, precision)
		if err == nil {
			k.Version = m.o.AgentVersion
		}
		return k, err
	}
	return kitFromManifest(m.o.Manifest, m.o.WorkerDir, m.o.AgentVersion, platform, engine, modelName, precision)
}

func (m *Manager) sessMu(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	mu := m.sess[id]
	if mu == nil {
		mu = &sync.Mutex{}
		m.sess[id] = mu
	}
	return mu
}

// OnChange adds a hook called when a server changed.
func (m *Manager) OnChange(fn func()) {
	m.mu.Lock()
	m.hooks = append(m.hooks, fn)
	m.mu.Unlock()
}

func (m *Manager) changed() {
	if m.o.OnChange != nil {
		m.o.OnChange()
	}
	m.mu.Lock()
	hooks := slices.Clone(m.hooks)
	m.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// Device returns the server of a device id (remote.Device).
func (m *Manager) Device(id string) (remote.Device, bool) {
	s := m.server(model.SSHSlug(id))
	if s == nil {
		return nil, false
	}
	return s, true
}

// DeviceIDs lists the servers' device ids.
func (m *Manager) DeviceIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s.devID)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) server(slug string) *Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.servers[slug]
}

// priorityFor is a new row's priority: above every local device for
// GroupPrefer, below them (above Colab) for GroupSupplement.
func (m *Manager) priorityFor(group string) int {
	if group != GroupPrefer || m.o.Devices == nil {
		return model.DefaultDevicePriority(model.DeviceColab) + 1
	}
	top := 0
	for _, d := range m.o.Devices.List() {
		top = max(top, d.Priority)
	}
	return top + 10
}

// idleLoop closes connections without work after IdleTimeout.
func (m *Manager) idleLoop() {
	t := time.NewTicker(min(m.o.IdleTimeout/4, 30*time.Second))
	defer t.Stop()
	defer close(m.done)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		list := make([]*Server, 0, len(m.servers))
		for _, s := range m.servers {
			list = append(list, s)
		}
		m.mu.Unlock()
		for _, s := range list {
			s.mu.Lock()
			idle := s.c != nil && len(s.runs) == 0 && s.installing == nil && m.now().Sub(s.lastUse) > m.o.IdleTimeout
			s.mu.Unlock()
			if idle {
				slog.Info("ssh server: closing the idle connection", "device", s.devID)
				s.close()
			}
		}
	}
}

// Close ends every connection (the agents keep the jobs they ran for
// their retain time) and stops the manager.
func (m *Manager) Close() error {
	m.cancel()
	<-m.done
	m.mu.Lock()
	list := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		s.close()
	}
	return nil
}

// ---------- the API's operations ----------

// ServerView is a server as the API shows it (no secret, no private key).
type ServerView struct {
	ID           string            `json:"id"`
	DeviceID     string            `json:"device_id"`
	Name         string            `json:"name"`
	Host         string            `json:"host"`
	Port         int               `json:"port"`
	Username     string            `json:"username"`
	Jumps        []Jump            `json:"jumps"`
	State        string            `json:"state"`
	Platform     string            `json:"platform,omitempty"`
	Engine       string            `json:"engine,omitempty"`
	Trusted      bool              `json:"trusted"`
	HostKeyFP    string            `json:"host_key_fingerprint,omitempty"`
	PublicKey    string            `json:"public_key"`
	KeyFP        string            `json:"public_key_fingerprint"`
	Settings     Settings          `json:"settings"`
	Probe        *ProbeView        `json:"probe,omitempty"`
	Install      *InstallView      `json:"install,omitempty"`
	LastError    *remote.CardError `json:"last_error,omitempty"`
	AgentVersion string            `json:"agent_version,omitempty"`
	KitReady     bool              `json:"kit_ready"`
}

// ProbeView is the probe for display.
type ProbeView struct {
	OSVersion   string `json:"os_version,omitempty"`
	CPUs        int64  `json:"cpus"`
	MemTotalMB  int64  `json:"mem_total_mb"`
	DiskFreeMB  int64  `json:"disk_free_mb"`
	GPUName     string `json:"gpu_name,omitempty"`
	VRAMTotalMB int64  `json:"vram_total_mb"`
	Driver      string `json:"driver,omitempty"`
	Metal       bool   `json:"metal"`
}

// InstallView is a running install.
type InstallView struct {
	Step     string  `json:"step"`
	Fraction float64 `json:"fraction"`
}

func (s *Server) view() ServerView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := ServerView{ID: s.id, DeviceID: s.devID, Name: s.rec.Name, Host: s.rec.Host, Port: s.rec.Port, Username: s.rec.Username,
		State: s.state, Platform: s.rec.Platform, Engine: s.rec.Engine, Trusted: s.rec.HostKey != "",
		PublicKey: s.rec.PublicKey, KeyFP: PublicFingerprint(s.rec.PublicKey), Settings: s.set, LastError: s.lastErr,
		AgentVersion: s.rec.AgentVersion, KitReady: s.rec.KitHash != "" && slices.Contains(s.probe.Kits, s.rec.KitHash)}
	if s.set.Paused {
		v.State = StatePaused
	} else if s.state == StateReady && len(s.runs) >= s.slotsLocked() {
		v.State = StateBusy
	}
	_ = jsonUnmarshal(s.rec.JumpJSON, &v.Jumps)
	if v.Jumps == nil {
		v.Jumps = []Jump{}
	}
	if s.rec.HostKey != "" {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.rec.HostKey)); err == nil {
			v.HostKeyFP = ssh.FingerprintSHA256(k)
		}
	}
	if s.rec.Platform != "" {
		v.Probe = &ProbeView{OSVersion: s.probe.OSVersion, CPUs: s.probe.CPUs, MemTotalMB: s.probe.MemTotalMB,
			DiskFreeMB: s.probe.DiskFreeMB, GPUName: s.probe.GPUName, VRAMTotalMB: s.probe.VRAMTotalMB,
			Driver: s.probe.Driver, Metal: s.probe.Metal}
	}
	if s.installing != nil {
		v.Install = &InstallView{Step: s.installing.step, Fraction: s.installing.fraction}
	}
	return v
}

// List returns every server.
func (m *Manager) List() []ServerView {
	m.mu.Lock()
	list := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		list = append(list, s)
	}
	m.mu.Unlock()
	out := make([]ServerView, 0, len(list))
	for _, s := range list {
		out = append(out, s.view())
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].Name < out[b].Name || (out[a].Name == out[b].Name && out[a].ID < out[b].ID)
	})
	return out
}

// Get returns one server (ok false when unknown).
func (m *Manager) Get(id string) (ServerView, bool) {
	s := m.server(id)
	if s == nil {
		return ServerView{}, false
	}
	return s.view(), true
}

// AddRequest is a new server.
type AddRequest struct {
	ID       string `json:"id"` // the slug ("" = from the name)
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Group    string `json:"group"`
	Jumps    []Jump `json:"jumps"`
}

// Validation errors (the API maps them to its codes).
var (
	ErrInvalid  = errors.New("sshremote: invalid server")
	ErrNotFound = errors.New("sshremote: no such server")
	ErrExists   = errors.New("sshremote: a server with this id exists")
	ErrBusy     = errors.New("sshremote: the server is transcribing")
)

// FieldError names the invalid field.
type FieldError struct{ Field string }

func (e *FieldError) Error() string { return "invalid " + e.Field }
func (e *FieldError) Unwrap() error { return ErrInvalid }

var (
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})?$|^[0-9a-fA-F:]{2,45}$`)
	userRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	slugCleanRe = regexp.MustCompile(`[^a-z0-9-]+`)
)

func checkEndpoint(host string, port int, user string, prefix string) error {
	switch {
	case !hostRe.MatchString(host) || strings.Contains(host, ".."):
		return &FieldError{Field: prefix + "host"}
	case port < 1 || port > 65535:
		return &FieldError{Field: prefix + "port"}
	case !userRe.MatchString(user):
		return &FieldError{Field: prefix + "username"}
	}
	return nil
}

func slugOf(name string) string {
	s := strings.Trim(slugCleanRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	if s == "" {
		s = "server"
	}
	return s
}

// Add creates a server: its key pair (the private key sealed in kv) and its
// row. It is not contacted yet (Connect).
func (m *Manager) Add(ctx context.Context, req AddRequest) (ServerView, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Name == "" || len([]rune(req.Name)) > 64 {
		return ServerView{}, &FieldError{Field: "name"}
	}
	if err := checkEndpoint(req.Host, req.Port, req.Username, ""); err != nil {
		return ServerView{}, err
	}
	for i := range req.Jumps {
		j := &req.Jumps[i]
		if j.Port == 0 {
			j.Port = 22
		}
		if err := checkEndpoint(j.Host, j.Port, j.Username, "jumps."); err != nil {
			return ServerView{}, err
		}
		j.HostKey = ""
	}
	if len(req.Jumps) > 3 {
		return ServerView{}, &FieldError{Field: "jumps"}
	}
	if req.Group == "" {
		req.Group = GroupSupplement
	}
	if req.Group != GroupPrefer && req.Group != GroupSupplement {
		return ServerView{}, &FieldError{Field: "group"}
	}
	id := req.ID
	if id == "" {
		id = slugOf(req.Name)
		m.mu.Lock()
		base := id
		for n := 2; m.servers[id] != nil; n++ {
			id = fmt.Sprintf("%s-%d", strings.TrimRight(base[:min(len(base), 29)], "-"), n)
		}
		m.mu.Unlock()
	}
	if !model.SSHSlugPattern.MatchString(id) {
		return ServerView{}, &FieldError{Field: "id"}
	}
	if m.server(id) != nil {
		return ServerView{}, ErrExists
	}
	sealed, pub, err := newKey("lectureflow-" + id)
	if err != nil {
		return ServerView{}, err
	}
	ref := KVKeyPrefix + id
	if err := m.o.Store.SetKV(ctx, ref, sealed); err != nil {
		return ServerView{}, err
	}
	now := m.now().UTC().Format(time.RFC3339)
	set := Settings{Group: req.Group}
	rec := db.RemoteServer{ID: id, Name: req.Name, Host: req.Host, Port: req.Port, Username: req.Username,
		JumpJSON: jsonString(req.Jumps, "[]"), KeyRef: ref, PublicKey: pub, SettingsJSON: jsonString(set, "{}"),
		CreatedAt: now, UpdatedAt: now}
	if err := m.o.Store.InsertRemoteServer(ctx, rec); err != nil {
		_ = m.o.Store.SetKV(ctx, ref, "")
		if errors.Is(err, db.ErrDuplicate) {
			return ServerView{}, ErrExists
		}
		return ServerView{}, err
	}
	s := newServer(m, rec)
	m.mu.Lock()
	m.servers[id] = s
	m.mu.Unlock()
	m.changed()
	slog.Info("ssh server added", "device", s.devID)
	return s.view(), nil
}

// Patch changes a server (nil = unchanged). A changed host or port drops
// the confirmed host key (it must be confirmed again).
type Patch struct {
	Name      *string `json:"name"`
	Host      *string `json:"host"`
	Port      *int    `json:"port"`
	Username  *string `json:"username"`
	Group     *string `json:"group"`
	Paused    *bool   `json:"paused"`
	Jumps     *[]Jump `json:"jumps"`
	RetainMin *int    `json:"retain_min"`
}

// Update applies a patch.
func (m *Manager) Update(ctx context.Context, id string, p Patch) (ServerView, error) {
	s := m.server(id)
	if s == nil {
		return ServerView{}, ErrNotFound
	}
	s.mu.Lock()
	rec, set := s.rec, s.set
	running := len(s.runs) > 0
	s.mu.Unlock()
	endpoint := false
	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if n == "" || len([]rune(n)) > 64 {
			return ServerView{}, &FieldError{Field: "name"}
		}
		rec.Name = n
	}
	if p.Host != nil && strings.TrimSpace(*p.Host) != rec.Host {
		rec.Host, endpoint = strings.TrimSpace(*p.Host), true
	}
	if p.Port != nil && *p.Port != rec.Port {
		rec.Port, endpoint = *p.Port, true
	}
	if p.Username != nil && *p.Username != rec.Username {
		rec.Username, endpoint = *p.Username, true
	}
	if p.Jumps != nil {
		js := *p.Jumps
		if len(js) > 3 {
			return ServerView{}, &FieldError{Field: "jumps"}
		}
		for i := range js {
			if js[i].Port == 0 {
				js[i].Port = 22
			}
			if err := checkEndpoint(js[i].Host, js[i].Port, js[i].Username, "jumps."); err != nil {
				return ServerView{}, err
			}
			js[i].HostKey = ""
		}
		rec.JumpJSON, endpoint = jsonString(js, "[]"), true
	}
	if err := checkEndpoint(rec.Host, rec.Port, rec.Username, ""); err != nil {
		return ServerView{}, err
	}
	if p.Group != nil {
		if *p.Group != GroupPrefer && *p.Group != GroupSupplement {
			return ServerView{}, &FieldError{Field: "group"}
		}
		set.Group = *p.Group
	}
	if p.Paused != nil {
		set.Paused = *p.Paused
	}
	if p.RetainMin != nil {
		if *p.RetainMin < 0 || *p.RetainMin > int(remote.MaxRetain/time.Minute) {
			return ServerView{}, &FieldError{Field: "retain_min"}
		}
		set.RetainMin = *p.RetainMin
	}
	if endpoint && running {
		return ServerView{}, ErrBusy
	}
	if endpoint {
		rec.HostKey = ""
	}
	s.mu.Lock()
	s.rec, s.set = rec, set
	if endpoint {
		s.state = StateUntrusted
		s.presented = nil
	}
	s.persistLocked(ctx)
	s.mu.Unlock()
	if endpoint {
		s.close()
	}
	if s.m.o.Devices != nil {
		if d, ok := s.m.o.Devices.Get(s.devID); ok && d.Label != rec.Name {
			d.Label = rec.Name
			_ = s.m.o.Devices.UpsertSSH(ctx, d)
		}
	}
	m.changed()
	return s.view(), nil
}

// ConnectResult is what Connect found.
type ConnectResult struct {
	Server ServerView `json:"server"`
	// HostKey is the presented key waiting for the user's confirmation
	// (Trust), of the hop named by HostKeyHop (0 = the first jump host or
	// the server).
	HostKey    *HostKeyInfo `json:"host_key,omitempty"`
	HostKeyHop int          `json:"host_key_hop"`
}

// Connect contacts the server: without a confirmed host key it returns
// the presented one; otherwise it connects, installs or updates the agent
// and probes the machine (no environment is installed: Install).
func (m *Manager) Connect(ctx context.Context, id string) (ConnectResult, error) {
	s := m.server(id)
	if s == nil {
		return ConnectResult{}, ErrNotFound
	}
	t, err := s.target(ctx)
	if err != nil {
		return ConnectResult{}, err
	}
	// Hops whose key is not confirmed: stop at the first and show it.
	var jumps []Jump
	_ = jsonUnmarshal(s.rec.JumpJSON, &jumps)
	for i, j := range jumps {
		if j.HostKey == "" {
			key, err := presentedKey(ctx, t, i)
			if err != nil {
				s.setState(StateOffline, err)
				return ConnectResult{Server: s.view()}, err
			}
			info := describeKey(key, j.Host, j.Port)
			return ConnectResult{Server: s.view(), HostKey: &info, HostKeyHop: i}, nil
		}
	}
	if s.rec.HostKey == "" {
		key, err := presentedKey(ctx, t, len(jumps))
		if err != nil {
			s.setState(StateOffline, err)
			return ConnectResult{Server: s.view()}, err
		}
		s.setState(StateUntrusted, nil)
		info := describeKey(key, s.rec.Host, s.rec.Port)
		return ConnectResult{Server: s.view(), HostKey: &info, HostKeyHop: len(jumps)}, nil
	}
	_, _, err = s.session(ctx, false)
	if errors.Is(err, errSetup) {
		err = nil // reachable, environment not installed yet: the wizard installs it
	}
	return ConnectResult{Server: s.view()}, err
}

// presentedKey connects up to hop and returns the key it presents (the
// connection stops there).
func presentedKey(ctx context.Context, t target, hop int) (ssh.PublicKey, error) {
	var got ssh.PublicKey
	t.seenKey = func(h int, k ssh.PublicKey) {
		if h == hop {
			got = k
		}
	}
	if hop < len(t.jumps) {
		t.jumps[hop].HostKey = ""
	} else {
		t.hostKey = ""
	}
	c, err := connect(ctx, t)
	if c != nil {
		c.close()
	}
	if got != nil {
		return got, nil
	}
	if err == nil {
		err = errors.New("no host key")
	}
	return nil, err
}

// Trust pins the host key with this fingerprint (the one Connect showed)
// for hop, then connects.
func (m *Manager) Trust(ctx context.Context, id string, hop int, fingerprint string) (ConnectResult, error) {
	s := m.server(id)
	if s == nil {
		return ConnectResult{}, ErrNotFound
	}
	t, err := s.target(ctx)
	if err != nil {
		return ConnectResult{}, err
	}
	var jumps []Jump
	_ = jsonUnmarshal(s.rec.JumpJSON, &jumps)
	if hop < 0 || hop > len(jumps) {
		return ConnectResult{}, &FieldError{Field: "hop"}
	}
	key, err := presentedKey(ctx, t, hop)
	if err != nil {
		return ConnectResult{}, err
	}
	if ssh.FingerprintSHA256(key) != fingerprint {
		// The server presents another key than the one the user confirmed.
		return ConnectResult{}, &FieldError{Field: "fingerprint"}
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	s.mu.Lock()
	if hop < len(jumps) {
		jumps[hop].HostKey = line
		s.rec.JumpJSON = jsonString(jumps, "[]")
	} else {
		s.rec.HostKey = line
		s.state = StateOffline
	}
	s.persistLocked(ctx)
	s.mu.Unlock()
	slog.Info("ssh server: host key confirmed", "device", s.devID, "hop", hop, "fingerprint", fingerprint)
	return m.Connect(ctx, id)
}

// Install installs (or checks) the environment in the background; the
// card and the view show its steps.
func (m *Manager) Install(id string) error {
	s := m.server(id)
	if s == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	if s.rec.HostKey == "" {
		s.mu.Unlock()
		return errHostKeyUnknown
	}
	if s.installing != nil {
		s.mu.Unlock()
		return nil
	}
	s.installing = &installState{step: "python"}
	s.mu.Unlock()
	s.setState(StateInstalling, nil)
	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Hour)
		defer cancel()
		s.close() // a fresh session checks the kit again
		_, _, err := s.session(ctx, true)
		s.mu.Lock()
		s.installing = nil
		s.mu.Unlock()
		if err != nil {
			slog.Warn("ssh server: installing the environment", "device", s.devID, "err", err)
			state, _, _ := connError(err)
			if errors.Is(err, errInstall) {
				state = StateSetup
			}
			s.setState(state, err)
			return
		}
		m.changed()
	}()
	return nil
}

// Test connects and probes now (the server's state says the result).
func (m *Manager) Test(ctx context.Context, id string) (ServerView, error) {
	s := m.server(id)
	if s == nil {
		return ServerView{}, ErrNotFound
	}
	s.close()
	_, _, err := s.session(ctx, false)
	return s.view(), err
}

// PublicKey is the line to add to the server's authorized_keys.
type PublicKey struct {
	Line        string `json:"line"`
	Fingerprint string `json:"fingerprint"`
	// Command appends the line on the server (to run there, by the user).
	Command string `json:"command"`
	// CommandWindowsAdmin is the variant for an administrator account on a
	// Windows server (administrators_authorized_keys).
	CommandWindowsAdmin string `json:"command_windows_admin"`
}

// PublicKeyOf returns the server's public key.
func (m *Manager) PublicKeyOf(id string) (PublicKey, error) {
	s := m.server(id)
	if s == nil {
		return PublicKey{}, ErrNotFound
	}
	s.mu.Lock()
	line := s.rec.PublicKey
	s.mu.Unlock()
	return PublicKey{Line: line, Fingerprint: PublicFingerprint(line),
		Command: "mkdir -p ~/.ssh && chmod 700 ~/.ssh && printf '%s\\n' " + shellQuote(line) +
			" >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys",
		CommandWindowsAdmin: `Add-Content -Path "$env:ProgramData\ssh\administrators_authorized_keys" -Value '` + line + `'`}, nil
}

// Remove deletes a server: with uninstall, "lf-agent uninstall" first
// (best effort); then its key, row and device row.
func (m *Manager) Remove(ctx context.Context, id string, uninstall bool) error {
	s := m.server(id)
	if s == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	running := len(s.runs) > 0
	rec := s.rec
	s.mu.Unlock()
	if running {
		return ErrBusy
	}
	if uninstall && rec.HostKey != "" {
		if err := s.uninstall(ctx); err != nil {
			slog.Warn("ssh server: uninstalling lf-agent failed (its directories stay on the server)", "device", s.devID, "err", err)
		}
	}
	s.close()
	if err := m.o.Store.DeleteRemoteServer(ctx, id, rec.KeyRef); err != nil {
		return err
	}
	if m.o.Devices != nil {
		if err := m.o.Devices.RemoveSSH(ctx, s.devID); err != nil {
			slog.Warn("ssh server: removing its device row", "device", s.devID, "err", err)
		}
	}
	m.mu.Lock()
	delete(m.servers, id)
	delete(m.builds, id)
	m.mu.Unlock()
	m.changed()
	slog.Info("ssh server removed", "device", s.devID)
	return nil
}

// uninstall runs "lf-agent uninstall" on the server.
func (s *Server) uninstall(ctx context.Context) error {
	t, err := s.target(ctx)
	if err != nil {
		return err
	}
	c, err := connect(ctx, t)
	if err != nil {
		return err
	}
	defer c.close()
	platform, err := detectPlatform(ctx, c)
	if err != nil {
		return err
	}
	b, err := findAgent(s.m.o.AgentDir, platform)
	if err != nil {
		return err
	}
	if _, ok, err := agentVersion(ctx, c, platform, b); err != nil || !ok {
		if _, err := installAgent(ctx, c, platform, b); err != nil {
			return err
		}
	}
	_, err = agentEvent(ctx, c, platform, b, nil, "uninstall")
	return err
}

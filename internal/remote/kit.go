// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

// The environment a remote runs the workers in (plan §4): a
// python-build-standalone runtime, a venv installed from a lock file with
// pip --require-hashes, the worker scripts and one model, all checked
// against SHA-256 from the component manifest. "lf-agent ensure" reads a
// KitSpec from stdin; its Hash names the kit, and every run carries it, so
// a remote never runs a worker with other files than the ones this build
// sent (kit_hash).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// KitSpec is one environment.
type KitSpec struct {
	// Version is the agent version the spec was made for.
	Version string     `json:"version"`
	Engine  string     `json:"engine"`
	Python  KitArchive `json:"python"`
	Lock    KitLock    `json:"lock"`
	// IndexURL is the package index (the user's mirror setting).
	IndexURL string `json:"index_url"`
	// Imports are checked after installing ("import a, b").
	Imports []string  `json:"imports"`
	Worker  []KitFile `json:"worker"`
	Model   KitModel  `json:"model"`
}

// KitArchive is the python-build-standalone archive.
type KitArchive struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Exe is the interpreter inside the unpacked archive ("python/bin/python3").
	Exe string `json:"exe"`
}

// KitLock is the lock file (uv pip compile --generate-hashes output).
type KitLock struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Text   string `json:"text"`
}

// KitFile is a worker script.
type KitFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

// KitModel is the model component.
type KitModel struct {
	ID    string        `json:"id"`
	Files []KitDownload `json:"files"`
}

// KitDownload is one file to download.
type KitDownload struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

var (
	kitNameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	modelIDRe    = regexp.MustCompile(`^model:[a-z-]{2,20}:[A-Za-z0-9._-]{1,64}:[A-Za-z0-9._-]{1,32}$`)
	importRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	exeRe        = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+){0,4}$`)
	maxKitFiles  = 32
	maxLockBytes = 1 << 20
)

// WorkerScript is the script an engine runs.
func WorkerScript(engine string) string {
	if engine == EngineMLX {
		return "asr_mlx.py"
	}
	return "asr.py"
}

// Validate checks the spec (the agent refuses anything else).
func (k KitSpec) Validate() error {
	if !VersionStringPattern.MatchString(k.Version) {
		return errors.New("bad version")
	}
	if k.Engine != EngineMLX && k.Engine != EngineFasterWhisper {
		return errors.New("bad engine")
	}
	if err := checkURL(k.Python.URL); err != nil {
		return fmt.Errorf("python url: %w", err)
	}
	if !SHA256Pattern.MatchString(k.Python.SHA256) || k.Python.Size <= 0 || k.Python.Size > 1<<30 {
		return errors.New("bad python archive")
	}
	if !exeRe.MatchString(k.Python.Exe) || strings.Contains(k.Python.Exe, "..") {
		return errors.New("bad python exe")
	}
	if !kitNameRe.MatchString(k.Lock.Name) || !SHA256Pattern.MatchString(k.Lock.SHA256) || len(k.Lock.Text) > maxLockBytes {
		return errors.New("bad lock")
	}
	if sha(k.Lock.Text) != k.Lock.SHA256 {
		return errors.New("lock hash mismatch")
	}
	if k.IndexURL != "" {
		if err := checkURL(k.IndexURL); err != nil {
			return fmt.Errorf("index url: %w", err)
		}
	}
	for _, im := range k.Imports {
		if !importRe.MatchString(im) {
			return errors.New("bad import")
		}
	}
	if len(k.Worker) == 0 || len(k.Worker) > maxKitFiles {
		return errors.New("bad worker files")
	}
	names := map[string]bool{}
	for _, f := range k.Worker {
		if !kitNameRe.MatchString(f.Name) || !strings.HasSuffix(f.Name, ".py") || names[f.Name] {
			return errors.New("bad worker file name")
		}
		names[f.Name] = true
		if sha(string(f.Data)) != f.SHA256 {
			return errors.New("worker file hash mismatch")
		}
	}
	if !names[WorkerScript(k.Engine)] {
		return errors.New("the engine's worker script is missing")
	}
	if !modelIDRe.MatchString(k.Model.ID) || len(k.Model.Files) == 0 || len(k.Model.Files) > maxKitFiles {
		return errors.New("bad model")
	}
	mnames := map[string]bool{}
	for _, f := range k.Model.Files {
		if !kitNameRe.MatchString(f.Name) || mnames[f.Name] {
			return errors.New("bad model file name")
		}
		mnames[f.Name] = true
		if err := checkURL(f.URL); err != nil {
			return fmt.Errorf("model url: %w", err)
		}
		if !SHA256Pattern.MatchString(f.SHA256) || f.Size <= 0 || f.Size > 32<<30 {
			return errors.New("bad model file")
		}
	}
	return nil
}

// checkURL allows https URLs (and http to a loopback host, for tests).
func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("not a URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		if h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return errors.New("not https")
		}
	default:
		return errors.New("not https")
	}
	return nil
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Hash is the kit hash: SHA-256 over everything that decides what runs.
func (k KitSpec) Hash() string {
	h := sha256.New()
	w := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	w("lectureflow-kit-1", k.Version, k.Engine, k.Python.SHA256, k.Python.Exe, k.Lock.SHA256)
	w(k.Imports...)
	files := slices.Clone(k.Worker)
	slices.SortFunc(files, func(a, b KitFile) int { return strings.Compare(a.Name, b.Name) })
	for _, f := range files {
		w(f.Name, f.SHA256)
	}
	w(k.Model.ID)
	mf := slices.Clone(k.Model.Files)
	slices.SortFunc(mf, func(a, b KitDownload) int { return strings.Compare(a.Name, b.Name) })
	for _, f := range mf {
		w(f.Name, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PythonKey, VenvKey, WorkerKey and ModelKey name the content-addressed
// directories of a kit on the remote (16 hex characters each).
func (k KitSpec) PythonKey() string { return k.Python.SHA256[:16] }

// VenvKey depends on the runtime and the lock.
func (k KitSpec) VenvKey() string {
	return sha(k.Engine + "\x00" + k.Python.SHA256 + "\x00" + k.Lock.SHA256)[:16]
}

// WorkerKey depends on the scripts.
func (k KitSpec) WorkerKey() string {
	h := sha256.New()
	files := slices.Clone(k.Worker)
	slices.SortFunc(files, func(a, b KitFile) int { return strings.Compare(a.Name, b.Name) })
	for _, f := range files {
		h.Write([]byte(f.Name + "\x00" + f.SHA256 + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ModelKey is the model's directory name.
func (k KitSpec) ModelKey() string {
	return strings.NewReplacer(":", "__").Replace(k.Model.ID)
}

// URLBase is the last element of a download URL (for messages).
func URLBase(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

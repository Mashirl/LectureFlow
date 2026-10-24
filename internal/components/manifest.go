// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// SchemaVersion is the manifest schema this package reads; a manifest with a
// higher schema is rejected (components.manifest_schema).
const SchemaVersion = 1

// Component kinds.
const (
	// KindPythonRuntime: a python-build-standalone archive unpacked to
	// runtime/ plus the base virtual environment venv/ from the lock file.
	KindPythonRuntime = "python-runtime"
	// KindVenv: a uv virtual environment venv/ built with the python
	// component's interpreter from the lock file.
	KindVenv = "venv"
	// KindModel: model files in model/.
	KindModel = "model"
	// KindBinary: files in bin/ (unpacked when Unpack is set); Bundled
	// binaries ship with the installer (core).
	KindBinary = "binary"
)

// Platforms (V0).
const (
	PlatformWindowsX64  = "windows-x64"
	PlatformDarwinARM64 = "darwin-arm64"
)

// Component ids (.claude/v3-brief.md §4) and the backends of Python /
// ModelDir.
const (
	IDCore        = "core"
	IDPython      = "python"
	IDASRCUDA     = "asr-cuda"
	IDASROpenVINO = "asr-openvino"
	IDASRMLX      = "asr-mlx"

	BackendBase          = "base" // Python("base"): the python component's base venv (pypdf)
	BackendFasterWhisper = "faster-whisper"
	BackendOpenVINO      = "openvino"
	BackendMLX           = "mlx"
)

// CurrentPlatform is the platform of this process: windows-x64,
// darwin-arm64, or GOOS-GOARCH for platforms without components.
func CurrentPlatform() string {
	switch {
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		return PlatformWindowsX64
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return PlatformDarwinARM64
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

// ModelID is the component id of a model: model:{backend}:{name}:{precision}.
func ModelID(backend, name, precision string) string {
	return "model:" + backend + ":" + name + ":" + precision
}

// ParseModelID splits a model component id.
func ParseModelID(id string) (backend, name, precision string, ok bool) {
	p := strings.Split(id, ":")
	if len(p) != 4 || p[0] != "model" || p[1] == "" || p[2] == "" || p[3] == "" {
		return "", "", "", false
	}
	return p[1], p[2], p[3], true
}

// DirName is the directory of a component under components/: the id with
// ":" replaced by "__" (model:openvino:small:int8 → model__openvino__small__int8).
func DirName(id string) string { return strings.ReplaceAll(id, ":", "__") }

// Manifest is components.json (V3.2).
type Manifest struct {
	Schema int `json:"schema"`
	// Serial orders manifests: a manifest from the network replaces the
	// current one only when its Serial is larger.
	Serial    int64  `json:"serial"`
	Published string `json:"published,omitempty"`
	// AppVersion is the application version this manifest was published with.
	AppVersion string `json:"app_version"`
	// Compat is the range of application versions that may use this
	// manifest (VersionRange syntax), e.g. ">=3.0.0 <4.0.0".
	Compat string `json:"compat"`
	// UpdateURL is where CheckManifest looks for a newer manifest ("" = none).
	UpdateURL string `json:"update_url,omitempty"`
	// Sources are the download sources of the URL placeholders.
	Sources    map[string]SourceGroup `json:"sources"`
	Components []Component            `json:"components"`
	Presets    Presets                `json:"presets"`

	compat VersionRange
	dir    string // directory the manifest was loaded from (lock paths are relative to it)
}

// SourceGroup is one independently selectable source (V3.3): "pypi" (Python
// packages and the Python runtime), "models", "files".
type SourceGroup struct {
	// Primary is the placeholder a custom mirror URL replaces.
	Primary string      `json:"primary"`
	Default SourceDef   `json:"default"`
	Mirrors []SourceDef `json:"mirrors,omitempty"`
}

// SourceDef is a source: URL bases of the group's placeholders. A mirror
// overrides only the placeholders it lists.
type SourceDef struct {
	ID      string            `json:"id"`
	NameKey string            `json:"name_key,omitempty"`
	URL     string            `json:"url,omitempty"`    // homepage, shown in settings
	Region  string            `json:"region,omitempty"` // "cn": suggested for users in mainland China
	Values  map[string]string `json:"values"`
}

// Component is one entry of the manifest.
type Component struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Upstream string `json:"upstream,omitempty"` // display: what the component contains
	Kind     string `json:"kind"`
	Bundled  bool   `json:"bundled,omitempty"` // core: part of the installer, never downloaded
	NameKey  string `json:"name_key,omitempty"`
	DescKey  string `json:"desc_key,omitempty"`
	// Depends are the ids this component needs (installed first).
	Depends []string `json:"depends,omitempty"`
	// Backend: venv components = the backend whose interpreter it is
	// (Python(backend)); model components = the model's backend.
	Backend   string `json:"backend,omitempty"`
	Model     string `json:"model,omitempty"`
	Precision string `json:"precision,omitempty"`
	// License (SPDX) and its origin (V7); Source is the upstream URL,
	// Revision the pinned upstream revision (models: the Hugging Face commit).
	License     string `json:"license,omitempty"`
	LicenseNote string `json:"license_note,omitempty"`
	Source      string `json:"source,omitempty"`
	Revision    string `json:"revision,omitempty"`
	// MinApp is the lowest application version that can use the component.
	MinApp string `json:"min_app,omitempty"`
	// Unavailable is a reason code; such a component cannot be installed.
	Unavailable string `json:"unavailable,omitempty"`
	// CheckImports are the modules the post-install check imports (venv and
	// python components).
	CheckImports []string `json:"check_imports,omitempty"`
	// Platforms holds one build per platform; a component without a build
	// for the current platform does not exist there.
	Platforms map[string]*Build `json:"platforms"`
}

// Build is a component's download for one platform.
type Build struct {
	// DownloadSize is the bytes to download; InstalledSize the bytes on disk
	// after installing (InstalledEstimated: an estimate, e.g. unpacked wheels).
	DownloadSize       int64  `json:"download_size"`
	InstalledSize      int64  `json:"installed_size"`
	InstalledEstimated bool   `json:"installed_size_estimated,omitempty"`
	Files              []File `json:"files,omitempty"`
	// Exe is the interpreter inside the unpacked runtime (python-runtime).
	Exe     string   `json:"exe,omitempty"`
	Lock    *Lock    `json:"lock,omitempty"`
	Offline *Offline `json:"offline,omitempty"`
	MinOS   string   `json:"min_os,omitempty"`
	// MaxPath is the length (characters) of the longest file path inside
	// the installed component, relative to its version directory
	// (components/{dir}/{version}/): measured on an installation, because a
	// venv's contents are known only after uv built it. 0 = unknown: model
	// and binary builds are computed from their file names, a venv or
	// runtime build without it is not checked. Windows only (see
	// pathlen.go).
	MaxPath int `json:"max_path,omitempty"`
}

// File is one downloaded file.
type File struct {
	// Name is the path inside the component directory (model/, bin/) or the
	// archive's file name; "/"-separated, relative.
	Name   string `json:"name"`
	URL    string `json:"url"` // template with {placeholder}s
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Unpack string `json:"unpack,omitempty"` // "tar.gz" | "zip" | ""
}

// Lock is a uv lock file (uv pip compile --generate-hashes).
type Lock struct {
	// Path is relative to the manifest's directory (the built-in
	// resources/ directory, or components/manifest.d/ for a downloaded one).
	Path     string `json:"path"`
	URL      string `json:"url,omitempty"` // template; required for manifests from the network
	SHA256   string `json:"sha256"`
	Packages int    `json:"packages,omitempty"`
}

// Offline is the offline archive of a component build (ImportFile): a zip
// whose SHA-256 must equal this entry.
type Offline struct {
	Name   string `json:"name,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	URL    string `json:"url,omitempty"`
}

var (
	placeholderRe = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)
	sha256Re      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	idRe          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// ParseManifest parses and validates a manifest. dir is the directory lock
// paths are relative to.
func ParseManifest(data []byte, dir string) (*Manifest, error) {
	var head struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, cerr(CodeManifestInvalid, nil, err)
	}
	if head.Schema < 1 || head.Schema > SchemaVersion {
		return nil, cerr(CodeManifestSchema, map[string]any{"schema": head.Schema},
			fmt.Errorf("manifest schema %d is not supported (this build reads %d)", head.Schema, SchemaVersion))
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, cerr(CodeManifestInvalid, nil, err)
	}
	m.dir = dir
	if err := m.Validate(); err != nil {
		return nil, cerr(CodeManifestInvalid, nil, err)
	}
	return &m, nil
}

// LoadManifest reads and parses a manifest file; lock paths are relative to
// its directory.
func LoadManifest(file string) (*Manifest, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, cerr(CodeManifestInvalid, nil, err)
	}
	return ParseManifest(data, dirOf(file))
}

func dirOf(file string) string {
	i := strings.LastIndexAny(file, `/\`)
	if i < 0 {
		return "."
	}
	return file[:i]
}

// Validate checks the manifest's structure; the error lists every problem.
func (m *Manifest) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if m.Schema != SchemaVersion {
		add("schema %d", m.Schema)
	}
	if _, err := ParseVersion(m.AppVersion); err != nil {
		add("app_version: %v", err)
	}
	r, err := ParseRange(m.Compat)
	if err != nil {
		add("compat: %v", err)
	}
	m.compat = r

	placeholders := map[string]string{} // placeholder -> group
	for g, sg := range m.Sources {
		if !idRe.MatchString(g) {
			add("source group %q: invalid name", g)
		}
		if sg.Default.ID == "" || len(sg.Default.Values) == 0 {
			add("source group %q: default needs an id and values", g)
		}
		if _, ok := sg.Default.Values[sg.Primary]; !ok {
			add("source group %q: primary %q is not a default value", g, sg.Primary)
		}
		for k, v := range sg.Default.Values {
			if prev, dup := placeholders[k]; dup {
				add("placeholder {%s} is defined by groups %q and %q", k, prev, g)
			}
			placeholders[k] = g
			if v != "" && !isHTTPURL(v) {
				add("source group %q: default {%s} = %q is not an http(s) URL", g, k, v)
			}
		}
		seen := map[string]bool{sg.Default.ID: true}
		for _, mr := range sg.Mirrors {
			if mr.ID == "" || seen[mr.ID] {
				add("source group %q: mirror id %q missing or duplicate", g, mr.ID)
			}
			seen[mr.ID] = true
			for k, v := range mr.Values {
				if _, ok := sg.Default.Values[k]; !ok {
					add("source group %q mirror %q: {%s} is not a placeholder of the group", g, mr.ID, k)
				}
				if !isHTTPURL(v) {
					add("source group %q mirror %q: {%s} = %q is not an http(s) URL", g, mr.ID, k, v)
				}
			}
		}
	}
	checkURL := func(where, tmpl string, required bool) {
		if tmpl == "" {
			if required {
				add("%s: url missing", where)
			}
			return
		}
		ms := placeholderRe.FindAllStringSubmatch(tmpl, -1)
		if len(ms) == 0 && !isHTTPURL(tmpl) {
			add("%s: url %q has no placeholder and is not an http(s) URL", where, tmpl)
		}
		if len(ms) > 1 {
			add("%s: url %q has more than one placeholder", where, tmpl)
		}
		for _, mm := range ms {
			if _, ok := placeholders[mm[1]]; !ok {
				add("%s: unknown placeholder {%s}", where, mm[1])
			}
		}
		if len(ms) == 1 && !strings.HasPrefix(tmpl, ms[0][0]) {
			add("%s: url %q must start with its placeholder", where, tmpl)
		}
	}

	byID := map[string]*Component{}
	for i := range m.Components {
		c := &m.Components[i]
		where := "component " + c.ID
		if c.ID == "" {
			add("component #%d: id missing", i)
			continue
		}
		if byID[c.ID] != nil {
			add("%s: duplicate id", where)
		}
		byID[c.ID] = c
		if _, err := ParseVersion(c.Version); err != nil {
			add("%s: version: %v", where, err)
		}
		if c.MinApp != "" {
			if _, err := ParseVersion(c.MinApp); err != nil {
				add("%s: min_app: %v", where, err)
			}
		}
		switch c.Kind {
		case KindPythonRuntime, KindVenv, KindModel, KindBinary:
		default:
			add("%s: unknown kind %q", where, c.Kind)
		}
		if b, n, p, ok := ParseModelID(c.ID); ok {
			if c.Kind != KindModel || c.Backend != b || c.Model != n || c.Precision != p {
				add("%s: a model id needs kind model and backend/model/precision %s/%s/%s", where, b, n, p)
			}
		} else if c.Kind == KindModel {
			add("%s: kind model needs an id model:{backend}:{name}:{precision}", where)
		} else if !idRe.MatchString(c.ID) {
			add("%s: invalid id", where)
		}
		if c.Kind == KindVenv && c.Backend == "" {
			add("%s: a venv needs its backend", where)
		}
		if c.Bundled {
			if c.Kind != KindBinary || len(c.Depends) > 0 {
				add("%s: a bundled component is a binary without dependencies", where)
			}
			continue
		}
		if len(c.Platforms) == 0 {
			add("%s: no platform", where)
		}
		for plat, b := range c.Platforms {
			pw := where + " " + plat
			if plat != PlatformWindowsX64 && plat != PlatformDarwinARM64 {
				add("%s: unknown platform", pw)
			}
			if b == nil {
				add("%s: empty build", pw)
				continue
			}
			if c.Unavailable != "" {
				continue // details may be missing: never installed
			}
			if b.DownloadSize < 0 || b.InstalledSize <= 0 {
				add("%s: download_size / installed_size", pw)
			}
			if b.MaxPath < 0 || b.MaxPath > 4096 {
				add("%s: max_path %d", pw, b.MaxPath)
			}
			names := map[string]bool{}
			var sum int64
			for _, f := range b.Files {
				fw := pw + " file " + f.Name
				if !safeRelPath(f.Name) {
					add("%s: unsafe name", fw)
				}
				if names[f.Name] {
					add("%s: duplicate name", fw)
				}
				names[f.Name] = true
				if !sha256Re.MatchString(f.SHA256) {
					add("%s: sha256 must be 64 lowercase hex digits", fw)
				}
				if f.Size <= 0 {
					add("%s: size", fw)
				}
				sum += f.Size
				checkURL(fw, f.URL, true)
				switch f.Unpack {
				case "", "tar.gz", "zip":
				default:
					add("%s: unknown unpack %q", fw, f.Unpack)
				}
			}
			if b.Lock != nil {
				if !safeRelPath(b.Lock.Path) || !sha256Re.MatchString(b.Lock.SHA256) {
					add("%s: lock needs a relative path and a sha256", pw)
				}
				checkURL(pw+" lock", b.Lock.URL, false)
			}
			if b.Offline != nil {
				if !sha256Re.MatchString(b.Offline.SHA256) || b.Offline.Size <= 0 {
					add("%s: offline needs a sha256 and a size", pw)
				}
				checkURL(pw+" offline", b.Offline.URL, false)
			}
			switch c.Kind {
			case KindPythonRuntime:
				if len(b.Files) != 1 || b.Files[0].Unpack == "" || b.Exe == "" || !safeRelPath(b.Exe) || b.Lock == nil {
					add("%s: a python runtime has one archive to unpack, an exe and a lock", pw)
				}
			case KindVenv:
				if b.Lock == nil || len(b.Files) > 0 {
					add("%s: a venv has a lock and no files", pw)
				}
			case KindModel:
				if len(b.Files) == 0 || b.Lock != nil {
					add("%s: a model has files and no lock", pw)
				}
				for _, f := range b.Files {
					if f.Unpack != "" {
						add("%s: model files are not unpacked", pw)
					}
				}
				if sum != b.DownloadSize {
					add("%s: download_size %d != sum of file sizes %d", pw, b.DownloadSize, sum)
				}
			case KindBinary:
				if len(b.Files) == 0 {
					add("%s: a binary has files", pw)
				}
			}
		}
	}
	for _, c := range m.Components {
		for _, d := range c.Depends {
			dc := byID[d]
			switch {
			case dc == nil:
				add("component %s: depends on unknown %q", c.ID, d)
			case dc.Kind == KindModel:
				add("component %s: depends on a model", c.ID)
			}
		}
		if c.Kind == KindVenv {
			py := byID[IDPython]
			if py == nil || !contains(c.Depends, IDPython) {
				add("component %s: a venv depends on %q", c.ID, IDPython)
			}
		}
	}
	if cyc := findCycle(m.Components); cyc != "" {
		add("dependency cycle through %s", cyc)
	}
	errs = append(errs, m.Presets.validate()...)
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func findCycle(cs []Component) string {
	deps := map[string][]string{}
	for _, c := range cs {
		deps[c.ID] = c.Depends
	}
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string) string
	visit = func(id string) string {
		switch state[id] {
		case 1:
			return id
		case 2:
			return ""
		}
		state[id] = 1
		for _, d := range deps[id] {
			if c := visit(d); c != "" {
				return c
			}
		}
		state[id] = 2
		return ""
	}
	ids := make([]string, 0, len(deps))
	for id := range deps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if c := visit(id); c != "" {
			return c
		}
	}
	return ""
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// safeRelPath: a "/"-separated relative path without "..", "." or empty
// elements and without a drive or backslash.
func safeRelPath(p string) bool {
	if p == "" || strings.ContainsAny(p, `\:`) || strings.HasPrefix(p, "/") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, e := range strings.Split(p, "/") {
		if e == "" || e == "." || e == ".." {
			return false
		}
	}
	return true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Compatible reports whether an application version may use the manifest.
// A version that is not semver (development builds: "dev") is compatible
// with every manifest.
func (m *Manifest) Compatible(appVersion string) bool {
	v, err := ParseVersion(appVersion)
	if err != nil {
		return true
	}
	return m.compat.Contains(v)
}

// Component returns the component with the id.
func (m *Manifest) Component(id string) *Component {
	for i := range m.Components {
		if m.Components[i].ID == id {
			return &m.Components[i]
		}
	}
	return nil
}

// Build returns the component's build for a platform (nil when there is none).
func (c *Component) Build(platform string) *Build {
	if c == nil || c.Platforms == nil {
		return nil
	}
	return c.Platforms[platform]
}

// availableFor returns "" when the component can be installed on the
// platform by the application version, else the reason code.
func (c *Component) availableFor(platform, appVersion string) string {
	if c.Unavailable != "" {
		return c.Unavailable
	}
	if c.Bundled {
		return ""
	}
	if c.Build(platform) == nil {
		return "platform"
	}
	if c.MinApp != "" {
		if av, err := ParseVersion(appVersion); err == nil {
			if mv, err := ParseVersion(c.MinApp); err == nil && av.Compare(mv) < 0 {
				return "app_too_old"
			}
		}
	}
	return ""
}

// placeholderOf returns the placeholder a URL template starts with.
func placeholderOf(tmpl string) string {
	if m := placeholderRe.FindStringSubmatch(tmpl); m != nil && strings.HasPrefix(tmpl, m[0]) {
		return m[1]
	}
	return ""
}

// groupOf returns the source group defining a placeholder.
func (m *Manifest) groupOf(ph string) (string, *SourceGroup) {
	for name, g := range m.Sources {
		if _, ok := g.Default.Values[ph]; ok {
			g := g
			return name, &g
		}
	}
	return "", nil
}

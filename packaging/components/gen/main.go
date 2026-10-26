// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command gen writes packaging/components/components.json from
// manifest.src.json: it fills in the SHA-256 and sizes of the
// python-build-standalone archives (official SHA256SUMS, cross-checked with
// the GitHub release API), of the lock files and their wheels (PyPI JSON
// API), and of the model files at their pinned Hugging Face revisions (LFS
// metadata; small files are fetched and checked against their git blob id).
// Model weights are never downloaded. With -offline it also builds offline
// component archives (this downloads everything they contain).
//
//	go run ./packaging/components/gen                      # refresh components.json
//	go run ./packaging/components/gen -offline python,model:openvino:small:int8 -offline-out dist/offline
//
// -offline refuses components whose lock file contains a Python package that
// packaging/licenses/allowlist.json accepts only as "user-fetched" (asr-cuda
// with the NVIDIA libraries, av and ctranslate2; asr-mlx with regex): an
// offline archive would distribute those packages (userfetched.go). It also
// refuses tools (gcloud), which are downloaded from their vendor; their
// pinned archives are checked with a HEAD request (tools.go).
//
// Run it from the repository root. It needs the network; nothing in the
// test suite runs it.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/devices"
)

type srcManifest struct {
	Doc        string `json:"_doc"`
	Schema     int    `json:"schema"`
	Serial     int64  `json:"serial"`
	Published  string `json:"published"`
	AppVersion string `json:"app_version"`
	Compat     string `json:"compat"`
	UpdateURL  string `json:"update_url"`
	// InstalledFactor estimates the unpacked size: wheels × Venv[platform]
	// (measured: the CUDA venv takes 1.6× its wheels; torch on macOS more),
	// runtime archive × Runtime.
	InstalledFactor struct {
		Venv    map[string]float64 `json:"venv"`
		Runtime float64            `json:"runtime"`
	} `json:"installed_factor"`
	Sources    map[string]components.SourceGroup `json:"sources"`
	Components []json.RawMessage                 `json:"components"`
	Models     []srcModel                        `json:"models"`
	Presets    components.Presets                `json:"presets"`
}

type srcGen struct {
	PBS *struct {
		Release   string `json:"release"`
		CPython   string `json:"cpython"`
		Flavor    string `json:"flavor"`
		Platforms map[string]struct {
			Triple string `json:"triple"`
			Exe    string `json:"exe"`
			MinOS  string `json:"min_os"`
		} `json:"platforms"`
	} `json:"python_build_standalone"`
	Locks []string `json:"locks"`
	// InstalledFactor / RuntimeFactor override installed_factor.venv and
	// installed_factor.runtime for this component, per platform: measured on
	// a real installation (installed bytes ÷ wheel bytes; the runtime's share
	// ÷ archive bytes), because the ratio differs a lot between environments
	// (V12.2: asr-openvino 3.07×, asr-cuda 1.63×).
	InstalledFactor map[string]float64 `json:"installed_factor"`
	RuntimeFactor   map[string]float64 `json:"runtime_factor"`
	// MaxPath is Build.MaxPath per platform: the longest file path inside
	// the installed component, relative to its version directory, measured
	// on an installation (Windows path length check, V12.2).
	MaxPath map[string]int `json:"max_path"`
}

type srcModel struct {
	Backend              string   `json:"backend"`
	Name                 string   `json:"name"`
	Precisions           []string `json:"precisions"`
	Version              string   `json:"version"`
	Repo                 string   `json:"repo"`
	Revision             string   `json:"revision"`
	Files                []string `json:"files"` // patterns; "from=name" renames
	Exclude              []string `json:"exclude"`
	RequireOpenVINOFiles bool     `json:"require_openvino_files"`
	Platforms            []string `json:"platforms"`
	Depends              []string `json:"depends"`
	License              string   `json:"license"`
	LicenseNote          string   `json:"license_note"`
	Upstream             string   `json:"upstream"`
}

const (
	pbsRepo   = "astral-sh/python-build-standalone"
	hfBase    = "https://huggingface.co"
	pypiJSON  = "https://pypi.org/pypi"
	userAgent = "LectureFlow-components-gen"
)

var client = &http.Client{Timeout: 120 * time.Second}

func main() {
	src := flag.String("src", "packaging/components/manifest.src.json", "manifest source")
	out := flag.String("out", "packaging/components/components.json", "manifest to write")
	offline := flag.String("offline", "", "comma-separated component ids to build offline archives for (downloads them; "+
		"components with user-fetched packages are refused)")
	offlineOut := flag.String("offline-out", "dist/offline", "directory for offline archives")
	allowlist := flag.String("allowlist", "packaging/licenses/allowlist.json", "license allowlist (its user-fetched packages never go into offline archives)")
	flag.Parse()
	if err := run(*src, *out, *offline, *offlineOut, *allowlist); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(srcPath, outPath, offline, offlineOut, allowlistPath string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	var src srcManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&src); err != nil {
		return fmt.Errorf("%s: %w", srcPath, err)
	}
	dir := filepath.Dir(outPath)
	var offlineIDs []string
	for _, id := range strings.Split(offline, ",") {
		if id = strings.TrimSpace(id); id != "" {
			offlineIDs = append(offlineIDs, id)
		}
	}
	uf, ufErr := loadUserFetched(allowlistPath)
	if len(offlineIDs) > 0 {
		if ufErr != nil {
			return fmt.Errorf("-offline needs the license allowlist: %w", ufErr)
		}
		if err := checkOffline(&src, dir, offlineIDs, uf); err != nil {
			return err
		}
	}
	m := &components.Manifest{
		Schema: src.Schema, Serial: src.Serial, Published: src.Published, AppVersion: src.AppVersion,
		Compat: src.Compat, UpdateURL: src.UpdateURL, Sources: src.Sources, Presets: src.Presets,
	}
	g := &gen{src: &src, dir: dir, pypi: map[string]*pypiRelease{}, userFetched: uf}
	for _, raw := range src.Components {
		c, err := g.component(raw)
		if err != nil {
			return err
		}
		m.Components = append(m.Components, c)
	}
	for _, sm := range src.Models {
		cs, err := g.models(sm)
		if err != nil {
			return err
		}
		m.Components = append(m.Components, cs...)
	}
	if err := checkTools(m); err != nil {
		return err
	}
	preserveOffline(m, outPath)
	if ufErr != nil {
		fmt.Fprintf(os.Stderr, "gen: WARNING %v; offline entries are not checked for user-fetched packages\n", ufErr)
	} else {
		dropUserFetchedOffline(&src, dir, m, uf)
	}
	for _, id := range offlineIDs {
		if err := g.buildOffline(m, id, offlineOut); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep ">=3.0.0 <4.0.0" readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	b := buf.Bytes()
	if _, err := components.ParseManifest(b, dir); err != nil {
		return fmt.Errorf("the generated manifest is invalid: %w", err)
	}
	if err := os.WriteFile(outPath, b, 0o644); err != nil {
		return err
	}
	g.summary(m)
	return nil
}

type gen struct {
	src         *srcManifest
	dir         string
	pypi        map[string]*pypiRelease
	userFetched userFetched
}

func (g *gen) component(raw json.RawMessage) (components.Component, error) {
	var withGen struct {
		Gen *srcGen `json:"gen"`
	}
	json.Unmarshal(raw, &withGen)
	var obj map[string]json.RawMessage
	json.Unmarshal(raw, &obj)
	delete(obj, "gen")
	clean, _ := json.Marshal(obj)
	var c components.Component
	dec := json.NewDecoder(bytes.NewReader(clean))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("component: %w", err)
	}
	if c.Platforms == nil {
		c.Platforms = map[string]*components.Build{}
	}
	if withGen.Gen == nil {
		return c, nil
	}
	gn := withGen.Gen
	builds := map[string]*components.Build{}
	if gn.PBS != nil {
		sums, sizes, err := pbsAssets(gn.PBS.Release)
		if err != nil {
			return c, err
		}
		for plat, p := range gn.PBS.Platforms {
			name := fmt.Sprintf("cpython-%s+%s-%s-%s.tar.gz", gn.PBS.CPython, gn.PBS.Release, p.Triple, gn.PBS.Flavor)
			sha, ok := sums[name]
			if !ok || sizes[name] <= 0 {
				return c, fmt.Errorf("%s: %s not in the release", c.ID, name)
			}
			builds[plat] = &components.Build{
				Files: []components.File{{
					Name:   name,
					URL:    "{python}/" + gn.PBS.Release + "/" + strings.ReplaceAll(name, "+", "%2B"),
					SHA256: sha, Size: sizes[name], Unpack: "tar.gz",
				}},
				Exe:   p.Exe,
				MinOS: p.MinOS,
			}
		}
	}
	for _, plat := range gn.Locks {
		b := builds[plat]
		if b == nil {
			b = &components.Build{}
			builds[plat] = b
		}
		lockRel := "locks/" + c.ID + "-" + plat + ".txt"
		lock, wheels, err := g.lock(filepath.Join(g.dir, filepath.FromSlash(lockRel)), plat)
		if err != nil {
			return c, fmt.Errorf("%s %s: %w", c.ID, plat, err)
		}
		lock.Path = lockRel
		b.Lock = lock
		var archive int64
		for _, f := range b.Files {
			archive += f.Size
		}
		b.DownloadSize = archive + wheels
		vf := g.src.InstalledFactor.Venv[plat]
		if f := gn.InstalledFactor[plat]; f > 0 {
			vf = f
		}
		if vf <= 0 {
			return c, fmt.Errorf("installed_factor.venv has no factor for %s", plat)
		}
		rf := g.src.InstalledFactor.Runtime
		if f := gn.RuntimeFactor[plat]; f > 0 {
			rf = f
		}
		b.InstalledSize = int64(math.Ceil(float64(archive)*rf + float64(wheels)*vf))
		b.InstalledEstimated = true
	}
	for plat, n := range gn.MaxPath {
		b := builds[plat]
		if b == nil {
			return c, fmt.Errorf("%s: max_path for %s, which has no build", c.ID, plat)
		}
		b.MaxPath = n
	}
	c.Platforms = builds
	return c, nil
}

// ---------------------------------------------------------------------------
// python-build-standalone
// ---------------------------------------------------------------------------

func pbsAssets(release string) (map[string]string, map[string]int64, error) {
	sumsURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/SHA256SUMS", pbsRepo, release)
	body, err := get(sumsURL, 4<<20)
	if err != nil {
		return nil, nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", pbsRepo, release)
	body, err = get(apiURL, 64<<20)
	if err != nil {
		return nil, nil, err
	}
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, nil, err
	}
	sizes := map[string]int64{}
	for _, a := range rel.Assets {
		sizes[a.Name] = a.Size
		if want, ok := sums[a.Name]; ok && a.Digest != "" && a.Digest != "sha256:"+want {
			return nil, nil, fmt.Errorf("%s: SHA256SUMS says %s, the release API %s", a.Name, want, a.Digest)
		}
	}
	return sums, sizes, nil
}

// ---------------------------------------------------------------------------
// Locks and PyPI
// ---------------------------------------------------------------------------

type pypiFile struct {
	Filename    string `json:"filename"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	PackageType string `json:"packagetype"`
	Digests     struct {
		SHA256 string `json:"sha256"`
	} `json:"digests"`
}

type pypiRelease struct {
	URLs []pypiFile `json:"urls"`
}

func (g *gen) release(name, version string) (*pypiRelease, error) {
	key := strings.ToLower(name) + "==" + version
	if r, ok := g.pypi[key]; ok {
		return r, nil
	}
	body, err := get(fmt.Sprintf("%s/%s/%s/json", pypiJSON, url.PathEscape(name), url.PathEscape(version)), 32<<20)
	if err != nil {
		return nil, err
	}
	var r pypiRelease
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	g.pypi[key] = &r
	return &r, nil
}

func target(plat string) wheelTarget {
	if plat == components.PlatformDarwinARM64 {
		return wheelTarget{pyMinor: 12, platform: plat, macMajor: 13, macMinor: 0}
	}
	return wheelTarget{pyMinor: 12, platform: plat}
}

// selectWheel picks the wheel uv installs for the target among the files
// whose hash the lock lists.
func (g *gen) selectWheel(p lockedPkg, plat string) (pypiFile, error) {
	rel, err := g.release(p.Name, p.Version)
	if err != nil {
		return pypiFile{}, err
	}
	allowed := map[string]bool{}
	for _, h := range p.Hashes {
		allowed[h] = true
	}
	var best pypiFile
	bestScore := 0
	for _, f := range rel.URLs {
		if !allowed[f.Digests.SHA256] {
			continue
		}
		if s := wheelScore(f.Filename, target(plat)); s > bestScore {
			best, bestScore = f, s
		}
	}
	if bestScore == 0 {
		return pypiFile{}, fmt.Errorf("%s==%s: no wheel for %s among the locked hashes", p.Name, p.Version, plat)
	}
	return best, nil
}

func (g *gen) lock(p, plat string) (*components.Lock, int64, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, 0, err
	}
	pkgs, err := parseLock(string(data))
	if err != nil {
		return nil, 0, err
	}
	var total int64
	for _, pk := range pkgs {
		w, err := g.selectWheel(pk, plat)
		if err != nil {
			return nil, 0, err
		}
		total += w.Size
	}
	return &components.Lock{SHA256: sha256Hex(data), Packages: len(pkgs)}, total, nil
}

// ---------------------------------------------------------------------------
// Hugging Face models
// ---------------------------------------------------------------------------

type hfSibling struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
	BlobID    string `json:"blobId"`
	LFS       *struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"lfs"`
}

func (g *gen) models(sm srcModel) ([]components.Component, error) {
	version := sm.Version
	if version == "" {
		version = "1.0.0"
	}
	mk := func(files []components.File, size int64, unavailable string) []components.Component {
		var out []components.Component
		for _, prec := range sm.Precisions {
			c := components.Component{
				ID: components.ModelID(sm.Backend, sm.Name, prec), Version: version,
				Upstream: sm.Upstream + " (" + sm.Repo + "@" + short(sm.Revision) + ")",
				Kind:     components.KindModel, Depends: sm.Depends, Backend: sm.Backend, Model: sm.Name, Precision: prec,
				License: sm.License, LicenseNote: sm.LicenseNote, Revision: sm.Revision,
				Source:      hfBase + "/" + sm.Repo + "/tree/" + sm.Revision,
				Unavailable: unavailable,
				Platforms:   map[string]*components.Build{},
			}
			for _, plat := range sm.Platforms {
				c.Platforms[plat] = &components.Build{DownloadSize: size, InstalledSize: size, Files: files}
			}
			out = append(out, c)
		}
		return out
	}
	files, size, err := g.hfFiles(sm)
	if err != nil {
		// no reliable hash: never invent one
		fmt.Fprintf(os.Stderr, "gen: WARNING %s %s: %v; marked unavailable\n", sm.Backend, sm.Name, err)
		return mk(nil, 0, "no_hash"), nil
	}
	return mk(files, size, ""), nil
}

func (g *gen) hfFiles(sm srcModel) ([]components.File, int64, error) {
	if len(sm.Revision) != 40 {
		return nil, 0, fmt.Errorf("%s: pin a full 40-character revision", sm.Repo)
	}
	body, err := get(fmt.Sprintf("%s/api/models/%s/revision/%s?blobs=true", hfBase, sm.Repo, sm.Revision), 16<<20)
	if err != nil {
		return nil, 0, err
	}
	var info struct {
		SHA      string      `json:"sha"`
		Siblings []hfSibling `json:"siblings"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, 0, err
	}
	if info.SHA != sm.Revision {
		return nil, 0, fmt.Errorf("%s: the API answered revision %s for %s", sm.Repo, info.SHA, sm.Revision)
	}
	type want struct{ pattern, rename string }
	var wants []want
	for _, f := range sm.Files {
		pat, rename, _ := strings.Cut(f, "=")
		wants = append(wants, want{pat, rename})
	}
	excluded := func(n string) bool {
		for _, x := range sm.Exclude {
			if x == n {
				return true
			}
		}
		return false
	}
	var files []components.File
	var total int64
	matched := map[string]bool{}
	for _, s := range info.Siblings {
		if excluded(s.RFilename) {
			continue
		}
		name := ""
		for _, w := range wants {
			if ok, _ := path.Match(w.pattern, s.RFilename); ok {
				matched[w.pattern] = true
				name = s.RFilename
				if w.rename != "" {
					name = w.rename
				}
				break
			}
		}
		if name == "" {
			continue
		}
		f := components.File{
			Name: name,
			URL:  "{models}/" + sm.Repo + "/resolve/" + sm.Revision + "/" + escapePath(s.RFilename),
		}
		if s.LFS != nil && s.LFS.SHA256 != "" {
			f.SHA256, f.Size = s.LFS.SHA256, s.LFS.Size
		} else {
			if s.Size > 64<<20 {
				return nil, 0, fmt.Errorf("%s: %s is %d bytes without LFS metadata", sm.Repo, s.RFilename, s.Size)
			}
			data, err := get(hfBase+"/"+sm.Repo+"/resolve/"+sm.Revision+"/"+escapePath(s.RFilename), 64<<20)
			if err != nil {
				return nil, 0, err
			}
			if got := gitBlobID(data); got != s.BlobID {
				return nil, 0, fmt.Errorf("%s: %s git blob %s, the API says %s", sm.Repo, s.RFilename, got, s.BlobID)
			}
			f.SHA256, f.Size = sha256Hex(data), int64(len(data))
		}
		files = append(files, f)
		total += f.Size
	}
	for _, w := range wants {
		if !matched[w.pattern] && !strings.ContainsAny(w.pattern, "*?[") {
			// a missing optional file (e.g. preprocessor_config.json in old repos) is fine
			fmt.Fprintf(os.Stderr, "gen: note: %s has no %s\n", sm.Repo, w.pattern)
		}
	}
	if sm.RequireOpenVINOFiles {
		have := map[string]bool{}
		for _, f := range files {
			have[f.Name] = true
		}
		for _, req := range devices.ModelFiles {
			if !have[req] {
				return nil, 0, fmt.Errorf("%s lacks %s that WhisperPipeline loads", sm.Repo, req)
			}
		}
	}
	if len(files) == 0 {
		return nil, 0, fmt.Errorf("%s: no files matched", sm.Repo)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, total, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func gitBlobID(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// ---------------------------------------------------------------------------
// Offline archives
// ---------------------------------------------------------------------------

// preserveOffline keeps the offline entries of the current manifest for
// builds whose content did not change (same version, files and lock).
func preserveOffline(m *components.Manifest, outPath string) {
	old, err := components.LoadManifest(outPath)
	if err != nil {
		return
	}
	for i := range m.Components {
		c := &m.Components[i]
		oc := old.Component(c.ID)
		if oc == nil || oc.Version != c.Version {
			continue
		}
		for plat, b := range c.Platforms {
			ob := oc.Build(plat)
			if ob != nil && ob.Offline != nil && sameContent(b, ob) {
				b.Offline = ob.Offline
			}
		}
	}
}

// dropUserFetchedOffline removes offline entries of components whose lock
// file contains user-fetched packages (kept from an earlier components.json).
func dropUserFetchedOffline(src *srcManifest, dir string, m *components.Manifest, uf userFetched) {
	for i := range m.Components {
		c := &m.Components[i]
		has := false
		for _, b := range c.Platforms {
			has = has || (b != nil && b.Offline != nil)
		}
		if !has {
			continue
		}
		if why := offlineRefusal(src, dir, c.ID, uf); why != "" {
			fmt.Fprintf(os.Stderr, "gen: WARNING dropping the offline entries of %s: %s\n", c.ID, why)
			for _, b := range c.Platforms {
				if b != nil {
					b.Offline = nil
				}
			}
		}
	}
}

func sameContent(a, b *components.Build) bool {
	if len(a.Files) != len(b.Files) || (a.Lock == nil) != (b.Lock == nil) {
		return false
	}
	for i := range a.Files {
		if a.Files[i].Name != b.Files[i].Name || a.Files[i].SHA256 != b.Files[i].SHA256 {
			return false
		}
	}
	return a.Lock == nil || a.Lock.SHA256 == b.Lock.SHA256
}

// buildOffline writes {dir}/{DirName(id)}-{version}-{platform}.zip for every
// platform of the component and records it in the manifest. The archive is
// deterministic (sorted entries, stored, fixed times), so rebuilding it
// gives the same SHA-256.
func (g *gen) buildOffline(m *components.Manifest, id, outDir string) error {
	c := m.Component(id)
	if c == nil || c.Bundled || c.Unavailable != "" {
		return fmt.Errorf("offline: %q is not an installable component", id)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	defaults := map[string]string{}
	for _, sg := range m.Sources {
		for k, v := range sg.Default.Values {
			defaults[k] = v
		}
	}
	plats := make([]string, 0, len(c.Platforms))
	for p := range c.Platforms {
		plats = append(plats, p)
	}
	sort.Strings(plats)
	for _, plat := range plats {
		b := c.Platforms[plat]
		entries := map[string][]byte{}
		info, _ := json.Marshal(components.OfflineInfo{Schema: 1, ID: c.ID, Version: c.Version, Platform: plat})
		entries["component.json"] = info
		for _, f := range b.Files {
			u := expand(f.URL, defaults)
			fmt.Fprintf(os.Stderr, "gen: offline %s: downloading %s\n", id, f.Name)
			data, err := get(u, f.Size+1)
			if err != nil {
				return err
			}
			if sha256Hex(data) != f.SHA256 {
				return fmt.Errorf("offline %s: %s does not match its SHA-256", id, f.Name)
			}
			entries["files/"+f.Name] = data
		}
		if b.Lock != nil {
			lock, err := os.ReadFile(filepath.Join(g.dir, filepath.FromSlash(b.Lock.Path)))
			if err != nil {
				return err
			}
			entries["lock.txt"] = lock
			pkgs, err := parseLock(string(lock))
			if err != nil {
				return err
			}
			if g.userFetched == nil {
				return fmt.Errorf("offline %s: the license allowlist was not read", id)
			}
			if bad := g.userFetched.in(pkgs); len(bad) > 0 {
				return fmt.Errorf("offline %s refused: %s contains user-fetched packages %s", id, b.Lock.Path, strings.Join(bad, ", "))
			}
			for _, pk := range pkgs {
				w, err := g.selectWheel(pk, plat)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "gen: offline %s: downloading %s\n", id, w.Filename)
				data, err := get(w.URL, w.Size+1)
				if err != nil {
					return err
				}
				if sha256Hex(data) != w.Digests.SHA256 {
					return fmt.Errorf("offline %s: %s does not match its SHA-256", id, w.Filename)
				}
				entries["wheels/"+w.Filename] = data
			}
		}
		name := fmt.Sprintf("%s-%s-%s.zip", components.DirName(c.ID), c.Version, plat)
		zipData, err := deterministicZip(entries)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, name), zipData, 0o644); err != nil {
			return err
		}
		b.Offline = &components.Offline{Name: name, SHA256: sha256Hex(zipData), Size: int64(len(zipData))}
		fmt.Fprintf(os.Stderr, "gen: offline %s %s: %s (%d bytes)\n", id, plat, name, len(zipData))
	}
	return nil
}

func expand(tmpl string, values map[string]string) string {
	for k, v := range values {
		tmpl = strings.ReplaceAll(tmpl, "{"+k+"}", strings.TrimRight(v, "/"))
	}
	return tmpl
}

var zipTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func deterministicZip(entries map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		h := &zip.FileHeader{Name: n, Method: zip.Store, Modified: zipTime}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(entries[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------

func get(u string, limit int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		if strings.HasPrefix(u, "https://api.github.com/") {
			req.Header.Set("Accept", "application/vnd.github+json")
			if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
			if resp.StatusCode == http.StatusNotFound {
				return nil, lastErr
			}
			continue
		}
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("GET %s: larger than %d bytes", u, limit)
		}
		return data, nil
	}
	return nil, errors.Join(fmt.Errorf("GET %s failed", u), lastErr)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (g *gen) summary(m *components.Manifest) {
	fmt.Printf("%-48s %-13s %12s %12s\n", "component", "platform", "download MB", "installed MB")
	for _, c := range m.Components {
		if c.Bundled {
			continue
		}
		plats := make([]string, 0, len(c.Platforms))
		for p := range c.Platforms {
			plats = append(plats, p)
		}
		sort.Strings(plats)
		for _, p := range plats {
			b := c.Platforms[p]
			note := ""
			if c.Unavailable != "" {
				note = " (unavailable: " + c.Unavailable + ")"
			}
			fmt.Printf("%-48s %-13s %12.1f %12.1f%s\n", c.ID, p, float64(b.DownloadSize)/1e6, float64(b.InstalledSize)/1e6, note)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Offline component archives (V3.3 "从文件导入组件"): a zip with
//
//	component.json  {"schema": 1, "id", "version", "platform"}
//	files/{name}    the build's files (models, binaries, the runtime archive)
//	wheels/*.whl    venv and python components: the lock's wheels for the platform
//	lock.txt        optional: the lock file, used when the manifest's is not on disk
//
// The SHA-256 of the whole archive must equal Build.Offline.SHA256 of a
// component in the current manifest; that, not component.json, identifies it.
//
// The user's file is never read after the first step: the task copies it
// to components/.imports/{task}.zip, hashing the bytes it writes, and every
// later step (identification, extraction, the lock file, the wheels) reads
// only that private copy. Replacing the original file afterwards, also
// while the task is paused, changes nothing; a copy interrupted by a pause
// is made again from scratch on resume and hashed again. Each extracted
// file is checked once more against File.SHA256 before it is moved into the
// component, the lock file against Lock.SHA256, and the wheels by uv's
// --require-hashes.

// OfflineInfo is component.json of an offline archive.
type OfflineInfo struct {
	Schema   int    `json:"schema"`
	ID       string `json:"id"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

type offlineSource struct {
	orig string // the user's file (read once, by the copy)
	// path is the verified private copy ("" until a copy completed); sha256
	// and size are what the copy hashed while it was written.
	path   string
	sha256 string
	size   int64
}

// importsDir holds the private copies of imported archives (removed at
// start: no task survives a restart).
func (m *Manager) importsDir() string { return filepath.Join(m.root, ".imports") }

// prepareImport makes the private copy of the archive (unless a paused run
// completed it) and identifies the component; an item identified before (a
// retried task) must find the same archive again.
func (m *Manager) prepareImport(ctx context.Context, t *task, it *item) error {
	o := it.offline
	if o.path == "" || !fileSizeIs(o.path, o.size) {
		m.mu.Lock()
		o.path = ""
		m.mu.Unlock()
		m.setPhase(t, it, PhaseVerifying)
		if err := os.MkdirAll(m.importsDir(), 0o755); err != nil {
			return asCoded(err, nil)
		}
		dst := filepath.Join(m.importsDir(), string(t.id)+".zip")
		first := true
		sum, n, err := copyHashCtx(ctx, o.orig, dst, func(k int64) {
			m.mu.Lock()
			it.curBytes = k
			m.emitTask(t, false)
			m.mu.Unlock()
			if first {
				first = false
				m.testEvent("import.copying", t.id)
			}
		})
		if err != nil {
			os.Remove(dst)
			removeIfEmpty(m.importsDir())
			if c := context.Cause(ctx); c != nil {
				return c
			}
			if isDiskFull(err) {
				return cerr(CodeDiskFull, map[string]any{"required_mb": -1, "free_mb": -1}, err)
			}
			if errors.Is(err, errCopyWrite) {
				return asCoded(err, nil)
			}
			return cerr(CodeImportInvalid, nil, err)
		}
		m.mu.Lock()
		o.path, o.sha256, o.size = dst, sum, n
		m.mu.Unlock()
		m.testEvent("import.copied", t.id)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
	}
	if it.id == "" {
		return m.identifyImport(ctx, t, it)
	}
	c := t.man.Component(it.id)
	var b *Build
	if c != nil {
		b = c.Build(m.platform)
	}
	if b == nil || b.Offline == nil || b.Offline.SHA256 != o.sha256 || b.Offline.Size != o.size {
		return cerr(CodeImportChecksum, map[string]any{"id": it.id},
			fmt.Errorf("the archive changed: SHA-256 %s", o.sha256))
	}
	return nil
}

// removeImportCopy deletes the task's private copy (the task finished or
// was cancelled; a retry copies the file again).
func (m *Manager) removeImportCopy(t *task) {
	m.mu.Lock()
	var paths []string
	for _, it := range t.items {
		if it.offline != nil && it.offline.path != "" {
			paths = append(paths, it.offline.path)
			it.offline.path = ""
		}
	}
	m.mu.Unlock()
	os.Remove(filepath.Join(m.importsDir(), string(t.id)+".zip")) // an interrupted copy
	for _, p := range paths {
		os.Remove(p)
	}
	removeIfEmpty(m.importsDir())
}

func fileSizeIs(p string, n int64) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == n
}

// errCopyWrite marks a failure to write the copy (not the archive's fault).
var errCopyWrite = errors.New("cannot write the copy")

// copyHashCtx copies src to dst and returns the SHA-256 and size of the
// bytes written, stopping when ctx ends.
func copyHashCtx(ctx context.Context, src, dst string, onBytes func(int64)) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", errCopyWrite, err)
	}
	h := sha256.New()
	w := &errMarkWriter{w: io.MultiWriter(out, h)}
	n, err := io.CopyBuffer(w, &ctxReader{ctx: ctx, r: in, onBytes: onBytes}, make([]byte, 1<<20))
	if cerr := out.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("%w: %w", errCopyWrite, cerr)
	}
	if err != nil {
		return "", n, err
	}
	onBytes(n)
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// errMarkWriter tags write errors with errCopyWrite.
type errMarkWriter struct{ w io.Writer }

func (e *errMarkWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		err = fmt.Errorf("%w: %w", errCopyWrite, err)
	}
	return n, err
}

// importPathCheck applies checkPathLen to the component the archive's
// component.json names (a later step identifies the component by the
// archive's SHA-256 and checks again); an unreadable archive passes here
// and fails in the task with components.import_invalid.
func (m *Manager) importPathCheck(path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil
	}
	info, ierr := readOfflineInfo(&zr.Reader)
	zr.Close()
	if ierr != nil {
		return nil
	}
	c := m.manifest().Component(info.ID)
	if c == nil {
		return nil
	}
	return m.checkPathLen(c, c.Build(m.platform))
}

// importPlan estimates the free space an import needs: the private copy,
// the archive's extracted entries (files and wheels), and the installed
// size of the component its component.json names when that unpacks or
// builds an environment (venv, python, an archive file). Nothing is
// needed beyond the copy when that component is installed already.
func (m *Manager) importPlan(path string, size int64) Plan {
	m.mu.Lock()
	man, st := m.man, m.st.clone()
	m.mu.Unlock()
	need := size
	if zr, err := zip.OpenReader(path); err == nil {
		var unpacked int64
		for _, f := range zr.File {
			if !f.FileInfo().IsDir() {
				unpacked += int64(f.UncompressedSize64)
			}
		}
		info, ierr := readOfflineInfo(&zr.Reader)
		zr.Close()
		var c *Component
		var b *Build
		if ierr == nil {
			if c = man.Component(info.ID); c != nil {
				b = c.Build(m.platform)
			}
		}
		cur, installed := Installed{}, false
		if c != nil {
			cur, installed = st.Active[c.ID]
		}
		if !installed || CompareVersions(cur.Version, c.Version) < 0 {
			need += unpacked
			if b != nil {
				extra := c.Kind == KindVenv || c.Kind == KindPythonRuntime
				for _, f := range b.Files {
					extra = extra || f.Unpack != ""
				}
				if extra {
					need += b.InstalledSize
				}
			}
		}
	}
	p := Plan{RequiredBytes: need + planMargin, FreeBytes: -1, Enough: true}
	os.MkdirAll(m.root, 0o755)
	if free, err := m.diskFree(m.root); err == nil {
		p.FreeBytes = free
		p.Enough = free >= p.RequiredBytes
	} else {
		m.log.Warn("components: cannot read the free disk space", "error", err)
	}
	return p
}

func readOfflineInfo(zr *zip.Reader) (OfflineInfo, error) {
	var info OfflineInfo
	for _, f := range zr.File {
		if f.Name != "component.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return info, err
		}
		defer rc.Close()
		if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&info); err != nil {
			return info, err
		}
		if info.Schema != 1 || info.ID == "" || info.Version == "" {
			return info, errors.New("component.json is incomplete")
		}
		return info, nil
	}
	return info, errors.New("no component.json")
}

// identifyImport finds the component of the copied archive by the SHA-256
// the copy computed.
func (m *Manager) identifyImport(ctx context.Context, t *task, it *item) error {
	sum, n := it.offline.sha256, it.offline.size
	zr, zerr := zip.OpenReader(it.offline.path)
	var info OfflineInfo
	var ierr error
	if zerr == nil {
		info, ierr = readOfflineInfo(&zr.Reader)
		zr.Close()
	}
	man := t.man
	var c *Component
	for i := range man.Components {
		b := man.Components[i].Build(m.platform)
		if b != nil && b.Offline != nil && b.Offline.SHA256 == sum && b.Offline.Size == n {
			c = &man.Components[i]
			break
		}
	}
	if c == nil {
		switch {
		case zerr != nil || ierr != nil:
			return cerr(CodeImportInvalid, nil, fmt.Errorf("not a component archive: %v", errors.Join(zerr, ierr)))
		case man.Component(info.ID) != nil && info.Platform == m.platform && info.Version == man.Component(info.ID).Version &&
			man.Component(info.ID).Build(m.platform) != nil && man.Component(info.ID).Build(m.platform).Offline != nil:
			return cerr(CodeImportChecksum, map[string]any{"id": info.ID},
				fmt.Errorf("the archive's SHA-256 %s differs from the manifest", sum))
		default:
			return cerr(CodeImportUnknown, map[string]any{"id": info.ID, "version": info.Version},
				fmt.Errorf("%s %s (%s) is not a component of this version", info.ID, info.Version, info.Platform))
		}
	}
	if zerr != nil || ierr != nil || info.ID != c.ID || info.Version != c.Version || info.Platform != m.platform {
		return cerr(CodeImportInvalid, map[string]any{"id": c.ID},
			fmt.Errorf("component.json does not describe %s %s (%v)", c.ID, c.Version, errors.Join(zerr, ierr)))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var missing []string
	for _, d := range c.Depends {
		if dc := man.Component(d); dc != nil && dc.Bundled {
			continue
		}
		if _, ok := m.st.Active[d]; !ok {
			missing = append(missing, d)
		}
	}
	if len(missing) > 0 {
		return cerr(CodeImportMissingDeps, map[string]any{"id": c.ID, "dependents": strings.Join(missing, ", ")},
			fmt.Errorf("install %s first", strings.Join(missing, ", ")))
	}
	cur, installed := m.st.Active[c.ID]
	if !installed || CompareVersions(cur.Version, c.Version) < 0 {
		// before the item is identified: a retry identifies (and checks) again
		if err := m.checkPathLen(c, c.Build(m.platform)); err != nil {
			return err
		}
	}
	it.id, it.version, it.source = c.ID, c.Version, SourceOffline
	t.requested = []string{c.ID}
	it.curBytes, it.doneBytes = 0, it.total
	switch {
	case installed && CompareVersions(cur.Version, c.Version) >= 0:
		it.phase = PhaseSkipped
	case installed:
		it.update = true
	}
	m.emitTask(t, true)
	return nil
}

func (o *offlineSource) open() (*zip.ReadCloser, error) {
	zr, err := zip.OpenReader(o.path)
	if err != nil {
		return nil, cerr(CodeImportInvalid, nil, err)
	}
	return zr, nil
}

// extractFile extracts files/{name} of the private copy into
// downloads/{sha256}: the bytes are hashed as they are written and must
// match File.SHA256 (and the size) before the file gets its final name.
func (o *offlineSource) extractFile(ctx context.Context, m *Manager, id string, f File, onBytes func(int64)) (string, error) {
	zr, err := o.open()
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if zf.Name != "files/"+f.Name {
			continue
		}
		if int64(zf.UncompressedSize64) != f.Size {
			return "", cerr(CodeImportInvalid, nil, fmt.Errorf("%s has %d bytes, expected %d", zf.Name, zf.UncompressedSize64, f.Size))
		}
		if err := os.MkdirAll(m.dl.dir, 0o755); err != nil {
			return "", asCoded(err, nil)
		}
		tmp := filepath.Join(m.dl.dir, f.SHA256+".import")
		rc, err := zf.Open()
		if err != nil {
			return "", cerr(CodeImportInvalid, nil, err)
		}
		out, err := os.Create(tmp)
		if err != nil {
			rc.Close()
			return "", asCoded(err, nil)
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, h), &ctxReader{ctx: ctx, r: rc, onBytes: onBytes})
		rc.Close()
		if cerr2 := out.Close(); err == nil {
			err = cerr2
		}
		if err != nil {
			os.Remove(tmp)
			if c := context.Cause(ctx); c != nil {
				return "", c
			}
			if errors.Is(err, zip.ErrChecksum) {
				return "", cerr(CodeImportInvalid, nil, err)
			}
			return "", asCoded(err, nil)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 || n != f.Size {
			os.Remove(tmp)
			return "", cerr(CodeImportChecksum, map[string]any{"id": id},
				fmt.Errorf("%s in the archive: SHA-256 %s (%d bytes), the manifest says %s (%d bytes)", f.Name, got, n, f.SHA256, f.Size))
		}
		final := m.dl.finalPath(f.SHA256)
		if err := renameRetry(tmp, final); err != nil {
			os.Remove(tmp)
			return "", asCoded(err, nil)
		}
		return final, nil
	}
	return "", cerr(CodeImportInvalid, nil, fmt.Errorf("the archive has no files/%s", f.Name))
}

// extractLock extracts lock.txt when its SHA-256 matches.
func (o *offlineSource) extractLock(dst, sha string) (string, error) {
	zr, err := o.open()
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if zf.Name != "lock.txt" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		rc.Close()
		if err != nil {
			return "", err
		}
		if sha256Hex(data) != sha {
			return "", errors.New("lock.txt does not match the manifest")
		}
		if err := writeFileAtomic(dst, data); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", errors.New("no lock.txt")
}

// extractWheels extracts wheels/*.whl into dir.
func (o *offlineSource) extractWheels(ctx context.Context, dir string) (string, error) {
	zr, err := o.open()
	if err != nil {
		return "", err
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", asCoded(err, nil)
	}
	n := 0
	for _, zf := range zr.File {
		if !strings.HasPrefix(zf.Name, "wheels/") || zf.FileInfo().IsDir() {
			continue
		}
		name := strings.TrimPrefix(zf.Name, "wheels/")
		if strings.Contains(name, "/") || !strings.HasSuffix(name, ".whl") {
			return "", cerr(CodeImportInvalid, nil, fmt.Errorf("unexpected entry %s", zf.Name))
		}
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		if err := extractZipEntry(zf, dir, name); err != nil {
			if errors.Is(err, zip.ErrChecksum) {
				return "", cerr(CodeImportInvalid, nil, err)
			}
			return "", asCoded(err, nil)
		}
		n++
	}
	if n == 0 {
		return "", cerr(CodeImportInvalid, nil, errors.New("the archive has no wheels"))
	}
	return filepath.Clean(dir), nil
}

// ctxReader stops a copy when ctx ends and reports progress.
type ctxReader struct {
	ctx     context.Context
	r       io.Reader
	n       int64
	onBytes func(int64)
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	k, err := c.r.Read(p)
	c.n += int64(k)
	if c.onBytes != nil && k > 0 {
		c.onBytes(c.n)
	}
	return k, err
}

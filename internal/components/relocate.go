// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Relocated fixes the absolute paths a moved data directory carries (V1.3
// "更改数据目录"): state.json and the receipts only hold paths relative to
// components/, but every venv's pyvenv.cfg names its base interpreter by
// absolute path (home = .../components/python/{version}/runtime/...), and
// on macOS venv/bin/python* are symlinks to it. Call it after the data
// directory was copied from oldDataDir to newDataDir and before a Manager
// opens newDataDir. It is idempotent; paths outside oldDataDir are left as
// they are.
func Relocated(oldDataDir, newDataDir string) error {
	oldRoot, err := filepath.Abs(oldDataDir)
	if err != nil {
		return err
	}
	newRoot, err := filepath.Abs(newDataDir)
	if err != nil {
		return err
	}
	if samePath(oldRoot, newRoot) {
		return nil
	}
	var errs []error
	for _, venv := range findVenvs(filepath.Join(newRoot, "components")) {
		if err := relocateVenv(venv, oldRoot, newRoot); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", venv, err))
		}
	}
	return errors.Join(errs...)
}

// findVenvs lists components/{dir}/{version}/venv directories.
func findVenvs(root string) []string {
	var out []string
	comps, _ := os.ReadDir(root)
	for _, c := range comps {
		if !c.IsDir() || strings.HasPrefix(c.Name(), ".") || isReservedDir(c.Name()) {
			continue
		}
		vers, _ := os.ReadDir(filepath.Join(root, c.Name()))
		for _, v := range vers {
			venv := filepath.Join(root, c.Name(), v.Name(), "venv")
			if fileExists(filepath.Join(venv, "pyvenv.cfg")) {
				out = append(out, venv)
			}
		}
	}
	return out
}

func relocateVenv(venv, oldRoot, newRoot string) error {
	cfg := filepath.Join(venv, "pyvenv.cfg")
	data, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	var b strings.Builder
	changed := false
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if k, v, ok := strings.Cut(line, "="); ok {
			val := strings.TrimSpace(v)
			if nv, ok := rebase(val, oldRoot, newRoot); ok {
				line = strings.TrimRight(k, " ") + " = " + nv
				changed = true
			} else if strings.TrimSpace(k) == "command" {
				// "command = <uv or python> ... <paths>": rebase every path in it
				if nv, ok := rebaseAll(val, oldRoot, newRoot); ok {
					line = strings.TrimRight(k, " ") + " = " + nv
					changed = true
				}
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if changed {
		if err := writeFileAtomic(cfg, []byte(b.String())); err != nil {
			return err
		}
	}
	if runtime.GOOS != "windows" {
		return relinkBin(filepath.Join(venv, "bin"), oldRoot, newRoot)
	}
	return nil
}

// relinkBin points symlinks under bin/ that target oldRoot at newRoot.
func relinkBin(bin, oldRoot, newRoot string) error {
	es, err := os.ReadDir(bin)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range es {
		p := filepath.Join(bin, e.Name())
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		nt, ok := rebase(target, oldRoot, newRoot)
		if !ok {
			continue
		}
		tmp := p + ".relink"
		os.Remove(tmp)
		if err := os.Symlink(nt, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}

// rebase replaces the oldRoot prefix of an absolute path p with newRoot
// (whole path elements only; case-insensitive on Windows, either slash).
func rebase(p, oldRoot, newRoot string) (string, bool) {
	if p == "" {
		return "", false
	}
	np := filepath.FromSlash(p)
	if runtime.GOOS == "windows" {
		np = strings.ReplaceAll(p, "/", `\`)
	}
	or := strings.TrimRight(filepath.FromSlash(oldRoot), `\/`)
	if len(np) < len(or) || !samePath(np[:len(or)], or) {
		return "", false
	}
	rest := np[len(or):]
	if rest != "" && rest[0] != filepath.Separator {
		return "", false // "/data2" is not under "/data"
	}
	return strings.TrimRight(filepath.FromSlash(newRoot), `\/`) + rest, true
}

// rebaseAll rebases every occurrence of oldRoot in a command line.
func rebaseAll(s, oldRoot, newRoot string) (string, bool) {
	or := strings.TrimRight(filepath.FromSlash(oldRoot), `\/`)
	nr := strings.TrimRight(filepath.FromSlash(newRoot), `\/`)
	src := s
	if runtime.GOOS == "windows" {
		src = strings.ReplaceAll(s, "/", `\`)
	}
	var b strings.Builder
	changed := false
	for {
		i := indexPath(src, or)
		if i < 0 {
			b.WriteString(src)
			break
		}
		end := i + len(or)
		if end < len(src) && src[end] != filepath.Separator && src[end] != ' ' && src[end] != '"' {
			b.WriteString(src[:end])
			src = src[end:]
			continue
		}
		b.WriteString(src[:i])
		b.WriteString(nr)
		src = src[end:]
		changed = true
	}
	return b.String(), changed
}

func indexPath(s, sub string) int {
	if runtime.GOOS == "windows" {
		return strings.Index(strings.ToLower(s), strings.ToLower(sub))
	}
	return strings.Index(s, sub)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

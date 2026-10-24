// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// refresh-python: update packaging/licenses/python.json from the metadata
// of installed virtual environments (offline: reads *.dist-info only).
// Entries marked "manual" keep their license, source and note; versions and
// components are always recomputed for the packages found.

type venvFlag []string

func (v *venvFlag) String() string     { return strings.Join(*v, ",") }
func (v *venvFlag) Set(s string) error { *v = append(*v, s); return nil }

func refreshPythonMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("refresh-python", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	var venvs venvFlag
	fs.Var(&venvs, "venv", "DIR=component (repeatable), e.g. .venv=asr-cuda")
	prune := fs.Bool("prune", false, "drop entries not found in any of the given venvs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(venvs) == 0 {
		fmt.Fprintln(stderr, "refresh-python: give at least one -venv DIR=component")
		return 2
	}
	pf, err := loadPython(*root)
	if err != nil {
		fmt.Fprintln(stderr, "refresh-python:", err)
		return 2
	}
	type found struct {
		versions, comps map[string]bool
		meta            pyMeta
	}
	seen := map[string]*found{}
	for _, v := range venvs {
		dir, comp, ok := strings.Cut(v, "=")
		if !ok || comp == "" {
			fmt.Fprintln(stderr, "refresh-python: -venv wants DIR=component, got", v)
			return 2
		}
		metas, err := readVenv(filepath.Join(*root, dir))
		if err != nil {
			fmt.Fprintln(stderr, "refresh-python:", err)
			return 2
		}
		for _, m := range metas {
			n := pyName(m.Name)
			if _, bad := forbiddenPython[n]; bad {
				fmt.Fprintf(stdout, "skip %s (%s): forbidden package\n", n, dir)
				continue
			}
			f := seen[n]
			if f == nil {
				f = &found{versions: map[string]bool{}, comps: map[string]bool{}, meta: m}
				seen[n] = f
			}
			f.versions[m.Version] = true
			f.comps[comp] = true
		}
	}
	for n, f := range seen {
		e := pf.Packages[n]
		e.Versions, e.Components = sortedKeys(f.versions), sortedKeys(f.comps)
		if !e.Manual {
			e.License, e.Evidence = f.meta.license()
			if e.Source == "" {
				e.Source = "https://pypi.org/project/" + n + "/"
			}
		}
		pf.Packages[n] = e
		if ok, rs := Evaluate(e.License); !ok {
			fmt.Fprintf(stdout, "check %s [%s]: %s\n", n, e.License, strings.Join(rs, "; "))
		}
	}
	if *prune {
		for n := range pf.Packages {
			if seen[n] == nil {
				delete(pf.Packages, n)
			}
		}
	}
	b, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "refresh-python:", err)
		return 2
	}
	path := filepath.Join(*root, pythonFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(stderr, "refresh-python:", err)
		return 2
	}
	if err := writeFileAtomic(path, append(b, '\n')); err != nil {
		fmt.Fprintln(stderr, "refresh-python:", err)
		return 2
	}
	fmt.Fprintf(stdout, "wrote %s (%d packages)\n", pythonFile, len(pf.Packages))
	return 0
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type pyMeta struct {
	Name, Version, Expression, License string
	Classifiers                        []string
	FileLicense                        string // recognised text of the dist-info license file
}

// license picks the best license expression and says where it came from.
func (m pyMeta) license() (string, string) {
	if m.Expression != "" {
		return m.Expression, "License-Expression"
	}
	if id, ok := freeTextLicense(m.License); ok {
		if id == "" { // ambiguous ("BSD"): the file decides
			if m.FileLicense != "" {
				return m.FileLicense, "license file (License: " + m.License + ")"
			}
		} else {
			return id, "License field"
		}
	}
	if m.FileLicense != "" {
		return m.FileLicense, "license file"
	}
	for _, c := range m.Classifiers {
		if id := classifierLicense(c); id != "" {
			return id, "classifier"
		}
	}
	return "NOASSERTION", "no license metadata"
}

// freeTextLicense maps common License: values. ok with id "" means
// "license family known, exact variant from the file".
func freeTextLicense(s string) (string, bool) {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	l := strings.ToLower(s)
	switch l {
	case "":
		return "", false
	case "mit", "mit license", "the mit license", "expat":
		return "MIT", true
	case "apache 2.0", "apache-2.0", "apache license 2.0", "apache 2.0 license", "apache license, version 2.0", "apache software license", "osi approved :: apache software license", "apache license", "apache":
		return "Apache-2.0", true
	case "bsd-3-clause", "3-clause bsd license", "bsd 3-clause license", "bsd 3-clause", "new bsd license", "modified bsd license":
		return "BSD-3-Clause", true
	case "bsd-2-clause", "2-clause bsd license", "simplified bsd license":
		return "BSD-2-Clause", true
	case "bsd", "bsd license":
		return "", true
	case "isc", "isc license", "isc license (iscl)":
		return "ISC", true
	case "mpl-2.0", "mpl 2.0", "mozilla public license 2.0 (mpl 2.0)":
		return "MPL-2.0", true
	case "psf", "psf-2.0", "psf license":
		return "PSF-2.0", true
	}
	// Already an SPDX expression (e.g. "MPL-2.0 AND MIT", "LicenseRef-...").
	if !strings.ContainsAny(s, ",;:") {
		if _, err := (&exprParser{toks: tokenize(s)}).parseOr(); err == nil {
			allKnown := true
			for _, t := range tokenize(s) {
				if t == "(" || t == ")" || isOperator(t) {
					continue
				}
				c := canonical(t)
				if _, ok := compatible[c]; !ok {
					if _, bad := knownBad[c]; !bad && !strings.HasPrefix(c, "LicenseRef-") {
						allKnown = false
					}
				}
			}
			if allKnown {
				return s, true
			}
		}
	}
	return "", false
}

func classifierLicense(c string) string {
	switch strings.TrimSpace(c[strings.LastIndex(c, "::")+2:]) {
	case "MIT License":
		return "MIT"
	case "Apache Software License":
		return "Apache-2.0"
	case "ISC License (ISCL)":
		return "ISC"
	case "Mozilla Public License 2.0 (MPL 2.0)":
		return "MPL-2.0"
	case "Python Software Foundation License":
		return "PSF-2.0"
	}
	return "" // "BSD License", "Other/Proprietary License", ...: needs the text
}

// readVenv reads every *.dist-info of a virtual environment.
func readVenv(venv string) ([]pyMeta, error) {
	var sites []string
	for _, pat := range []string{filepath.Join(venv, "Lib", "site-packages"), filepath.Join(venv, "lib", "python3*", "site-packages")} {
		m, _ := filepath.Glob(pat)
		sites = append(sites, m...)
	}
	if len(sites) == 0 {
		return nil, fmt.Errorf("%s: no site-packages", venv)
	}
	var out []pyMeta
	for _, site := range sites {
		dis, _ := filepath.Glob(filepath.Join(site, "*.dist-info"))
		sort.Strings(dis)
		for _, d := range dis {
			b, err := os.ReadFile(filepath.Join(d, "METADATA"))
			if err != nil {
				continue
			}
			m := parseMetadata(b)
			m.FileLicense = distInfoLicense(d)
			out = append(out, m)
		}
	}
	return out, nil
}

func parseMetadata(b []byte) pyMeta {
	var m pyMeta
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	key, val := "", ""
	flush := func() {
		switch key {
		case "Name":
			m.Name = val
		case "Version":
			m.Version = val
		case "License-Expression":
			m.Expression = val
		case "License":
			m.License = val
		case "Classifier":
			if strings.HasPrefix(val, "License") {
				m.Classifiers = append(m.Classifiers, val)
			}
		}
		key, val = "", ""
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			break // body
		}
		if (line[0] == ' ' || line[0] == '\t') && key != "" {
			val += "\n" + strings.TrimSpace(line)
			continue
		}
		flush()
		k, v, ok := strings.Cut(line, ":")
		if ok {
			key, val = k, strings.TrimSpace(v)
		}
	}
	flush()
	return m
}

// distInfoLicense recognises the top-level license file of a dist-info
// (licenses/LICENSE*, LICENSE*, COPYING*, LICENCE*).
func distInfoLicense(d string) string {
	for _, dir := range []string{filepath.Join(d, "licenses"), d} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !isPrimaryLicenseName(e.Name()) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if id := Identify(string(b)); id != "" {
				return id
			}
		}
	}
	return ""
}

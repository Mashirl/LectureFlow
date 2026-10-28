// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The browser extension (SPEC-ext). Its packages (Chrome, Edge, Firefox)
// carry third-party files copied unchanged from npm into
// extension/src/vendor, together with extension/src/vendor/NOTICE and the
// license texts (extension/scripts/build.mjs copies the directory into
// dist/<target>/vendor). packaging/licenses/extension.json registers them;
// the audit checks that
//
//   - every file under extension/src/vendor is the NOTICE, the license file
//     of a registered package or one of its files (anything else fails: the
//     SPDX header check skips this directory);
//   - a single file is pinned by its SHA-256; a pattern of files (icons)
//     names a text every matched file must contain ({stem} is the file name
//     without extension), so a file from another source does not slip in;
//   - each license file is recognised as the declared license;
//   - the NOTICE names exactly the registered packages, with the same
//     version, license, license file, npm tarball, integrity and file sum.
//
// npm packages of extension/package-lock.json are judged as shipped unless
// they are devDependencies (Playwright drives the end-to-end tests and is
// not in any package). The platform profiles and fixtures are the
// project's own content under CC0-1.0; extension/profiles/LICENSE must say
// so.

// KindExtension: third-party content of the browser extension's packages.
const KindExtension = "extension"

const (
	extensionFile      = licDir + "/extension.json"
	extVendorDir       = "extension/src/vendor"
	extNoticeName      = "NOTICE"
	extLockFile        = "extension/package-lock.json"
	extProfilesDir     = "extension/profiles"
	extProfilesLicense = extProfilesDir + "/LICENSE"
)

// ExtFile is one file, or one pattern of files, of a vendored package
// (relative to extension/src/vendor, slash-separated).
type ExtFile struct {
	Path     string `json:"path,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Glob     string `json:"glob,omitempty"`
	Contains string `json:"contains,omitempty"`
}

// ExtPackage is one package of packaging/licenses/extension.json.
type ExtPackage struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	License     string    `json:"license"`
	Source      string    `json:"source"`     // the npm tarball (NOTICE "Source:")
	Repository  string    `json:"repository"` // upstream repository
	Integrity   string    `json:"integrity"`  // npm integrity of the tarball (NOTICE "Integrity:")
	Files       []ExtFile `json:"files"`
	LicenseFile string    `json:"license_file"` // relative to extension/src/vendor
	ShippedAs   string    `json:"shipped_as"`   // what ends up in the extension package
	Note        string    `json:"note,omitempty"`
}

type extTable struct {
	Packages []ExtPackage `json:"packages"`
}

// noticeBlock is one entry of extension/src/vendor/NOTICE: a top-level line
// (the file or pattern) followed by indented "Key: value" lines.
type noticeBlock struct {
	header, name, version, source, integrity, sha256, license, licenseFile string
}

var (
	reNoticePackage   = regexp.MustCompile(`^Package:\s+(\S+)\s+(\S+)`)
	reNoticeSource    = regexp.MustCompile(`^Source:\s+(\S+)`)
	reNoticeIntegrity = regexp.MustCompile(`^Integrity:\s+(\S+)`)
	reNoticeSHA       = regexp.MustCompile(`^SHA-256 of the file:\s+([0-9a-fA-F]{64})\b`)
	reNoticeLicense   = regexp.MustCompile(`^License:\s+([^\s(]+)(?:\s+\(([^)]+)\))?`)
	reIntegrity       = regexp.MustCompile(`^sha(256|384|512)-[A-Za-z0-9+/]+=*$`)
)

func parseExtNotice(text string) []noticeBlock {
	var out []noticeBlock
	var cur *noticeBlock
	top := ""
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			top = strings.TrimSpace(line)
			cur = nil
			continue
		}
		l := strings.TrimSpace(line)
		if m := reNoticePackage.FindStringSubmatch(l); m != nil {
			out = append(out, noticeBlock{header: top, name: m[1], version: m[2]})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if m := reNoticeSource.FindStringSubmatch(l); m != nil {
			cur.source = m[1]
		} else if m := reNoticeIntegrity.FindStringSubmatch(l); m != nil {
			cur.integrity = m[1]
		} else if m := reNoticeSHA.FindStringSubmatch(l); m != nil {
			cur.sha256 = strings.ToLower(m[1])
		} else if m := reNoticeLicense.FindStringSubmatch(l); m != nil {
			cur.license = strings.TrimRight(m[1], ".,;")
			cur.licenseFile = m[2]
		}
	}
	return out
}

func auditExtension(opt Options, r *Report, al *allowlist) error {
	if err := auditExtensionVendor(opt, r, al); err != nil {
		return err
	}
	if err := auditNPMLock(opt, r, al, extensionNPM); err != nil {
		return err
	}
	auditProfilesLicense(opt, r)
	return nil
}

func auditExtensionVendor(opt Options, r *Report, al *allowlist) error {
	vendor := filepath.Join(opt.Root, filepath.FromSlash(extVendorDir))
	_, verr := os.Stat(vendor)
	haveVendor := verr == nil
	b, err := os.ReadFile(filepath.Join(opt.Root, extensionFile))
	haveTable := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !haveVendor && !haveTable {
		r.Skipped = append(r.Skipped, extVendorDir+" (not present)")
		return nil
	}
	var tbl extTable
	if haveTable {
		if err := json.Unmarshal(b, &tbl); err != nil {
			return fmt.Errorf("%s: %w", extensionFile, err)
		}
	} else {
		r.fail(KindExtension, extVendorDir, "", "", extensionFile+" is missing: the vendored files are not registered")
	}

	var onDisk []string
	if haveVendor {
		err := filepath.WalkDir(vendor, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(vendor, p)
			onDisk = append(onDisk, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return err
		}
		sort.Strings(onDisk)
	}
	exists := map[string]bool{}
	for _, f := range onDisk {
		exists[f] = true
	}

	covered := map[string]string{} // vendor file → package (or the notice)
	var blocks []noticeBlock
	if exists[extNoticeName] {
		covered[extNoticeName] = extNoticeName
		nb, err := os.ReadFile(filepath.Join(vendor, extNoticeName))
		if err != nil {
			return err
		}
		blocks = parseExtNotice(string(nb))
	} else if haveVendor {
		r.fail(KindExtension, extVendorDir+"/"+extNoticeName, "", "", "missing: the extension package must carry a NOTICE for its third-party files")
	}
	byName := map[string]*noticeBlock{}
	for i := range blocks {
		if byName[blocks[i].name] != nil {
			r.fail(KindExtension, blocks[i].name, blocks[i].version, "", "named twice in "+extVendorDir+"/"+extNoticeName)
		}
		byName[blocks[i].name] = &blocks[i]
	}

	registered := map[string]bool{}
	for i, p := range tbl.Packages {
		name := strings.TrimSpace(p.Name)
		var extra []string
		if name == "" {
			name = fmt.Sprintf("%s entry %d", extensionFile, i+1)
			extra = append(extra, "name missing")
		} else if registered[name] {
			extra = append(extra, "listed twice in "+extensionFile)
		}
		registered[name] = true
		if p.Version == "" {
			extra = append(extra, "version missing")
		}
		if !isURL(p.Source) {
			extra = append(extra, "source must be the http(s) URL of the npm tarball")
		}
		if !isURL(p.Repository) {
			extra = append(extra, "repository must be an http(s) URL")
		}
		if !reIntegrity.MatchString(p.Integrity) {
			extra = append(extra, "integrity must be the npm integrity of the tarball (sha512-…)")
		}
		if strings.TrimSpace(p.ShippedAs) == "" {
			extra = append(extra, "shipped_as missing (what the extension package carries)")
		}
		if len(p.Files) == 0 {
			extra = append(extra, "files missing")
		}
		claim := func(f string) {
			if prev, ok := covered[f]; ok && prev != name {
				extra = append(extra, fmt.Sprintf("%s is also listed under %s", f, prev))
			}
			covered[f] = name
		}
		pinned := map[string]string{} // file → sha256 (for the NOTICE)
		var specs []string
		for _, f := range p.Files {
			fp, fg := path.Clean(f.Path), f.Glob
			switch {
			case f.Path != "" && f.Glob != "":
				extra = append(extra, "a file entry has both path and glob")
			case f.Path != "":
				specs = append(specs, fp)
				if !exists[fp] {
					extra = append(extra, fp+" not found in "+extVendorDir)
					continue
				}
				claim(fp)
				sum, err := fileSHA256(filepath.Join(vendor, filepath.FromSlash(fp)))
				if err != nil {
					return err
				}
				pinned[fp] = sum
				switch {
				case f.SHA256 == "":
					extra = append(extra, fp+": sha256 missing (actual "+sum+")")
				case !strings.EqualFold(f.SHA256, sum):
					extra = append(extra, fp+": sha256 mismatch, the file changed since it was audited (actual "+sum+")")
				}
			case f.Glob != "":
				specs = append(specs, fg)
				if _, err := path.Match(fg, ""); err != nil {
					extra = append(extra, fmt.Sprintf("glob %q: %v", fg, err))
					continue
				}
				if strings.TrimSpace(f.Contains) == "" {
					extra = append(extra, fmt.Sprintf("glob %q needs \"contains\": a text every matched file has (files under a pattern are not pinned by sum)", fg))
				}
				n := 0
				for _, d := range onDisk {
					if ok, _ := path.Match(fg, d); !ok || d == extNoticeName {
						continue
					}
					n++
					claim(d)
					if f.Contains == "" {
						continue
					}
					stem := strings.TrimSuffix(path.Base(d), path.Ext(d))
					want := strings.ReplaceAll(f.Contains, "{stem}", stem)
					fb, err := os.ReadFile(filepath.Join(vendor, filepath.FromSlash(d)))
					if err != nil {
						return err
					}
					if !strings.Contains(string(fb), want) {
						extra = append(extra, fmt.Sprintf("%s does not contain %q: not a file of this package?", d, want))
					}
				}
				if n == 0 {
					extra = append(extra, fmt.Sprintf("glob %q matches no file in %s", fg, extVendorDir))
				}
			default:
				extra = append(extra, "a file entry needs path or glob")
			}
		}

		c := Component{Kind: KindExtension, Name: name, Version: p.Version, License: strings.TrimSpace(p.License), Source: p.Source, Group: p.ShippedAs}
		c.Note = strings.TrimSpace(p.Note)
		if p.Repository != "" {
			c.Note = strings.TrimSpace(c.Note + " Upstream: " + p.Repository + ".")
		}
		lf := path.Clean(p.LicenseFile)
		switch {
		case p.LicenseFile == "":
			extra = append(extra, "license_file missing")
		case !exists[lf]:
			extra = append(extra, "license file "+lf+" not found in "+extVendorDir)
		default:
			claim(lf)
			tb, err := os.ReadFile(filepath.Join(vendor, filepath.FromSlash(lf)))
			if err != nil {
				return err
			}
			id := Identify(string(tb))
			switch {
			case id == "":
				extra = append(extra, "license file "+lf+" not recognised")
			case !licenseIDs(c.License)[strings.ToLower(canonical(id))]:
				extra = append(extra, fmt.Sprintf("license file %s is %s, but the declared license is %q", lf, id, c.License))
			}
			c.Texts = append(c.Texts, LicenseText{File: "vendor/" + lf, Text: string(tb)})
		}

		// The NOTICE shipped in the package must say the same.
		nb := byName[name]
		if nb == nil {
			if exists[extNoticeName] {
				extra = append(extra, "not named in "+extVendorDir+"/"+extNoticeName)
			}
		} else {
			if nb.version != p.Version {
				extra = append(extra, fmt.Sprintf("NOTICE says version %s", nb.version))
			}
			if !strings.EqualFold(canonical(nb.license), canonical(c.License)) {
				extra = append(extra, fmt.Sprintf("NOTICE says license %q", nb.license))
			}
			if nb.licenseFile != p.LicenseFile {
				extra = append(extra, fmt.Sprintf("NOTICE names license file %q, the registry %q", nb.licenseFile, p.LicenseFile))
			}
			if nb.source != p.Source {
				extra = append(extra, fmt.Sprintf("NOTICE says source %s", nb.source))
			}
			if nb.integrity != p.Integrity {
				extra = append(extra, "NOTICE gives another integrity")
			}
			matched := false
			for _, s := range specs {
				matched = matched || s == nb.header
			}
			if !matched {
				extra = append(extra, fmt.Sprintf("NOTICE entry %q is none of the registered files %v", nb.header, specs))
			}
			if want, ok := pinned[nb.header]; ok && nb.sha256 != want {
				extra = append(extra, "NOTICE gives another SHA-256 for "+nb.header)
			}
		}
		judge(r, al, c, extra...)
	}
	for _, nb := range blocks {
		if !registered[nb.name] {
			r.fail(KindExtension, nb.name, nb.version, nb.license, "named in "+extVendorDir+"/"+extNoticeName+" but not registered in "+extensionFile)
		}
	}
	for _, f := range onDisk {
		if _, ok := covered[f]; !ok {
			r.fail(KindExtension, extVendorDir+"/"+f, "", "", "not registered in "+extensionFile+" (third-party files only; the extension's own code does not belong in vendor/)")
		}
	}
	return nil
}

// auditProfilesLicense: the platform profiles and fixtures are released
// under CC0-1.0 (SPEC-ext 贡献与合并); the file that says so must be there.
func auditProfilesLicense(opt Options, r *Report) {
	if _, err := os.Stat(filepath.Join(opt.Root, filepath.FromSlash(extProfilesDir))); err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(opt.Root, filepath.FromSlash(extProfilesLicense)))
	if err != nil {
		r.fail(KindExtension, extProfilesLicense, "", "", "missing: profiles and fixtures are released under CC0-1.0 and the package carries that notice")
		return
	}
	for _, id := range IdentifyAll(string(b)) {
		if id == "CC0-1.0" {
			return
		}
	}
	r.fail(KindExtension, extProfilesLicense, "", "", "not recognised as CC0-1.0")
}

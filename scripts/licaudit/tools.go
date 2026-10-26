// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"
	"sort"
	"strings"
)

// Tools (3.1.0): components of kind "tool" in the component manifests, today
// the Google Cloud CLI (gcloud) that signs in to Google for Colab. The
// component manager downloads such a program from its vendor on the user's
// computer and runs it as a separate program; LectureFlow neither ships it
// in the installer nor redistributes it. Each one is listed in its own
// section of THIRD_PARTY_NOTICES.md with the license of the program itself
// (judged like every other license); what the vendor bundles inside its
// archive (gcloud on Windows: a Python runtime with the Microsoft C
// runtime) keeps its own licenses, named in the component's license_note.
//
// A tool must not have an offline archive in any manifest: that would make
// LectureFlow distribute it. Like the rule for user-fetched packages, this
// cannot be allowlisted.

// KindTool is the notices kind of tools.
const KindTool = "tool"

// auditTools lists the tool components of the manifests (components.json
// and manifest.src.json describe the same ones: the first one read wins,
// a different license elsewhere fails).
func auditTools(opt Options, r *Report, al *allowlist) error {
	manifests, err := componentManifests(opt.Root)
	if err != nil {
		return err
	}
	type seenTool struct {
		license, file string
	}
	seen := map[string]seenTool{}
	var out []Component
	var extras [][]string
	for _, mf := range manifests {
		for _, comp := range mf.components {
			if kind, _ := comp["kind"].(string); kind != KindTool {
				continue
			}
			s := func(k string) string { x, _ := comp[k].(string); return strings.TrimSpace(x) }
			id := s("id")
			if prev, ok := seen[id]; ok {
				if prev.license != s("license") {
					r.fail(KindTool, id, "", s("license"), fmt.Sprintf("license %q in %s differs from %q in %s", s("license"), mf.rel, prev.license, prev.file))
				}
				continue
			}
			seen[id] = seenTool{license: s("license"), file: mf.rel}
			version := s("revision")
			if version == "" {
				version = s("version")
			}
			note := s("license_note")
			if up := s("upstream"); up != "" && !strings.Contains(note, up) {
				note = strings.TrimSpace(up + ". " + note)
			}
			c := Component{Kind: KindTool, Name: id, Version: version, License: s("license"), Source: s("source"), Note: note,
				Group: strings.Join(toolPlatforms(comp), " ")}
			var extra []string
			if c.License == "" {
				extra = append(extra, "no license field in "+mf.rel)
			}
			if c.Source == "" {
				extra = append(extra, "source missing in "+mf.rel)
			}
			out = append(out, c)
			extras = append(extras, extra)
		}
	}
	for i, c := range out {
		judge(r, al, c, extras[i]...)
	}
	return nil
}

func toolPlatforms(comp map[string]any) []string {
	ps, _ := comp["platforms"].(map[string]any)
	out := make([]string, 0, len(ps))
	for p := range ps {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// toolOfflineViolations reports the offline entries of tool components
// (called by auditOffline for every manifest).
func toolOfflineViolations(r *Report, mf componentManifest, comp map[string]any) bool {
	if kind, _ := comp["kind"].(string); kind != KindTool {
		return false
	}
	id, _ := comp["id"].(string)
	for _, h := range offlineEntries(comp) {
		r.fail("offline", id, "", "", fmt.Sprintf("%s: %s is an offline archive of the tool %s, which the component manager downloads from its vendor on the user's computer; an offline archive would make LectureFlow distribute it. Remove the offline entry", mf.rel, h, id))
	}
	return true
}

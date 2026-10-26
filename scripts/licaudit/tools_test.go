// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"strings"
	"testing"
)

// Tools (3.1.0): kind "tool" components of the manifests, e.g. gcloud.

func aTool(id, license string, offline any) map[string]any {
	b := map[string]any{"exe": "bin/x"}
	if offline != nil {
		b["offline"] = offline
	}
	c := map[string]any{"id": id, "kind": "tool", "version": "1.0.0", "revision": "590.0.1", "license": license,
		"license_note": "bundled parts keep their own licenses", "upstream": "Example CLI 590.0.1",
		"source": "https://example.org/cli", "platforms": map[string]any{"windows-x64": b, "darwin-arm64": map[string]any{"exe": "bin/x"}}}
	if license == "" {
		delete(c, "license")
	}
	return c
}

func toolComponents(rep *Report) []Component {
	return componentsOf(rep, KindTool)
}

func TestToolsListedInTheirOwnSection(t *testing.T) {
	r := newRepo(t)
	r.manifest("components.json", aTool("gcloud", "Apache-2.0", nil))
	r.manifest("manifest.src.json", aTool("gcloud", "Apache-2.0", nil)) // the same tool again: listed once
	rep := audit(t, r.root, nil)
	if len(rep.Violations) != 0 {
		t.Fatalf("violations %v", rep.Violations)
	}
	cs := toolComponents(rep)
	if len(cs) != 1 || cs[0].Name != "gcloud" || cs[0].Version != "590.0.1" || cs[0].Group != "darwin-arm64 windows-x64" ||
		!strings.Contains(cs[0].Note, "Example CLI 590.0.1") || !strings.Contains(cs[0].Note, "keep their own licenses") {
		t.Fatalf("tools %+v", cs)
	}
	notices := RenderNotices(rep)
	sec := notices[strings.Index(notices, "## 10. "):]
	if !strings.Contains(sec, "| gcloud | 590.0.1 | Apache-2.0 | darwin-arm64 windows-x64 | https://example.org/cli |") {
		t.Errorf("section 10:\n%s", sec)
	}
	if strings.Index(notices, "## 10. ") > strings.Index(notices, "## Appendix A") {
		t.Error("section 10 is after the appendix")
	}
}

func TestToolFailures(t *testing.T) {
	r := newRepo(t)
	r.manifest("components.json",
		aTool("proprietary", "LicenseRef-Proprietary", nil),
		aTool("nolicense", "", nil),
		func() map[string]any { c := aTool("nosource", "MIT", nil); delete(c, "source"); return c }(),
		aTool("differs", "MIT", nil),
	)
	r.manifest("manifest.src.json", aTool("differs", "Apache-2.0", nil))
	rep := audit(t, r.root, nil)
	for _, name := range []string{"proprietary", "nolicense", "nosource", "differs"} {
		if len(violationsOf(rep, name)) == 0 {
			t.Errorf("%s: no violation (%v)", name, rep.Violations)
		}
	}
}

// A tool never has an offline archive, whatever the allowlist says.
func TestToolOfflineArchiveFails(t *testing.T) {
	r := newRepo(t)
	r.writeJSON(allowlistFile, map[string]any{"entries": []AllowEntry{
		{Kind: "offline", Name: "gcloud", Scope: ScopeDistributed, Reason: "trying to exempt the archive"},
	}})
	r.manifest("components.json", aTool("gcloud", "Apache-2.0", anOffline))
	r.manifest("manifest.src.json", aTool("gcloud", "Apache-2.0", nil))
	rep := audit(t, r.root, nil)
	vs := offlineViolations(rep)
	if len(vs) != 1 || vs[0].Name != "gcloud" || !strings.Contains(vs[0].Reason, "platforms.windows-x64.offline") ||
		!strings.Contains(vs[0].Reason, "components.json") || !strings.Contains(vs[0].Reason, "vendor") {
		t.Fatalf("violations %v", rep.Violations)
	}
}

// The repository's own manifests list gcloud as Apache-2.0 without findings.
func TestRepositoryGcloud(t *testing.T) {
	rep := audit(t, "../..", nil)
	var found bool
	for _, c := range toolComponents(rep) {
		if c.Name == "gcloud" {
			found = true
			if c.License != "Apache-2.0" || !strings.HasPrefix(c.Source, "https://") || c.Version == "" {
				t.Errorf("gcloud %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("gcloud is not listed")
	}
	for _, v := range rep.Violations {
		if v.Kind == KindTool || (v.Kind == "offline" && v.Name == "gcloud") {
			t.Errorf("violation %v", v)
		}
	}
}

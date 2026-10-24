// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "10.0.0", -1},
		{"1.0.0-alpha", "1.0.0", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0+build1", "1.0.0+build2", 0},
		{"v3.1.0", "3.1.0", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
	for _, bad := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "1.2.3-", "1.2.3+", "dev"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	if CompareVersions("dev", "1.0.0") != -1 {
		t.Error("an invalid version must sort first")
	}
}

func TestVersionRange(t *testing.T) {
	r, err := ParseRange(">=3.0.0 <4.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"3.0.0": true, "3.9.9": true, "4.0.0": false, "2.9.9": false, "3.0.0-beta.1": false} {
		pv, _ := ParseVersion(v)
		if got := r.Contains(pv); got != want {
			t.Errorf("%s in %s = %v", v, r, got)
		}
	}
	r2, _ := ParseRange("=3.1.0 || >=4.0.0")
	for v, want := range map[string]bool{"3.1.0": true, "3.2.0": false, "4.1.0": true} {
		pv, _ := ParseVersion(v)
		if got := r2.Contains(pv); got != want {
			t.Errorf("%s in %s = %v", v, r2, got)
		}
	}
	any, _ := ParseRange("*")
	if !any.Contains(Version{Major: 99}) {
		t.Error("* must match everything")
	}
	for _, bad := range []string{">=x", "||", ">=1.0.0 ||"} {
		if _, err := ParseRange(bad); err == nil {
			t.Errorf("ParseRange(%q) accepted", bad)
		}
	}
}

func TestManifestValidation(t *testing.T) {
	e := newTestEnv(t)
	good := e.manifest(defaultSpec())
	data := marshalManifest(t, good)
	m, err := ParseManifest(data, e.manDir)
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if !m.Compatible("3.0.0") || !m.Compatible("3.5.1") || m.Compatible("4.0.0") || m.Compatible("2.9.0") {
		t.Error("compat range not applied")
	}
	if !m.Compatible("dev") {
		t.Error("a development build must accept any manifest")
	}

	mutate := func(name string, f func(m map[string]any), wantCode, wantText string) {
		t.Helper()
		var raw map[string]any
		json.Unmarshal(data, &raw)
		f(raw)
		b, _ := json.Marshal(raw)
		_, err := ParseManifest(b, e.manDir)
		if err == nil {
			t.Errorf("%s: accepted", name)
			return
		}
		if CodeOf(err) != wantCode || !strings.Contains(err.Error(), wantText) {
			t.Errorf("%s: got %v, want %s containing %q", name, err, wantCode, wantText)
		}
	}
	comp := func(raw map[string]any, id string) map[string]any {
		for _, c := range raw["components"].([]any) {
			if c.(map[string]any)["id"] == id {
				return c.(map[string]any)
			}
		}
		t.Fatalf("no %s", id)
		return nil
	}
	build := func(raw map[string]any, id string) map[string]any {
		return comp(raw, id)["platforms"].(map[string]any)[PlatformWindowsX64].(map[string]any)
	}
	mutate("schema too new", func(m map[string]any) { m["schema"] = 2 }, CodeManifestSchema, "schema 2")
	mutate("unknown field", func(m map[string]any) { m["surprise"] = true }, CodeManifestInvalid, "surprise")
	mutate("bad sha", func(m map[string]any) {
		build(m, "model:openvino:tiny:int8")["files"].([]any)[0].(map[string]any)["sha256"] = "ABC"
	}, CodeManifestInvalid, "sha256")
	mutate("unknown placeholder", func(m map[string]any) {
		build(m, "model:openvino:tiny:int8")["files"].([]any)[0].(map[string]any)["url"] = "{nowhere}/x"
	}, CodeManifestInvalid, "unknown placeholder {nowhere}")
	mutate("unsafe name", func(m map[string]any) {
		build(m, "model:openvino:tiny:int8")["files"].([]any)[0].(map[string]any)["name"] = "../evil"
	}, CodeManifestInvalid, "unsafe name")
	mutate("unknown dependency", func(m map[string]any) {
		comp(m, IDASRCUDA)["depends"] = []any{"python", "ghost"}
	}, CodeManifestInvalid, `unknown "ghost"`)
	mutate("cycle", func(m map[string]any) {
		comp(m, IDPython)["depends"] = []any{IDASRCUDA}
	}, CodeManifestInvalid, "cycle")
	mutate("model id mismatch", func(m map[string]any) {
		comp(m, "model:openvino:tiny:int8")["precision"] = "fp16"
	}, CodeManifestInvalid, "model id")
	mutate("venv without python", func(m map[string]any) {
		comp(m, IDASRCUDA)["depends"] = []any{}
	}, CodeManifestInvalid, `depends on "python"`)
	mutate("size sum", func(m map[string]any) {
		build(m, "model:openvino:tiny:int8")["download_size"] = 1
	}, CodeManifestInvalid, "sum of file sizes")
	mutate("bad compat", func(m map[string]any) { m["compat"] = ">=three" }, CodeManifestInvalid, "compat")
	mutate("preset provider", func(m map[string]any) {
		p := m["presets"].(map[string]any)
		p["tiers"].([]any)[0].(map[string]any)["roles"] = map[string]any{"translate": map[string]any{"provider": "nobody"}}
	}, CodeManifestInvalid, `unknown provider "nobody"`)
	mutate("free provider", func(m map[string]any) {
		m["presets"].(map[string]any)["free"] = map[string]any{"default": "deepseek"}
	}, CodeManifestInvalid, "not a free provider")
	if _, err := ParseManifest([]byte("{not json"), e.manDir); CodeOf(err) != CodeManifestInvalid {
		t.Errorf("garbage: %v", err)
	}
}

func TestResolveURL(t *testing.T) {
	e := newTestEnv(t)
	m := e.manifest(defaultSpec())
	got, err := m.resolveURL("{models}/a/b.bin", Sources{Models: "hf-mirror"})
	if err != nil || len(got) != 2 || got[0].URL != e.url("/mirror/hf/a/b.bin") || got[0].Mirror != "hf-mirror" ||
		got[1].URL != e.url("/default/hf/a/b.bin") || got[1].Mirror != "" {
		t.Fatalf("mirror: %+v %v", got, err)
	}
	got, _ = m.resolveURL("{models}/a/b.bin", Sources{})
	if len(got) != 1 || got[0].URL != e.url("/default/hf/a/b.bin") {
		t.Fatalf("default: %+v", got)
	}
	// a custom URL replaces the primary placeholder only
	got, _ = m.resolveURL("{pypi}", Sources{PyPI: "https://pypi.example.invalid/simple/"})
	if got[0].URL != "https://pypi.example.invalid/simple" || got[1].URL != e.url("/default/simple") {
		t.Fatalf("custom: %+v", got)
	}
	got, _ = m.resolveURL("{python}/x.tar.gz", Sources{PyPI: "https://pypi.example.invalid/simple"})
	if len(got) != 1 || got[0].URL != e.url("/default/python/x.tar.gz") {
		t.Fatalf("custom pypi must not move {python}: %+v", got)
	}
	got, _ = m.resolveURL("{python}/x.tar.gz", Sources{PyPI: "mirror"})
	if got[0].URL != e.url("/mirror/python/x.tar.gz") {
		t.Fatalf("pypi mirror moves {python}: %+v", got)
	}
	if err := m.ValidateSources(Sources{PyPI: "nope"}); CodeOf(err) != CodeManifestInvalid {
		t.Errorf("unknown mirror: %v", err)
	}
	if err := m.ValidateSources(Sources{PyPI: "mirror", Models: "default"}); err != nil {
		t.Error(err)
	}
}

func TestSafeRelPath(t *testing.T) {
	for p, want := range map[string]bool{
		"model.bin": true, "a/b.json": true, "": false, "../x": false, "a/../b": false, "/abs": false,
		`a\b`: false, "C:x": false, "a//b": false, "./a": false, "a/": false,
	} {
		if got := safeRelPath(p); got != want {
			t.Errorf("safeRelPath(%q) = %v", p, got)
		}
	}
}

func TestModelID(t *testing.T) {
	id := ModelID("openvino", "large-v3-turbo", "fp16")
	if id != "model:openvino:large-v3-turbo:fp16" || DirName(id) != "model__openvino__large-v3-turbo__fp16" {
		t.Fatal(id)
	}
	b, n, p, ok := ParseModelID(id)
	if !ok || b != "openvino" || n != "large-v3-turbo" || p != "fp16" {
		t.Fatal(b, n, p, ok)
	}
	if _, _, _, ok := ParseModelID("model:x:y"); ok {
		t.Fatal("short id accepted")
	}
}

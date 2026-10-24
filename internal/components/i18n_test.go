// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// zhTexts collects the zh-CN texts of the proposals in
// packaging/components/i18n and of web/i18n (merged files and parts).
func zhTexts(t *testing.T) (proposal, all map[string]string) {
	t.Helper()
	read := func(p string, into map[string]string) {
		data, err := os.ReadFile(p)
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			t.Fatalf("%s is not flat JSON", p)
		}
		for k, v := range m {
			if s, ok := v.(string); ok {
				into[k] = s
			}
		}
	}
	proposal, all = map[string]string{}, map[string]string{}
	read("../../packaging/components/i18n/v3-components.zh-CN.json", proposal)
	for k, v := range proposal {
		all[k] = v
	}
	read("../../web/i18n/zh-CN.json", all)
	parts, _ := filepath.Glob("../../web/i18n/_parts/*.zh-CN.json")
	for _, p := range parts {
		read(p, all)
	}
	return proposal, all
}

func TestErrorCodesHaveTexts(t *testing.T) {
	// The proposal under packaging/components/i18n was merged by the lead
	// into the catalogs proper; check them.
	zh, en := map[string]string{}, map[string]string{}
	for loc, into := range map[string]map[string]string{"zh-CN": zh, "en": en} {
		data, err := os.ReadFile("../../web/i18n/" + loc + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
		for k, v := range m {
			if s, ok := v.(string); ok {
				into[k] = s
			}
		}
	}
	// Texts of codes added later wait in proposals under
	// packaging/components/i18n/*.{zh-CN,en}.json until the lead merges them
	// (and removes the proposal); both languages are required either way.
	for loc, into := range map[string]map[string]string{"zh-CN": zh, "en": en} {
		props, _ := filepath.Glob("../../packaging/components/i18n/*." + loc + ".json")
		// and in web/i18n/_parts (the lead's merge queue, e.g.
		// v3-acc-fix-components for components.path_too_long)
		parts, _ := filepath.Glob("../../web/i18n/_parts/*." + loc + ".json")
		props = append(props, parts...)
		for _, p := range props {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			for k, v := range m {
				if s, ok := v.(string); ok && into[k] == "" {
					into[k] = s
				}
			}
		}
	}
	for _, c := range ErrorCodes {
		if zh["errors."+c] == "" || en["errors."+c] == "" {
			t.Errorf("no zh-CN / en text for errors.%s", c)
		}
	}
}

// Every i18n key the shipped manifest names has a text somewhere.
func TestBuiltinManifestKeysHaveTexts(t *testing.T) {
	_, all := zhTexts(t)
	m, err := LoadManifest(builtinManifest)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range m.Components {
		keys = append(keys, c.NameKey, c.DescKey)
	}
	for _, g := range m.Sources {
		keys = append(keys, g.Default.NameKey)
		for _, mr := range g.Mirrors {
			keys = append(keys, mr.NameKey)
		}
	}
	for _, tier := range m.Presets.Tiers {
		keys = append(keys, tier.NameKey, tier.DescKey, tier.CostKey)
	}
	for _, p := range m.Presets.Providers {
		keys = append(keys, p.CostKey)
		keys = append(keys, p.StepsKeys...)
	}
	for _, k := range keys {
		if k != "" && all[k] == "" {
			t.Errorf("no zh-CN text for %s", k)
		}
	}
}

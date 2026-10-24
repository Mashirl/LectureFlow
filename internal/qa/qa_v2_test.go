// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package qa

import (
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// tiered is a glossary with both tiers (SPEC-v2 G1). Tier "" is read as enforce.
var tiered = []model.Term{
	{En: "key", EnKey: "key", Zh: "密钥", Enabled: true, Tier: model.TierEnforce},
	{En: "secret key", EnKey: "secret key", Zh: "秘钥", Enabled: true}, // tier "" = enforce
	{En: "private key", EnKey: "private key", Zh: "私钥", Enabled: true, Tier: model.TierEnforce},
	{En: "SSH key", EnKey: "ssh key", Zh: "", Enabled: true, Tier: model.TierEnforce}, // stays in English
	{En: "public", EnKey: "public", Zh: "公钥", Enabled: true, Tier: model.TierReference},
	{En: "public key", EnKey: "public key", Zh: "公钥", Enabled: true, Tier: model.TierReference},
	{En: "block", EnKey: "block", Zh: "分组", Enabled: true, Tier: model.TierReference},
	{En: "Python", EnKey: "python", Zh: "", Enabled: true, Tier: model.TierReference},
	{En: "LRU", EnKey: "lru", Zh: "", Enabled: true, Tier: model.TierEnforce},
	{En: "cipher", EnKey: "cipher", Zh: "密码", Enabled: false, Tier: model.TierEnforce},
}

// TestCheckTiers covers SPEC-v2 G5.4: only enabled enforce terms with a zh
// are checked; a term that only occurs inside a longer matched enforce term
// is not checked on its own; the English-residue exclusion only uses
// enforce terms without zh.
func TestCheckTiers(t *testing.T) {
	cases := []struct {
		name, target, zh string
		ok               bool
		reason           string // substring of the reason when !ok
		notReason        string // must not be in the reason
	}{
		// a shorter term inside a longer matched term: only the longer one is checked
		{"key inside secret key", "The secret key is short.", "秘钥很短。", true, "", ""},
		{"key inside plural secret keys", "Secret keys are short.", "秘钥都很短。", true, "", ""},
		{"key inside private key", "Keep the private key safe.", "妥善保管私钥。", true, "", ""},
		{"longer term missing: only the longer one is reported", "The secret key is short.", "这个密钥很短。", false,
			`glossary term "secret key" must be translated as "秘钥"`, `term "key"`},
		{"key also on its own", "The secret key and the key schedule.", "秘钥和轮密钥编排。", true, "", ""},
		{"key on its own missing", "The secret key and the key schedule.", "秘钥和编排。", false,
			`glossary term "key" must be translated as "密钥"`, `term "secret key"`},
		{"key on its own, plural", "Both keys are short.", "两把钥匙都很短。", false, `glossary term "key"`, ""},
		// an enforce term kept in English still covers the words inside it
		{"key inside an English-only enforce term", "Add your SSH key to the server.", "把你的 SSH key 加到服务器上。", true, "", ""},
		// reference terms are never checked
		{"reference term not checked", "The algorithms are public knowledge.", "这些算法是公开的知识。", true, "", ""},
		{"reference block not checked", "A block of text.", "一段文字。", true, "", ""},
		{"reference multi-word term not checked", "The public key is published.", "公开密钥被发布了。", true, "", ""},
		// a reference term does not shadow an enforce term inside it: the
		// prompt lists key under MANDATORY TERMS
		{"reference term does not cover key", "The public key is published.", "公钥被发布了。", false,
			`glossary term "key" must be translated as "密钥"`, `"public key"`},
		// disabled terms are never checked
		{"disabled term ignored", "A cipher.", "一种加密方法。", true, "", ""},
		// English residue: only enforce terms without zh are removed first
		{"enforce English-only term not residue", "LRU replacement.", "LRU 替换。", true, "", ""},
		{"reference English-only term is residue", "Python is popular.", "Python 很流行。", false, "mostly English", ""},
	}
	for _, c := range cases {
		ok, reason := Check(c.target, c.zh, tiered)
		if ok != c.ok {
			t.Errorf("%s: ok=%v reason=%q, want ok=%v", c.name, ok, reason, c.ok)
			continue
		}
		if !ok && !strings.Contains(reason, c.reason) {
			t.Errorf("%s: reason %q lacks %q", c.name, reason, c.reason)
		}
		if c.notReason != "" && strings.Contains(reason, c.notReason) {
			t.Errorf("%s: reason %q must not contain %q", c.name, reason, c.notReason)
		}
	}
}

// TestCheckReferenceOnlyGlossary: a glossary of reference terms checks no
// term at all (the old v1 behaviour would have demanded "公钥" here).
func TestCheckReferenceOnlyGlossary(t *testing.T) {
	g := []model.Term{{En: "public", EnKey: "public", Zh: "公钥", Enabled: true, Tier: model.TierReference}}
	if ok, reason := Check("This is public knowledge.", "这是公开的知识。", g); !ok {
		t.Errorf("reference term checked: %s", reason)
	}
	g[0].Tier = model.TierEnforce
	if ok, _ := Check("This is public knowledge.", "这是公开的知识。", g); ok {
		t.Error("the same term in the enforce tier must be checked")
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package ctxbuild

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// TestJanitorPromptVerbatim: JanitorSystem is the G3.4 code block of SPEC-v2.md.
func TestJanitorPromptVerbatim(t *testing.T) {
	b, err := os.ReadFile("testdata/prompt-spec/v2.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(spec, "\n### G3.4")
	if i < 0 {
		t.Fatal("SPEC-v2.md: ### G3.4 not found")
	}
	rest := spec[i:]
	open := strings.Index(rest, "\n```\n")
	if open < 0 {
		t.Fatal("G3.4: no code block")
	}
	body := rest[open+len("\n```\n"):]
	end := strings.Index(body, "\n```")
	if end < 0 {
		t.Fatal("G3.4: unterminated code block")
	}
	if want := body[:end]; JanitorSystem != want {
		t.Errorf("JanitorSystem differs from SPEC-v2 G3.4:\n got: %q\nwant: %q", JanitorSystem, want)
	}
	if FirstLine(JanitorSystem) != "You are the glossary curator for a university course. The glossary is used in two ways:" {
		t.Errorf("first line %q", FirstLine(JanitorSystem))
	}
}

// janitorSample is a small course: terms out of order (unlocked, locked,
// disabled, empty tier), materials out of upload order, a syllabus.
func janitorSample() JanitorInput {
	lec := "L1"
	return JanitorInput{
		CourseName: "Cryptography",
		CourseCode: "CSC1003",
		Syllabus:   "# CSC1003 Cryptography\n\n## Weekly Topics\n| Week | Topic |\n|---|---|\n| 1 | Classical ciphers: Caesar and the Vigenère cipher |\n| 2 | Public key cryptography |\n",
		Terms: []model.Term{
			{ID: "t5", En: "Vigenère cipher", EnKey: "vigenère cipher", Zh: "维吉尼亚密码", Source: model.SourceSyllabus, Enabled: true, Freq: 2, Tier: model.TierEnforce},
			{ID: "t1", En: "public", EnKey: "public", Zh: "公钥", Source: model.SourceMaterial, Enabled: true, Freq: 3, Tier: model.TierEnforce, CommonWord: true},
			{ID: "t3", En: "key", EnKey: "key", Zh: "密钥", Source: model.SourceManual, Enabled: true, Freq: 9, Tier: model.TierEnforce, CommonWord: true, Locked: true, ZhSource: model.ZhSourceUser},
			{ID: "t2", En: "IV", EnKey: "iv", Zh: "初始向量", Source: model.SourceMaterial, Enabled: true, Freq: 1},
			{ID: "t4", En: "world", EnKey: "world", Zh: "世界", Source: model.SourceMaterial, Enabled: false, Freq: 1, Tier: model.TierEnforce, CommonWord: true},
			{ID: "t6", En: "Kasiski examination", EnKey: "kasiski examination", Zh: "", Source: model.SourceMaterial, Enabled: true, Tier: model.TierReference},
			{ID: "t7", En: "keyboard <shortcut> & co", EnKey: "keyboard <shortcut> & co", Zh: "", Source: model.SourceSyllabus, Enabled: true},
		},
		Materials: []model.Material{
			{ID: "m2", LectureID: nil, Filename: "week2.pdf", CreatedAt: "2026-01-02T00:00:00Z", PagesMD: []string{
				"Public key cryptography\nThe public key is published; the private key is kept secret.",
				"Key   exchange.   Keyboard shortcuts are not keys.",
			}},
			{ID: "m1", LectureID: &lec, Filename: "week1.pdf", CreatedAt: "2026-01-01T00:00:00Z", PagesMD: []string{
				"Classical ciphers",
				"The Vigenère cipher repeats a key; the Kasiski examination finds the key length. In the real world the algorithms are public knowledge and security relies only on the secrecy of the key, as Kerckhoffs said a long time ago.",
				"",
			}},
		},
	}
}

func TestJanitorRequestsGolden(t *testing.T) {
	reqs := JanitorRequests(janitorSample())
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	golden(t, "janitor_system.golden", reqs[0].System)
	golden(t, "janitor_user.golden", reqs[0].User)

	// byte-stable: the same input in another order gives the same bytes
	in := janitorSample()
	for i, j := 0, len(in.Terms)-1; i < j; i, j = i+1, j-1 {
		in.Terms[i], in.Terms[j] = in.Terms[j], in.Terms[i]
	}
	in.Materials[0], in.Materials[1] = in.Materials[1], in.Materials[0]
	again := JanitorRequests(in)
	if len(again) != 1 || again[0] != reqs[0] {
		t.Error("JanitorRequests is not stable under input order")
	}
}

type janitorLine struct {
	ID         string   `json:"id"`
	En         string   `json:"en"`
	Zh         string   `json:"zh"`
	Tier       string   `json:"tier"`
	CommonWord bool     `json:"common_word"`
	Freq       int      `json:"freq"`
	Source     string   `json:"source"`
	Contexts   []string `json:"contexts"`
	Locked     bool     `json:"locked"`
}

func parseJanitorUser(t *testing.T, user string) (open, locked []janitorLine) {
	t.Helper()
	parts := strings.SplitN(user, "\n\n", 2)
	for k, part := range parts {
		for _, l := range strings.Split(part, "\n") {
			var x janitorLine
			if err := json.Unmarshal([]byte(l), &x); err != nil {
				t.Fatalf("line %q: %v", l, err)
			}
			if k == 0 && !x.Locked {
				open = append(open, x)
			} else if k == 1 && x.Locked {
				locked = append(locked, x)
			} else {
				t.Fatalf("line %q in the wrong group", l)
			}
		}
	}
	return open, locked
}

func TestJanitorRequestsContent(t *testing.T) {
	in := janitorSample()
	r := JanitorRequests(in)[0]
	if !strings.HasPrefix(r.System, JanitorSystem+"\n\nCOURSE: Cryptography (CSC1003)\n\nSYLLABUS.md:\n# CSC1003 Cryptography") {
		t.Errorf("system prefix:\n%s", r.System)
	}
	open, locked := parseJanitorUser(t, r.User)
	var ids []string
	for _, x := range open {
		ids = append(ids, x.ID)
	}
	// en_key order; disabled "world" and locked "key" are not entries
	if got := strings.Join(ids, ","); got != "t2,t6,t7,t1,t5" {
		t.Errorf("entries %s", got)
	}
	if len(locked) != 1 || locked[0].ID != "t3" || locked[0].Contexts != nil {
		t.Errorf("locked %+v", locked)
	}
	byID := map[string]janitorLine{}
	for _, x := range open {
		byID[x.ID] = x
	}
	if x := byID["t2"]; x.Tier != model.TierEnforce || x.Contexts == nil || len(x.Contexts) != 0 {
		t.Errorf("IV: %+v (tier \"\" sent as enforce; no context: [])", x)
	}
	pub := byID["t1"].Contexts
	if len(pub) != 2 || !strings.Contains(pub[0], "public knowledge") || !strings.Contains(pub[1], "Public key cryptography") {
		t.Errorf("public contexts %q (materials in upload order, at most 2)", pub)
	}
	if x := byID["t5"]; len(x.Contexts) != 2 || !strings.Contains(x.Contexts[1], "Caesar and the Vigenère cipher") {
		t.Errorf("Vigenère contexts %q (the syllabus after the materials)", x.Contexts)
	}
	for _, c := range append(pub, byID["t5"].Contexts...) {
		if n := len([]rune(strings.Trim(c, "…"))); n > 2*JanitorContextChars+30 {
			t.Errorf("context too long (%d): %q", n, c)
		}
	}
	if !strings.Contains(r.User, `"en":"keyboard <shortcut> & co"`) {
		t.Errorf("HTML characters must not be escaped:\n%s", r.User)
	}
}

func TestJanitorContexts(t *testing.T) {
	long := strings.Repeat("word ", 30)
	in := JanitorInput{
		Terms: []model.Term{{ID: "k", En: "key", EnKey: "key", Enabled: true}},
		Materials: []model.Material{{ID: "m", CreatedAt: "x", PagesMD: []string{
			"Keyboards and monkeys and turkeys.", // no whole-word match
			long + "the KEY is here " + long,
			long + "the KEY is here " + long, // same snippet again: skipped
			"a key, and another key right after it",
		}}},
	}
	open, _ := parseJanitorUser(t, JanitorRequests(in)[0].User)
	got := open[0].Contexts
	if len(got) != 2 {
		t.Fatalf("contexts %q", got)
	}
	if !strings.HasPrefix(got[0], "…") || !strings.HasSuffix(got[0], "…") || !strings.Contains(got[0], "the KEY is here") {
		t.Errorf("first context %q: cut on both sides, original case", got[0])
	}
	if strings.Contains(got[0], "wor…") || strings.Contains(got[0], "…ord") {
		t.Errorf("context %q cut inside a word", got[0])
	}
	if got[1] != "a key, and another key right after it" {
		t.Errorf("second context %q", got[1])
	}
}

func TestJanitorBatches(t *testing.T) {
	mk := func(n int, locked int) JanitorInput {
		var in JanitorInput
		for i := n - 1; i >= 0; i-- { // reverse order: the batches sort by en_key
			en := fmt.Sprintf("term%03d", i)
			in.Terms = append(in.Terms, model.Term{ID: "t" + en, En: en, EnKey: en, Enabled: true})
		}
		for i := 0; i < locked; i++ {
			en := fmt.Sprintf("locked%d", i)
			in.Terms = append(in.Terms, model.Term{ID: "l" + en, En: en, EnKey: en, Enabled: true, Locked: true})
		}
		return in
	}
	reqs := JanitorRequests(mk(450, 2))
	if len(reqs) != 3 {
		t.Fatalf("450 entries: %d requests, want 3", len(reqs))
	}
	for k, want := range []struct {
		n           int
		first, last string
	}{{200, "term000", "term199"}, {200, "term200", "term399"}, {50, "term400", "term449"}} {
		open, locked := parseJanitorUser(t, reqs[k].User)
		if len(open) != want.n || open[0].En != want.first || open[len(open)-1].En != want.last {
			t.Errorf("batch %d: %d entries %s..%s, want %d %s..%s", k, len(open), open[0].En, open[len(open)-1].En, want.n, want.first, want.last)
		}
		if len(locked) != 2 {
			t.Errorf("batch %d: %d locked lines, want 2 in every batch", k, len(locked))
		}
		if reqs[k].System != reqs[0].System {
			t.Error("every batch shares the fixed prefix")
		}
	}
	if n := len(JanitorRequests(mk(400, 0))); n != 1 {
		t.Errorf("400 entries: %d requests, want 1 (batches only above 400)", n)
	}
	if n := len(JanitorRequests(mk(401, 0))); n != 3 {
		t.Errorf("401 entries: %d requests, want 3", n)
	}
	if reqs := JanitorRequests(mk(0, 3)); reqs != nil {
		t.Errorf("only locked terms: %d requests, want none", len(reqs))
	}
}

func TestJanitorSyllabusTruncated(t *testing.T) {
	in := JanitorInput{CourseName: "C", Syllabus: strings.Repeat("é", JanitorSyllabusMaxChars+100),
		Terms: []model.Term{{ID: "a", En: "ab", EnKey: "ab", Enabled: true}}}
	sys := JanitorRequests(in)[0].System
	if n := strings.Count(sys, "é"); n != JanitorSyllabusMaxChars {
		t.Errorf("syllabus %d characters, want %d", n, JanitorSyllabusMaxChars)
	}
	in.Syllabus, in.CourseName = "", ""
	if sys := JanitorRequests(in)[0].System; !strings.HasSuffix(sys, "\n\nCOURSE: (none)\n\nSYLLABUS.md:\n(none)") {
		t.Errorf("empty course and syllabus: %q", sys[len(JanitorSystem):])
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package glossary

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

func TestIsCommonWord(t *testing.T) {
	cases := []struct {
		en   string
		want bool
	}{
		// G0 examples: everyday words
		{"public", true}, {"private", true}, {"real", true}, {"block", true}, {"state", true},
		{"world", true}, {"zero", true}, {"key", true}, {"data", true}, {"analysis", true},
		{"practical", true}, {"theory", true},
		{"Public", true}, {"  key  ", true}, // case and surrounding space do not matter
		// plural forms: trailing s / es (and y → ies)
		{"keys", true}, {"blocks", true}, {"classes", true}, {"boxes", true}, {"properties", true},
		{"algorithms", false}, // "algorithm" is not an everyday word
		// acronyms: all capital letters
		{"AES", false}, {"IV", false}, {"PUBLIC", false}, {"KEYS", false},
		// hyphens and digits
		{"IND-CCA", false}, {"write-back", false}, {"AES-256", false}, {"SHA1", false}, {"x86", false}, {"key2", false},
		// several words
		{"public key", false}, {"block cipher", false}, {"real world", false},
		// technical words
		{"cipher", false}, {"ciphertext", false}, {"Vigenère", false}, {"Kasiski", false}, {"hash", false},
		{"", false}, {"   ", false},
	}
	for _, c := range cases {
		if got := IsCommonWord(c.en); got != c.want {
			t.Errorf("IsCommonWord(%q) = %v, want %v", c.en, got, c.want)
		}
	}
}

// TestCommonWordList: the embedded list is the project's own (D4 header),
// 3000–5000 lower-case single words, with the G0 words in and technical words out.
func TestCommonWordList(t *testing.T) {
	if !strings.Contains(commonWordsTxt, "由项目自行编写（LectureFlow），原创，无第三方许可问题") {
		t.Error("commonwords.txt must say it was written by the project (appendix D4)")
	}
	words := commonWords()
	if n := len(words); n < 3000 || n > 5000 {
		t.Errorf("%d words, want 3000–5000 (appendix D4)", n)
	}
	for w := range words {
		for _, r := range w {
			if r < 'a' || r > 'z' {
				t.Fatalf("word %q is not a lower-case ASCII word", w)
			}
		}
	}
	for _, w := range []string{"public", "private", "real", "block", "state", "world", "zero", "key", "data", "analysis", "practical", "theory"} {
		if _, ok := words[w]; !ok {
			t.Errorf("%q must be a common word (G0)", w)
		}
	}
	for _, w := range []string{"cipher", "algorithm", "encryption", "hash", "cache", "matrix", "byte", "nonce", "plaintext", "ciphertext"} {
		if _, ok := words[w]; ok {
			t.Errorf("%q is a technical word and must not be in the list", w)
		}
	}
}

// ---------- store helpers ----------

type tenv struct {
	t      *testing.T
	ctx    context.Context
	st     db.Store
	course model.Course
}

func newTenv(t *testing.T) *tenv {
	t.Helper()
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &tenv{t: t, ctx: context.Background(), st: st}
	e.course = e.newCourse("Cryptography")
	return e
}

func (e *tenv) newCourse(name string) model.Course {
	e.t.Helper()
	c := model.Course{ID: db.NewID(), Name: name, CreatedAt: db.Now()}
	e.must(e.st.CreateCourse(e.ctx, c))
	return c
}

func (e *tenv) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// term inserts an enabled extractor term; mod adjusts it first.
func (e *tenv) term(courseID, en, zh string, mod func(*model.Term)) model.Term {
	e.t.Helper()
	t := model.Term{ID: db.NewID(), CourseID: courseID, En: en, EnKey: db.EnKey(en), Zh: zh, Source: model.SourceMaterial,
		Enabled: true, Tier: model.TierEnforce, ZhSource: model.ZhSourceExtractor}
	if mod != nil {
		mod(&t)
	}
	e.must(e.st.InsertTerm(e.ctx, t))
	return t
}

func (e *tenv) get(id string) model.Term {
	e.t.Helper()
	t, err := e.st.GetTerm(e.ctx, id)
	e.must(err)
	return t
}

func (e *tenv) version(courseID string) int {
	e.t.Helper()
	c, err := e.st.GetCourse(e.ctx, courseID)
	e.must(err)
	return c.GlossaryVersion
}

func locked(t *model.Term)   { t.Locked = true; t.ZhSource = model.ZhSourceUser }
func disabled(t *model.Term) { t.Enabled = false }

var testNow = time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)

// ---------- Fallback ----------

func TestFallbackRule(t *testing.T) {
	e := newTenv(t)
	c := e.course.ID
	public := e.term(c, "public", "公钥", nil)
	private := e.term(c, "private", "私钥", locked) // locked: never changed
	algo := e.term(c, "algorithm", "算法", nil)
	algos := e.term(c, "algorithms", "算法", nil)
	key := e.term(c, "key", "密钥", nil)
	keys := e.term(c, "keys", "", nil) // a common word and a plural: the plural rule wins
	mac := e.term(c, "MAC", "消息认证码", nil)
	macs := e.term(c, "MACs", "", nil)
	des := e.term(c, "DES", "", nil) // not a plural of "DE": the suffix is a capital letter
	e.term(c, "DE", "", nil)
	block := e.term(c, "block", "分组", func(t *model.Term) { t.Tier = model.TierReference }) // already reference
	world := e.term(c, "world", "世界", disabled)                                             // disabled: left alone
	zero := e.term(c, "zero", "零", locked)
	cipher := e.term(c, "block cipher", "分组密码", nil)
	ciphers := e.term(c, "Block Ciphers", "", nil) // a multi-word plural
	v0 := e.version(c)

	var items []model.JanitorItem
	e.must(e.st.Tx(e.ctx, func(tx db.Store) error {
		var err error
		items, err = Fallback(e.ctx, tx, c)
		return err
	}))
	if e.version(c) != v0 {
		t.Error("Fallback must not bump glossary_version")
	}

	type want struct {
		action, note string
	}
	wants := map[string]want{
		public.ID:  {model.JanitorReference, "Common English word (automatic rule)."},
		algos.ID:   {model.JanitorDisable, "Plural of 'algorithm' (automatic rule)."},
		key.ID:     {model.JanitorReference, "Common English word (automatic rule)."},
		keys.ID:    {model.JanitorDisable, "Plural of 'key' (automatic rule)."},
		macs.ID:    {model.JanitorDisable, "Plural of 'MAC' (automatic rule)."},
		ciphers.ID: {model.JanitorDisable, "Plural of 'block cipher' (automatic rule)."},
	}
	if len(items) != len(wants) {
		t.Errorf("items %+v, want %d", items, len(wants))
	}
	prev := ""
	for _, it := range items {
		w, ok := wants[it.TermID]
		if !ok {
			t.Errorf("unexpected item %+v", it)
			continue
		}
		if it.Action != w.action || it.Reason != w.note {
			t.Errorf("item %s: %s %q, want %s %q", it.En, it.Action, it.Reason, w.action, w.note)
		}
		if k := db.EnKey(it.En); k < prev {
			t.Errorf("items not ordered by en_key: %q after %q", k, prev)
		} else {
			prev = k
		}
		got := e.get(it.TermID)
		if got.JanitorNote != w.note || model.StateOf(got) != it.After {
			t.Errorf("term %s: note %q state %+v, item after %+v", got.En, got.JanitorNote, model.StateOf(got), it.After)
		}
		if it.Before.Tier != model.TierEnforce || !it.Before.Enabled {
			t.Errorf("item %s before %+v", it.En, it.Before)
		}
	}
	if g := e.get(public.ID); g.Tier != model.TierReference || !g.Enabled || g.Zh != "公钥" || g.ZhSource != model.ZhSourceExtractor || !g.CommonWord {
		t.Errorf("public %+v", g)
	}
	if g := e.get(algos.ID); g.Enabled || g.Tier != model.TierEnforce {
		t.Errorf("algorithms %+v (disabled, tier kept)", g)
	}
	for _, id := range []string{algo.ID, mac.ID, des.ID, cipher.ID} {
		if g := e.get(id); !g.Enabled || g.Tier != model.TierEnforce || g.JanitorNote != "" {
			t.Errorf("%s must be unchanged: %+v", g.En, g)
		}
	}
	if g := e.get(private.ID); g.Tier != model.TierEnforce || !g.Enabled || !g.CommonWord || g.JanitorNote != "" {
		t.Errorf("locked private: %+v (common_word computed, nothing else)", g)
	}
	if g := e.get(zero.ID); g.Tier != model.TierEnforce || g.JanitorNote != "" {
		t.Errorf("locked zero: %+v", g)
	}
	if g := e.get(block.ID); g.JanitorNote != "" || g.Tier != model.TierReference {
		t.Errorf("block already reference: %+v", g)
	}
	if g := e.get(world.ID); g.Enabled || g.Tier != model.TierEnforce || g.JanitorNote != "" {
		t.Errorf("disabled world: %+v", g)
	}

	// idempotent
	e.must(e.st.Tx(e.ctx, func(tx db.Store) error {
		again, err := Fallback(e.ctx, tx, c)
		if len(again) != 0 {
			t.Errorf("second Fallback changed %+v", again)
		}
		return err
	}))
}

func TestRunFallbackReportAndUndo(t *testing.T) {
	e := newTenv(t)
	c := e.course.ID
	public := e.term(c, "public", "公钥", nil)
	e.term(c, "algorithm", "", nil)
	algos := e.term(c, "algorithms", "算法", nil)
	e.term(c, "Vigenère cipher", "维吉尼亚密码", nil)
	v0 := e.version(c)

	if _, err := LatestReport(e.ctx, e.st, c); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("LatestReport before any report: %v, want ErrNotFound", err)
	}
	if _, err := LastRun(e.ctx, e.st, c); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("LastRun before any run: %v, want ErrNotFound", err)
	}
	rep, changed, err := RunFallback(e.ctx, e.st, c, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || rep.FromVersion != v0 || rep.Version != v0+1 || e.version(c) != v0+1 {
		t.Errorf("changed %v report v%d → v%d, course v%d (from v%d): version + exactly 1", changed, rep.FromVersion, rep.Version, e.version(c), v0)
	}
	if !rep.Fallback || rep.CourseID != c || rep.CreatedAt != "2026-10-23T08:00:00Z" || rep.Note == "" || len(rep.Items) != 2 || rep.ProviderName != "" || rep.Model != "" {
		t.Errorf("report %+v", rep)
	}
	latest, err := LatestReport(e.ctx, e.st, c)
	if err != nil || latest.Version != rep.Version || len(latest.Items) != 2 {
		t.Errorf("LatestReport %+v %v", latest, err)
	}
	if byV, err := Report(e.ctx, e.st, c, rep.Version); err != nil || byV.CreatedAt != rep.CreatedAt {
		t.Errorf("Report(v%d) %+v %v", rep.Version, byV, err)
	}
	if _, err := Report(e.ctx, e.st, c, rep.Version+7); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Report of an unknown version: %v", err)
	}

	if lr, err := LastRun(e.ctx, e.st, c); err != nil || lr.CreatedAt != rep.CreatedAt || len(lr.Items) != 2 || !lr.Fallback {
		t.Errorf("LastRun after a run with changes: %+v %v", lr, err)
	}

	// a run that changes nothing keeps the version: it is recorded as the
	// last run only; the versioned report with changes (for undo) stays latest
	rep2, changed2, err := RunFallback(e.ctx, e.st, c, testNow.Add(time.Hour))
	if err != nil || changed2 || len(rep2.Items) != 0 || rep2.Version != v0+1 || rep2.FromVersion != v0+1 || e.version(c) != v0+1 {
		t.Errorf("no-op run: %+v %v %v", rep2, changed2, err)
	}
	if l, _ := LatestReport(e.ctx, e.st, c); len(l.Items) != 2 || l.CreatedAt != rep.CreatedAt {
		t.Errorf("no-op run replaced the report with changes: %+v", l)
	}
	if byV, _ := Report(e.ctx, e.st, c, v0+1); len(byV.Items) != 2 {
		t.Errorf("no-op run overwrote the versioned report: %+v", byV)
	}
	if lr, err := LastRun(e.ctx, e.st, c); err != nil || lr.CreatedAt != "2026-10-23T09:00:00Z" || len(lr.Items) != 0 ||
		!lr.Fallback || lr.NoteInfo == nil || lr.NoteInfo.Code != CodeNoteFallbackManual || lr.Version != v0+1 {
		t.Errorf("LastRun after a no-op run: %+v %v", lr, err)
	}

	// undo
	v1 := e.version(c)
	undone, err := Undo(e.ctx, e.st, c, rep.Version, public.ID, testNow.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g := e.get(public.ID)
	if g.Tier != model.TierEnforce || !g.Enabled || g.Zh != "公钥" || g.ZhSource != model.ZhSourceExtractor || !g.Locked || g.JanitorNote != "" {
		t.Errorf("undone public %+v: before state, locked, note cleared", g)
	}
	if e.version(c) != v1+1 {
		t.Errorf("undo: version %d, want %d", e.version(c), v1+1)
	}
	found := false
	for _, it := range undone.Items {
		if it.TermID == public.ID {
			found = true
			if !it.Undone || it.UndoneAt != "2026-10-23T10:00:00Z" {
				t.Errorf("undone item %+v", it)
			}
		} else if it.Undone {
			t.Errorf("other item marked undone: %+v", it)
		}
	}
	if !found {
		t.Error("undo result lacks the item")
	}
	if stored, _ := Report(e.ctx, e.st, c, rep.Version); !stored.Items[0].Undone && !stored.Items[1].Undone {
		t.Error("the stored report must mark the item undone")
	}
	if _, err := Undo(e.ctx, e.st, c, rep.Version, public.ID, testNow); !errors.Is(err, ErrAlreadyUndone) {
		t.Errorf("second undo: %v, want ErrAlreadyUndone", err)
	}
	if e.version(c) != v1+1 {
		t.Error("a refused undo must not bump the version")
	}
	if _, err := Undo(e.ctx, e.st, c, rep.Version, "t_unknown", testNow); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("undo of an item not in the report: %v", err)
	}
	if _, err := Undo(e.ctx, e.st, c, rep.Version+5, algos.ID, testNow); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("undo in a missing report: %v", err)
	}
	// the term was deleted meanwhile
	e.must(e.st.DeleteTerm(e.ctx, algos.ID))
	if _, err := Undo(e.ctx, e.st, c, rep.Version, algos.ID, testNow); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("undo of a deleted term: %v", err)
	}
	// locked terms are not touched by a later run
	if _, _, err := RunFallback(e.ctx, e.st, c, testNow); err != nil {
		t.Fatal(err)
	}
	if g := e.get(public.ID); g.Tier != model.TierEnforce {
		t.Errorf("a fallback run changed the undone (locked) term: %+v", g)
	}
}

// mergeReport stores the report of a merge of "IVs" into "IV" that filled
// the target's empty zh: two items, the merge and the target's fix_zh
// (Into = the merged term). Another item makes sure only the pair is undone.
func (e *tenv) mergeReport() (rep model.JanitorReport, target, merged, other model.Term) {
	c := e.course.ID
	target = e.term(c, "IV", "初始化向量", func(t *model.Term) { t.JanitorNote = "Filled from merged 'IVs'." })
	merged = e.term(c, "IVs", "初始化向量", func(t *model.Term) { t.Enabled = false; t.JanitorNote = "Plural of 'IV'." })
	other = e.term(c, "public", "公钥", func(t *model.Term) { t.Tier = model.TierReference; t.JanitorNote = "Common word." })
	v := e.version(c)
	ext := model.ZhSourceExtractor
	rep = model.JanitorReport{CourseID: c, FromVersion: v, Version: v, CreatedAt: "2026-10-23T08:00:00Z", Items: []model.JanitorItem{
		{TermID: target.ID, En: target.En, Action: model.JanitorFixZh, Into: merged.ID, IntoEn: merged.En, Reason: "Filled from merged 'IVs'.",
			Before: model.TermState{Tier: model.TierEnforce, Enabled: true, Zh: "", ZhSource: ext},
			After:  model.TermState{Tier: model.TierEnforce, Enabled: true, Zh: "初始化向量", ZhSource: ext}},
		{TermID: merged.ID, En: merged.En, Action: model.JanitorMerge, Into: target.ID, IntoEn: target.En, Reason: "Plural of 'IV'.",
			Before: model.TermState{Tier: model.TierEnforce, Enabled: true, Zh: "初始化向量", ZhSource: ext},
			After:  model.TermState{Tier: model.TierEnforce, Enabled: false, Zh: "初始化向量", ZhSource: ext}},
		{TermID: other.ID, En: other.En, Action: model.JanitorReference, Reason: "Common word.",
			Before: model.TermState{Tier: model.TierEnforce, Enabled: true, Zh: "公钥", ZhSource: ext},
			After:  model.TermState{Tier: model.TierReference, Enabled: true, Zh: "公钥", ZhSource: ext}},
	}}
	e.must(SaveRunReport(e.ctx, e.st, rep))
	return rep, target, merged, other
}

// TestUndoMerge: undoing a merge also restores the target whose empty zh it
// filled (both items undone, version + 1 once); the target's item alone
// restores only the target; a merge whose target item was undone first
// restores only the merged term (lead, Gate review round 1).
func TestUndoMerge(t *testing.T) {
	t.Run("merge restores both", func(t *testing.T) {
		e := newTenv(t)
		c := e.course.ID
		rep, target, merged, other := e.mergeReport()
		v0 := e.version(c)
		got, err := Undo(e.ctx, e.st, c, rep.Version, merged.ID, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if g := e.get(merged.ID); !g.Enabled || !g.Locked || g.JanitorNote != "" {
			t.Errorf("merged term %+v", g)
		}
		if g := e.get(target.ID); g.Zh != "" || g.ZhSource != model.ZhSourceExtractor || !g.Locked || g.JanitorNote != "" {
			t.Errorf("target %+v: back to its empty zh, locked", g)
		}
		if g := e.get(other.ID); g.Tier != model.TierReference || g.Locked {
			t.Errorf("unrelated item touched: %+v", g)
		}
		if e.version(c) != v0+1 {
			t.Errorf("version %d, want %d (once for both)", e.version(c), v0+1)
		}
		if !got.Items[0].Undone || !got.Items[1].Undone || got.Items[2].Undone || got.Items[0].UndoneAt != got.Items[1].UndoneAt {
			t.Errorf("items %+v", got.Items)
		}
		if stored, _ := Report(e.ctx, e.st, c, rep.Version); !stored.Items[0].Undone || !stored.Items[1].Undone {
			t.Error("stored report not marked")
		}
		if lr, _ := LastRun(e.ctx, e.st, c); !lr.Items[0].Undone || !lr.Items[1].Undone || lr.Items[2].Undone {
			t.Errorf("the last run (the same run) must follow the undo: %+v", lr.Items)
		}
		if _, err := Undo(e.ctx, e.st, c, rep.Version, target.ID, testNow); !errors.Is(err, ErrAlreadyUndone) {
			t.Errorf("target item after the merge undo: %v", err)
		}
	})
	t.Run("target alone, then merge", func(t *testing.T) {
		e := newTenv(t)
		c := e.course.ID
		rep, target, merged, _ := e.mergeReport()
		v0 := e.version(c)
		got, err := Undo(e.ctx, e.st, c, rep.Version, target.ID, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if g := e.get(target.ID); g.Zh != "" || !g.Locked {
			t.Errorf("target %+v", g)
		}
		if g := e.get(merged.ID); g.Enabled || g.Locked {
			t.Errorf("merged term must stay merged: %+v", g)
		}
		if !got.Items[0].Undone || got.Items[1].Undone {
			t.Errorf("items %+v", got.Items)
		}
		// the user edits the target, then undoes the merge: the target keeps the user's zh
		x := e.get(target.ID)
		x.Zh, x.ZhSource = "初始化矢量", model.ZhSourceUser
		e.must(e.st.UpdateTerm(e.ctx, x))
		if _, err := Undo(e.ctx, e.st, c, rep.Version, merged.ID, testNow); err != nil {
			t.Fatal(err)
		}
		if g := e.get(target.ID); g.Zh != "初始化矢量" {
			t.Errorf("target restored twice: %+v", g)
		}
		if g := e.get(merged.ID); !g.Enabled || !g.Locked {
			t.Errorf("merged term %+v", g)
		}
		if e.version(c) != v0+2 {
			t.Errorf("version %d, want %d (two undos)", e.version(c), v0+2)
		}
	})
}

// TestUndoMergeTargetChanged: a target changed after the run (by the user,
// or in any field) is left alone when the merge is undone; its item stays
// not undone (lead, Gate review round 2).
func TestUndoMergeTargetChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*model.Term)
	}{
		{"user edit", func(t *model.Term) { t.Zh, t.ZhSource, t.Locked = "初始化矢量", model.ZhSourceUser, true }},
		{"locked only", func(t *model.Term) { t.Locked = true }},
		{"tier changed", func(t *model.Term) { t.Tier = model.TierReference }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTenv(t)
			c := e.course.ID
			rep, target, merged, _ := e.mergeReport()
			x := e.get(target.ID)
			tc.change(&x)
			e.must(e.st.UpdateTerm(e.ctx, x))
			v0 := e.version(c)
			got, err := Undo(e.ctx, e.st, c, rep.Version, merged.ID, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if g := e.get(target.ID); g != x {
				t.Errorf("target changed by the undo:\n got %+v\nwant %+v", g, x)
			}
			if g := e.get(merged.ID); !g.Enabled || !g.Locked {
				t.Errorf("merged term %+v", g)
			}
			if got.Items[0].Undone || !got.Items[1].Undone {
				t.Errorf("items %+v: only the merge is undone", got.Items)
			}
			if e.version(c) != v0+1 {
				t.Errorf("version %d, want %d", e.version(c), v0+1)
			}
		})
	}
}

// TestLastRunFallsBack: without a recorded last run, LastRun reads the
// latest versioned report (reports stored before last runs existed).
func TestLastRunFallsBack(t *testing.T) {
	e := newTenv(t)
	rep := model.JanitorReport{CourseID: e.course.ID, Version: 3, CreatedAt: "2026-10-01T00:00:00Z",
		Items: []model.JanitorItem{{TermID: "x", En: "x", Action: model.JanitorDisable}}}
	e.must(SaveReport(e.ctx, e.st, rep))
	if lr, err := LastRun(e.ctx, e.st, e.course.ID); err != nil || lr.Version != 3 || len(lr.Items) != 1 {
		t.Errorf("LastRun %+v %v", lr, err)
	}
}

func TestBackfillCommonWords(t *testing.T) {
	e := newTenv(t)
	c1 := e.course.ID
	c2 := e.newCourse("Other").ID
	a := e.term(c1, "public", "公钥", nil)
	b := e.term(c2, "Block", "", locked)
	n := e.term(c2, "Feistel network", "", func(t *model.Term) { t.CommonWord = true }) // wrong flag: corrected
	v1, v2 := e.version(c1), e.version(c2)

	got, err := BackfillCommonWords(e.ctx, e.st)
	if err != nil || got != 3 {
		t.Fatalf("BackfillCommonWords = %d %v, want 3", got, err)
	}
	if g := e.get(a.ID); !g.CommonWord || g.Tier != model.TierEnforce || g.JanitorNote != "" {
		t.Errorf("public %+v: common_word only", g)
	}
	if g := e.get(b.ID); !g.CommonWord || !g.Locked {
		t.Errorf("Block %+v", g)
	}
	if g := e.get(n.ID); g.CommonWord {
		t.Errorf("Feistel network %+v", g)
	}
	if e.version(c1) != v1 || e.version(c2) != v2 {
		t.Error("backfill must not bump glossary_version")
	}
	if v, ok, _ := e.st.GetKV(e.ctx, model.KVCommonWordBackfill); !ok || v != "1" {
		t.Errorf("kv %s = %q %v", model.KVCommonWordBackfill, v, ok)
	}
	// once only
	e.must(e.st.UpdateTerm(e.ctx, func() model.Term { x := e.get(a.ID); x.CommonWord = false; return x }()))
	if got, err := BackfillCommonWords(e.ctx, e.st); err != nil || got != 0 {
		t.Errorf("second backfill = %d %v, want 0", got, err)
	}
	if e.get(a.ID).CommonWord {
		t.Error("second backfill must not run")
	}
}

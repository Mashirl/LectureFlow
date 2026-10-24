// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// Stage-level tests of SPEC-v1.1 B.1 (model configuration snapshots, A4).

// setRoleModel changes the current setting of role (the legacy config here).
func (e *env) setRoleModel(role, modelName string) {
	r := e.cfg.LLM.Roles[role]
	r.Model = modelName
	e.cfg.LLM.Roles[role] = r
}

func requestModels(reqs []llmtest.Request) []string {
	var out []string
	for _, r := range reqs {
		out = append(out, r.Model)
	}
	return out
}

func allEqual(xs []string, want string) bool {
	if len(xs) == 0 {
		return false
	}
	for _, x := range xs {
		if x != want {
			return false
		}
	}
	return true
}

// TestRetryKeepsSnapshot: the settings moved to another model after launch;
// the run and its retry still send the snapshot's model (A4.1, A4.2).
func TestRetryKeepsSnapshot(t *testing.T) {
	e := newEnv(t)
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	snap := model.MarshalSnapshot(e.llmSnapshot(model.StageSummary, "snap-model-a"))
	e.setSnapshot(l.ID, model.StageSummary, snap)
	e.setRoleModel(config.RoleSummary, "settings-model-b")

	e.fake.FailNext(llmtest.RoleSummary, 1, llmtest.FailHTTP401)
	stg := e.stage(e.deps(nil), model.StageSummary)
	if r := e.run(stg, l.ID); r.err == nil || r.job.Status != model.StatusFailed {
		t.Fatalf("first run: %v %s", r.err, r.job.Status)
	}
	e.must(state.Retry(e.ctx, e.st, model.ScopeLecture, l.ID, model.StageSummary))
	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := requestModels(e.fake.Requests(llmtest.RoleSummary)); len(got) != 2 || !allEqual(got, "snap-model-a") {
		t.Errorf("request models %v, want the snapshot's model on the run and the retry", got)
	}
	// The retry keeps provider, model and params; the prompt version becomes
	// the current one (the run uses the current prompt: v2 G5.5, lead Gate 1).
	want, _ := model.ParseLLMSnapshot(snap)
	want.PromptVersion = model.PromptVersionSummary
	if r.job.Attempt != 1 || !model.SameSnapshot(model.StageSummary, r.job.ConfigSnapshot, model.MarshalSnapshot(want)) {
		t.Errorf("retry: attempt %d snapshot %s", r.job.Attempt, r.job.ConfigSnapshot)
	}
}

// TestIncrementalTranslateKeepsSnapshot: the incremental run after a review
// uses the snapshot of the lecture's latest translate job, not the current
// settings (A4.2: one model for the whole lecture).
func TestIncrementalTranslateKeepsSnapshot(t *testing.T) {
	e := newEnv(t)
	e.fixtureTerms()
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "A lecture about caches.")
	snap := model.MarshalSnapshot(e.llmSnapshot(model.StageTranslate, "snap-model-a"))
	e.setSnapshot(l.ID, model.StageTranslate, snap)
	stg := e.stage(e.deps(nil), model.StageTranslate)
	first := e.run(stg, l.ID)
	if first.err != nil {
		t.Fatal(first.err)
	}
	if got := requestModels(e.fake.Requests(llmtest.RoleTranslate)); !allEqual(got, "snap-model-a") {
		t.Fatalf("first run models %v", got)
	}

	e.setRoleModel(config.RoleTranslate, "settings-model-b")
	if _, err := state.RetranslateSentence(e.ctx, e.st, l.ID, 3); err != nil {
		t.Fatal(err)
	}
	started, err := state.StartIncrementalTranslate(e.ctx, e.st, l.ID)
	if err != nil || !started {
		t.Fatalf("incremental translate: %v %v", started, err)
	}
	e.fake.Reset()
	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.job.ID == first.job.ID || !state.IsIncremental(r.job.Stats.Note) {
		t.Fatalf("not an incremental job: %+v", r.job)
	}
	if got := requestModels(e.fake.Requests(llmtest.RoleTranslate)); !allEqual(got, "snap-model-a") {
		t.Errorf("incremental run models %v, want the latest translate snapshot's model", got)
	}
	if idxs := e.fake.TranslateIdxs(); !slices.Equal(idxs, []int{3}) {
		t.Errorf("incremental run translated %v, want [3]", idxs)
	}
}

// TestTranslateWindowFromSnapshot: x comes from the translate job's
// snapshot (lead ruling 4) — for the requests, the per-sentence zh_hash and
// the stage input_hash — and repair/translate record stats.glossary_version.
func TestTranslateWindowFromSnapshot(t *testing.T) {
	e := newEnv(t)
	e.fixtureTerms()
	for range 4 {
		if _, err := e.st.BumpGlossaryVersion(e.ctx, e.course.ID); err != nil {
			t.Fatal(err)
		}
	}
	l := e.lecture("L1", "m.mp4")
	e.insertSample(l.ID)
	e.setSummary(l.ID, "A lecture about caches.")
	s := e.llmSnapshot(model.StageTranslate, "")
	w := 1
	s.Params.Window = &w
	e.setSnapshot(l.ID, model.StageTranslate, model.MarshalSnapshot(s))
	if e.cfg.LLM.Roles[config.RoleTranslate].Window != 3 {
		t.Fatal("the settings window must differ from the snapshot's")
	}
	d := e.deps(nil)
	rr := e.run(e.stage(d, model.StageRepair), l.ID)
	if rr.err != nil {
		t.Fatal(rr.err)
	}
	stg := e.stage(d, model.StageTranslate)
	r := e.run(stg, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if rr.stats.GlossaryVersion == nil || *rr.stats.GlossaryVersion != 4 || r.stats.GlossaryVersion == nil || *r.stats.GlossaryVersion != 4 {
		t.Errorf("glossary_version repair %v translate %v, want 4", rr.stats.GlossaryVersion, r.stats.GlossaryVersion)
	}
	ld, err := d.loadLecture(e.ctx, l.ID)
	e.must(err)
	want := ctxbuild.TranslateStageHash(ld.snap, 1)
	if r.job.InputHash == nil || *r.job.InputHash != want || want == ctxbuild.TranslateStageHash(ld.snap, 3) {
		t.Errorf("input_hash %v, want the hash with the snapshot's x = 1", r.job.InputHash)
	}
	if h, _ := stg.InputHash(e.ctx, l.ID); h != want {
		t.Error("InputHash must use the snapshot's window")
	}
	hashes := ctxbuild.TranslateHashes(ld.snap, 1)
	for i, sn := range e.sentences(l.ID) {
		if sn.ZhHash == nil || *sn.ZhHash != hashes[i] {
			t.Errorf("sentence %d: zh_hash not computed with x = 1", i)
		}
	}
	for _, req := range e.fake.Requests(llmtest.RoleTranslate) {
		if req.TargetIdx == 5 {
			if want := ctxbuild.BuildTranslateRequest(ld.snap, 5, 1).User; req.User != want {
				t.Errorf("request for #5:\n%s\nwant (x = 1):\n%s", req.User, want)
			}
			break
		}
	}
	// review invalidation uses the same x (state.TranslateWindow reads the snapshot)
	if x, err := state.TranslateWindow(e.ctx, e.st, l.ID); err != nil || x != 1 {
		t.Errorf("state.TranslateWindow = %d %v", x, err)
	}
}

// TestSettingsPath: with Deps.Settings, the per-stage concurrency is the
// max_concurrency of the snapshot's provider and keep_original_media is the
// general setting (Cfg is not consulted).
func TestSettingsPath(t *testing.T) {
	e := newEnv(t)
	set, err := settings.Open(e.ctx, e.st, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	str := func(s string) *string { return &s }
	three := 3
	narrow, err := set.CreateProvider(e.ctx, settings.ProviderInput{
		Name: str("Narrow"), Type: str(model.ProviderOpenAI), BaseURL: str(e.fake.URL), APIKey: str(llmtest.TestKey), MaxConcurrency: &three,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := set.Provider("deepseek"); !ok || p.MaxConcurrency == three {
		t.Fatalf("imported provider %+v: its max_concurrency must differ", p)
	}

	t.Run("concurrency of the snapshot's provider", func(t *testing.T) {
		e.fixtureTerms()
		l := e.lecture("L1", "m.mp4")
		e.insertSample(l.ID)
		e.setSummary(l.ID, "A lecture about caches.")
		s := e.llmSnapshot(model.StageTranslate, "")
		s.ProviderID, s.ProviderName = narrow.ID, narrow.Name
		e.setSnapshot(l.ID, model.StageTranslate, model.MarshalSnapshot(s))

		var (
			mu       sync.Mutex
			inflight int
			peak     int
			targets  = map[string]bool{}
		)
		target := regexp.MustCompile(`\[TARGET #(\d+)\]`) // SPEC-v2 G5.3 label
		client := &scriptLLM{fn: func(ctx context.Context, n int, c llmCall) (string, error) {
			tg, _ := llm.TargetFrom(ctx)
			mu.Lock()
			inflight++
			peak = max(peak, inflight)
			targets[tg.ProviderID] = true
			mu.Unlock()
			// wait until three requests are in flight (or give up), so the
			// peak shows the limit rather than timing luck
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				k := inflight
				mu.Unlock()
				if k >= three || ctx.Err() != nil {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inflight--
			mu.Unlock()
			idx, _ := strconv.Atoi(target.FindStringSubmatch(c.User)[1])
			return fmt.Sprintf(`{"idx": %d, "zh": "缓存层次结构里的第 %d 句译文。"}`, idx, idx), nil
		}}
		d := e.deps(client)
		d.Settings = set
		if r := e.run(e.stage(d, model.StageTranslate), l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		if peak != three {
			t.Errorf("peak concurrency %d, want the snapshot provider's max_concurrency %d", peak, three)
		}
		if len(targets) != 1 || !targets[narrow.ID] {
			t.Errorf("requests carried targets %v, want only %s", targets, narrow.ID)
		}
		if got := d.concurrency(config.RoleGlossary, ""); got == three {
			t.Error("course stages must use the role's current provider")
		}
	})

	t.Run("keep_original_media from the general settings", func(t *testing.T) {
		e.useFakeFFmpeg("ok")
		for _, keep := range []bool{false, true} {
			e.cfg.Storage.KeepOriginalMedia = boolp(!keep) // the legacy field says the opposite
			g := set.General()
			g.KeepOriginalMedia = keep
			e.must(set.SetGeneral(e.ctx, g))
			l := e.lecture(fmt.Sprintf("keep=%v", keep), "")
			dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
			media := filepath.Join(dir, "upload.wav")
			e.must(os.WriteFile(media, []byte("RIFF media"), 0o644))
			e.must(e.st.SetLectureMedia(e.ctx, l.ID, media))
			d := e.deps(nil)
			d.Settings = set
			// an audio-only source: the original may go (lead ruling on
			// keep_original_media; the fake ffmpeg's input is not probeable)
			if r := e.run(&prepStage{d: d, media: &fakeMedia{probe: probeM4A}}, l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			if _, err := os.Stat(media); os.IsNotExist(err) == keep {
				t.Errorf("keep_original_media=%v: media exists=%v", keep, err == nil)
			}
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// chaptersStage is the auxiliary task chapters (SPEC-v2 P7, lead ruling 9
// of phase 2): chapters for the player from the summary and every sentence.
// Scope lecture, role chapters (current setting). Queued only by the user;
// the result is kv chapters:{lecture_id} (model.Chapters) with the hash of
// its input, which the API compares to say "可能已过时". A rejected answer is
// asked again once with the reason appended to the request (askJSONRetry,
// like title and filename_rule); a second rejection fails the task and
// leaves the kv as it was.
type chaptersStage struct{ apiStage }

func (s *chaptersStage) Name() string { return model.StageChapters }

func (s *chaptersStage) InputHash(context.Context, string) (string, error) { return "", nil }

type chaptersAnswer struct {
	Chapters []model.ChapterEntry `json:"chapters"`
}

// checkChapters applies the P7 constraints for a lecture of n sentences:
// 3–12 chapters, titles within the limits, start_idx strictly increasing,
// the first 0, all in [0, n).
func checkChapters(a *chaptersAnswer, n int) error {
	cs := a.Chapters
	if len(cs) < ctxbuild.ChaptersMin || len(cs) > ctxbuild.ChaptersMax {
		return fmt.Errorf("%d chapters (want %d–%d)", len(cs), ctxbuild.ChaptersMin, ctxbuild.ChaptersMax)
	}
	for i := range cs {
		c := &cs[i]
		c.TitleEn, c.TitleZh = oneLineTrim(c.TitleEn), oneLineTrim(c.TitleZh)
		switch {
		case i == 0 && c.StartIdx != 0:
			return fmt.Errorf("the first chapter starts at %d, not 0", c.StartIdx)
		case i > 0 && c.StartIdx <= cs[i-1].StartIdx:
			return fmt.Errorf("start_idx %d after %d is not increasing", c.StartIdx, cs[i-1].StartIdx)
		case c.StartIdx < 0 || c.StartIdx >= n:
			return fmt.Errorf("start_idx %d is outside the transcript (0–%d)", c.StartIdx, n-1)
		case c.TitleEn == "" || c.TitleZh == "":
			return fmt.Errorf("chapter %d has an empty title", i+1)
		case ctxbuild.EnglishWords(c.TitleEn) > ctxbuild.ChapterTitleWords:
			return fmt.Errorf("title_en %q has more than %d words", c.TitleEn, ctxbuild.ChapterTitleWords)
		case ctxbuild.ZhLen(c.TitleZh) > ctxbuild.ChapterTitleZhLen:
			return fmt.Errorf("title_zh %q has more than %d characters", c.TitleZh, ctxbuild.ChapterTitleZhLen)
		}
	}
	return nil
}

func (s *chaptersStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	summary, ss, err := d.chaptersInput(ctx, lectureID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(summary) == "" {
		return model.Errorf("chapters.no_summary", nil, errors.New("the lecture has no summary yet"))
	}
	if len(ss) < ctxbuild.ChaptersMin {
		return model.Errorf("chapters.too_short", map[string]any{"sentences": len(ss)},
			fmt.Errorf("%d sentences cannot make %d chapters", len(ss), ctxbuild.ChaptersMin))
	}
	req := ctxbuild.ChaptersRequest(summary, ss)
	progress(0.1)
	a, unusable, err := askJSONRetry(ctx, d.LLM, s.role, req.System, req.User,
		func(a *chaptersAnswer) error { return checkChapters(a, len(ss)) })
	if err != nil {
		if unusable {
			return model.Errorf("chapters.invalid_output", map[string]any{"attempts": auxAttempts}, err)
		}
		return err
	}
	providerName, modelName := d.roleModel(s.role)
	rec := model.Chapters{
		Chapters:    a.Chapters,
		InputHash:   ctxbuild.ChaptersInputHash(summary, ss),
		GeneratedAt: d.now().UTC().Format(time.RFC3339),
		Provider:    providerName,
		Model:       modelName,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := d.Store.SetKV(context.WithoutCancel(ctx), model.KVChaptersPrefix+lectureID, string(b)); err != nil {
		return err
	}
	d.events().Changed("chapters", lectureID)
	progress(1)
	return nil
}

// chaptersInput reads the summary ("" when none) and the sentences in one
// transaction, so the stored hash describes exactly what the model saw.
func (d Deps) chaptersInput(ctx context.Context, lectureID string) (string, []ctxbuild.Sentence, error) {
	var (
		summary string
		ss      []ctxbuild.Sentence
	)
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		if _, err := tx.GetLecture(ctx, lectureID); err != nil {
			return err
		}
		sum, err := tx.GetSummary(ctx, lectureID)
		switch {
		case err == nil:
			summary = sum.Text
		case errors.Is(err, db.ErrNotFound):
			summary = ""
		default:
			return err
		}
		rows, err := tx.ListSentences(ctx, lectureID, 0, 0)
		if err != nil {
			return err
		}
		ss = ctxbuild.FromModel(rows)
		return nil
	})
	return summary, ss, err
}

// ChaptersInputHash is the current input hash of the lecture's chapters
// (P7): SHA-256 of the summary and every sentence.text. The API compares it
// with model.Chapters.InputHash; a difference means "章节可能已过时".
func ChaptersInputHash(ctx context.Context, d Deps, lectureID string) (string, error) {
	summary, ss, err := d.chaptersInput(ctx, lectureID)
	if err != nil {
		return "", err
	}
	return ctxbuild.ChaptersInputHash(summary, ss), nil
}

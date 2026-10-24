// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"strings"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/model"
)

// summaryStage writes the lecture summary (SPEC §6.3, §13.3).
type summaryStage struct{ apiStage }

func (s *summaryStage) Name() string { return model.StageSummary }

// InputHash = all asr_text + slides text (SPEC §4.4).
func (s *summaryStage) InputHash(ctx context.Context, lectureID string) (string, error) {
	ld, err := s.d.loadLecture(ctx, lectureID)
	if err != nil {
		return "", err
	}
	return ctxbuild.SummaryInputHash(ld.snap), nil
}

func (s *summaryStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	ctx, _, _, err := s.llmTarget(ctx, lectureID, model.StageSummary)
	if err != nil {
		return err
	}
	ld, err := d.loadLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	if len(ld.sentences) == 0 {
		return model.Errorf(CodeSummaryNoSentences, nil, errors.New("讲座没有句子（asr 尚未产出结果），无法生成摘要"))
	}
	req := ctxbuild.SummaryRequest(ld.snap)
	progress(0.05)
	res, err := d.LLM.Complete(ctx, config.RoleSummary, req.System, req.User, false)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(res.Text)
	if text == "" {
		return model.Errorf(CodeSummaryEmpty, nil, errors.New("模型返回了空摘要"))
	}
	if err := d.Store.UpsertSummary(context.WithoutCancel(ctx), model.Summary{
		LectureID: lectureID, Text: text, InputHash: ctxbuild.SummaryInputHash(ld.snap),
	}); err != nil {
		return err
	}
	d.events().Changed("lecture", lectureID)
	progress(1)
	return nil
}

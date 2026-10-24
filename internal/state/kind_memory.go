// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package state

import (
	"context"
	"strings"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// The title task's kind judgement survives a stronger source (lecture kinds
// review, item 1). The order is user > filename > llm > none (lead ruling
// 1.2) and lecture.kind holds one value, so a file-name or user kind used
// to replace the model's judgement for good: when the rule later gave no
// kind, or the user chose the automatic kind again, the lecture fell to
// none until the next title task — which never comes once the summary is
// done. The judgement is now remembered in kv and is what the lecture falls
// back to. No DDL change, no extra request, deterministic.

// KVKindLLMPrefix: kind_llm:{lecture_id} → the latest kind id the title
// task judged for the lecture (written whether or not it became the
// lecture's kind) or the llm kind a stronger source replaced; "" = none
// (cleared when the lecture is deleted).
const KVKindLLMPrefix = "kind_llm:"

// RememberLLMKind records kind (a catalog id) as the title task's latest
// judgement for the lecture. The title task calls it when a stronger
// source holds the lecture (the judgement is not written to the lecture).
// An empty kind changes nothing.
func RememberLLMKind(ctx context.Context, st db.Store, lectureID, kind string) error {
	if strings.TrimSpace(kind) == "" {
		return nil
	}
	return st.SetKV(ctx, KVKindLLMPrefix+lectureID, kind)
}

// RememberedLLMKind is the model's judgement for lecture l: the kv record,
// else l's own kind while its source is llm (a kind written before the
// record existed); "" when there is none. The id may have left the catalog
// since (FallbackKind checks).
func RememberedLLMKind(ctx context.Context, st db.Store, l model.Lecture) (string, error) {
	v, ok, err := st.GetKV(ctx, KVKindLLMPrefix+l.ID)
	if err != nil {
		return "", err
	}
	if v = strings.TrimSpace(v); ok && v != "" {
		return v, nil
	}
	if l.KindSource == model.KindSourceLLM {
		return l.Kind, nil
	}
	return "", nil
}

// LLMFallback is the automatic kind of a lecture when the file name gives
// none (llm is RememberedLLMKind): the remembered judgement with source llm
// when it is a kind of the catalog, else not decided ("", none).
func LLMFallback(llm string, kinds []model.LectureKind) (kind, source string) {
	if llm != "" {
		for _, k := range kinds {
			if k.ID == llm {
				return llm, model.KindSourceLLM
			}
		}
	}
	return "", model.KindSourceNone
}

// FallbackKind is LLMFallback for lecture l read from st.
func FallbackKind(ctx context.Context, st db.Store, l model.Lecture, kinds []model.LectureKind) (kind, source string, err error) {
	llm, err := RememberedLLMKind(ctx, st, l)
	if err != nil {
		return "", "", err
	}
	kind, source = LLMFallback(llm, kinds)
	return kind, source, nil
}

// WriteLectureKind sets lecture l's kind and source (db SetLectureKind) and
// keeps the model's judgement: an llm kind replaced by another source is
// remembered first, and an llm kind written is the latest judgement. l is
// the lecture as stored before the write (read in the same transaction).
func WriteLectureKind(ctx context.Context, st db.Store, l model.Lecture, kind, source string) error {
	switch {
	case source == model.KindSourceLLM:
		if err := RememberLLMKind(ctx, st, l.ID, kind); err != nil {
			return err
		}
	case l.KindSource == model.KindSourceLLM && l.Kind != "":
		if err := RememberLLMKind(ctx, st, l.ID, l.Kind); err != nil {
			return err
		}
	}
	return st.SetLectureKind(ctx, l.ID, kind, source)
}

// ForgetLectureKind clears the remembered judgement of a deleted lecture.
func ForgetLectureKind(ctx context.Context, st db.Store, lectureID string) error {
	return st.SetKV(ctx, KVKindLLMPrefix+lectureID, "")
}

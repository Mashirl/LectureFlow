// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"errors"
	"time"

	"lectureflow/internal/calendar"
)

// Built-in fallback of the filename_rule task (pipeline-builder, v3
// acceptance item 8; see NOTES.md). P13.3 lets the model write the rule; it
// is still asked first, with its retry. Only when both answers are rejected
// (or unusable) does the task try this one pattern: the default name of a
// Panopto download, the lecture-capture system the user downloads from
// (SPEC §0.1), "{session} {weekday} {Mon} {DD} {HH-MM-SS} {zone} {YYYY}",
// e.g. "CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4". The model has
// been seen to miss the weekday in such names and so leave every lecture
// without a recording time.
//
// The fallback is an ordinary regular expression: it goes through the same
// deterministic check (calendar.ValidateRule) and, when it passes, the same
// first-exception / proposal flow as a model's rule (source llm: it is
// regenerated like one, and the user can edit or ignore it). It captures no
// title (the session name is often a code, not a title) and no kind (the
// title task judges the kind instead).
//
// The zone abbreviation is not captured: the clock time in the name is
// taken as the course's local time, as every rule's times are (P13.3.4;
// NOTES.md: dates are compared with the upload by day for this reason).
const panoptoFallbackRegex = `(?:^|[ _])(?i:mon|tue|wed|thu|fri|sat|sun)[a-z]*\.?[ _]+` +
	`(?P<month>(?i:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*)\.?[ _]+` +
	`(?P<day>\d{1,2})[ _]+(?P<hour>\d{1,2})[-:.h](?P<minute>\d{2})(?:[-:.]\d{2})?[ _]+` +
	`(?:[A-Za-z]{2,5}(?:[+-]\d{1,2}(?::?\d{2})?)?[ _]+)?(?P<year>\d{4})(?:\D|$)`

// panoptoFallbackExplanation is the rule's one-sentence explanation in the
// preview (P13.3.3), in English like the model's.
const panoptoFallbackExplanation = "Built-in pattern for Panopto download names (weekday, month, day, time, time zone, year), used because the model's pattern was rejected."

// panoptoNoModelExplanation replaces panoptoFallbackExplanation when the
// organizer role has no model (3.1.0 "只转写": the model is not asked, the
// built-in pattern is the only one tried).
const panoptoNoModelExplanation = "Built-in pattern for Panopto download names (weekday, month, day, time, time zone, year), used because no model is configured for this task."

// errNoFilenameModel: the organizer role has no model, so only the
// built-in pattern was tried (filenameRuleStage.Run).
var errNoFilenameModel = errors.New("no model configured for the organizer role; the built-in Panopto pattern does not fit these file names")

// roleConfigured reports whether role has a provider and a model now: the
// settings' RoleTarget (what the LLM client resolves), or in legacy mode
// (no settings service) the config's role provider.
func (d Deps) roleConfigured(role string) bool {
	if d.Settings != nil {
		_, err := d.Settings.RoleTarget(role)
		return err == nil
	}
	return d.provider(role) != ""
}

// fallbackRule tries the built-in pattern on samples. ok is false when it
// fails calendar.ValidateRule; otherwise it returns the answer and the
// preview rows as the model's accepted answer would.
func fallbackRule(samples []calendar.Sample, termStart *time.Time) (a filenameRuleAnswer, parsed []calendar.Parsed, matched int, ok bool) {
	p, m, valid, _ := calendar.ValidateRule(calendar.FilenameRule{Regex: panoptoFallbackRegex}, samples, termStart)
	if !valid {
		return filenameRuleAnswer{}, nil, 0, false
	}
	return filenameRuleAnswer{Regex: panoptoFallbackRegex, Explanation: panoptoFallbackExplanation}, p, m, true
}

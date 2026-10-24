// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

// Per-role "深度思考" (.claude/thinking-brief.md). The default is off
// (SPEC-v2 appendix D2: every role runs without thinking unless the user
// turns it on). Which providers support it is settings.SupportsThinking;
// the request parameters are internal/llm's.

// Thinking levels of RoleParams.Thinking.
const (
	ThinkingOff  = "off"
	ThinkingLow  = "low"
	ThinkingHigh = "high"
	ThinkingMax  = "max"
)

// ThinkingLevels are the valid values of RoleParams.Thinking, in the order
// of the settings page.
var ThinkingLevels = []string{ThinkingOff, ThinkingLow, ThinkingHigh, ThinkingMax}

// ValidThinking reports whether s is one of ThinkingLevels.
func ValidThinking(s string) bool {
	for _, l := range ThinkingLevels {
		if s == l {
			return true
		}
	}
	return false
}

// ThinkingOf returns the thinking level of p as stored: ThinkingOff when
// unset or empty, otherwise the value (which may be invalid in a database
// row edited by hand; ThinkingLevel normalises those).
func ThinkingOf(p RoleParams) string {
	if p.Thinking == nil || *p.Thinking == "" {
		return ThinkingOff
	}
	return *p.Thinking
}

// ThinkingLevel returns the effective thinking level of p: ThinkingOf when
// it is valid, ThinkingOff otherwise.
func ThinkingLevel(p RoleParams) string {
	if l := ThinkingOf(p); ValidThinking(l) {
		return l
	}
	return ThinkingOff
}

// ThinkingOn reports whether p turns thinking on (a valid level other than
// off).
func ThinkingOn(p RoleParams) bool { return ThinkingLevel(p) != ThinkingOff }

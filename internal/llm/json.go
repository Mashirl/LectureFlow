// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrBadJSON wraps every ParseJSON failure. A stage counts it as one failed
// attempt of its business retry (repair: 2 per chunk, translate: 2 per
// sentence; SPEC §12).
var ErrBadJSON = errors.New("LLM output is not valid JSON")

// ParseJSON decodes model output into v (SPEC §12): first the text as is;
// if that fails, with ``` code fences removed, the substring from the first
// "{" to the last "}".
func ParseJSON(text string, v any) error {
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), v); err == nil {
		return nil
	}
	s := stripFences(text)
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return fmt.Errorf("%w: no JSON object in %q", ErrBadJSON, short(text))
	}
	if err := json.Unmarshal([]byte(s[i:j+1]), v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	return nil
}

// stripFences drops every line that starts with ``` (e.g. "```json", "```").
func stripFences(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func short(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

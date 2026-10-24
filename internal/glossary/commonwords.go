// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package glossary

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"lectureflow/internal/model"
)

// commonWordsTxt is the G2 word list, written by the project (appendix D4;
// see the header of the file and NOTES.md).
//
//go:embed commonwords.txt
var commonWordsTxt string

// commonWords is the parsed list: lower-case words, comment lines skipped.
var commonWords = sync.OnceValue(func() map[string]struct{} {
	m := make(map[string]struct{}, 5000)
	for _, line := range strings.Split(commonWordsTxt, "\n") {
		w := strings.TrimSpace(line)
		if w == "" || strings.HasPrefix(w, "#") {
			continue
		}
		m[strings.ToLower(w)] = struct{}{}
	}
	return m
})

// isCommon reports whether a lower-case word, or its singular by one of the
// plural rules, is in the list.
func isCommon(lower string) bool {
	words := commonWords()
	if _, ok := words[lower]; ok {
		return true
	}
	for _, s := range singulars(lower) {
		if _, ok := words[s]; ok {
			return true
		}
	}
	return false
}

// singulars returns the candidate singular forms of a lower-case word or
// phrase (its last word) under the plural rules of G2: a trailing "s" or
// "es" removed, and also "ies" → "y" (NOTES.md, pipeline-A). Candidates
// shorter than 2 characters are not returned.
func singulars(lower string) []string {
	var out []string
	add := func(s string) {
		if len([]rune(s)) >= 2 {
			out = append(out, s)
		}
	}
	if strings.HasSuffix(lower, "ies") {
		add(strings.TrimSuffix(lower, "ies") + "y")
	}
	if strings.HasSuffix(lower, "es") {
		add(strings.TrimSuffix(lower, "es"))
	}
	if strings.HasSuffix(lower, "s") && !strings.HasSuffix(lower, "ss") {
		add(strings.TrimSuffix(lower, "s"))
	}
	return out
}

// pluralSuffix returns the suffix by which plural (a term's en) extends the
// singular en_key: "s", "es" or "ies" (for a singular ending in "y"). The
// suffix must be written in lower case in plural, so that an acronym such as
// "DES" is not taken for the plural of "DE" while "MACs" is the plural of "MAC".
func pluralSuffix(pluralKey, pluralEn, singularKey string) (string, bool) {
	en := strings.Join(strings.Fields(pluralEn), " ")
	for _, suf := range []string{"s", "es"} {
		if pluralKey == singularKey+suf && strings.HasSuffix(en, suf) {
			return suf, true
		}
	}
	if strings.HasSuffix(singularKey, "y") && pluralKey == strings.TrimSuffix(singularKey, "y")+"ies" && strings.HasSuffix(en, "ies") {
		return "ies", true
	}
	return "", false
}

// singularOf finds the term t is the plural of (G2 fallback: "en_key 只差复数形式").
func singularOf(t model.Term, byKey map[string]*model.Term) (*model.Term, bool) {
	key := termKey(t)
	for _, s := range singulars(key) {
		other, ok := byKey[s]
		if !ok || other.ID == t.ID {
			continue
		}
		if _, ok := pluralSuffix(key, t.En, s); ok {
			return other, true
		}
	}
	return nil, false
}

// termKey is the term's en_key (computed when empty).
func termKey(t model.Term) string {
	if t.EnKey != "" {
		return t.EnKey
	}
	return strings.ToLower(strings.Join(strings.Fields(t.En), " "))
}

// allCapitals reports whether every letter of s is a capital letter (and s
// has a letter): acronyms such as AES and IV are never common words.
func allCapitals(s string) bool {
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
			if !unicode.IsUpper(r) {
				return false
			}
		}
	}
	return letters > 0
}

// Notes written by the fallback rule (janitor_note and report reason).
const noteCommonWord = "Common English word (automatic rule)."

func notePlural(singularEn string) string {
	return fmt.Sprintf("Plural of '%s' (automatic rule).", strings.Join(strings.Fields(singularEn), " "))
}

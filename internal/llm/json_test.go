// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"errors"
	"testing"
)

func TestParseJSON(t *testing.T) {
	type out struct {
		Idx int    `json:"idx"`
		Zh  string `json:"zh"`
	}
	ok := []struct{ name, text string }{
		{"plain", `{"idx": 3, "zh": "好"}`},
		{"surrounding whitespace", "\n  {\"idx\": 3, \"zh\": \"好\"}  \n"},
		{"json fence", "```json\n{\"idx\": 3, \"zh\": \"好\"}\n```"},
		{"bare fence", "```\n{\"idx\": 3, \"zh\": \"好\"}\n```"},
		{"prose around", "Here is the result:\n{\"idx\": 3, \"zh\": \"好\"}\nHope this helps."},
		{"fence with prose", "Sure!\n```json\n{\"idx\": 3, \"zh\": \"好\"}\n```\nDone."},
		{"braces inside strings", "x {\"idx\": 3, \"zh\": \"好\"} y"},
	}
	for _, c := range ok {
		var v out
		if err := ParseJSON(c.text, &v); err != nil || v != (out{3, "好"}) {
			t.Errorf("%s: %+v %v", c.name, v, err)
		}
	}
	bad := []struct{ name, text string }{
		{"empty", ""},
		{"no object", "I cannot do that."},
		{"broken object", "{\"idx\": 3, \"zh\": }"},
		{"reversed braces", "} nope {"},
		{"truncated", "```json\n{\"idx\": 3, \"zh\": \"好"},
	}
	for _, c := range bad {
		var v out
		err := ParseJSON(c.text, &v)
		if err == nil || !errors.Is(err, ErrBadJSON) {
			t.Errorf("%s: err %v, want ErrBadJSON", c.name, err)
		}
	}
	// nested objects: first "{" to last "}"
	var nested struct {
		Patches []struct {
			Idx int `json:"idx"`
		} `json:"patches"`
	}
	if err := ParseJSON("result: {\"patches\": [{\"idx\": 1}, {\"idx\": 2}]} end", &nested); err != nil || len(nested.Patches) != 2 {
		t.Errorf("nested: %+v %v", nested, err)
	}
}

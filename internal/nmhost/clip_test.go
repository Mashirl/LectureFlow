// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// clip cuts at the start of a character, so that a cut type or id comes
// back from JSON as it was sent (FuzzAnswerNotRunning).
func TestClipKeepsUTF8(t *testing.T) {
	s := strings.Repeat("0", 38) + "�" // 41 bytes
	got := clip(s, 40)
	if got != strings.Repeat("0", 38) || !utf8.ValidString(got) {
		t.Fatalf("clip %q", got)
	}
	env := envelopeOf([]byte(`{"type":"` + strings.Repeat("0", 38) + "\xff" + `","id":"` + strings.Repeat("长", 40) + `"}`))
	b, _ := json.Marshal(env)
	var back struct{ Type, ID string }
	json.Unmarshal(b, &back)
	if back.Type != env.Type || back.ID != env.ID || len(env.Type) > 40 || len(env.ID) > 100 {
		t.Fatalf("%q %q → %s", env.Type, env.ID, b)
	}
}

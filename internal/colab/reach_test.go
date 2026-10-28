// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"testing"
)

// Reach refreshes the token and reads ccu-info once: no assignment, no
// other request, nothing cached into Status.
func TestReachIsReadOnly(t *testing.T) {
	e := newEnv(t, "reach.json").withCredential(t).startup(t)
	ctx := context.Background()
	if err := e.b.Reach(ctx); err != nil {
		t.Fatalf("Reach: %v", err)
	}
	if st := e.b.Status(ctx); st.State != StateNone || st.BalanceCU != nil {
		t.Errorf("status after Reach %+v", st)
	}
	e.rs.finish() // exactly the token refresh and ccu-info
	e.checkClean(t)
}

func TestReachErrors(t *testing.T) {
	ctx := context.Background()
	// No credential: no request at all.
	e := newEnv(t, "reach.json").startup(t)
	if err := e.b.Reach(ctx); Code(err) != CodeAuthMissing {
		t.Errorf("without a credential: %v", err)
	}
	// Colab does not answer.
	e = newEnv(t, "reach_unavailable.json").withCredential(t).startup(t)
	err := e.b.Reach(ctx)
	if Code(err) == "" {
		t.Fatalf("503: %v", err)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

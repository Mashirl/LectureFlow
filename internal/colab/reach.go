// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import "context"

// Reach is the diagnostics' reachability check (SPEC-ext 诊断页 colab_reach,
// ext-step1): one read-only request, GET /tun/m/ccu-info (the account's
// compute units), with the stored credential, refreshing Google's access
// token when needed. No runtime is assigned and nothing is spent. The
// credential is checked as for any other use (none: colab.auth_missing; its
// lifecycle ended: deleted, colab.auth_expired). The answer is not cached,
// so Status is the same before and after a check. Errors are
// *model.CodedError and carry no credential.
func (b *Backend) Reach(ctx context.Context) error {
	_, err := b.tun.ccuInfo(ctx)
	return b.secrets.scrubErr(err)
}

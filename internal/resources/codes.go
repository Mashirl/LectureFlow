// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

// Codes of a failed balance query (Provider.BalanceErrorInfo, v2 P14.3);
// the Chinese texts are in testdata/api/error_codes_backend.json.
const (
	CodeBalanceNoAPIKey    = "balance.no_api_key"   // the provider has no key: no query
	CodeBalanceBadBaseURL  = "balance.bad_base_url" //
	CodeBalanceNetwork     = "balance.network"      // the request or reading the answer failed
	CodeBalanceHTTP        = "balance.http"         // status
	CodeBalanceBadResponse = "balance.bad_response" // not the expected JSON
	CodeBalanceMissing     = "balance.missing"      // JSON without a balance
)

// ErrorCodes lists the codes this package defines.
var ErrorCodes = []string{CodeBalanceNoAPIKey, CodeBalanceBadBaseURL, CodeBalanceNetwork, CodeBalanceHTTP,
	CodeBalanceBadResponse, CodeBalanceMissing}

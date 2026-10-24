// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/model"
)

// BalanceTTL is how long a fetched balance is shown before it is refreshed
// (A10.2: every 5 minutes while a front end is connected).
const BalanceTTL = 5 * time.Minute

// balanceBodyLimit caps the response body read from the balance API.
const balanceBodyLimit = 64 << 10

// balanceEntry is the in-memory cache of one provider's balance (A10.3:
// never written to the database).
type balanceEntry struct {
	fp       string // fingerprint of base_url + key; a change invalidates the entry
	balance  *string
	currency *string
	err      string
	errInfo  *model.ErrorInfo // coded err (v2 P14.3)
	at       time.Time        // when the last query finished; zero = never
	fetching bool
	dirty    bool // RefreshBalance asked for a new query
}

// balanceResult is the outcome of one query.
type balanceResult struct {
	balance  string
	currency string
	err      error
	errInfo  *model.ErrorInfo // coded err, Detail = err's text
}

// fingerprint identifies the provider settings a balance belongs to without
// keeping the key itself in the cache.
func fingerprint(p model.Provider) string {
	h := sha256.Sum256([]byte(p.BalanceAPI + "\x00" + p.BaseURL + "\x00" + p.APIKey))
	return hex.EncodeToString(h[:])
}

// balanceURL is {base_url}/user/balance (A10.2), tolerating a trailing "/".
func balanceURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/user/balance"
}

// deepSeekBalance is the response of GET /user/balance.
type deepSeekBalance struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency     string     `json:"currency"`
		TotalBalance flexString `json:"total_balance"`
	} `json:"balance_infos"`
}

// flexString accepts a JSON string or number (DeepSeek documents strings).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// fetchDeepSeekBalance queries the DeepSeek balance API with Bearer
// authentication. Errors are readable and never contain the key.
func fetchDeepSeekBalance(ctx context.Context, client *http.Client, p model.Provider) balanceResult {
	bal, cur, err := doFetchDeepSeekBalance(ctx, client, p)
	if err != nil {
		var ce *model.CodedError
		text := err.Error()
		info := &model.ErrorInfo{Code: CodeBalanceNetwork}
		if errors.As(err, &ce) {
			text, info.Code, info.Params = ce.Err.Error(), ce.Info.Code, ce.Info.Params
		}
		text = redact(text, p.APIKey)
		info.Detail = text
		return balanceResult{err: errors.New(text), errInfo: info}
	}
	return balanceResult{balance: bal, currency: cur}
}

func doFetchDeepSeekBalance(ctx context.Context, client *http.Client, p model.Provider) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, balanceURL(p.BaseURL), nil)
	if err != nil {
		return "", "", model.Errorf(CodeBalanceBadBaseURL, nil, fmt.Errorf("余额查询失败：base_url 无效：%v", err))
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", model.Errorf(CodeBalanceNetwork, nil, fmt.Errorf("余额查询失败：%v", err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, balanceBodyLimit))
	if err != nil {
		return "", "", model.Errorf(CodeBalanceNetwork, nil, fmt.Errorf("余额查询失败：读取响应：%v", err))
	}
	if resp.StatusCode/100 != 2 {
		msg := fmt.Sprintf("余额查询失败：HTTP %d", resp.StatusCode)
		if s := strings.TrimSpace(string(body)); s != "" {
			msg += "：" + truncate(s, 200)
		}
		return "", "", model.Errorf(CodeBalanceHTTP, map[string]any{"status": resp.StatusCode}, errors.New(msg))
	}
	var r deepSeekBalance
	if err := json.Unmarshal(body, &r); err != nil {
		return "", "", model.Errorf(CodeBalanceBadResponse, nil, fmt.Errorf("余额查询失败：无法解析响应 %s", quoteShort(string(body))))
	}
	if len(r.BalanceInfos) == 0 {
		return "", "", model.Errorf(CodeBalanceMissing, nil, errors.New("余额查询失败：响应中没有余额信息"))
	}
	// DeepSeek returns one entry per currency (e.g. CNY and USD) in no fixed
	// order; show the first entry with a non-zero balance, else the first
	// entry (lead fix after the real-data check: "$0.00" was shown while the
	// CNY balance was 390.44).
	pick := -1
	for i, b := range r.BalanceInfos {
		if b.TotalBalance == "" {
			continue
		}
		if pick < 0 {
			pick = i
		}
		if v, err := strconv.ParseFloat(string(b.TotalBalance), 64); err == nil && v != 0 {
			pick = i
			break
		}
	}
	if pick < 0 {
		return "", "", model.Errorf(CodeBalanceMissing, nil, errors.New("余额查询失败：响应中没有 total_balance"))
	}
	b := r.BalanceInfos[pick]
	return string(b.TotalBalance), b.Currency, nil
}

// redact removes the key from a message (A5.2: a key never reaches the UI or
// the logs, even if a server echoes it back).
func redact(msg, key string) string {
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, model.KeyHint(key))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

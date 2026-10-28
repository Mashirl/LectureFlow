// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// rpcClient speaks aria2's JSON-RPC over HTTP on 127.0.0.1. Every call
// carries the secret ("token:..."); the secret and the request bodies
// (which hold cookies) are never logged.
type rpcClient struct {
	url   string
	token string
	hc    *http.Client
	seq   atomic.Int64
}

func newRPC(port int, secret string, hc *http.Client) *rpcClient {
	return &rpcClient{url: "http://127.0.0.1:" + strconv.Itoa(port) + "/jsonrpc", token: "token:" + secret, hc: hc}
}

// rpcError is an error answer of aria2 (its message never holds a URL of
// ours: the messages are about the call itself, e.g. "Unauthorized").
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("aria2 rpc error %d: %s", e.Code, e.Message) }

// errRPCTransport marks a failure to reach aria2 at all (it exited, or has
// not started listening yet).
var errRPCTransport = errors.New("aria2 rpc unreachable")

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// call invokes method with the token prepended to params and decodes the
// result into out (may be nil).
func (c *rpcClient) call(ctx context.Context, method string, params []any, out any) error {
	return c.raw(ctx, method, append([]any{c.token}, params...), out)
}

func (c *rpcClient) raw(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: strconv.FormatInt(c.seq.Add(1), 10), Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errPortTaken) {
			// the connection check refused the port: nothing was sent
			return fmt.Errorf("%w: %s: %w", errRPCTransport, method, errPortTaken)
		}
		return fmt.Errorf("%w: %s", errRPCTransport, method)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%w: %s: read: %v", errRPCTransport, method, err)
	}
	var r rpcResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("aria2 rpc %s: bad answer (HTTP %d)", method, res.StatusCode)
	}
	if r.Error != nil {
		return r.Error
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("aria2 rpc %s: decode result: %w", method, err)
		}
	}
	return nil
}

// ariaStatus is the part of aria2.tellStatus the queue reads. aria2 sends
// numbers as strings.
type ariaStatus struct {
	GID             string `json:"gid"`
	Status          string `json:"status"` // active | waiting | paused | error | complete | removed
	TotalLength     string `json:"totalLength"`
	CompletedLength string `json:"completedLength"`
	DownloadSpeed   string `json:"downloadSpeed"`
	ErrorCode       string `json:"errorCode"`
	ErrorMessage    string `json:"errorMessage"` // may contain a URL: never log it as is
	// FollowedBy lists the downloads aria2c started from this one's
	// content (a Metalink or a .torrent). The options forbid it; one is
	// still taken for a failure (engine.go).
	FollowedBy []string `json:"followedBy"`
}

var statusKeys = []string{"gid", "status", "totalLength", "completedLength", "downloadSpeed", "errorCode", "errorMessage", "followedBy"}

func num(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (c *rpcClient) version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := c.call(ctx, "aria2.getVersion", nil, &v)
	return v.Version, err
}

// addURIDetached is addURI that finishes even when ctx ends meanwhile (up
// to 10 s): an add whose answer were lost to the cancellation would leave
// a download running in aria2c that nobody knows to stop, still writing
// the partial file (a later resume then asks for the bytes after its end).
// The caller stops the download it gets when ctx has ended.
func (c *rpcClient) addURIDetached(ctx context.Context, uri string, opts map[string]any) (string, error) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return c.addURI(actx, uri, opts)
}

// addURI adds one download and returns its gid.
func (c *rpcClient) addURI(ctx context.Context, uri string, opts map[string]any) (string, error) {
	var gid string
	err := c.call(ctx, "aria2.addUri", []any{[]string{uri}, opts}, &gid)
	return gid, err
}

// tellStatuses asks for several downloads at once (system.multicall). A
// gid aria2 no longer knows comes back with Status "" (the caller treats it
// as removed).
func (c *rpcClient) tellStatuses(ctx context.Context, gids []string) ([]ariaStatus, error) {
	if len(gids) == 0 {
		return nil, nil
	}
	calls := make([]map[string]any, len(gids))
	for i, g := range gids {
		calls[i] = map[string]any{"methodName": "aria2.tellStatus", "params": []any{c.token, g, statusKeys}}
	}
	var raw []json.RawMessage
	if err := c.raw(ctx, "system.multicall", []any{calls}, &raw); err != nil {
		return nil, err
	}
	if len(raw) != len(gids) {
		return nil, fmt.Errorf("aria2 rpc multicall: %d answers for %d calls", len(raw), len(gids))
	}
	out := make([]ariaStatus, len(gids))
	for i, r := range raw {
		var one []ariaStatus
		if json.Unmarshal(r, &one) == nil && len(one) == 1 {
			out[i] = one[0]
			continue
		}
		// a fault ({faultCode, faultString}): the gid is unknown
		out[i] = ariaStatus{GID: gids[i]}
	}
	return out, nil
}

// forceRemove stops a download at once; errors (an unknown or finished gid)
// are ignored by the callers.
func (c *rpcClient) forceRemove(ctx context.Context, gid string) error {
	return c.call(ctx, "aria2.forceRemove", []any{gid}, nil)
}

func (c *rpcClient) removeResult(ctx context.Context, gid string) error {
	return c.call(ctx, "aria2.removeDownloadResult", []any{gid}, nil)
}

func (c *rpcClient) shutdown(ctx context.Context, force bool) error {
	m := "aria2.shutdown"
	if force {
		m = "aria2.forceShutdown"
	}
	return c.call(ctx, m, nil, nil)
}

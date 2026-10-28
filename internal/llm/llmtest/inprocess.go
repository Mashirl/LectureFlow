// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest

import (
	"net/http"
	"net/http/httptest"
)

// Transport returns an http.RoundTripper that answers every request with
// the server's handler in the calling goroutine, without a connection
// (perf-llm). It is for tests that run in a testing/synctest bubble: I/O on
// a real connection is not durably blocking, so the bubble's clock could not
// advance while a request waits for its answer, whereas in-process the answer
// delay of SetDelay runs on the bubble's fake clock. Use it as the Transport
// of the http.Client given to the llm client; the request's host is ignored.
// Everything else is the HTTP server's behaviour: injected failures, delays,
// the cache and concurrency models, the recorded requests.
func (s *Server) Transport() http.RoundTripper { return inProcess{s: s} }

type inProcess struct{ s *Server }

func (p inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close() // a RoundTripper always closes the body
	}
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sr := req.Clone(ctx) // shares the body, which only the handler reads
	if sr.Body == nil {
		sr.Body = http.NoBody
	}
	sr.RequestURI = req.URL.RequestURI()
	sr.RemoteAddr = "127.0.0.1:1"
	sr.Proto, sr.ProtoMajor, sr.ProtoMinor = "HTTP/1.1", 1, 1
	if sr.Host == "" {
		sr.Host = req.URL.Host
	}
	rec := httptest.NewRecorder()
	p.s.handle(rec, sr)
	// A request cancelled while it waited gets the context's error, as it
	// would over a connection (the handler returns without an answer).
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

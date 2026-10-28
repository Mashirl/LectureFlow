// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The format of the replay samples (testdata/replay/*.json, V14.6): an
// ordered list of HTTP exchanges and WebSocket scripts the backend must
// perform exactly in that order. The samples in this repository are
// constructed from upstream's source (each names the file and function it
// follows; "recorded": false); cmd/record writes real, redacted
// recordings in the same format once the user authorises one.
//
// Placeholders in strings: {{any}} matches anything (in expectations);
// {{runtime_url}} is the replay server's runtime proxy URL; {{NAME}} is a
// var; {{b64:NAME}}, {{sha256:NAME}}, {{len:NAME}} are the base64, SHA-256
// and byte length of var NAME (a stdout_lines value that is exactly
// "{{len:NAME}}" becomes a number).

// ReplayFile is one sample.
type ReplayFile struct {
	// Source says what the sample is based on ("依据上游 x.y.z 的某文件某函数构造，非真实录制").
	Source   string         `json:"_source"`
	Upstream ReplayUpstream `json:"upstream"`
	// Recorded is true only for real recordings made with cmd/record.
	Recorded bool `json:"recorded"`
	// Extends: this sample starts with another sample's steps (and vars),
	// up to but not including the step named Until (all of them when Until
	// is empty; the sample's vars override the parent's).
	Extends string `json:"extends,omitempty"`
	Until   string `json:"until,omitempty"`
	// Resume: after this sample's own steps, the parent's steps continue
	// from the step named Resume (a sample that inserts steps in the middle
	// of its parent, e.g. the memory probe before the uploads).
	Resume string            `json:"resume,omitempty"`
	Vars   map[string]string `json:"vars,omitempty"`
	Steps  []ReplayStep      `json:"steps"`
}

// ReplayUpstream names the upstream code a sample follows.
type ReplayUpstream struct {
	Package string   `json:"package"`
	Version string   `json:"version"`
	Files   []string `json:"files"`
}

// ReplayStep is one HTTP exchange or one WebSocket connection.
type ReplayStep struct {
	Name string `json:"name"`
	// Group: consecutive steps of the same group may come in any order
	// (requests the backend sends at once, e.g. the deletions of a job's
	// files; colab-batch).
	Group     string          `json:"group,omitempty"`
	Request   *ReplayRequest  `json:"request,omitempty"`
	Response  *ReplayResponse `json:"response,omitempty"`
	WebSocket *ReplayWS       `json:"websocket,omitempty"`
}

// ReplayRequest is what the backend must send. Host is "colab" (the tunnel
// API), "oauth" (token and revoke endpoints) or "runtime" (the runtime
// proxy). Query must match exactly (same keys); Headers, Form and JSON are
// subsets.
type ReplayRequest struct {
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Form    map[string]string `json:"form,omitempty"`
	JSON    map[string]any    `json:"json,omitempty"`
}

// ReplayResponse is the answer. Body is sent as is; otherwise JSON is
// marshalled, prefixed with the XSSI guard ")]}'\n" when XSSI is set.
type ReplayResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	JSON    any               `json:"json,omitempty"`
	XSSI    bool              `json:"xssi,omitempty"`
}

// ReplayWS is a kernel channels connection and its script.
type ReplayWS struct {
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Script  []ReplayWSAction  `json:"script"`
}

// ReplayWSAction is one step of a WebSocket script: expect a client
// message, send a server message, send stdout stream text (stdout_lines:
// event objects, one line each, in one stream message; stdout_raw: text as
// is), wait until the HTTP step named After was served (the cell runs while
// the audio uploads: its lecture events follow job.json), or close.
type ReplayWSAction struct {
	After       string           `json:"after,omitempty"`
	Expect      *ReplayKernelMsg `json:"expect,omitempty"`
	Send        *ReplayKernelMsg `json:"send,omitempty"`
	StdoutLines []map[string]any `json:"stdout_lines,omitempty"`
	StdoutRaw   *string          `json:"stdout_raw,omitempty"`
	Close       bool             `json:"close,omitempty"`
}

// ReplayKernelMsg is a Jupyter message of a script. For expect,
// ContentContains maps content fields to substrings they must contain. For
// send, Parent "" answers the last expected message; "other" makes it
// belong to an unrelated request (the backend must ignore it).
type ReplayKernelMsg struct {
	Channel         string            `json:"channel"`
	MsgType         string            `json:"msg_type"`
	Content         map[string]any    `json:"content,omitempty"`
	ContentContains map[string]string `json:"content_contains,omitempty"`
	Parent          string            `json:"parent,omitempty"`
}

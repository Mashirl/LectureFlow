// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package bridge is the message contract between the browser extension and
// LectureFlow (SPEC-ext "Native Messaging 协议与下载队列"; frozen in
// .claude/ext-brief.md §3). The native host relays these frames unchanged
// between the browser and the running app, so both sides share this package.
//
// Every message is one JSON object with a "type". Requests come from the
// extension and carry an "id" of its choosing; the reply has the same type
// and id, plus "ok" and, on failure, "error". Status pushes come from the app
// and have no id. Field names are camelCase because the other side is
// JavaScript.
package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
)

// ProtocolVersion is the version this build speaks. The extension sends its
// own in hello; a mismatch is answered with CodeProtocol.
const ProtocolVersion = 1

// Message types.
const (
	TypeHello   = "hello"
	TypeCheck   = "check"
	TypeEnqueue = "enqueue"
	TypeRefresh = "refresh"
	// TypeOpen is not in the SPEC-ext message table: the popup's
	// "在 LectureFlow 中打开" needs a way to bring the app forward.
	TypeOpen   = "open"
	TypeStatus = "status"
)

// Error codes. Params, when present, are listed beside each code.
const (
	// Sent by the native host when it can neither reach nor start
	// LectureFlow. Params: reason ("not_installed" | "start_failed" |
	// "timeout" | "not_running": not started because the first message
	// was a hello with NoLaunch).
	CodeAppNotRunning = "app.not_running"
	// Params: app (int), extension (int).
	CodeProtocol    = "protocol.unsupported"
	CodeBadRequest  = "request.invalid" // Params: field.
	CodeUnknownType = "request.unknown_type"
	CodeCourseGone  = "course.not_found"
	// The session is already imported or queued. Params: state, taskId,
	// lectureId (when imported).
	CodeDuplicate = "queue.duplicate"
	CodeNoTracks  = "enqueue.no_tracks"
	// More than one audio track, or more than two others (video or
	// unknown: the main picture and the second camera angle, in this
	// order; "双机位").
	CodeTooManyTracks = "enqueue.too_many_tracks"
	CodeProtected     = "enqueue.protected" // an EME-protected track was sent
	CodeTaskNotFound  = "refresh.not_found"
	CodeNotFound      = "open.not_found"
	CodeInternal      = "internal"
)

// Download task states (SPEC-ext 下载队列). They belong to the download task,
// not to the lecture: the lecture is created only when the download is done.
const (
	StateQueued       = "queued"
	StateDownloading  = "downloading"
	StateMerging      = "merging"   // HLS segments or audio + video into one file
	StateImporting    = "importing" // creating the lecture draft
	StateDone         = "done"
	StateFailed       = "failed"
	StateNeedsRefresh = "needs_refresh" // credentials gone or rejected
	StateCancelled    = "cancelled"
	// StateImported appears only in CheckItem: the lecture exists.
	StateImported = "imported"
)

// Track kinds and formats.
const (
	KindAudio   = "audio"
	KindVideo   = "video"
	KindUnknown = "unknown"

	FormatFile = "file"
	FormatHLS  = "hls"
	FormatDASH = "dash"
)

// GenericProfile is Profile.ID (and EnqueueRequest.Platform) when no profile
// matched and the tracks come from generic detection alone.
const GenericProfile = "generic"

// Envelope is what every message shares.
type Envelope struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// ReplyHeader starts every reply.
type ReplyHeader struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error *Error `json:"error,omitempty"`
}

// Error is a coded failure. Handlers return it to answer ok:false; any other
// error becomes CodeInternal without details.
type Error struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

func (e *Error) Error() string { return "bridge: " + e.Code }

// NewError builds a coded failure; params may be nil.
func NewError(code string, params map[string]any) *Error {
	return &Error{Code: code, Params: params}
}

// --- hello ---

type HelloRequest struct {
	Envelope
	Protocol  int           `json:"protocol"`
	Extension ExtensionInfo `json:"extension"`
	// NoLaunch asks the native host not to start LectureFlow when it is
	// not running: passive checks (row states on a page the user merely
	// opened) must not start the app; only a user action may. The host
	// reads it from the first frame; the app ignores it.
	NoLaunch bool `json:"noLaunch,omitempty"`
}

type ExtensionInfo struct {
	Version string `json:"version"`
	Browser string `json:"browser"` // chrome | edge | firefox
	Locale  string `json:"locale"`
}

type HelloReply struct {
	ReplyHeader
	Protocol int      `json:"protocol"`
	App      AppInfo  `json:"app"`
	Courses  []Course `json:"courses"`
}

type AppInfo struct {
	Version string `json:"version"`
	Running bool   `json:"running"`
	Mode    string `json:"mode"` // desktop | web
}

// Course is one entry of the popup's course list: LectureFlow's own course
// names only, the extension never reads course information from the page.
// Course and lecture ids are the database's ULID strings; task ids are the
// download table's integers.
type Course struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// --- check ---

type CheckRequest struct {
	Envelope
	Platform   string   `json:"platform"`
	SessionIDs []string `json:"sessionIds"`
	// Titles, when given, has one platform title per session id (same
	// index; "" when unknown): a session whose title matches a lecture of
	// CourseID (or of any course when CourseID is "") counts as imported
	// even without a download task, e.g. a lecture uploaded earlier from a
	// file the platform named after the session (the user's "update this
	// course" asks for dedup by title). Matching ignores case and treats
	// runs of spaces and / \ : _ - as one space; the lecture's title and
	// its original file name without extension are compared.
	Titles   []string `json:"titles,omitempty"`
	CourseID string   `json:"courseId,omitempty"`
}

// CheckReply lists only the sessions LectureFlow knows; the others are new.
type CheckReply struct {
	ReplyHeader
	Items []CheckItem `json:"items"`
}

type CheckItem struct {
	SessionID string `json:"sessionId"`
	State     string `json:"state"` // a task state, or StateImported
	TaskID    int64  `json:"taskId,omitempty"`
	LectureID string `json:"lectureId,omitempty"`
	CourseID  string `json:"courseId,omitempty"`
	// MatchedBy is "session" (the download task or its lecture) or
	// "title" (CheckRequest.Titles); a session match wins.
	MatchedBy string `json:"matchedBy,omitempty"`
}

// --- enqueue ---

type Profile struct {
	ID       string `json:"id"`      // profile id, or GenericProfile
	Version  string `json:"version"` // semver of the profile; "" for generic
	Reviewed bool   `json:"reviewed"`
}

type Lecture struct {
	Title string `json:"title"`
	// RecordedAt is the wall time the page states, not converted to this
	// computer's zone: "YYYY-MM-DD", "YYYY-MM-DDTHH:MM", or RFC 3339, whose
	// own offset's clock is kept (tz-fix); "" when unknown.
	RecordedAt  string `json:"recordedAt"`
	DurationSec int    `json:"durationSec"` // 0 when unknown
}

type Track struct {
	URL       string `json:"url"`
	Kind      string `json:"kind"`   // KindAudio | KindVideo | KindUnknown
	Format    string `json:"format"` // FormatFile | FormatHLS | FormatDASH
	Role      string `json:"role"`   // profile role, e.g. "primary-audio"; "" for generic
	Label     string `json:"label"`
	Quality   string `json:"quality,omitempty"`
	SizeBytes int64  `json:"sizeBytes"` // -1 when unknown
	// Selector picks one track out of a manifest that holds several under
	// the same URL (a DASH MPD): "kind:lang:index" as the extension's
	// detection writes it; "" for files and HLS.
	Selector string `json:"selector,omitempty"`
}

// String and GoString keep signed query parameters and the path out of
// logs (LogURL).
func (t Track) String() string {
	return fmt.Sprintf("%s/%s %s", t.Kind, t.Format, LogURL(t.URL))
}
func (t Track) GoString() string { return t.String() }

// Cookie is one cookie of a track URL's domain. It lives only in memory.
type Cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  int64  `json:"expires"` // Unix seconds; 0 for a session cookie
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"httpOnly"`
	HostOnly bool   `json:"hostOnly"`
}

// String and GoString never print the value, so a stray %v or %#v cannot
// leak it.
func (c Cookie) String() string   { return "cookie " + c.Name + "@" + c.Domain }
func (c Cookie) GoString() string { return c.String() }

type EnqueueRequest struct {
	Envelope
	Profile Profile `json:"profile"`
	// Platform with SessionID is the dedup key: the profile id, or
	// GenericProfile.
	Platform  string  `json:"platform"`
	SessionID string  `json:"sessionId"`
	Lecture   Lecture `json:"lecture"`
	CourseID  string  `json:"courseId"`
	// Tracks: at most one audio track and two others. The first track
	// that is not audio is the main picture (combined with the audio into
	// the lecture's media); a second one is the second camera angle, kept
	// as the lecture's video-only angle "angle2" ("双机位").
	Tracks  []Track  `json:"tracks"`
	Referer string   `json:"referer"`
	Cookies []Cookie `json:"cookies"`
	// MaxConnections is the profile's request.maxConnectionsPerFile; 0
	// means the profile sets no limit.
	MaxConnections int `json:"maxConnections"`
	// OverrideDisabled is true when the user overrode "下载已关闭" after the
	// warning. Recorded with the task; it changes nothing else.
	OverrideDisabled bool `json:"overrideDisabled"`
}

type EnqueueReply struct {
	ReplyHeader
	TaskID   int64 `json:"taskId"`
	Position int   `json:"position"` // 1-based among waiting tasks; 0 once started
}

// --- refresh ---

// RefreshRequest gives a needs_refresh task new credentials. Tracks are sent
// again because signed URLs expire with the session; they are matched to
// the task's tracks by role, then by kind.
type RefreshRequest struct {
	Envelope
	Platform  string   `json:"platform"`
	SessionID string   `json:"sessionId"`
	Tracks    []Track  `json:"tracks"`
	Referer   string   `json:"referer"`
	Cookies   []Cookie `json:"cookies"`
}

type RefreshReply struct {
	ReplyHeader
	TaskID int64  `json:"taskId"`
	State  string `json:"state"`
}

// --- open ---

// OpenRequest brings LectureFlow forward on a lecture, or on the course page
// of a task that has no lecture yet. Exactly one id is set.
type OpenRequest struct {
	Envelope
	LectureID string `json:"lectureId,omitempty"`
	TaskID    int64  `json:"taskId,omitempty"`
}

type OpenReply struct {
	ReplyHeader
}

// --- status (push) ---

// Status is pushed to every connected extension whenever a task changes,
// and once per active task right after hello.
type Status struct {
	Type       string `json:"type"` // always TypeStatus
	TaskID     int64  `json:"taskId"`
	Platform   string `json:"platform"`
	SessionID  string `json:"sessionId"`
	CourseID   string `json:"courseId"`
	State      string `json:"state"`
	Position   int    `json:"position"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"` // -1 when unknown
	LectureID  string `json:"lectureId,omitempty"`
	Error      *Error `json:"error,omitempty"`
}

// Handler is what the app serves over the bridge. The download queue
// implements Check, Enqueue, Refresh and Subscribe; cmd composes Hello and
// Open. Methods return *Error for coded failures.
type Handler interface {
	Hello(ctx context.Context, req *HelloRequest) (*HelloReply, error)
	Check(ctx context.Context, req *CheckRequest) (*CheckReply, error)
	Enqueue(ctx context.Context, req *EnqueueRequest) (*EnqueueReply, error)
	Refresh(ctx context.Context, req *RefreshRequest) (*RefreshReply, error)
	Open(ctx context.Context, req *OpenRequest) (*OpenReply, error)
	// Subscribe delivers status pushes until ctx ends, then closes the
	// channel. A slow reader may miss intermediate pushes, never the last.
	Subscribe(ctx context.Context) <-chan Status
}

// RedactURL is the form of a public URL (an update package's) in a log:
// scheme, host and path. Query, fragment and user info are dropped (SPEC-ext
// 凭据处理: signatures and tokens live in the query). A track or page URL
// goes to a log as LogURL.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// LogURL is the only form of a track or page URL that may reach a log or a
// failure detail: scheme, host and the first 16 hex digits of the SHA-256
// of the escaped path, "https://cdn.example.edu (path 0123456789abcdef)".
// The path is a credential too (lead's ruling 3: on Panopto's CloudFront
// the GUID path alone downloads the recording); the hash only tells two
// URLs apart. internal/fetch writes track URLs the same way.
func LogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid url)"
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	sum := sha256.Sum256([]byte(p))
	return u.Scheme + "://" + u.Host + " (path " + hex.EncodeToString(sum[:8]) + ")"
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
)

// The lead's rulings 3–7 after the first download queue (NOTES
// "决定（ext 第三波前的裁决：lead）").

// testPathSecret is a path segment that stands for a recording's GUID: on
// Panopto's CloudFront the path alone downloads the recording (M0).
const testPathSecret = "PATHSECRET_5d1e0a"

// ---------- ruling 3: paths are credentials ----------

// TestTrackPathsNeverStored (落盘扫描): after a file, an HLS and a DASH
// task, a task the server failed (aria2's message quotes the URL), one it
// refused and a restart, no track path is in any file under the data
// directory (database, -wal, task files), in the log, on aria2c's command
// line or in ffmpeg's arguments; the rows keep the host and the path's
// hash, and the failure detail names the host and the hash.
func TestTrackPathsNeverStored(t *testing.T) {
	e := newQEnv(t, nil)
	p := "/files/" + testPathSecret
	e.media.put(p+"/a.m4a", pattern(1, 30_000))
	hlsSegments(e, testPathSecret+"/h", 3, true)
	video, audio := dashFixture(e, testPathSecret+"/d", "")

	e.media.put(p+"/cam2.mp4", pattern(7, 20_000))
	// (with a second camera angle: its path is a credential as well)
	id1 := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, p+"/a.m4a"), e.hlsTrack(bridge.KindVideo, "/"+testPathSecret+"/h/index.m3u8"),
		e.fileTrack(bridge.KindVideo, p+"/cam2.mp4")))
	id2 := e.enqueue(e.request("s2", e.dashTrack(bridge.KindVideo, "/"+testPathSecret+"/d/manifest.mpd", "video:-:0"),
		e.dashTrack(bridge.KindAudio, "/"+testPathSecret+"/d/manifest.mpd", "audio:en:0")))
	e.wait(id1, bridge.StateDone)
	info := e.wait(id2, bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, append(append([]byte{}, video...), audio...)) {
		t.Fatalf("DASH media: %d bytes", len(got))
	}
	e.media.setStatus(p+"/b.m4a", 500)
	id3 := e.enqueue(e.request("s3", e.fileTrack(bridge.KindAudio, p+"/b.m4a")))
	failed := e.wait(id3, bridge.StateFailed)
	host := strings.TrimPrefix(e.media.srv.URL, "http://")
	if d := failed.Error.Detail; strings.Contains(d, testPathSecret) || strings.Contains(d, testSig) ||
		!strings.Contains(d, host+" (path "+pathHash(p+"/b.m4a")+")") {
		t.Fatalf("failure detail %q", d)
	}
	e.media.setStatus(p+"/c.m4a", 403)
	id4 := e.enqueue(e.request("s4", e.fileTrack(bridge.KindAudio, p+"/c.m4a")))
	e.wait(id4, bridge.StateNeedsRefresh)
	for _, id := range []int64{id1, id2, id3, id4} {
		rec := e.row(id)
		if strings.Contains(rec.TracksJSON, testPathSecret) || strings.Contains(rec.TracksJSON, `"path"`) ||
			!strings.Contains(rec.TracksJSON, `"host":"`+host+`"`) || !strings.Contains(rec.TracksJSON, `"path_hash":"`) {
			t.Fatalf("row %d tracks %s", id, rec.TracksJSON)
		}
	}
	if rec := e.row(id2); !strings.Contains(rec.TracksJSON, `"selector":"video:-:0"`) {
		t.Fatalf("DASH row %s", rec.TracksJSON)
	}
	e.restart()
	e.q.Close(5 * time.Second)

	scanned := 0
	err := filepath.WalkDir(e.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if bytes.Contains(b, []byte(testPathSecret)) {
			t.Errorf("%s contains a track path", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 4 {
		t.Fatalf("only %d files scanned", scanned)
	}
	logs := e.logs.String()
	_, args, conf, _, _ := e.aria.snapshot()
	var ff []string
	for _, c := range e.ffmpegCalls() {
		ff = append(ff, c...)
	}
	for what, s := range map[string]string{"the log": logs, "aria2c's command line": strings.Join(args, " "), "aria2c's conf": conf,
		"ffmpeg's arguments": strings.Join(ff, " ")} {
		if strings.Contains(s, testPathSecret) {
			t.Errorf("%s contains a track path:\n%s", what, s)
		}
	}
	// the log still tells which server: host and hash
	if !strings.Contains(logs, "downloads: task stopped") || !strings.Contains(logs, host+" (path "+pathHash(p+"/a.m4a")+")") {
		t.Fatalf("the log does not name the tracks' hosts:\n%s", logs)
	}
}

// Rows written before ruling 3 kept the path: they read as before (a
// refresh finds the same files) and lose the path at the next start.
func TestLegacyTrackPaths(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	host := strings.TrimPrefix(e.media.srv.URL, "http://")
	legacy := `[{"kind":"video","format":"file","role":"video","label":"video","size_bytes":-1,"host":"` + host + `","path":"/files/v.mp4"}]`
	st := e.opts.Store
	id, err := st.InsertDownloadTask(ctx, db.DownloadTask{Platform: "panopto", SessionID: "old", ProfileID: "panopto",
		CourseID: e.course, Title: "Old", TracksJSON: legacy, State: bridge.StateDownloading, BytesTotal: -1})
	if err != nil {
		t.Fatal(err)
	}
	done, err := st.InsertDownloadTask(ctx, db.DownloadTask{Platform: "panopto", SessionID: "old-done", ProfileID: "panopto",
		CourseID: e.course, Title: "Old", TracksJSON: strings.Replace(legacy, "/files/v.mp4", "/files/w.mp4", 1), State: bridge.StateDone,
		BytesTotal: -1, UpdatedAt: "2026-10-01T00:00:00Z", CreatedAt: "2026-10-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	// a partial file of the old task: kept when the same track comes again
	if err := os.MkdirAll(e.q.taskDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	e.restart()
	for _, rid := range []int64{id, done} {
		rec := e.row(rid)
		if strings.Contains(rec.TracksJSON, `"path"`) || strings.Contains(rec.TracksJSON, "/files/") {
			t.Fatalf("row %d still has the path: %s", rid, rec.TracksJSON)
		}
	}
	if rec := e.row(done); rec.UpdatedAt != "2026-10-01T00:00:00Z" ||
		!strings.Contains(rec.TracksJSON, `"path_hash":"`+pathHash("/files/w.mp4")+`"`) {
		t.Fatalf("done row %+v", rec)
	}
	if !sameTracks(legacy, e.row(id).TracksJSON) {
		t.Fatal("the scrubbed row is not the same track as the legacy one")
	}
	e.media.put("/files/v.mp4", pattern(3, 5000))
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "old",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/v.mp4")}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	e.wait(id, bridge.StateDone)
}

// ---------- ruling 4: a failed task's credentials expire ----------

// fakeClock is real time plus an offset the test moves forward.
type fakeClock struct{ off atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *fakeClock) advance(d time.Duration) { c.off.Add(int64(d)) }
func (e *qenv) inMemory(id int64) (inMem bool) {
	e.q.mu.Lock()
	_, inMem = e.q.tasks[id]
	e.q.mu.Unlock()
	return
}

func TestFailedCredentialsExpire(t *testing.T) {
	clk := &fakeClock{}
	e := newQEnv(t, func(o *Options) {
		o.Now = clk.now
		o.CredentialSweep = 5 * time.Millisecond
	})
	ctx := context.Background()
	e.media.put("/files/a.m4a", pattern(1, 4000))
	e.media.setStatus("/files/a.m4a", 500)
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id, bridge.StateFailed)
	if !e.q.hasCredsOf(id) {
		t.Fatal("a failed task lost its credentials at once")
	}
	clk.advance(29 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if info, _ := e.q.Get(ctx, id); info.State != bridge.StateFailed || !e.q.hasCredsOf(id) {
		t.Fatalf("after 29 minutes %+v", info)
	}
	clk.advance(time.Minute)
	info := e.wait(id, bridge.StateNeedsRefresh)
	if info.Error == nil || info.Error.Code != CodeCredentialsCleared || info.Error.Params["minutes"] != float64(30) {
		t.Fatalf("after 30 minutes %+v", info.Error)
	}
	if e.inMemory(id) {
		t.Fatal("the task's credentials are still in memory")
	}
	if _, err := e.q.Retry(ctx, id); codedOf(err) != CodeNeedsRefresh {
		t.Fatalf("retry: %v", err)
	}
	if !slices.Equal(info.AllowedActions, []string{ActionCancel}) {
		t.Fatalf("actions %v", info.AllowedActions)
	}
	// the extension brings new credentials
	e.media.setStatus("/files/a.m4a", 0)
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindAudio, "/files/a.m4a")}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	e.wait(id, bridge.StateDone)
}

// A retry after the grace period, before the sweep came by, does not use
// the old credentials.
func TestFailedCredentialsExpireOnRetry(t *testing.T) {
	clk := &fakeClock{}
	e := newQEnv(t, func(o *Options) {
		o.Now = clk.now
		o.CredentialSweep = time.Hour
	})
	ctx := context.Background()
	e.media.put("/files/a.m4a", pattern(1, 4000))
	e.media.setStatus("/files/a.m4a", 500)
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id, bridge.StateFailed)
	e.media.setStatus("/files/a.m4a", 0)
	clk.advance(31 * time.Minute)
	adds, _, _, _, _ := e.aria.snapshot()
	info, err := e.q.Retry(ctx, id)
	if err != nil || info.State != bridge.StateNeedsRefresh || info.Error == nil || info.Error.Code != CodeCredentialsCleared {
		t.Fatalf("retry after 31 minutes %+v %v", info, err)
	}
	time.Sleep(50 * time.Millisecond)
	if after, _, _, _, _ := e.aria.snapshot(); len(after) != len(adds) || e.inMemory(id) {
		t.Fatalf("downloaded with expired credentials: %d → %d", len(adds), len(after))
	}
}

// Merging failed: every track is on disk, so the expiry only clears the
// credentials and the task stays retryable.
func TestFailedCredentialsExpireFilesComplete(t *testing.T) {
	clk := &fakeClock{}
	e := newQEnv(t, func(o *Options) {
		o.Now = clk.now
		o.CredentialSweep = 5 * time.Millisecond
	})
	ctx := context.Background()
	e.media.put("/files/v.mp4", pattern(1, 3000))
	e.media.put("/files/a.m4a", pattern(2, 1000))
	t.Setenv("LF_FAKE_FFMPEG", "fail")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id, bridge.StateFailed)
	clk.advance(30 * time.Minute)
	e.waitFor("credentials cleared", func() bool { return !e.q.hasCredsOf(id) })
	if info, _ := e.q.Get(ctx, id); info.State != bridge.StateFailed || info.Error.Code != CodeMergeFailed {
		t.Fatalf("after the expiry %+v", info)
	}
	t.Setenv("LF_FAKE_FFMPEG", "ok")
	adds, _, _, _, _ := e.aria.snapshot()
	if _, err := e.q.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.wait(id, bridge.StateDone)
	if after, _, _, _, _ := e.aria.snapshot(); len(after) != len(adds) {
		t.Fatal("the retry downloaded again")
	}
}

// ---------- ruling 5: cookies only for the final host ----------

// otherHost is a second media server reached as "localhost": another host
// for cookies (they ignore ports), on this machine.
func otherHost(t *testing.T) (*mediaServer, string) {
	o := newMediaServer(t)
	o.mu.Lock()
	o.cookie, o.sigFor = "", ""
	o.mu.Unlock()
	return o, strings.Replace(o.srv.URL, "127.0.0.1", "localhost", 1)
}

func TestCookiesStayOnTheirHost(t *testing.T) {
	e := newQEnv(t, nil)
	other, otherURL := otherHost(t)
	video, audio := pattern(1, 80_000), pattern(2, 20_000)
	other.put("/cdn/v.mp4", video)
	e.media.put("/files/a.m4a", audio)
	e.media.mu.Lock()
	// a cross-host redirect (to a CDN with its own signature) and a
	// redirect on the same host
	e.media.redir["/files/r/v.mp4"] = otherURL + "/cdn/v.mp4?Expires=1&Sig=cdn"
	e.media.redir["/files/r/a.m4a"] = "/files/a.m4a?Signature=" + testSig
	e.media.mu.Unlock()

	// the fake follows redirects like aria2c: given the first URL and the
	// cookie it would hand the cookie to the other host
	req, _ := http.NewRequest(http.MethodGet, e.fileTrack(bridge.KindVideo, "/files/r/v.mp4").URL, nil)
	req.Header.Set("Cookie", "sid="+testCookie)
	req.Header.Set("Referer", testReferer)
	if res, err := e.aria.hc.Do(req); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
	}
	if r := other.requests("/cdn/v.mp4"); len(r) != 1 || r[0].Cookie != testCookie {
		t.Fatalf("the fake aria2c does not forward cookies like aria2c: %+v", r)
	}
	for _, m := range []*mediaServer{other, e.media} {
		m.mu.Lock()
		m.seen = nil
		m.mu.Unlock()
	}

	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/r/v.mp4"), e.fileTrack(bridge.KindAudio, "/files/r/a.m4a")))
	info := e.wait(id, bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, append(append([]byte{}, video...), audio...)) {
		t.Fatalf("media %d bytes", len(got))
	}
	seen := other.requests("")
	for _, r := range seen {
		if r.CookieHeader != "" {
			t.Fatalf("a cookie reached the other host: %+v", r)
		}
		if r.Referer != testRefOrigin {
			t.Fatalf("the other host got another Referer (a previous URL?): %+v", r)
		}
	}
	if len(seen) < 2 { // Go's HEAD and aria2c's GET
		t.Fatalf("the other host saw %+v", seen)
	}
	adds, _, _, _, _ := e.aria.snapshot()
	for _, a := range adds {
		switch {
		case strings.Contains(a.URI, "/cdn/v.mp4"):
			if !strings.HasPrefix(a.URI, otherURL+"/cdn/v.mp4?") || slices.ContainsFunc(a.Headers, func(h string) bool { return strings.HasPrefix(h, "Cookie:") }) {
				t.Errorf("cross-host download %+v", a)
			}
		case strings.Contains(a.URI, "/files/a.m4a"):
			if !strings.Contains(a.URI, "/files/a.m4a?Signature=") || !slices.Contains(a.Headers, "Cookie: sid="+testCookie) {
				t.Errorf("same-host download %+v", a)
			}
		default:
			t.Errorf("aria2c got a URL that redirects: %+v", a)
		}
	}
	// Go found the redirects with the session, HEAD first
	if r := e.media.requests("/files/r/v.mp4"); len(r) == 0 || r[0].Method != http.MethodHead || r[0].Cookie != testCookie {
		t.Fatalf("redirect probe %+v", r)
	}
}

// A playlist on the page's host that redirects to another host is read
// there without the cookie; HEAD refused (405, or 403 for a URL signed for
// GET only) falls back to GET of byte 0.
func TestRedirectProbes(t *testing.T) {
	e := newQEnv(t, nil)
	other, otherURL := otherHost(t)
	other.put("/hls/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nseg1.ts\n#EXTINF:6,\nseg2.ts\n#EXT-X-ENDLIST\n"))
	other.put("/hls/seg1.ts", pattern(1, 5000))
	other.put("/hls/seg2.ts", pattern(2, 5000))
	other.mu.Lock()
	other.noHead["/hls/seg1.ts"] = http.StatusMethodNotAllowed
	other.noHead["/hls/seg2.ts"] = http.StatusForbidden
	other.mu.Unlock()
	e.media.mu.Lock()
	e.media.redir["/files/r/index.m3u8"] = otherURL + "/hls/index.m3u8?Policy=p"
	e.media.mu.Unlock()
	hls := bridge.Track{URL: e.fileTrack(bridge.KindVideo, "/files/r/index.m3u8").URL, Kind: bridge.KindVideo,
		Format: bridge.FormatHLS, Role: "video", SizeBytes: -1}
	e.wait(e.enqueue(e.request("s1", hls)), bridge.StateDone)
	for _, r := range other.requests("") {
		if r.CookieHeader != "" {
			t.Fatalf("a cookie reached the other host: %+v", r)
		}
	}
	for _, p := range []string{"/hls/seg1.ts", "/hls/seg2.ts"} {
		var methods []string
		for _, r := range other.requests(p) {
			methods = append(methods, r.Method+" "+r.Range)
		}
		// HEAD, GET of byte 0, then aria2c's GET; the segments took the
		// playlist's query (ruling 6)
		if len(methods) < 3 || methods[0] != "HEAD " || methods[1] != "GET bytes=0-0" {
			t.Errorf("%s: %v", p, methods)
		}
		if q := other.requests(p)[0].Query; q != "Policy=p" {
			t.Errorf("%s query %q", p, q)
		}
	}
	// refused to HEAD and to GET: needs a refresh, aria2c never sees it
	e.media.setStatus("/files/x.mp4", 403)
	id3 := e.enqueue(e.request("s3", e.fileTrack(bridge.KindVideo, "/files/x.mp4")))
	if info := e.wait(id3, bridge.StateNeedsRefresh); info.Error.Code != CodeCredentialsRejected {
		t.Fatalf("%+v", info.Error)
	}
	if adds, _, _, _, _ := e.aria.snapshot(); slices.ContainsFunc(adds, func(a fakeAdd) bool { return strings.Contains(a.URI, "/files/x.mp4") }) {
		t.Fatal("aria2c got a URL Go could not resolve")
	}
}

// Without cookies (Panopto on CloudFront, M0) nothing is probed.
func TestNoCookiesNoProbe(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.mu.Lock()
	e.media.cookie = ""
	e.media.mu.Unlock()
	e.media.put("/files/v.mp4", pattern(1, 5000))
	req := e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"))
	req.Cookies = nil
	e.wait(e.enqueue(req), bridge.StateDone)
	for _, r := range e.media.requests("") {
		if r.Method != http.MethodGet || r.Range == "bytes=0-0" {
			t.Fatalf("probe without cookies: %+v", r)
		}
	}
}

// ---------- ruling 6: HLS segments take the playlist's signed query ----------

func TestHLSInheritsSignedQuery(t *testing.T) {
	e := newQEnv(t, nil)
	// under /files/ the server wants ?Signature=: segments listed by bare
	// relative names must carry the playlist's query (Echo360 public link)
	e.media.put("/files/echo/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"avc1.4d401e,mp4a.40.2\"\ns1_av.m3u8\n"))
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.mp4\"\n")
	want := pattern(9, 700)
	e.media.put("/files/echo/init.mp4", want)
	for i := 1; i <= 3; i++ {
		seg := pattern(byte(i), 3000)
		want = append(want, seg...)
		e.media.put("/files/echo/seg"+itoa(int64(i))+".m4s", seg)
		b.WriteString("#EXTINF:4,\nseg" + itoa(int64(i)) + ".m4s\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	e.media.put("/files/echo/s1_av.m3u8", []byte(b.String()))
	signed := "?Policy=P&Signature=" + testSig + "&Key-Pair-Id=K"
	tr := bridge.Track{URL: e.media.url("/files/echo/master.m3u8") + signed, Kind: bridge.KindVideo, Format: bridge.FormatHLS, SizeBytes: -1}
	info := e.wait(e.enqueue(e.request("s1", tr)), bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, want) {
		t.Fatalf("media %d bytes", len(got))
	}
	for _, r := range e.media.requests("") {
		if "?"+r.Query != signed {
			t.Errorf("%s %s without the playlist's query: %q", r.Method, r.Path, r.Query)
		}
	}
}

// ---------- ruling 7: DASH ----------

func (e *qenv) dashTrack(kind, p, selector string) bridge.Track {
	return bridge.Track{URL: e.media.url(p) + "?Policy=" + testSig, Kind: kind, Format: bridge.FormatDASH,
		Role: "dash-" + kind, Label: kind, SizeBytes: -1, Selector: selector}
}

// dashFixture serves an MPD at /dir/manifest.mpd: video (SegmentTemplate
// with a timeline, two Representations), audio "en" and "fr"
// (SegmentList), audio "de" (SegmentBase, one file). extraVideo is put
// into the video AdaptationSet (e.g. a ContentProtection). It returns the
// bytes of the best video and of the "en" audio, init sections first.
func dashFixture(e *qenv, dir, extraVideo string) (video, audio []byte) {
	put := func(p string, b []byte) []byte {
		e.media.put("/"+dir+"/"+p, b)
		return b
	}
	video = append(video, put("v/v2/init.mp4", pattern(10, 900))...)
	put("v/v1/init.mp4", pattern(11, 900))
	for k, name := range []string{"seg-00001-0", "seg-00002-2000", "seg-00003-4000", "seg-00004-6000"} {
		video = append(video, put("v/v2/"+name+".m4s", pattern(byte(20+k), 6000+k))...)
	}
	audio = append(audio, put("a/en/init.m4a", pattern(30, 600))...)
	audio = append(audio, put("a/en/1.m4s", pattern(31, 2000))...)
	audio = append(audio, put("a/en/2.m4s", pattern(32, 2100))...)
	put("a/fr/init.m4a", pattern(40, 600))
	put("a/fr/1.m4s", pattern(41, 2000))
	put("a/fr/2.m4s", pattern(42, 2000))
	put("a/de.m4a", pattern(50, 9000))
	put("manifest.mpd", []byte(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT8S" minBufferTime="PT2S">
  <Period>
    <AdaptationSet contentType="video" mimeType="video/mp4">`+extraVideo+`
      <SegmentTemplate timescale="1000" initialization="v/$RepresentationID$/init.mp4" media="v/$RepresentationID$/seg-$Number%05d$-$Time$.m4s">
        <SegmentTimeline><S t="0" d="2000" r="-1"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v1" bandwidth="500000" width="640" height="360" codecs="avc1.4d401e"/>
      <Representation id="v2" bandwidth="1500000" width="1280" height="720" codecs="avc1.64001f"/>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="en">
      <Representation id="a-en" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentList><Initialization sourceURL="a/en/init.m4a"/><SegmentURL media="a/en/1.m4s"/><SegmentURL media="a/en/2.m4s"/></SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="fr">
      <Representation id="a-fr" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentList><Initialization sourceURL="a/fr/init.m4a"/><SegmentURL media="a/fr/1.m4s"/><SegmentURL media="a/fr/2.m4s"/></SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="de">
      <Representation id="a-de" bandwidth="64000" codecs="mp4a.40.2"><BaseURL>a/de.m4a</BaseURL><SegmentBase indexRange="0-100"/></Representation>
    </AdaptationSet>
  </Period>
</MPD>`))
	return video, audio
}

func TestDASHDownload(t *testing.T) {
	e := newQEnv(t, nil)
	video, _ := dashFixture(e, "d", "")
	fr := append(append(pattern(40, 600), pattern(41, 2000)...), pattern(42, 2000)...)
	req := e.request("s1", e.dashTrack(bridge.KindVideo, "/d/manifest.mpd", "video:-:0"), e.dashTrack(bridge.KindAudio, "/d/manifest.mpd", "audio:fr:0"))
	req.MaxConnections = 2
	info := e.wait(e.enqueue(req), bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, append(append([]byte{}, video...), fr...)) {
		t.Fatalf("media %d bytes, want %d", len(got), len(video)+len(fr))
	}
	adds, _, _, maxActive, _ := e.aria.snapshot()
	if len(adds) != 8 { // video init + 4, audio init + 2
		t.Fatalf("%d downloads", len(adds))
	}
	if maxActive > 4 { // two tracks, at most 2 parts each
		t.Fatalf("%d downloads at once", maxActive)
	}
	for _, a := range adds {
		if strings.Contains(a.URI, "/v/v1/") || strings.Contains(a.URI, "/a/en/") {
			t.Errorf("the wrong representation or set: %s", a.URI)
		}
		if a.Split != "1" || !strings.Contains(a.URI, "?Policy="+testSig) || !slices.Contains(a.Headers, "Cookie: sid="+testCookie) {
			t.Errorf("part %+v", a)
		}
	}
	// the MPD was read with the session; each track remuxed once, then
	// combined
	if r := e.media.requests("/d/manifest.mpd"); len(r) != 2 || r[0].Cookie != testCookie || r[0].Referer != testRefOrigin {
		t.Fatalf("MPD requests %+v", r)
	}
	calls := e.ffmpegCalls()
	if len(calls) != 3 {
		t.Fatalf("ffmpeg %v", calls)
	}
	for _, c := range calls[:2] {
		j := strings.Join(c, " ")
		if !strings.Contains(j, "joined") || !strings.Contains(j, "-c copy") || strings.Contains(j, "allowed_extensions") {
			t.Errorf("remux %s", j)
		}
	}
}

// A video track alone: the MPD's audio comes with it (a lecture needs its
// sound); a SegmentBase track is one file; DRM is refused.
func TestDASHVideoOnlySingleFileAndDRM(t *testing.T) {
	e := newQEnv(t, nil)
	video, en := dashFixture(e, "d", "")
	info := e.wait(e.enqueue(e.request("s1", e.dashTrack(bridge.KindVideo, "/d/manifest.mpd", "video:-:0"))), bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, append(append([]byte{}, video...), en...)) {
		t.Fatalf("video with its audio: %d bytes", len(got))
	}
	if calls := e.ffmpegCalls(); len(calls) != 1 || !strings.Contains(strings.Join(calls[0], " "), "-map 1:a:0") {
		t.Fatalf("join %v", calls)
	}

	info = e.wait(e.enqueue(e.request("s2", e.dashTrack(bridge.KindAudio, "/d/manifest.mpd", "audio:de:0"))), bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, pattern(50, 9000)) {
		t.Fatalf("SegmentBase %d bytes", len(got))
	}
	adds, _, _, _, _ := e.aria.snapshot()
	last := adds[len(adds)-1]
	if !strings.Contains(last.URI, "/d/a/de.m4a?Policy=") || last.Split != "4" {
		t.Fatalf("one file, split: %+v", last)
	}

	dashFixture(e, "p", `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>`)
	info = e.wait(e.enqueue(e.request("s3", e.dashTrack(bridge.KindVideo, "/p/manifest.mpd", "video:-:0"))), bridge.StateFailed)
	if info.Error.Code != CodeDRMProtected || info.Error.Params != nil || !strings.Contains(info.Error.Detail, "drm") {
		t.Fatalf("drm %+v", info.Error)
	}
}

// A restart in the middle of a DASH download: after the refresh only the
// missing segments are fetched.
func TestDASHResume(t *testing.T) {
	e := newQEnv(t, nil)
	_, audio := dashFixture(e, "d", "")
	e.media.hold("/d/a/en/2.m4s")
	tr := e.dashTrack(bridge.KindAudio, "/d/manifest.mpd", "audio:en:0")
	id := e.enqueue(e.request("s1", tr))
	e.waitFor("the first parts", func() bool {
		return fileExists(filepath.Join(e.q.taskDir(id), "t0", "s000001.seg")) && fileExists(filepath.Join(e.q.taskDir(id), "t0", "init.m4a"))
	})
	e.restart()
	e.media.release()
	if _, err := e.q.Refresh(context.Background(), &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{tr}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
		t.Fatal(err)
	}
	info := e.wait(id, bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, audio) {
		t.Fatalf("resumed media %d bytes", len(got))
	}
	for _, p := range []string{"/d/a/en/init.m4a", "/d/a/en/1.m4s"} {
		n := 0
		for _, r := range e.media.requests(p) {
			if r.Method == http.MethodGet {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s fetched %d times", p, n)
		}
	}
}

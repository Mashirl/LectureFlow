// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"lectureflow/internal/blocklist"
	"lectureflow/internal/bridge"
)

// The second line behind the extension's do-not-handle list
// (internal/blocklist; user 2026-10-27: no YouTube, no MOOC or paid course
// platform): enqueue and refresh refuse a page or a track on such a site
// with downloads.site_blocked {category}, before anything is stored or
// requested.
func TestEnqueueRefusesBlockedSites(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	refused := func(err error, category string) bool {
		var be *bridge.Error
		return errors.As(err, &be) && be.Code == CodeSiteBlocked && be.Params["category"] == category && len(be.Params) == 1
	}
	e.media.put("/files/a.m4a", pattern(1, 100))
	cases := []struct {
		referer, track, category string
	}{
		{"https://www.youtube.com/watch?v=abc", "", blocklist.CategoryYouTube},
		{"https://www.udemy.com/course/x/learn/lecture/1", "", blocklist.CategoryCourses},
		{"https://www.linkedin.com/learning/some-course/intro", "", blocklist.CategoryCourses},
		{"https://www.coursera.org/", "", blocklist.CategoryCourses}, // the origin only: tracks on another site
		{"", "https://rr1---sn-abc.googlevideo.com/videoplayback?itag=18", blocklist.CategoryYouTube},
		{testReferer, "https://mp4-c.udemycdn.com/2026/lecture.mp4", blocklist.CategoryCourses},
	}
	for i, c := range cases {
		tr := e.fileTrack(bridge.KindAudio, "/files/a.m4a")
		if c.track != "" {
			tr = bridge.Track{URL: c.track, Kind: bridge.KindVideo, Format: bridge.FormatFile}
		}
		req := e.request(fmt.Sprint("blocked", i), tr)
		req.Referer = c.referer
		if _, err := e.q.Enqueue(ctx, req); !refused(err, c.category) {
			t.Errorf("%d %s %s: %v", i, c.referer, c.track, err)
		}
	}
	if rows, _ := e.opts.Store.ListDownloadTasks(ctx); len(rows) != 0 {
		t.Fatalf("refused requests left rows: %+v", rows)
	}
	// the rest of a site with a blocked path is not refused (the test
	// media server checks the Referer against its own page otherwise)
	e.media.mu.Lock()
	e.media.refPrefix = ""
	e.media.mu.Unlock()
	ok := e.request("feed", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
	ok.Referer = "https://www.linkedin.com/feed/"
	id := e.enqueue(ok)
	e.wait(id, bridge.StateDone)
	// refresh, with a page or a track of such a site
	for i, c := range cases[:2] {
		_, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Envelope: bridge.Envelope{Type: bridge.TypeRefresh, ID: fmt.Sprint("r", i)},
			Platform: "panopto", SessionID: "feed", Referer: c.referer, Tracks: []bridge.Track{e.fileTrack(bridge.KindAudio, "/files/a.m4a")}})
		if !refused(err, c.category) {
			t.Errorf("refresh %s: %v", c.referer, err)
		}
	}
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Envelope: bridge.Envelope{Type: bridge.TypeRefresh, ID: "r9"},
		Platform: "panopto", SessionID: "feed", Referer: testReferer,
		Tracks: []bridge.Track{{URL: "https://www.youtube-nocookie.com/a.m4a", Kind: bridge.KindAudio, Format: bridge.FormatFile}}}); !refused(err, blocklist.CategoryYouTube) {
		t.Errorf("refresh with a YouTube track: %v", err)
	}
}

// A playlist on an ordinary host that leads to a blocked one: the task
// fails with downloads.site_blocked {category, host} before anything is
// requested there, and aria2c gets nothing.
func TestPlaylistToBlockedSite(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/hls/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nhttps://rr2---sn-abc.googlevideo.com/v/index.m3u8\n"))
	e.media.put("/seg/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\nhttps://www.udemy.com/assets/1/seg0.ts\n#EXT-X-ENDLIST\n"))
	for i, c := range []struct{ path, host, category string }{
		{"/hls/master.m3u8", "rr2---sn-abc.googlevideo.com", blocklist.CategoryYouTube},
		{"/seg/index.m3u8", "www.udemy.com", blocklist.CategoryCourses},
	} {
		id := e.enqueue(e.request(fmt.Sprint("p", i), e.hlsTrack(bridge.KindVideo, c.path)))
		info := e.wait(id, bridge.StateFailed)
		if info.Error == nil || info.Error.Code != CodeSiteBlocked || info.Error.Params["host"] != c.host || info.Error.Params["category"] != c.category {
			t.Errorf("%d: %+v", i, info.Error)
		}
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Fatalf("aria2c was given %+v", adds)
	}
}

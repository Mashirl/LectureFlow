// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package blocklist

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The extension's list (extension/src/lib/blocklist.js, between the
// blocklist:begin and blocklist:end markers, one entry per line) and this
// one are the same, in the same order: a site added to one is added to the
// other.
func TestSameListAsTheExtension(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "extension", "src", "lib", "blocklist.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	begin, end := strings.Index(src, "// blocklist:begin"), strings.Index(src, "// blocklist:end")
	if begin < 0 || end < begin {
		t.Fatal("blocklist.js has no blocklist:begin / blocklist:end markers")
	}
	line := regexp.MustCompile(`(?m)^\s*\['(youtube|courses)', '([^']+)'\],`)
	var js [][2]string
	for _, m := range line.FindAllStringSubmatch(src[begin:end], -1) {
		js = append(js, [2]string{m[1], m[2]})
	}
	if len(js) < 90 {
		t.Fatalf("read only %d entries from blocklist.js", len(js))
	}
	goList := Entries()
	for i := 0; i < max(len(js), len(goList)); i++ {
		var a, b [2]string
		if i < len(js) {
			a = js[i]
		}
		if i < len(goList) {
			b = goList[i]
		}
		if a != b {
			t.Fatalf("entry %d: blocklist.js has %v, internal/blocklist has %v (keep both lists equal, in the same order)", i, a, b)
		}
	}
}

func TestHosts(t *testing.T) {
	blocked := map[string]string{
		"youtube.com":                  CategoryYouTube,
		"www.youtube.com":              CategoryYouTube,
		"rr3---sn-abc.googlevideo.com": CategoryYouTube,
		"YOUTUBE.COM.":                 CategoryYouTube,
		"www.youtube.com:443":          CategoryYouTube,
		"ｙｏｕｔｕｂｅ。com":                  CategoryYouTube,
		"user:pw@www.udemy.com:8443":   CategoryCourses,
		"mp4-c.udemycdn.com":           CategoryCourses,
		"learning.oreilly.com":         CategoryCourses,
		"www.icourse163.org":           CategoryCourses,
		"practicum.yandex.ru":          CategoryCourses,
		"mitxonline.mit.edu":           CategoryCourses,
		"www.ximalaya.com":             CategoryCourses,
		"higher.smartedu.cn":           CategoryCourses,
	}
	for h, want := range blocked {
		if got := HostCategory(h); got != want {
			t.Errorf("HostCategory(%q) = %q, want %q", h, got, want)
		}
	}
	for _, h := range []string{"", "notyoutube.com", "youtube.com.example.org", "udemy.com.evil.example", "oreilly.com",
		"www.linkedin.com", "www.bilibili.com", "yandex.ru", "tenant.cloud.panopto.eu", "yuketang.cn", "www.open.edu", "127.0.0.1",
		"mooc1.chaoxing.com", "www.zhihuishu.com", "mit.edu", "ocw.mit.edu"} {
		if got := HostCategory(h); got != "" {
			t.Errorf("HostCategory(%q) = %q, want none", h, got)
		}
	}
}

func TestURLs(t *testing.T) {
	blocked := map[string]string{
		"https://www.youtube.com/watch?v=x":                    CategoryYouTube,
		"https://rr1---sn-x.googlevideo.com/videoplayback?a=b": CategoryYouTube,
		"https://WWW.UDEMY.COM.:443/course/x/":                 CategoryCourses,
		"https://evil.example@udemy.com/":                      CategoryCourses,
		"https://www.linkedin.com/learning":                    CategoryCourses,
		"https://www.linkedin.com/learning/course/intro":       CategoryCourses,
		"https://LinkedIn.com/LEARNING/x":                      CategoryCourses,
		"https://www.linkedin.com//learning/x":                 CategoryCourses,
		"https://www.linkedin.com/%6Cearning/x":                CategoryCourses,
		"https://www.linkedin.com/feed/../learning/x":          CategoryCourses,
		"https://www.linkedin.com/learning;jsessionid=1":       CategoryCourses,
		"https://m.bilibili.com/cheese/play/ep1":               CategoryCourses,
		"blob:https://www.udemy.com/0000":                      CategoryCourses,
		"HTTP://www.coursera.org:8080/learn/x":                 CategoryCourses,
	}
	for u, want := range blocked {
		if got := URLCategory(u); got != want {
			t.Errorf("URLCategory(%q) = %q, want %q", u, got, want)
		}
	}
	for _, u := range []string{"", "not a url", "ftp://www.udemy.com/", "https://www.linkedin.com/feed/",
		"https://www.linkedin.com/learning-login/", "https://www.bilibili.com/video/BV1", "https://www.bilibili.com/cheesecake",
		"http://udemy.com.evil.example/course", "https://lms.example.edu/?u=https://www.udemy.com/", "https://www.oreilly.com/library/",
		"https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1", "https://www.yuketang.cn/v2/web/index", "https://www.open.edu/openlearn/"} {
		if got := URLCategory(u); got != "" {
			t.Errorf("URLCategory(%q) = %q, want none", u, got)
		}
	}
	if got := NormalizePath("/A//B/./c/../D/"); got != "/a/b/d/" {
		t.Errorf("NormalizePath: %q", got)
	}
}

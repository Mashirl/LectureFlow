// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// testMPD has a video set (SegmentTemplate with a SegmentTimeline, two
// Representations), a subtitle set (not counted), and two audio sets
// (SegmentList) told apart by language.
const testMPD = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:xlink="http://www.w3.org/1999/xlink" type="static"
     mediaPresentationDuration="PT8S" minBufferTime="PT2S" profiles="urn:mpeg:dash:profile:isoff-live:2011">
  <Period id="p0">
    <AdaptationSet contentType="video" mimeType="video/mp4" segmentAlignment="true">
      <SegmentTemplate timescale="1000" initialization="v/$RepresentationID$/init.mp4"
                       media="v/$RepresentationID$/seg-$Number%05d$-$Time$.m4s" startNumber="1">
        <SegmentTimeline><S t="0" d="2000" r="2"/><S d="2000"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v1" bandwidth="500000" width="640" height="360" codecs="avc1.4d401e"/>
      <Representation id="v2" bandwidth="1500000" width="1280" height="720" codecs="avc1.64001f"/>
    </AdaptationSet>
    <AdaptationSet contentType="text" mimeType="text/vtt" lang="en">
      <Representation id="sub" bandwidth="100"><BaseURL>sub/en.vtt</BaseURL></Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="EN">
      <Representation id="a-en" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentList timescale="1" duration="4">
          <Initialization sourceURL="a/en/init.m4a"/>
          <SegmentURL media="a/en/1.m4s"/><SegmentURL media="a/en/2.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet lang="fr" codecs="mp4a.40.2">
      <Representation id="a-fr" bandwidth="96000">
        <SegmentList>
          <Initialization sourceURL="a/fr/init.m4a"/>
          <SegmentURL media="a/fr/1.m4s"/><SegmentURL media="https://other.example.net/a/fr/2.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func mustMPD(t *testing.T, body, src string) *mpd {
	t.Helper()
	m, err := parseMPD([]byte(body), mustURL(t, src))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustSource(t *testing.T, m *mpd, kind, selector string) *dashSource {
	t.Helper()
	s, err := m.find(kind, selector)
	if err != nil {
		t.Fatal(err)
	}
	src, err := m.source(s)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestDASHSelectAndTemplate(t *testing.T) {
	m := mustMPD(t, testMPD, "https://media.example.edu/s/abc/manifest.mpd?Policy=x")
	var sels []string
	for _, s := range m.sets {
		sels = append(sels, s.selector())
	}
	// the extension's selectors: kind:lang:ordinal, lang lower-cased,
	// "-" for none, text sets not counted
	if !slices.Equal(sels, []string{"video:-:0", "audio:en:0", "audio:fr:0"}) {
		t.Fatalf("selectors %v", sels)
	}
	v := mustSource(t, m, bridge.KindVideo, "video:-:0")
	// the Representation of the highest bandwidth; the timeline's times
	// and numbers; the MPD's query on its own host (ruling 6)
	if v.init != "https://media.example.edu/s/abc/v/v2/init.mp4?Policy=x" || v.ext != ".mp4" || v.single != "" {
		t.Fatalf("video %+v", v)
	}
	want := []string{"seg-00001-0", "seg-00002-2000", "seg-00003-4000", "seg-00004-6000"}
	if len(v.media) != len(want) {
		t.Fatalf("media %v", v.media)
	}
	for k, w := range want {
		if v.media[k] != "https://media.example.edu/s/abc/v/v2/"+w+".m4s?Policy=x" {
			t.Errorf("segment %d: %s", k, v.media[k])
		}
	}
	if r := v.resources(); len(r) != 5 || r[0] != v.init {
		t.Fatalf("resources %v", r)
	}
	fr := mustSource(t, m, bridge.KindAudio, "audio:fr:0")
	if fr.init != "https://media.example.edu/s/abc/a/fr/init.m4a?Policy=x" || fr.ext != ".m4a" ||
		!slices.Equal(fr.media, []string{"https://media.example.edu/s/abc/a/fr/1.m4s?Policy=x", "https://other.example.net/a/fr/2.m4s"}) {
		t.Fatalf("fr %+v", fr)
	}
	en := mustSource(t, m, bridge.KindAudio, "audio:en:0")
	if len(en.media) != 2 || !strings.HasSuffix(en.media[1], "/a/en/2.m4s?Policy=x") {
		t.Fatalf("en %+v", en)
	}
	// no selector: the first set of the track's kind; a selector naming
	// nothing (the MPD changed): the first of the selector's kind
	if s, _ := m.find(bridge.KindAudio, ""); s.selector() != "audio:en:0" {
		t.Fatalf("no selector: %s", s.selector())
	}
	if s, _ := m.find(bridge.KindUnknown, "audio:de:0"); s.selector() != "audio:en:0" {
		t.Fatalf("stale selector: %s", s.selector())
	}
	if a := m.audioFor(); a == nil || a.selector() != "audio:en:0" {
		t.Fatalf("audio for the video %+v", a)
	}
}

func TestDASHTemplateForms(t *testing.T) {
	// an open timeline (r=-1) runs to the end of the Period
	open := strings.Replace(testMPD, `<S t="0" d="2000" r="2"/><S d="2000"/>`, `<S t="0" d="2000" r="-1"/>`, 1)
	if v := mustSource(t, mustMPD(t, open, "https://m.example.edu/x.mpd"), bridge.KindVideo, "video:-:0"); len(v.media) != 4 ||
		!strings.HasSuffix(v.media[3], "seg-00004-6000.m4s") {
		t.Fatalf("open timeline %v", v.media)
	}
	// @duration without a timeline: ceil(7 s / 2 s) segments from
	// startNumber; $Bandwidth$; BaseURLs down the levels on another host
	// (no query of the MPD there)
	byDuration := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" mediaPresentationDuration="PT7S">
  <BaseURL>https://cdn.example.net/root/</BaseURL>
  <Period><BaseURL>p/</BaseURL>
    <AdaptationSet mimeType="video/webm" codecs="vp9">
      <BaseURL>v/</BaseURL>
      <SegmentTemplate duration="2" startNumber="0" media="$Bandwidth$/$Number$.webm" initialization="$Bandwidth$/init.webm"/>
      <Representation id="r" bandwidth="900"/>
    </AdaptationSet>
  </Period>
</MPD>`
	v := mustSource(t, mustMPD(t, byDuration, "https://media.example.edu/x.mpd?sig=1"), bridge.KindVideo, "video:-:0")
	if v.init != "https://cdn.example.net/root/p/v/900/init.webm" || v.ext != ".webm" ||
		!slices.Equal(v.media, []string{"https://cdn.example.net/root/p/v/900/0.webm", "https://cdn.example.net/root/p/v/900/1.webm",
			"https://cdn.example.net/root/p/v/900/2.webm", "https://cdn.example.net/root/p/v/900/3.webm"}) {
		t.Fatalf("by duration %+v", v)
	}
	// a Representation's own SegmentTemplate overrides the set's; $$
	override := `<MPD mediaPresentationDuration="PT4S"><Period>
    <AdaptationSet contentType="audio">
      <SegmentTemplate timescale="10" duration="20" media="all/$Number$.m4s"/>
      <Representation id="x" bandwidth="1"><SegmentTemplate media="own$$/$Number%03d$.m4s"/></Representation>
    </AdaptationSet></Period></MPD>`
	a := mustSource(t, mustMPD(t, override, "https://m.example.edu/d/x.mpd"), bridge.KindAudio, "")
	if a.init != "" || !slices.Equal(a.media, []string{"https://m.example.edu/d/own$/001.m4s", "https://m.example.edu/d/own$/002.m4s"}) {
		t.Fatalf("override %+v", a)
	}
}

func TestDASHSingleFile(t *testing.T) {
	segBase := `<MPD mediaPresentationDuration="PT60S"><Period><AdaptationSet contentType="audio" mimeType="audio/mp4">
    <Representation id="a" bandwidth="1"><BaseURL>audio.m4a</BaseURL><SegmentBase indexRange="800-1000"><Initialization range="0-799"/></SegmentBase></Representation>
  </AdaptationSet></Period></MPD>`
	a := mustSource(t, mustMPD(t, segBase, "https://m.example.edu/d/x.mpd?t=1"), bridge.KindAudio, "audio:-:0")
	if a.single != "https://m.example.edu/d/audio.m4a?t=1" || len(a.resources()) != 1 {
		t.Fatalf("SegmentBase %+v", a)
	}
	// byte ranges of the BaseURL's one file: the whole file
	ranged := `<MPD mediaPresentationDuration="PT60S"><Period><AdaptationSet contentType="video">
    <Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL>
      <SegmentList><Initialization range="0-99"/><SegmentURL mediaRange="100-199"/><SegmentURL mediaRange="200-299"/></SegmentList>
    </Representation></AdaptationSet></Period></MPD>`
	if v := mustSource(t, mustMPD(t, ranged, "https://m.example.edu/d/x.mpd"), bridge.KindVideo, ""); v.single != "https://m.example.edu/d/v.mp4" {
		t.Fatalf("ranged list %+v", v)
	}
	// byte ranges of separate files: unsupported
	split := strings.Replace(ranged, `<SegmentURL mediaRange="100-199"/>`, `<SegmentURL media="a.mp4" mediaRange="100-199"/>`, 1)
	m := mustMPD(t, split, "https://m.example.edu/d/x.mpd")
	s, _ := m.find(bridge.KindVideo, "")
	if _, err := m.source(s); featureOf(err) != "byte_range" {
		t.Fatalf("split ranges: %v", err)
	}
}

func featureOf(err error) string {
	var pe *errPlaylist
	if errors.As(err, &pe) {
		return pe.feature
	}
	return ""
}

func TestDASHRefusals(t *testing.T) {
	base := "https://m.example.edu/d/x.mpd"
	drm := strings.Replace(testMPD, `<AdaptationSet contentType="video" mimeType="video/mp4" segmentAlignment="true">`,
		`<AdaptationSet contentType="video" mimeType="video/mp4"><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>`, 1)
	m := mustMPD(t, drm, base)
	s, _ := m.find(bridge.KindVideo, "video:-:0")
	if _, err := m.source(s); featureOf(err) != "drm" {
		t.Fatalf("drm: %v", err)
	} else if f := playlistFailure(err).(*failure); f.code != CodeDRMProtected {
		t.Fatalf("drm failure %+v", f)
	}
	// the audio of the same MPD has no DRM: it still downloads
	if _ = mustSource(t, m, bridge.KindAudio, "audio:en:0"); m.audioFor() == nil {
		t.Fatal("audio")
	}
	repDRM := strings.Replace(testMPD, `<Representation id="a-en" bandwidth="128000" codecs="mp4a.40.2">`,
		`<Representation id="a-en" bandwidth="128000" codecs="mp4a.40.2"><ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"/>`, 1)
	m = mustMPD(t, repDRM, base)
	s, _ = m.find(bridge.KindAudio, "audio:en:0")
	if _, err := m.source(s); featureOf(err) != "drm" {
		t.Fatalf("representation drm: %v", err)
	}
	if a := m.audioFor(); a == nil || a.selector() != "audio:fr:0" {
		t.Fatalf("audio without drm %+v", a)
	}
	cases := map[string]string{
		"live":         strings.Replace(testMPD, `type="static"`, `type="dynamic"`, 1),
		"multi_period": strings.Replace(testMPD, `</Period>`, `</Period><Period id="p1"><AdaptationSet contentType="audio"/></Period>`, 1),
		"xlink":        strings.Replace(testMPD, `<Period id="p0">`, `<Period id="p0" xlink:href="https://x.example/p.xml">`, 1),
	}
	for feature, body := range cases {
		_, err := parseMPD([]byte(body), mustURL(t, base))
		if featureOf(err) != feature {
			t.Errorf("%s: %v", feature, err)
		}
		if f := playlistFailure(err).(*failure); f.code != CodePlaylistUnsupported || f.params["feature"] != feature {
			t.Errorf("%s: %+v", feature, f)
		}
	}
	for name, body := range map[string]string{
		"not xml":     "#EXTM3U\n",
		"html":        "<html><body>login</body></html>",
		"no period":   `<MPD></MPD>`,
		"only text":   `<MPD><Period><AdaptationSet contentType="text"/></Period></MPD>`,
		"broken":      `<MPD><Period>`,
		"entity bomb": `<!DOCTYPE MPD [<!ENTITY a "aaaaaaaa"><!ENTITY b "&a;&a;&a;&a;">]><MPD>&b;</MPD>`,
	} {
		if _, err := parseMPD([]byte(body), mustURL(t, base)); err == nil || featureOf(err) != "" {
			t.Errorf("%s: %v", name, err)
		} else if f := playlistFailure(err).(*failure); f.code != CodePlaylistInvalid {
			t.Errorf("%s: %+v", name, f)
		}
	}
	// a template the MPD gets wrong
	for _, media := range []string{"$Foo$.m4s", "$Number.m4s", "$RepresentationID%05d$.m4s", "$Number%5d$.m4s"} {
		body := strings.Replace(testMPD, "v/$RepresentationID$/seg-$Number%05d$-$Time$.m4s", media, 1)
		m := mustMPD(t, body, base)
		s, _ := m.find(bridge.KindVideo, "")
		if _, err := m.source(s); err == nil || featureOf(err) != "" {
			t.Errorf("%s: %v", media, err)
		}
	}
	// a timeline that never ends
	open := strings.Replace(strings.Replace(testMPD, `<S t="0" d="2000" r="2"/><S d="2000"/>`, `<S t="0" d="2000" r="-1"/>`, 1),
		` mediaPresentationDuration="PT8S"`, "", 1)
	m = mustMPD(t, open, base)
	s, _ = m.find(bridge.KindVideo, "")
	if _, err := m.source(s); err == nil {
		t.Fatal("an open timeline without a length accepted")
	}
}

// Kinds as the extension counts them: contentType, ContentComponent,
// mimeType, codecs.
func TestDASHKinds(t *testing.T) {
	body := `<MPD mediaPresentationDuration="PT2S"><Period>
  <AdaptationSet><ContentComponent contentType="video"/><Representation id="1" bandwidth="1"><BaseURL>1.mp4</BaseURL></Representation></AdaptationSet>
  <AdaptationSet><Representation id="2" mimeType="audio/mp4" bandwidth="1"><BaseURL>2.m4a</BaseURL></Representation></AdaptationSet>
  <AdaptationSet codecs="ec-3"><Representation id="3" bandwidth="1"><BaseURL>3.m4a</BaseURL></Representation></AdaptationSet>
  <AdaptationSet><Representation id="4" codecs="stpp" bandwidth="1"><BaseURL>4.mp4</BaseURL></Representation></AdaptationSet>
  <AdaptationSet><Representation id="5" codecs="foo" bandwidth="1"><BaseURL>5.bin</BaseURL></Representation></AdaptationSet>
</Period></MPD>`
	m := mustMPD(t, string([]byte{0xef, 0xbb, 0xbf})+body, "https://m.example.edu/x.mpd") // with a BOM
	var sels []string
	for _, s := range m.sets {
		sels = append(sels, s.selector())
	}
	if !slices.Equal(sels, []string{"video:-:0", "audio:-:0", "audio:-:1", "unknown:-:0"}) {
		t.Fatalf("%v", sels)
	}
	if d, ok := parseISODuration("PT1H2M3.5S"); !ok || d != 3723.5 {
		t.Fatalf("duration %v %v", d, ok)
	}
	if _, ok := parseISODuration("P"); ok {
		t.Fatal("P accepted")
	}
}

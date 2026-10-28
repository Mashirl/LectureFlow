// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// Native fuzz tests of the DASH parser (go test -fuzz=FuzzXxx
// ./internal/fetch). Under a plain go test they run their seeds only.

// FuzzParseMPD: an MPD parses without panicking, its selectors find their
// sets, and every set plans a bounded list of usable URLs (or refuses with
// a playlist error).
func FuzzParseMPD(f *testing.F) {
	for _, s := range []string{testMPD,
		`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" mediaPresentationDuration="PT7S"><BaseURL>https://cdn.example.net/root/</BaseURL><Period><BaseURL>p/</BaseURL><AdaptationSet mimeType="video/webm" codecs="vp9"><BaseURL>v/</BaseURL><SegmentTemplate duration="2" startNumber="0" media="$Bandwidth$/$Number$.webm" initialization="$Bandwidth$/init.webm"/><Representation id="r" bandwidth="900"/></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="PT4S"><Period><AdaptationSet contentType="audio"><SegmentTemplate timescale="10" duration="20" media="all/$Number$.m4s"/><Representation id="x" bandwidth="1"><SegmentTemplate media="own$$/$Number%03d$.m4s"/></Representation></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="PT60S"><Period><AdaptationSet contentType="audio" mimeType="audio/mp4"><Representation id="a" bandwidth="1"><BaseURL>audio.m4a</BaseURL><SegmentBase indexRange="800-1000"><Initialization range="0-799"/></SegmentBase></Representation></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="PT60S"><Period><AdaptationSet contentType="video"><Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL><SegmentList><Initialization range="0-99"/><SegmentURL mediaRange="100-199"/></SegmentList></Representation></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="P99999999999999999999Y"><Period start="P99999999999999999999Y"><AdaptationSet contentType="video"><SegmentTemplate duration="1" media="$Number$"/><Representation id="v"/></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="PT1S"><Period><AdaptationSet contentType="video"><SegmentTemplate media="$Time$"><SegmentTimeline><S t="9223372036854775806" d="9223372036854775807" r="-1"/></SegmentTimeline></SegmentTemplate><Representation id="v"/></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="PT100000S"><Period><AdaptationSet contentType="video"><SegmentTemplate duration="1" media="$RepresentationID$$RepresentationID$$RepresentationID$$RepresentationID$"/><Representation id="` + strings.Repeat("i", 2000) + `"/></AdaptationSet></Period></MPD>`,
		`<!DOCTYPE MPD [<!ENTITY a "aaaaaaaa"><!ENTITY b "&a;&a;&a;&a;">]><MPD>&b;</MPD>`,
		`<?xml version="1.0" encoding="ISO-8859-1"?><MPD/>`,
		`<MPD><Period><AdaptationSet lang="-" contentType="audio"/><AdaptationSet contentType="audio"/></Period></MPD>`,
	} {
		f.Add(uint8(0), []byte(s))
	}
	f.Fuzz(func(t *testing.T, bi uint8, body []byte) {
		src := fuzzBase(t, bi)
		m, err := parseMPD(body, src)
		if err != nil {
			return
		}
		if len(m.sets) == 0 {
			t.Fatal("no sets")
		}
		for i := range m.sets {
			s := &m.sets[i]
			if s.kind != bridge.KindAudio && s.kind != bridge.KindVideo && s.kind != bridge.KindUnknown {
				t.Fatalf("set kind %q", s.kind)
			}
			got, err := m.find(s.kind, s.selector())
			if err != nil || got.kind != s.kind {
				t.Fatalf("find(%q) = %+v, %v", s.selector(), got, err)
			}
			d, err := m.source(s)
			if err != nil {
				continue
			}
			if d.ext != ".mp4" && d.ext != ".m4a" && d.ext != ".webm" {
				t.Fatalf("ext %q", d.ext)
			}
			res := d.resources()
			if len(res) == 0 || len(d.media) > maxDASHSegments || (d.single != "" && (d.init != "" || len(d.media) > 0)) {
				t.Fatalf("plan %d resources, %d segments, single %q", len(res), len(d.media), d.single)
			}
			planned := 0
			for _, u := range res {
				checkPlanURL(t, "dash", u)
				planned += len(u)
			}
			if planned > maxPlanBytes {
				t.Fatalf("%d bytes of URLs planned (max %d)", planned, maxPlanBytes)
			}
		}
		if a := m.audioFor(); a != nil && (a.kind != bridge.KindAudio || protected(a.node)) {
			t.Fatalf("audioFor %+v", a)
		}
	})
}

// FuzzExpandTemplate: a SegmentTemplate expands without panicking, and its
// output grows only by the identifiers' values.
func FuzzExpandTemplate(f *testing.F) {
	for _, s := range []string{"v/$RepresentationID$/seg-$Number%05d$-$Time$.m4s", "own$$/$Number%03d$.m4s", "$Bandwidth$/init.webm",
		"$Foo$.m4s", "$Number.m4s", "$RepresentationID%05d$.m4s", "$Number%5d$.m4s", "$Number%032d$", "$Number%0999999999999d$", "$$$", "$"} {
		f.Add(s, "v1", int64(1), int64(-9223372036854775808), true)
	}
	f.Fuzz(func(t *testing.T, tmpl, repID string, number, tm int64, segment bool) {
		out, err := expandTemplate(tmpl, tmplVars{repID: repID, bandwidth: number, number: number, time: tm}, segment)
		if err != nil {
			return
		}
		ids := strings.Count(tmpl, "$")/2 + 1
		if max := len(tmpl) + ids*(len(repID)+32+20); len(out) > max {
			t.Fatalf("expandTemplate(%q) gave %d bytes (max %d)", tmpl, len(out), max)
		}
	})
}

// FuzzISODuration: no panic; a duration that parses is not negative.
func FuzzISODuration(f *testing.F) {
	for _, s := range []string{"PT1H2M3.5S", "P", "PT", "P1Y2M3W4DT5H6M7S", "P99999999999999999999999Y", "PT-1S", "P1.5.5D", " PT1S "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if d, ok := parseISODuration(s); ok && !(d >= 0) {
			t.Fatalf("parseISODuration(%q) = %v", s, d)
		}
	})
}

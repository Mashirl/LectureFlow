// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"lectureflow/internal/bridge"
)

// DASH (lead's ruling 7). The bundled ffmpeg has neither TLS nor a DASH
// demuxer, so Go reads the MPD (with the task's cookies, like an HLS
// playlist), picks the AdaptationSet the extension's selector names and
// its Representation of the highest bandwidth, and lists the URLs:
// SegmentTemplate ($Number$, $Time$, $RepresentationID$, $Bandwidth$, with
// or without a SegmentTimeline), SegmentList, or one file (SegmentBase or
// a bare BaseURL). aria2c downloads them; the init section and the
// segments joined in order make one fragmented MP4 (or WebM), which the
// bundled ffmpeg remuxes offline (-c copy). ContentProtection is answered
// downloads.drm_protected; not supported, answered
// downloads.playlist_unsupported {feature}: a dynamic MPD ("live"), more
// than one Period ("multi_period"), byte ranges of separate files
// ("byte_range"), remote elements ("xlink").

// maxDASHSegments bounds the URLs of one Representation (a 3-hour
// recording in 1-second segments is about 11,000).
const maxDASHSegments = 200_000

// maxMPDElements bounds the elements of an MPD: room for a SegmentList or
// a SegmentTimeline of maxDASHSegments entries and more, while a 16 MB
// document of empty elements no longer takes half a gigabyte to refuse
// (fuzz, NOTES; it was 2,000,000).
const maxMPDElements = 500_000

// ---------- a small XML tree ----------

type xnode struct {
	name     string  // local name
	attrs    []xattr // local names; xlink attributes as "xlink:<name>"
	children []*xnode
	text     string // character data (kept up to 4 KB)
}

// xattr is one attribute (a slice of them takes a fraction of a map's
// memory, and an element has few).
type xattr struct{ k, v string }

// get returns the value of attribute k (the last one of that name).
func (n *xnode) get(k string) (string, bool) {
	if n == nil {
		return "", false
	}
	v, ok := "", false
	for _, a := range n.attrs {
		if a.k == k {
			v, ok = a.v, true
		}
	}
	return v, ok
}

func (n *xnode) attr(k string) string {
	v, _ := n.get(k)
	return strings.TrimSpace(v)
}

func (n *xnode) has(k string) bool {
	_, ok := n.get(k)
	return ok
}

// kids returns the child elements of a local name.
func (n *xnode) kids(name string) []*xnode {
	if n == nil {
		return nil
	}
	var out []*xnode
	for _, c := range n.children {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func (n *xnode) child(name string) *xnode {
	if k := n.kids(name); len(k) > 0 {
		return k[0]
	}
	return nil
}

const xlinkNS = "http://www.w3.org/1999/xlink"

// parseXMLTree reads a whole document. encoding/xml expands no external or
// custom entities, so an entity bomb cannot grow it; size, depth and the
// number of elements are bounded besides. Its error texts are not passed
// on: they could quote a piece of the document.
func parseXMLTree(body []byte) (*xnode, error) {
	body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		switch strings.ToLower(label) {
		case "utf-8", "utf8", "us-ascii", "ascii":
			return in, nil
		}
		return nil, errors.New("charset")
	}
	var root *xnode
	var stack []*xnode
	elements := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, badPlaylist("the MPD is not well-formed XML")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			elements++
			if elements > maxMPDElements || len(stack) >= 64 {
				return nil, badPlaylist("the MPD is too large")
			}
			n := &xnode{name: t.Name.Local}
			if len(t.Attr) > 0 {
				n.attrs = make([]xattr, 0, len(t.Attr))
			}
			for _, a := range t.Attr {
				k := a.Name.Local
				if a.Name.Space == xlinkNS || a.Name.Space == "xlink" {
					k = "xlink:" + k
				}
				n.attrs = append(n.attrs, xattr{k, a.Value})
			}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			} else if root == nil {
				root = n
			} else {
				return nil, badPlaylist("the MPD has two root elements")
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				if len(top.text) < 4096 {
					top.text += string(t)
				}
			}
		}
	}
	if root == nil {
		return nil, badPlaylist("empty MPD")
	}
	return root, nil
}

// ---------- the MPD ----------

// The kinds of an AdaptationSet as the extension's dash.js counts them
// (its selector "kind:lang:ordinal" must name the same set here).
var (
	dashAudioCodec = regexp.MustCompile(`(?i)^(mp4a|ac-3|ec-3|ac-4|opus|flac|mp3|vorbis|alac|dtsc|dtse|dtsh|dtsl)`)
	dashVideoCodec = regexp.MustCompile(`(?i)^(avc1|avc3|hvc1|hev1|av01|vp09|vp8|vp9|mp4v|dvh1|dvhe|dva1|dvav|theora)`)
	dashTextCodec  = regexp.MustCompile(`(?i)^(stpp|wvtt)`)
)

const kindText = "text"

func kindOfCodecList(codecs string) string {
	var list []string
	for _, c := range strings.Split(codecs, ",") {
		if c = strings.TrimSpace(c); c != "" {
			list = append(list, c)
		}
	}
	if len(list) == 0 {
		return bridge.KindUnknown
	}
	for _, c := range list {
		if dashVideoCodec.MatchString(c) {
			return bridge.KindVideo
		}
	}
	for _, c := range list {
		if !dashAudioCodec.MatchString(c) {
			return bridge.KindUnknown
		}
	}
	return bridge.KindAudio
}

func kindOfMime(mime string) string {
	m := strings.ToLower(mime)
	switch {
	case strings.HasPrefix(m, "audio/"):
		return bridge.KindAudio
	case strings.HasPrefix(m, "video/"):
		return bridge.KindVideo
	case strings.HasPrefix(m, "text/"), strings.HasPrefix(m, "image/"), strings.HasPrefix(m, "application/ttml"):
		return kindText
	}
	return ""
}

func kindOfSet(as *xnode) string {
	switch ct := strings.ToLower(as.attr("contentType")); ct {
	case "audio", "video":
		return ct
	case "text", "image":
		return kindText
	}
	for _, cc := range as.kids("ContentComponent") {
		switch strings.ToLower(cc.attr("contentType")) {
		case "video":
			return bridge.KindVideo
		case "audio":
			return bridge.KindAudio
		}
	}
	if k := kindOfMime(as.attr("mimeType")); k != "" {
		return k
	}
	reps := as.kids("Representation")
	for _, r := range reps {
		if k := kindOfMime(r.attr("mimeType")); k != "" {
			return k
		}
	}
	codecs := as.attr("codecs")
	if codecs == "" {
		var cs []string
		for _, r := range reps {
			cs = append(cs, r.attr("codecs"))
		}
		codecs = strings.Join(cs, ",")
	}
	if dashTextCodec.MatchString(codecs) {
		return kindText
	}
	return kindOfCodecList(codecs)
}

type dashSet struct {
	node    *xnode
	kind    string
	lang    string
	ordinal int
}

func (s *dashSet) selector() string {
	lang := s.lang
	if lang == "" {
		lang = "-"
	}
	return s.kind + ":" + lang + ":" + strconv.Itoa(s.ordinal)
}

type mpd struct {
	root, period *xnode
	src          *url.URL // the MPD's URL after redirects
	periodBase   *url.URL
	periodDur    float64 // seconds; 0 = unknown
	sets         []dashSet
}

var isoDuration = regexp.MustCompile(`^P(?:(\d+(?:\.\d+)?)Y)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)W)?(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// parseISODuration reads "PT1H2M3.5S" (years 365 days, months 30); false
// when s is not a duration.
func parseISODuration(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	m := isoDuration.FindStringSubmatch(s)
	if m == nil || s == "P" || strings.HasSuffix(s, "T") {
		return 0, false
	}
	units := []float64{365 * 86400, 30 * 86400, 7 * 86400, 86400, 3600, 60, 1}
	var total float64
	for i, u := range units {
		if m[i+1] != "" {
			v, _ := strconv.ParseFloat(m[i+1], 64)
			total += v * u
		}
	}
	return total, true
}

// dashResolve makes ref absolute against base (a BaseURL, a segment);
// like an HLS URI it takes base's query when it has none of its own and
// stays on base's host (inheritQuery).
func dashResolve(base *url.URL, ref string) (*url.URL, error) {
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, badPlaylist("bad URL in the MPD")
	}
	u := base.ResolveReference(r)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, badPlaylist("URL scheme %q in the MPD", u.Scheme)
	}
	if u.Host == "" { // ("http:" alone; fuzz)
		return nil, badPlaylist("URL without a host in the MPD")
	}
	u.Fragment = ""
	inheritQuery(base, u)
	if _, err := url.Parse(u.String()); err != nil { // ("//::" parses, "https://::" does not; fuzz)
		return nil, badPlaylist("bad URL in the MPD")
	}
	return u, nil
}

// baseOf applies the first BaseURL child of n to parent.
func baseOf(n *xnode, parent *url.URL) (*url.URL, error) {
	b := n.child("BaseURL")
	if b == nil || strings.TrimSpace(b.text) == "" {
		return parent, nil
	}
	return dashResolve(parent, b.text)
}

func parseMPD(body []byte, src *url.URL) (*mpd, error) {
	root, err := parseXMLTree(body)
	if err != nil {
		return nil, err
	}
	if root.name != "MPD" {
		return nil, badPlaylist("not an MPD")
	}
	if strings.EqualFold(root.attr("type"), "dynamic") {
		return nil, &errPlaylist{feature: "live"}
	}
	periods := root.kids("Period")
	switch {
	case len(periods) == 0:
		return nil, badPlaylist("MPD without a Period")
	case len(periods) > 1:
		return nil, &errPlaylist{feature: "multi_period"}
	}
	m := &mpd{root: root, period: periods[0], src: src}
	if m.period.has("xlink:href") {
		return nil, &errPlaylist{feature: "xlink"}
	}
	mpdBase, err := baseOf(root, src)
	if err != nil {
		return nil, err
	}
	if m.periodBase, err = baseOf(m.period, mpdBase); err != nil {
		return nil, err
	}
	if d, ok := parseISODuration(m.period.attr("duration")); ok && d > 0 {
		m.periodDur = d
	} else if d, ok := parseISODuration(root.attr("mediaPresentationDuration")); ok && d > 0 {
		start, _ := parseISODuration(m.period.attr("start"))
		m.periodDur = d - start
	}
	ordinals := map[string]int{}
	for _, as := range m.period.kids("AdaptationSet") {
		kind := kindOfSet(as)
		if kind == kindText {
			continue
		}
		lang := strings.ToLower(as.attr("lang"))
		key := kind + "\x00" + lang
		m.sets = append(m.sets, dashSet{node: as, kind: kind, lang: lang, ordinal: ordinals[key]})
		ordinals[key]++
	}
	if len(m.sets) == 0 {
		return nil, badPlaylist("MPD without audio or video")
	}
	return m, nil
}

// find returns the AdaptationSet of a track: the one its selector names;
// without a selector (or when the MPD changed and it names nothing) the
// first set of the track's kind.
func (m *mpd) find(kind, selector string) (*dashSet, error) {
	if selector != "" {
		for i := range m.sets {
			if m.sets[i].selector() == selector {
				return &m.sets[i], nil
			}
		}
		if k, _, ok := strings.Cut(selector, ":"); ok && k != "" {
			kind = k
		}
	}
	for i := range m.sets {
		if m.sets[i].kind == kind || kind == "" || kind == bridge.KindUnknown {
			return &m.sets[i], nil
		}
	}
	return nil, badPlaylist("the MPD has no %s adaptation set", kind)
}

// audioFor is the audio of a video set when the task has no audio track
// of its own: the first audio set without DRM (nil = none).
func (m *mpd) audioFor() *dashSet {
	for i := range m.sets {
		if m.sets[i].kind == bridge.KindAudio && !protected(m.sets[i].node) {
			return &m.sets[i]
		}
	}
	return nil
}

func protected(as *xnode) bool {
	if len(as.kids("ContentProtection")) > 0 {
		return true
	}
	for _, r := range as.kids("Representation") {
		if len(r.kids("ContentProtection")) > 0 {
			return true
		}
	}
	return false
}

// dashSource is what one Representation needs downloaded.
type dashSource struct {
	single string   // the whole file (SegmentBase, a bare BaseURL, byte ranges of one file)
	init   string   // the init section ("" = none)
	media  []string // the segments, in order
	ext    string   // container of the local files: .mp4, .m4a or .webm
}

// resources lists the URLs to download, init section first.
func (d *dashSource) resources() []string {
	if d.single != "" {
		return []string{d.single}
	}
	var out []string
	if d.init != "" {
		out = append(out, d.init)
	}
	return append(out, d.media...)
}

// source plans the download of a set: its Representation of the highest
// bandwidth (then size), and that one's URLs.
func (m *mpd) source(s *dashSet) (*dashSource, error) {
	if protected(s.node) {
		return nil, &errPlaylist{feature: "drm"}
	}
	if s.node.has("xlink:href") {
		return nil, &errPlaylist{feature: "xlink"}
	}
	reps := s.node.kids("Representation")
	if len(reps) == 0 {
		return nil, badPlaylist("adaptation set without representations")
	}
	num := func(n *xnode, k string) int64 {
		v, _ := strconv.ParseInt(n.attr(k), 10, 64)
		return v
	}
	sort.SliceStable(reps, func(i, j int) bool {
		if bi, bj := num(reps[i], "bandwidth"), num(reps[j], "bandwidth"); bi != bj {
			return bi > bj
		}
		return num(reps[i], "width")*num(reps[i], "height") > num(reps[j], "width")*num(reps[j], "height")
	})
	rep := reps[0]
	asBase, err := baseOf(s.node, m.periodBase)
	if err != nil {
		return nil, err
	}
	repBase, err := baseOf(rep, asBase)
	if err != nil {
		return nil, err
	}
	src := &dashSource{ext: dashExt(firstNonEmpty(rep.attr("mimeType"), s.node.attr("mimeType")), s.kind)}
	levels := []*xnode{m.period, s.node, rep}
	addressing := ""
	for i := len(levels) - 1; i >= 0 && addressing == ""; i-- {
		for _, k := range []string{"SegmentTemplate", "SegmentList", "SegmentBase"} {
			if levels[i].child(k) != nil {
				addressing = k
				break
			}
		}
	}
	vars := tmplVars{repID: rep.attr("id"), bandwidth: num(rep, "bandwidth")}
	// the URLs a template expands to are bounded like a playlist's: a long
	// $RepresentationID$ or query times many segments (fuzz)
	var budget planBudget
	switch addressing {
	case "SegmentTemplate":
		err = m.template(src, levels, repBase, vars, &budget)
	case "SegmentList":
		err = m.list(src, levels, repBase, &budget)
	default: // SegmentBase, or the BaseURL alone: one file read with ranges
		src.single = repBase.String()
	}
	if err != nil {
		return nil, err
	}
	if n := len(src.media); n > maxDASHSegments {
		return nil, badPlaylist("too many segments (%d)", n)
	}
	return src, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func dashExt(mime, kind string) string {
	if strings.Contains(strings.ToLower(mime), "webm") {
		return ".webm"
	}
	return defaultExt(kind)
}

// template lists the URLs of a SegmentTemplate. Its attributes are
// inherited down the levels (Period, AdaptationSet, Representation), a
// lower level overriding a higher one.
func (m *mpd) template(src *dashSource, levels []*xnode, base *url.URL, vars tmplVars, budget *planBudget) error {
	attrs := map[string]string{}
	var timeline, initEl *xnode
	for _, l := range levels {
		t := l.child("SegmentTemplate")
		if t == nil {
			continue
		}
		for _, a := range t.attrs {
			attrs[a.k] = strings.TrimSpace(a.v)
		}
		if tl := t.child("SegmentTimeline"); tl != nil {
			timeline = tl
		}
		if in := t.child("Initialization"); in != nil {
			initEl = in
		}
	}
	initT := attrs["initialization"]
	if initT == "" && initEl != nil {
		if initEl.attr("range") != "" {
			return &errPlaylist{feature: "byte_range"}
		}
		initT = initEl.attr("sourceURL")
	}
	if initT != "" {
		s, err := expandTemplate(initT, vars, false)
		if err != nil {
			return err
		}
		u, err := dashResolve(base, s)
		if err != nil {
			return err
		}
		src.init = u.String()
		if err := budget.add(src.init); err != nil {
			return err
		}
	}
	mediaT := attrs["media"]
	if mediaT == "" {
		return badPlaylist("SegmentTemplate without media")
	}
	intAttr := func(k string, def int64) (int64, error) {
		v, ok := attrs[k]
		if !ok || v == "" {
			return def, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, badPlaylist("SegmentTemplate @%s", k)
		}
		return n, nil
	}
	startNumber, err := intAttr("startNumber", 1)
	if err != nil {
		return err
	}
	timescale, err := intAttr("timescale", 1)
	if err != nil || timescale <= 0 {
		return badPlaylist("SegmentTemplate @timescale")
	}
	pto, err := intAttr("presentationTimeOffset", 0)
	if err != nil {
		return err
	}
	endNumber, err := intAttr("endNumber", -1)
	if err != nil {
		return err
	}
	var segs []tmplVars
	if timeline != nil {
		end := int64(-1)
		if m.periodDur > 0 {
			end = pto + int64(math.Round(m.periodDur*float64(timescale)))
		}
		if segs, err = timelineSegments(timeline, startNumber, end); err != nil {
			return err
		}
	} else {
		d, err := intAttr("duration", 0)
		if err != nil || d <= 0 {
			return badPlaylist("SegmentTemplate without a duration or a SegmentTimeline")
		}
		if m.periodDur <= 0 {
			return badPlaylist("the length of the presentation is unknown")
		}
		count := int64(math.Ceil(m.periodDur*float64(timescale)/float64(d) - 1e-9))
		if count > maxDASHSegments {
			return badPlaylist("too many segments (%d)", count)
		}
		for k := int64(0); k < count; k++ {
			segs = append(segs, tmplVars{number: startNumber + k, time: pto + k*d})
		}
	}
	for _, sv := range segs {
		if endNumber >= 0 && sv.number > endNumber {
			break
		}
		sv.repID, sv.bandwidth = vars.repID, vars.bandwidth
		s, err := expandTemplate(mediaT, sv, true)
		if err != nil {
			return err
		}
		u, err := dashResolve(base, s)
		if err != nil {
			return err
		}
		src.media = append(src.media, u.String())
		if err := budget.add(src.media[len(src.media)-1]); err != nil {
			return err
		}
	}
	if len(src.media) == 0 {
		return badPlaylist("SegmentTemplate without segments")
	}
	return nil
}

// timelineSegments lists the segments of a SegmentTimeline: S@t starts a
// run (else it follows the previous one), S@d is the length, S@r the
// repeats (-1: until the next S@t, or the end of the Period).
func timelineSegments(tl *xnode, startNumber, end int64) ([]tmplVars, error) {
	ss := tl.kids("S")
	var out []tmplVars
	var cur int64
	n := startNumber
	for i, s := range ss {
		if s.has("t") {
			t, err := strconv.ParseInt(s.attr("t"), 10, 64)
			if err != nil {
				return nil, badPlaylist("SegmentTimeline S@t")
			}
			cur = t
		}
		d, err := strconv.ParseInt(s.attr("d"), 10, 64)
		if err != nil || d <= 0 {
			return nil, badPlaylist("SegmentTimeline S@d")
		}
		r := int64(0)
		if s.has("r") {
			if r, err = strconv.ParseInt(s.attr("r"), 10, 64); err != nil {
				return nil, badPlaylist("SegmentTimeline S@r")
			}
		}
		emit := func() error {
			if len(out) >= maxDASHSegments {
				return badPlaylist("too many segments")
			}
			out = append(out, tmplVars{number: n, time: cur})
			cur += d
			n++
			return nil
		}
		if r < 0 {
			stop := end
			if i+1 < len(ss) && ss[i+1].has("t") {
				stop, _ = strconv.ParseInt(ss[i+1].attr("t"), 10, 64)
			}
			if stop < 0 {
				return nil, badPlaylist("open SegmentTimeline without the length of the presentation")
			}
			for cur < stop {
				if err := emit(); err != nil {
					return nil, err
				}
			}
			continue
		}
		for k := int64(0); k <= r; k++ {
			if err := emit(); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		return nil, badPlaylist("empty SegmentTimeline")
	}
	return out, nil
}

// list lists the URLs of a SegmentList (the lowest level that has
// SegmentURLs; its Initialization likewise).
func (m *mpd) list(src *dashSource, levels []*xnode, base *url.URL, budget *planBudget) error {
	var urls []*xnode
	var initEl *xnode
	for _, l := range levels {
		sl := l.child("SegmentList")
		if sl == nil {
			continue
		}
		if k := sl.kids("SegmentURL"); len(k) > 0 {
			urls = k
		}
		if in := sl.child("Initialization"); in != nil {
			initEl = in
		}
		if sl.has("xlink:href") {
			return &errPlaylist{feature: "xlink"}
		}
	}
	if len(urls) == 0 {
		return badPlaylist("SegmentList without segments")
	}
	ranged, bare := false, true
	for _, su := range urls {
		ranged = ranged || su.attr("mediaRange") != ""
		bare = bare && su.attr("media") == ""
	}
	initRanged := initEl != nil && initEl.attr("range") != ""
	initSrc := ""
	if initEl != nil {
		initSrc = initEl.attr("sourceURL")
	}
	if ranged || initRanged {
		// byte ranges: fine when they are all of the BaseURL's one file,
		// which is then downloaded whole
		if bare && initSrc == "" {
			src.single = base.String()
			return nil
		}
		return &errPlaylist{feature: "byte_range"}
	}
	if bare {
		src.single = base.String()
		return nil
	}
	if initSrc != "" {
		u, err := dashResolve(base, initSrc)
		if err != nil {
			return err
		}
		src.init = u.String()
		if err := budget.add(src.init); err != nil {
			return err
		}
	}
	for _, su := range urls {
		ref := su.attr("media")
		if ref == "" {
			return badPlaylist("SegmentURL without media")
		}
		u, err := dashResolve(base, ref)
		if err != nil {
			return err
		}
		src.media = append(src.media, u.String())
		if err := budget.add(src.media[len(src.media)-1]); err != nil {
			return err
		}
	}
	return nil
}

// tmplVars are the values of a template's identifiers.
type tmplVars struct {
	repID     string
	bandwidth int64
	number    int64
	time      int64
}

// expandTemplate substitutes $RepresentationID$, $Number$, $Time$,
// $Bandwidth$ (the last three with an optional %0<width>d) and $$;
// segment false is an initialization template, which has no number or
// time.
func expandTemplate(t string, v tmplVars, segment bool) (string, error) {
	var b strings.Builder
	for {
		i := strings.IndexByte(t, '$')
		if i < 0 {
			b.WriteString(t)
			return b.String(), nil
		}
		b.WriteString(t[:i])
		rest := t[i+1:]
		j := strings.IndexByte(rest, '$')
		if j < 0 {
			return "", badPlaylist("unterminated $ in a SegmentTemplate")
		}
		id, format := rest[:j], ""
		t = rest[j+1:]
		if id == "" {
			b.WriteByte('$')
			continue
		}
		if k := strings.IndexByte(id, '%'); k >= 0 {
			id, format = id[:k], id[k:]
		}
		var n int64
		switch id {
		case "RepresentationID":
			if format != "" {
				return "", badPlaylist("$RepresentationID$ with a format")
			}
			b.WriteString(v.repID)
			continue
		case "Bandwidth":
			n = v.bandwidth
		case "Number":
			n = v.number
		case "Time":
			n = v.time
		default:
			return "", badPlaylist("SegmentTemplate identifier %q", id)
		}
		if !segment && id != "Bandwidth" {
			return "", badPlaylist("$%s$ in an initialization template", id)
		}
		s := strconv.FormatInt(n, 10)
		if format != "" {
			w, ok := strings.CutPrefix(format, "%0")
			if !ok || !strings.HasSuffix(w, "d") {
				return "", badPlaylist("SegmentTemplate format %q", format)
			}
			width, err := strconv.Atoi(strings.TrimSuffix(w, "d"))
			if err != nil || width < 1 || width > 32 {
				return "", badPlaylist("SegmentTemplate format %q", format)
			}
			for len(s) < width {
				s = "0" + s
			}
		}
		b.WriteString(s)
	}
}

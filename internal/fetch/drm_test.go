// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/bridge"
)

// box is an MP4 box of typ holding the parts.
func box(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

// largeBox is box with a 64-bit size.
func largeBox(typ string, body []byte) []byte {
	b := make([]byte, 16, 16+len(body))
	binary.BigEndian.PutUint32(b, 1)
	copy(b[4:], typ)
	binary.BigEndian.PutUint64(b[8:], uint64(16+len(body)))
	return append(b, body...)
}

// moovWith is a moov with one track whose sample description is entry
// (and its sinf when protected), plus extra boxes.
func moovWith(entry string, extra ...[]byte) []byte {
	sample := box(entry, make([]byte, 78), box("sinf", box("frma", []byte("avc1")), box("schm", make([]byte, 12))))
	stsd := box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, sample)
	trak := box("trak", box("tkhd", make([]byte, 84)),
		box("mdia", box("mdhd", make([]byte, 24)), box("hdlr", make([]byte, 25)),
			box("minf", box("vmhd", make([]byte, 12)), box("stbl", stsd, box("stts", make([]byte, 8))))))
	return box("moov", append([][]byte{box("mvhd", make([]byte, 100)), trak}, extra...)...)
}

var ftyp = box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2avc1mp41"))

func TestMP4Protection(t *testing.T) {
	mdat := box("mdat", bytes.Repeat([]byte{0x47}, 5000))
	cases := []struct {
		name string
		file []byte
		want string
	}{
		{"clear", bytes.Join([][]byte{ftyp, moovWith("avc1"), mdat}, nil), ""},
		{"cenc video", bytes.Join([][]byte{ftyp, moovWith("encv"), mdat}, nil), "encv"},
		{"cenc audio, moov at the end", bytes.Join([][]byte{ftyp, mdat, moovWith("enca")}, nil), "enca"},
		{"pssh", bytes.Join([][]byte{ftyp, moovWith("avc1", box("pssh", make([]byte, 32))), mdat}, nil), "pssh"},
		{"fairplay", bytes.Join([][]byte{ftyp, moovWith("drms")}, nil), "drms"},
		{"64-bit mdat first", bytes.Join([][]byte{ftyp, largeBox("mdat", make([]byte, 100)), moovWith("encv")}, nil), "encv"},
		{"fragmented init", bytes.Join([][]byte{ftyp, moovWith("encv", box("mvex", box("trex", make([]byte, 24))))}, nil), "encv"},
		{"mpeg-ts", bytes.Repeat([]byte{0x47, 0x40, 0x00, 0x10}, 500), ""},
		{"webm", []byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01encv"), ""},
		{"empty", nil, ""},
		{"truncated moov", bytes.Join([][]byte{ftyp, moovWith("encv")[:60]}, nil), ""},
		{"size past the end", append(append([]byte{}, ftyp...), 0x7f, 0xff, 0xff, 0xff, 'm', 'o', 'o', 'v'), ""},
		{"size zero to the end", bytes.Join([][]byte{ftyp, append([]byte{0, 0, 0, 0}, moovWith("encv")[4:]...)}, nil), "encv"},
	}
	dir := t.TempDir()
	for i, c := range cases {
		p := filepath.Join(dir, string(rune('a'+i))+".mp4")
		if err := os.WriteFile(p, c.file, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := mp4Protection(p)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
}

func FuzzMP4Protection(f *testing.F) {
	f.Add(bytes.Join([][]byte{ftyp, moovWith("encv")}, nil))
	f.Add(bytes.Join([][]byte{ftyp, largeBox("mdat", nil), moovWith("avc1", box("pssh", nil))}, nil))
	f.Add([]byte{0, 0, 0, 1, 'f', 't', 'y', 'p', 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		got := readerProtection(bytes.NewReader(b), int64(len(b)))
		if got != "" && got != "pssh" && !protectedEntries[got] {
			t.Fatalf("protection %q", got)
		}
	})
}

// A file track that is a Common Encryption MP4 fails the task as DRM
// before anything is combined or imported.
func TestEncryptedFileTrack(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/files/v.mp4", bytes.Join([][]byte{ftyp, moovWith("encv"), box("mdat", pattern(2, 3000))}, nil))
	e.media.put("/files/a.m4a", bytes.Join([][]byte{ftyp, moovWith("mp4a"), box("mdat", pattern(1, 3000))}, nil))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeDRMProtected {
		t.Fatalf("%+v", info.Error)
	}
	if calls := e.ffmpegCalls(); len(calls) != 0 {
		t.Fatalf("ffmpeg ran: %v", calls)
	}
	if len(e.importsSnapshot()) != 0 {
		t.Fatal("the protected file was imported")
	}
	// a clear MP4 goes through
	e.media.put("/files/c.mp4", bytes.Join([][]byte{ftyp, moovWith("avc1"), box("mdat", pattern(3, 3000))}, nil))
	e.wait(e.enqueue(e.request("s2", e.fileTrack(bridge.KindVideo, "/files/c.mp4"))), bridge.StateDone)
}

// The one file of a DASH track (SegmentBase / BaseURL) is checked too: an
// MPD without ContentProtection may still point at encrypted media.
func TestEncryptedDASHFile(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/d/manifest.mpd", []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT6S"><Period><AdaptationSet mimeType="video/mp4"><Representation id="v" bandwidth="1"><BaseURL>v.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`))
	e.media.put("/d/v.mp4", bytes.Join([][]byte{ftyp, moovWith("encv"), box("mdat", pattern(2, 3000))}, nil))
	dash := bridge.Track{URL: e.media.url("/d/manifest.mpd") + "?Policy=" + testSig, Kind: bridge.KindVideo,
		Format: bridge.FormatDASH, Role: "dash", SizeBytes: -1}
	info := e.wait(e.enqueue(e.request("s1", dash)), bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeDRMProtected {
		t.Fatalf("%+v", info.Error)
	}
	if calls := e.ffmpegCalls(); len(calls) != 0 {
		t.Fatalf("ffmpeg ran: %v", calls)
	}
}

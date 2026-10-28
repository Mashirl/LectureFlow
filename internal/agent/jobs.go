// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

// Job directories: <jobs>/<id>/ with meta.json (the audio's kind, size and
// SHA-256 as the controller announced them), audio.<kind> (audio.<kind>.part
// while it is uploaded), job.json, result.json, the worker's checkpoint and
// two markers: .alive (touched while a session uses the job) and .expires
// (when a job left behind by a lost connection may be deleted). Nothing
// else is written there; the worker's TMPDIR is <id>/tmp.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/remote"
)

const (
	fileMeta    = "meta.json"
	fileJob     = "job.json"
	fileResult  = "result.json"
	fileAlive   = ".alive"
	fileExpires = ".expires"
	dirTmp      = "tmp"
	// staleAlive: a job whose .alive is older than this (and that has no
	// .expires) was left by an agent that died; reap deletes it.
	staleAlive = remote.MaxRetain + 10*time.Minute
)

type jobMeta struct {
	AudioKind   string `json:"audio_kind"`
	AudioSize   int64  `json:"audio_size"`
	AudioSHA256 string `json:"audio_sha256"`
}

func (e *env) jobDir(id string) (string, error) {
	if !remote.JobIDPattern.MatchString(id) {
		return "", coded(remote.AgentBadRequest, nil)
	}
	return inside(e.dirs.Jobs, id)
}

// existingJob returns the job's directory and meta, or unknown_job.
func (e *env) existingJob(id string) (string, jobMeta, error) {
	var m jobMeta
	dir, err := e.jobDir(id)
	if err != nil {
		return "", m, err
	}
	if err := checkPrivateDir(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", m, coded(remote.AgentUnknownJob, nil)
		}
		return "", m, err
	}
	b, err := readSmall(filepath.Join(dir, fileMeta), 4096)
	if err != nil || json.Unmarshal(b, &m) != nil || (m.AudioKind != remote.AudioWAV && m.AudioKind != remote.AudioFLAC) ||
		!remote.SHA256Pattern.MatchString(m.AudioSHA256) || m.AudioSize <= 0 {
		return "", m, coded(remote.AgentUnknownJob, err)
	}
	return dir, m, nil
}

// openJob creates or reopens a job. A job that exists with other audio is
// started over (its files are removed). It returns the bytes of audio
// already there and whether a checkpoint exists.
func (e *env) openJob(id, kind string, size int64, sum string) (have int64, resume bool, err error) {
	dir, err := e.jobDir(id)
	if err != nil {
		return 0, false, err
	}
	want := jobMeta{AudioKind: kind, AudioSize: size, AudioSHA256: sum}
	if _, m, err := e.existingJob(id); err == nil && m != want {
		if err := removeAllInside(e.dirs.Jobs, dir); err != nil {
			return 0, false, err
		}
	}
	if err := ensurePrivateDir(dir); err != nil {
		return 0, false, err
	}
	b, _ := json.Marshal(want)
	if err := writeFileAtomic(filepath.Join(dir, fileMeta), b); err != nil {
		return 0, false, err
	}
	_ = os.Remove(filepath.Join(dir, fileExpires))
	touch(filepath.Join(dir, fileAlive))
	audio := filepath.Join(dir, "audio."+kind)
	if n, ok := regularFile(audio); ok && n == size {
		have = size
	} else if n, ok := regularFile(audio + ".part"); ok && n <= size {
		have = n
	}
	_, resume = regularFile(filepath.Join(dir, "asr.ckpt.json"))
	return have, resume, nil
}

// put appends stdin to the job's audio at offset; when the file is
// complete it checks the SHA-256 and renames it.
func (e *env) put(f map[string]string) int {
	id := f["job"]
	dir, m, err := e.existingJob(id)
	if err != nil {
		e.failErr(id, err)
		return 1
	}
	offset, err := strconv.ParseInt(f["offset"], 10, 64)
	if err != nil || offset < 0 || offset > m.AudioSize {
		e.fail(id, remote.AgentBadRequest)
		return 1
	}
	final := filepath.Join(dir, "audio."+m.AudioKind)
	part := final + ".part"
	if n, ok := regularFile(final); ok && n == m.AudioSize {
		e.out.emit(map[string]any{"type": remote.EvOK, "job": id, "bytes": n})
		return 0
	}
	if free, ok := diskFreeMB(dir); ok && (m.AudioSize-offset)>>20 > free {
		e.failErr(id, &codedErr{code: remote.AgentNoSpace, freeMB: free, needMB: (m.AudioSize - offset) >> 20})
		return 1
	}
	if _, err := inside(dir, "audio."+m.AudioKind+".part"); err != nil {
		e.failErr(id, err)
		return 1
	}
	have, _ := regularFile(part)
	if offset > have {
		e.fail(id, remote.AgentBadRequest) // a gap: the controller resumes at stat's "have"
		return 1
	}
	fh, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		e.failErr(id, err)
		return 1
	}
	if err := fh.Truncate(offset); err != nil {
		fh.Close()
		e.failErr(id, err)
		return 1
	}
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		fh.Close()
		e.failErr(id, err)
		return 1
	}
	n, err := io.Copy(fh, io.LimitReader(e.stdin, m.AudioSize-offset+1))
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		e.failErr(id, err)
		return 1
	}
	total := offset + n
	switch {
	case total > m.AudioSize:
		_ = os.Remove(part)
		e.fail(id, remote.AgentBadRequest)
		return 1
	case total < m.AudioSize:
		e.fail(id, remote.AgentIncompleteUpload)
		return 1
	}
	sum, err := fileSHA256(part)
	if err != nil || sum != m.AudioSHA256 {
		_ = os.Remove(part)
		e.fail(id, remote.AgentAudioMismatch)
		return 1
	}
	if err := os.Rename(part, final); err != nil {
		e.failErr(id, err)
		return 1
	}
	e.out.emit(map[string]any{"type": remote.EvOK, "job": id, "bytes": total})
	return 0
}

// stat reports how much of the audio is there.
func (e *env) stat(f map[string]string) int {
	id := f["job"]
	dir, m, err := e.existingJob(id)
	if err != nil {
		e.failErr(id, err)
		return 1
	}
	final := filepath.Join(dir, "audio."+m.AudioKind)
	if n, ok := regularFile(final); ok && n == m.AudioSize {
		e.out.emit(map[string]any{"type": remote.EvStat, "have": n, "complete": true})
		return 0
	}
	n, _ := regularFile(final + ".part")
	e.out.emit(map[string]any{"type": remote.EvStat, "have": min(n, m.AudioSize), "complete": false})
	return 0
}

// get streams the job's result.json (at most remote.MaxResultBytes).
func (e *env) get(f map[string]string) int {
	id := f["job"]
	dir, _, err := e.existingJob(id)
	if err != nil {
		fmt.Fprintln(e.stderr, codeOf(err))
		return 1
	}
	p := filepath.Join(dir, fileResult)
	n, ok := regularFile(p)
	if !ok {
		fmt.Fprintln(e.stderr, remote.AgentResultMissing)
		return 1
	}
	if n > remote.MaxResultBytes {
		fmt.Fprintln(e.stderr, remote.AgentResultTooLarge)
		return 1
	}
	fh, err := os.Open(p)
	if err != nil {
		fmt.Fprintln(e.stderr, remote.AgentResultMissing)
		return 1
	}
	defer fh.Close()
	w := bufio.NewWriterSize(e.out.w, 64<<10)
	if _, err := io.Copy(w, io.LimitReader(fh, remote.MaxResultBytes)); err != nil {
		return 1
	}
	if err := w.Flush(); err != nil {
		return 1
	}
	return 0
}

// deleteJob removes a job directory.
func (e *env) deleteJob(id string) error {
	dir, err := e.jobDir(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return removeAllInside(e.dirs.Jobs, dir)
}

// keepJob marks a job left by a lost session: it may be deleted after retain.
func (e *env) keepJob(id string, retain time.Duration) {
	dir, err := e.jobDir(id)
	if err != nil {
		return
	}
	if _, err := os.Lstat(dir); err != nil {
		return
	}
	at := e.now().Add(retain).Unix()
	_ = writeFileAtomic(filepath.Join(dir, fileExpires), []byte(strconv.FormatInt(at, 10)))
}

// keptJobs lists the job directories there are (for hello).
func (e *env) keptJobs() []string {
	ents, err := os.ReadDir(e.dirs.Jobs)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range ents {
		if d.IsDir() && d.Type()&fs.ModeSymlink == 0 && remote.JobIDPattern.MatchString(d.Name()) {
			out = append(out, d.Name())
		}
		if len(out) >= 100 {
			break
		}
	}
	return out
}

// reap deletes the job directories whose time is up (.expires passed, or
// .alive stale without .expires) and anything in the job directory that is
// not a job.
func (e *env) reap() int {
	ents, err := os.ReadDir(e.dirs.Jobs)
	if err != nil {
		return 0
	}
	now := e.now()
	removed := 0
	for _, d := range ents {
		p := filepath.Join(e.dirs.Jobs, d.Name())
		if !d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !remote.JobIDPattern.MatchString(d.Name()) {
			if removeAllInside(e.dirs.Jobs, p) == nil {
				removed++
			}
			continue
		}
		expired := false
		if b, err := readSmall(filepath.Join(p, fileExpires), 64); err == nil {
			at, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			expired = err != nil || now.Unix() >= at
		} else if fi, err := os.Lstat(filepath.Join(p, fileAlive)); err == nil {
			expired = now.Sub(fi.ModTime()) > staleAlive
		} else if fi, err := os.Lstat(p); err == nil {
			expired = now.Sub(fi.ModTime()) > staleAlive
		}
		if expired && removeAllInside(e.dirs.Jobs, p) == nil {
			removed++
		}
	}
	return removed
}

// readSmall reads a regular file of at most max bytes (Lstat first).
func readSmall(p string, max int64) ([]byte, error) {
	n, ok := regularFile(p)
	if !ok {
		return nil, fs.ErrNotExist
	}
	if n > max {
		return nil, errors.New("file too large")
	}
	return os.ReadFile(p)
}

// writeFileAtomic writes p via a temporary file and rename (0600).
func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func touch(p string) {
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		_ = os.WriteFile(p, nil, 0o600)
	}
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

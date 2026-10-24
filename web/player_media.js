// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Media sources of the player (SPEC-v2 P3, P8). Both expose the same small
// interface in lecture milliseconds, so the player does not care where the
// sound comes from:
//
//   videoSource(url)          <video> on GET /media (stream.mp4, Range requests)
//   audioSource(lectureId, …) the existing /audio clips, played window by
//                             window (audio mode: no stream.mp4 yet, audio-only
//                             lectures, or /media failed)
//
//   fileSource(url, 'audio')  <audio> on GET /media?audio=1 (audio.wav, Range
//                             requests): the audio mode's first choice
//
// Interface: el, kind, currentMs(), durationMs(), paused, play(), pause(),
// seek(ms), setRate(r), setVolume(v, muted), buffered() → [[startMs, endMs]],
// aspect(), on(type, fn), destroy(). Events: play, pause, playing, waiting,
// seeked, ended, timeupdate, progress, durationchange, loadedmetadata,
// error ({message}).

import { api, errorText } from './api.js';
import { t } from './i18n.js';

function emitter() {
  const fns = new Map();
  return {
    on(type, fn) {
      if (!fns.has(type)) fns.set(type, new Set());
      fns.get(type).add(fn);
      return () => fns.get(type).delete(fn);
    },
    emit(type, detail) {
      for (const fn of fns.get(type) || []) fn(detail);
    },
  };
}

const FORWARD = ['play', 'pause', 'playing', 'waiting', 'seeked', 'ended', 'timeupdate', 'progress', 'durationchange', 'loadedmetadata'];

// ---------------------------------------------------------------------------
// Video (or a whole audio file): one media element, the browser does the work
// (Range requests, buffering).
// ---------------------------------------------------------------------------

export function videoSource(url) {
  return fileSource(url, 'video');
}

export function fileSource(url, kind) {
  const ev = emitter();
  const el = document.createElement(kind === 'video' ? 'video' : 'audio');
  el.className = kind === 'video' ? 'pl-video' : 'pl-audio-el';
  el.preload = 'auto';
  if (kind === 'video') {
    el.playsInline = true;
    el.disablePictureInPicture = true;
  }
  el.preservesPitch = true;
  el.setAttribute('aria-hidden', 'true'); // the controls are the player's own
  for (const type of FORWARD) el.addEventListener(type, () => ev.emit(type));
  el.addEventListener('error', () => ev.emit('error', { code: el.error ? el.error.code : 0, started: el.played.length > 0 }));
  el.src = url;
  return {
    kind,
    el,
    on: ev.on,
    currentMs: () => el.currentTime * 1000,
    durationMs: () => (isFinite(el.duration) ? el.duration * 1000 : 0),
    get paused() {
      return el.paused;
    },
    play: () => el.play().catch(() => {}),
    pause: () => el.pause(),
    seek(ms) {
      el.currentTime = Math.max(0, ms) / 1000;
    },
    setRate(r) {
      el.playbackRate = r;
      el.preservesPitch = true; // P6.2
    },
    setVolume(v, muted) {
      el.volume = v;
      el.muted = muted;
    },
    buffered() {
      const out = [];
      for (let i = 0; i < el.buffered.length; i++) out.push([el.buffered.start(i) * 1000, el.buffered.end(i) * 1000]);
      return out;
    },
    aspect: () => (el.videoWidth && el.videoHeight ? el.videoWidth / el.videoHeight : 0),
    destroy() {
      el.pause();
      el.removeAttribute('src');
      el.load();
    },
  };
}

// ---------------------------------------------------------------------------
// Audio: windows of /audio?start_ms&end_ms (the clip endpoint of A13.3 is
// limited to 10 minutes and does not support Range requests, so each window
// is fetched whole as a blob). The X-Clip-Start-Ms / X-Clip-End-Ms headers
// give the real bounds of every window (the server pads and clips them; the
// front end does not assume any padding). The next window is fetched while
// the current one still has WINDOW_PREFETCH_MS left and swapped in at its end.
// ---------------------------------------------------------------------------

const WINDOW_MS = 4 * 60 * 1000;
const WINDOW_PREFETCH_MS = 30 * 1000;

export function audioSource(lectureId, getTotalMs) {
  const ev = emitter();
  const players = [newAudio(), newAudio()];
  let cur = null; // {el, start, end, url}
  let next = null; // {el, start, end, url} ready, or {loading: Promise, start}
  let pending = null; // {ctrl, at} while the window under `at` loads
  let posMs = 0; // the position while no window is playing it
  let wantPlay = false;
  let rate = 1;
  let vol = 1;
  let muted = false;
  let destroyed = false;
  let waiting = false;

  function newAudio() {
    const a = document.createElement('audio');
    a.preload = 'auto';
    a.preservesPitch = true;
    return a;
  }

  function total() {
    return getTotalMs() || 0;
  }

  function setWaiting(on) {
    if (waiting === on) return;
    waiting = on;
    ev.emit(on ? 'waiting' : 'playing');
  }

  async function fetchWindow(startMs, ctrl) {
    const endMs = Math.max(startMs + 1, total() ? Math.min(startMs + WINDOW_MS, total()) : startMs + WINDOW_MS);
    // a network failure reads like the audio bar's (audiobar.js), not the
    // browser's "Failed to fetch"; an abort is passed on (the caller ignores it)
    const unreachable = (e) => (ctrl.signal.aborted ? e : new Error(t('common.net.unreachable')));
    let res;
    try {
      res = await fetch(api.audioURL(lectureId, Math.floor(startMs), Math.ceil(endMs)), { cache: 'no-store', signal: ctrl.signal });
    } catch (e) {
      throw unreachable(e);
    }
    if (!res.ok) {
      let body = null;
      try {
        body = await res.json();
      } catch {
        // not JSON: errorText falls back to the HTTP status
      }
      // the coded ErrorBody of P14.3, translated like every other API error
      const err = new Error(errorText(res.status, body));
      err.status = res.status;
      err.body = body;
      throw err;
    }
    const hs = Number(res.headers.get('X-Clip-Start-Ms'));
    const he = Number(res.headers.get('X-Clip-End-Ms'));
    let blob;
    try {
      blob = await res.blob();
    } catch (e) {
      throw unreachable(e);
    }
    const url = URL.createObjectURL(blob);
    return { url, start: Number.isFinite(hs) ? hs : startMs, end: Number.isFinite(he) && he > 0 ? he : 0 };
  }

  // Loads a window into the idle <audio> element and resolves when it can play.
  function prepare(win) {
    const el = players.find((p) => !cur || p !== cur.el);
    return new Promise((resolve, reject) => {
      const done = () => {
        el.removeEventListener('loadedmetadata', done);
        el.removeEventListener('error', fail);
        if (!win.end) win.end = win.start + el.duration * 1000; // headers missing (old server)
        resolve({ el, start: win.start, end: win.end, url: win.url });
      };
      const fail = () => {
        el.removeEventListener('loadedmetadata', done);
        el.removeEventListener('error', fail);
        reject(new Error(t('common.audio.decode_failed'))); // as in the audio bar (audiobar.js)
      };
      el.addEventListener('loadedmetadata', done);
      el.addEventListener('error', fail);
      el.playbackRate = rate;
      el.preservesPitch = true;
      el.volume = vol;
      el.muted = muted;
      el.src = win.url;
    });
  }

  function release(w) {
    if (!w || !w.url) return;
    if (w.el) {
      w.el.pause();
      w.el.removeAttribute('src');
    }
    URL.revokeObjectURL(w.url);
  }

  function activate(w, atMs) {
    const old = cur;
    cur = w;
    w.el.currentTime = Math.max(0, atMs - w.start) / 1000;
    if (old && old.el !== w.el) release(old);
    if (wantPlay) w.el.play().catch(() => {});
    setWaiting(false);
  }

  // Loads (or reuses) the window that contains atMs.
  async function goTo(atMs) {
    posMs = atMs;
    if (cur && atMs >= cur.start && atMs < cur.end - 40) {
      cur.el.currentTime = (atMs - cur.start) / 1000;
      ev.emit('seeked');
      return;
    }
    if (next && next.el && atMs >= next.start && atMs < next.end - 40) {
      const w = next;
      next = null;
      activate(w, atMs);
      ev.emit('seeked');
      return;
    }
    if (total() && atMs >= total()) {
      finish();
      return;
    }
    if (pending) pending.ctrl.abort();
    if (next && next.el) release(next);
    next = null;
    if (cur) cur.el.pause();
    const ctrl = new AbortController();
    pending = { ctrl, at: atMs };
    setWaiting(true);
    try {
      const win = await fetchWindow(atMs, ctrl);
      if (destroyed || ctrl.signal.aborted) {
        URL.revokeObjectURL(win.url);
        return;
      }
      const w = await prepare(win);
      if (destroyed || ctrl.signal.aborted) {
        release(w);
        return;
      }
      pending = null;
      activate(w, posMs);
      ev.emit('durationchange');
      ev.emit('seeked');
    } catch (e) {
      if (ctrl.signal.aborted || destroyed) return;
      pending = null;
      setWaiting(false);
      wantPlay = false;
      if (e.status === 400 && total() && atMs > total() - 60000) {
        finish(); // past the end of the audio
        return;
      }
      ev.emit('error', { message: e.message || '' });
      ev.emit('pause');
    }
  }

  function finish() {
    wantPlay = false;
    if (cur) cur.el.pause();
    setWaiting(false);
    ev.emit('pause');
    ev.emit('ended');
  }

  function prefetch() {
    if (!cur || next || pending) return;
    if (total() && cur.end >= total() - 50) return;
    const ctrl = new AbortController();
    const start = cur.end;
    const job = { loading: true, start, ctrl };
    next = job;
    fetchWindow(start, ctrl)
      .then((win) => (destroyed || next !== job ? (URL.revokeObjectURL(win.url), null) : prepare(win)))
      .then((w) => {
        if (!w) return;
        if (next !== job || destroyed) {
          release(w);
          return;
        }
        next = w;
        ev.emit('progress');
      })
      .catch(() => {
        if (next === job) next = null; // goTo() will try again at the end
      });
  }

  // The window boundary: continue in the next window (or wait for it).
  function onBoundary() {
    if (!cur || pending) return;
    const at = cur.end;
    if (total() && at >= total() - 50) {
      finish();
      return;
    }
    if (next && next.el) {
      const w = next;
      next = null;
      activate(w, Math.max(at, w.start));
      return;
    }
    if (next && next.loading) {
      next.ctrl.abort();
      next = null;
    }
    goTo(at);
  }

  for (const a of players) {
    const mine = () => cur && cur.el === a;
    a.addEventListener('timeupdate', () => {
      if (!mine()) return;
      const t = cur.start + a.currentTime * 1000;
      ev.emit('timeupdate');
      if (cur.end - t < WINDOW_PREFETCH_MS) prefetch();
      if (total() && t >= total()) finish();
    });
    a.addEventListener('ended', () => mine() && onBoundary());
    a.addEventListener('play', () => mine() && ev.emit('play'));
    a.addEventListener('playing', () => mine() && !pending && ev.emit('playing'));
    a.addEventListener('pause', () => mine() && !wantPlay && ev.emit('pause'));
    a.addEventListener('waiting', () => mine() && ev.emit('waiting'));
  }

  return {
    kind: 'audio',
    el: null,
    on: ev.on,
    currentMs() {
      if (cur && !pending) return cur.start + cur.el.currentTime * 1000;
      return posMs;
    },
    durationMs: total,
    get paused() {
      return !wantPlay;
    },
    play() {
      if (total() && this.currentMs() >= total() - 100) {
        this.seek(0);
      }
      wantPlay = true;
      ev.emit('play');
      if (cur && !pending) cur.el.play().catch(() => {});
      else if (!pending) goTo(posMs);
    },
    pause() {
      wantPlay = false;
      if (cur) cur.el.pause();
      ev.emit('pause');
    },
    seek(ms) {
      goTo(Math.max(0, total() ? Math.min(ms, total()) : ms));
    },
    setRate(r) {
      rate = r;
      for (const a of players) {
        a.playbackRate = r;
        a.preservesPitch = true;
      }
    },
    setVolume(v, m) {
      vol = v;
      muted = m;
      for (const a of players) {
        a.volume = v;
        a.muted = m;
      }
    },
    buffered() {
      const out = [];
      if (cur && !pending) out.push([cur.start, cur.end]);
      if (next && next.el) out.push([next.start, next.end]);
      return out;
    },
    aspect: () => 0,
    // Loads the window of the start position without playing.
    load(ms) {
      goTo(ms);
    },
    destroy() {
      destroyed = true;
      wantPlay = false;
      if (pending) pending.ctrl.abort();
      if (next && next.ctrl) next.ctrl.abort();
      release(next && next.el ? next : null);
      release(cur);
      cur = next = pending = null;
    },
  };
}

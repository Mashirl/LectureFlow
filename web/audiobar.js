// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Sentence audio clips and the small progress bar (v1.1 A13; SPEC §9.6, §9.7).
//
// One <audio> element for the whole page plays GET /api/lectures/{id}/audio
// clips, and only one clip is active at a time: starting another clip stops
// the previous one (A13). A clip is fetched once as a blob — so seeking works
// whatever the server streams, and the X-Clip-* headers (A13.3) say where the
// sentence lies inside the clip; the 1 s padding is never hard-coded here —
// and played from an object URL. Clips live in a small cache; prefetches go
// through a queue with at most two requests in flight.
//
// audioBar() is the only UI for clips and is shared by the review cards and
// the transcript row that is playing (A13.3: a single implementation).

import { api, errorText } from './api.js';
import { h, toast } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';

export const SLOW_RATE = 0.75;
const STEP_MS = 500; // ← / → (A13.2)
const CACHE_MAX = 40;
const MAX_FETCHES = 2;

// key -> Clip. Clip: {key, lectureId, startMs, endMs, status, url, durMs,
// regionStart, regionEnd, error, urgent}; status: idle | queued | loading |
// ready | error. Map order is the LRU order (oldest first).
const clips = new Map();
const queue = []; // keys waiting for a fetch slot
let fetching = 0;

let audio = null;
let active = null; // the Clip the <audio> element is playing / paused on
let phase = 'idle'; // of the active clip: idle | paused | playing | ended
let rate = 1;
let wantPlay = null; // {key, rate}: start this clip once it has loaded
let failedKey = null; // a clip the user asked to play that failed to load
let posMs = 0; // position inside the active clip while not playing
let syncMs = 0; // audio.currentTime (ms) at syncAt, for smooth interpolation
let syncAt = 0;
let running = false; // the element has actually started (the 'playing' event)
let raf = 0;

const bars = new Set();
const listeners = new Set();

// ---------- clip cache and fetching ----------

export function clipKey(spec) {
  return `${spec.lectureId}:${spec.startMs}:${spec.endMs}`;
}

function ensure(spec) {
  const key = clipKey(spec);
  let c = clips.get(key);
  if (c) {
    clips.delete(key); // LRU touch
    clips.set(key, c);
    return c;
  }
  c = { key, lectureId: spec.lectureId, startMs: spec.startMs, endMs: spec.endMs, status: 'idle', url: null, durMs: null, regionStart: null, regionEnd: null, error: '', urgent: false };
  clips.set(key, c);
  evict();
  return c;
}

// Drops the oldest clips nobody shows any more (never the active one).
function evict() {
  if (clips.size <= CACHE_MAX) return;
  const shown = new Set([...bars].map((b) => b.key));
  for (const c of clips.values()) {
    if (clips.size <= CACHE_MAX) break;
    if (c === active || shown.has(c.key) || c.status === 'loading' || c.status === 'queued') continue;
    if (c.url) URL.revokeObjectURL(c.url);
    clips.delete(c.key);
  }
}

// Asks for the clip's audio. urgent: the user is waiting (play, seek) — it
// jumps the queue and failures are also shown as a toast.
function request(c, urgent) {
  if (urgent) c.urgent = true;
  if (c.status === 'ready' || c.status === 'loading') return;
  if (c.status === 'queued') {
    if (urgent) {
      const i = queue.indexOf(c.key);
      if (i > 0) {
        queue.splice(i, 1);
        queue.unshift(c.key);
      }
    }
    return;
  }
  c.status = 'queued';
  c.error = '';
  if (urgent) queue.unshift(c.key);
  else queue.push(c.key);
  renderKey(c.key);
  pump();
}

function pump() {
  while (fetching < MAX_FETCHES && queue.length) {
    const c = clips.get(queue.shift());
    if (!c || c.status !== 'queued') continue;
    fetching++;
    load(c).finally(() => {
      fetching--;
      pump();
    });
  }
}

function headerMs(res, name) {
  const v = res.headers.get(name);
  if (v == null || v.trim() === '') return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

// Length of a PCM WAV blob in ms (fallback when the X-Clip-* headers are
// missing): data bytes / byte rate from the fmt chunk.
async function wavDurationMs(blob) {
  try {
    const head = new DataView(await blob.slice(0, Math.min(blob.size, 4096)).arrayBuffer());
    let off = 12;
    let byteRate = 0;
    while (off + 8 <= head.byteLength) {
      const id = String.fromCharCode(head.getUint8(off), head.getUint8(off + 1), head.getUint8(off + 2), head.getUint8(off + 3));
      const size = head.getUint32(off + 4, true);
      if (id === 'fmt ' && off + 20 <= head.byteLength) byteRate = head.getUint32(off + 16, true);
      if (id === 'data') return byteRate ? ((blob.size - off - 8) / byteRate) * 1000 : null;
      off += 8 + size + (size % 2);
    }
  } catch {
    // not a WAV we understand
  }
  return null;
}

async function load(c) {
  c.status = 'loading';
  renderKey(c.key);
  let msg = '';
  let status = 0;
  try {
    let res;
    try {
      res = await fetch(api.audioURL(c.lectureId, c.startMs, c.endMs), { cache: 'no-store' });
    } catch {
      throw new Error(t('common.net.unreachable'));
    }
    status = res.status;
    if (!res.ok) {
      let text = '';
      try {
        const body = JSON.parse(await res.text());
        text = body && body.error ? errorText(res.status, body) : ''; // v1.1 string or v2 {code, params, detail}
      } catch {
        // not JSON
      }
      throw new Error(text || t('errors.http', { status: res.status }));
    }
    const blob = await res.blob();
    const clipStart = headerMs(res, 'X-Clip-Start-Ms');
    const clipEnd = headerMs(res, 'X-Clip-End-Ms');
    const sentStart = headerMs(res, 'X-Clip-Sentence-Start-Ms');
    const sentEnd = headerMs(res, 'X-Clip-Sentence-End-Ms');
    if (clipStart != null && clipEnd != null && clipEnd > clipStart) {
      c.durMs = clipEnd - clipStart;
      if (sentStart != null && sentEnd != null) {
        c.regionStart = clamp(sentStart - clipStart, 0, c.durMs);
        c.regionEnd = clamp(sentEnd - clipStart, c.regionStart, c.durMs);
      }
    } else {
      // Older back end: length from the WAV itself, no sentence region.
      c.durMs = await wavDurationMs(blob);
    }
    if (!blob.size || !c.durMs) throw new Error(t('common.audio.no_clip'));
    if (c.url) URL.revokeObjectURL(c.url);
    c.url = URL.createObjectURL(blob);
    c.status = 'ready';
  } catch (e) {
    msg = (e && e.message) || t('common.unknown_error');
    c.status = 'error';
    c.error = msg;
    if (wantPlay && wantPlay.key === c.key) {
      wantPlay = null;
      failedKey = c.key; // its bar stays, showing the error and a retry button
    }
    if (c.urgent) toast(t('common.audio.load_failed_toast', { error: msg }), status >= 500 || status === 0 ? 'error' : 'warn');
  }
  c.urgent = false;
  renderKey(c.key);
  if (c.status === 'ready' && wantPlay && wantPlay.key === c.key) {
    const r = wantPlay.rate;
    wantPlay = null;
    start(c, r);
  } else if (c.status === 'error') {
    emit();
  }
}

// ---------- the one <audio> element ----------

function el() {
  if (audio) return audio;
  audio = new Audio();
  audio.preload = 'auto';
  audio.preservesPitch = true;
  const resync = () => {
    syncMs = audio.currentTime * 1000;
    syncAt = performance.now();
  };
  // ~4 times a second: full render (aria-valuenow); every frame only the
  // position moves (loop below).
  audio.addEventListener('timeupdate', () => {
    if (!active || phase !== 'playing') return;
    resync();
    renderKey(active.key);
  });
  audio.addEventListener('seeked', () => {
    if (phase === 'playing') resync();
  });
  audio.addEventListener('playing', () => {
    running = true;
    resync();
  });
  audio.addEventListener('waiting', () => {
    running = false;
  });
  audio.addEventListener('ended', () => {
    if (active && phase === 'playing') finish();
  });
  audio.addEventListener('error', () => {
    if (!active || !audio.getAttribute('src')) return;
    const c = active;
    c.status = 'error';
    c.error = t('common.audio.decode_failed');
    stop();
    toast(t('common.audio.play_failed', { error: c.error }), 'error');
  });
  return audio;
}

function clamp(v, lo, hi) {
  return Math.min(hi, Math.max(lo, v));
}

// Position inside the active clip, in ms (smooth while playing: the last
// timeupdate plus the time since, A13.3).
function position() {
  if (!active) return 0;
  const dur = active.durMs || 0;
  if (phase === 'playing') return clamp(running ? syncMs + (performance.now() - syncAt) * rate : syncMs, 0, dur);
  return clamp(posMs, 0, dur);
}

function activate(c) {
  failedKey = null;
  if (active === c) return;
  const prev = active;
  if (audio) audio.pause();
  running = false;
  active = c;
  phase = 'paused';
  posMs = 0;
  syncMs = 0;
  syncAt = performance.now();
  const a = el();
  a.src = c.url;
  if (prev) renderKey(prev.key);
  renderKey(c.key);
  emit();
}

function start(c, r) {
  activate(c);
  rate = r;
  const a = el();
  if (phase === 'ended' || position() >= (c.durMs || 0) - 1) seekActive(0);
  a.defaultPlaybackRate = r;
  a.playbackRate = r;
  phase = 'playing';
  running = false;
  syncMs = posMs;
  syncAt = performance.now();
  a.play().catch((err) => {
    if (active !== c || (err && err.name === 'AbortError')) return;
    phase = 'paused';
    renderAll();
    emit();
    toast(t('common.audio.play_failed', { error: err && err.message ? err.message : t('common.audio.play_refused') }), 'error');
  });
  loop();
  renderAll();
  emit();
}

function finish() {
  if (!active) return;
  if (audio) audio.pause();
  running = false;
  phase = 'ended';
  posMs = active.durMs || 0;
  renderKey(active.key);
  emit();
}

function seekActive(ms) {
  const dur = active.durMs || 0;
  const t = clamp(ms, 0, dur);
  posMs = t;
  syncMs = t;
  syncAt = performance.now();
  const a = el();
  try {
    a.currentTime = t / 1000;
  } catch {
    // not seekable yet; the position applies once metadata has loaded
  }
  if (phase === 'ended' && t < dur) phase = 'paused';
}

function loop() {
  if (raf) return;
  const tick = () => {
    raf = 0;
    if (!active || phase !== 'playing') return;
    const dur = active.durMs || 0;
    // The header length is authoritative: stop there even if the decoded
    // audio is a little longer (A13.2: stop at the end, head stays there).
    if (position() >= dur) {
      finish();
      return;
    }
    renderKey(active.key, true);
    raf = requestAnimationFrame(tick);
  };
  raf = requestAnimationFrame(tick);
}

// ---------- public player API (spec = {lectureId, startMs, endMs}) ----------

export function activeKey() {
  return active ? active.key : null;
}

export function isPlaying(spec) {
  return !!active && active.key === clipKey(spec) && phase === 'playing';
}

// The key of the clip that is playing or about to play (loading after a
// play request), for "this sentence is playing" markers.
export function playingKey() {
  if (wantPlay) return wantPlay.key;
  return active && phase === 'playing' ? active.key : null;
}

// The clip the user is dealing with: about to play (loading) or active
// (playing, paused or at its end). The transcript shows its bar there.
export function currentKey() {
  return wantPlay ? wantPlay.key : activeKey() || failedKey;
}

export function onChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function emit() {
  for (const fn of listeners) fn(activeKey());
}

export function prefetch(spec) {
  request(ensure(spec), false);
}

// Plays the clip at rate r (loading it first when needed). Another clip that
// was playing stops (A13: one clip at a time); from the end it starts over.
export function play(spec, r = 1) {
  const c = ensure(spec);
  failedKey = null;
  if (c.status !== 'ready') {
    if (active && active !== c) stop();
    wantPlay = { key: c.key, rate: r };
    request(c, true);
    renderKey(c.key);
    emit();
    return;
  }
  start(c, r);
}

export function pause() {
  wantPlay = null;
  if (!active || phase !== 'playing') return;
  posMs = position();
  if (audio) audio.pause();
  running = false;
  phase = 'paused';
  renderKey(active.key);
  emit();
}

// Space: play / pause (SPEC §9.7). Shift+Space asks for 0.75×: while playing
// at 1× it switches speed instead of pausing; any other request while the
// clip plays pauses it.
export function toggle(spec, r = 1) {
  const key = clipKey(spec);
  if (wantPlay && wantPlay.key === key) {
    wantPlay = null;
    renderKey(key);
    emit();
    return;
  }
  if (active && active.key === key && phase === 'playing') {
    if (r === SLOW_RATE && rate !== SLOW_RATE) setRate(r);
    else pause();
    return;
  }
  play(spec, r);
}

export function setRate(r) {
  rate = r;
  if (audio) {
    audio.defaultPlaybackRate = r;
    audio.playbackRate = r;
  }
  syncMs = position();
  syncAt = performance.now();
  renderAll();
}

// Stops playback; every bar goes back to its initial state (A13.2: J / K).
export function stop() {
  wantPlay = null;
  if (failedKey) {
    failedKey = null;
    emit();
  }
  if (!active) return;
  const prev = active;
  if (audio) audio.pause();
  active = null;
  phase = 'idle';
  posMs = 0;
  renderKey(prev.key);
  emit();
}

// Jumps to ms (relative to the clip start). A clip that is not loaded yet is
// requested; the jump then needs a second press.
export function seekTo(spec, ms) {
  const c = ensure(spec);
  if (c.status !== 'ready') {
    request(c, true);
    return;
  }
  if (active !== c) {
    if (wantPlay && wantPlay.key !== c.key) wantPlay = null;
    activate(c);
  }
  seekActive(ms);
  renderKey(c.key);
}

export function seekBy(spec, deltaMs) {
  const key = clipKey(spec);
  const base = active && active.key === key ? position() : 0;
  seekTo(spec, base + deltaMs);
}

// Shift+←: to where the sentence itself starts (skipping the lead-in).
export function seekSentence(spec) {
  const c = clips.get(clipKey(spec));
  seekTo(spec, c && c.regionStart != null ? c.regionStart : 0);
}

// Forgets the clips of a lecture (leaving its page): stops playback and frees
// the blobs.
export function release(lectureId) {
  if (active && (!lectureId || active.lectureId === lectureId)) stop();
  for (const c of [...clips.values()]) {
    if (lectureId && c.lectureId !== lectureId) continue;
    if (c.status === 'loading') continue; // finishes and is evicted later
    if (c.url) URL.revokeObjectURL(c.url);
    clips.delete(c.key);
    const i = queue.indexOf(c.key);
    if (i >= 0) queue.splice(i, 1);
  }
}

// ---------- the bar ----------

// "0:03.2"
export function fmtClipTime(ms) {
  if (ms == null || !Number.isFinite(ms)) return '–:––.–';
  const tenths = Math.max(0, Math.floor(ms / 100));
  const s = Math.floor(tenths / 10);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}.${tenths % 10}`;
}

function rateText(r) {
  return `${r === 1 ? '1' : String(r)}×`;
}

let io = null;
function observer() {
  if (!io) {
    io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (!e.isIntersecting) continue;
          io.unobserve(e.target);
          const b = e.target._abar;
          if (b && bars.has(b)) prefetch(b.spec);
        }
      },
      { rootMargin: '240px 0px' },
    );
  }
  return io;
}

function renderKey(key, posOnly = false) {
  for (const b of bars) {
    if (!b.el.isConnected) {
      if (b.mounted) b.destroy();
      continue;
    }
    if (b.key === key) b.render(posOnly);
  }
}

function renderAll() {
  for (const b of bars) {
    if (b.el.isConnected) b.render(false);
    else if (b.mounted) b.destroy();
  }
}

// audioBar(spec, opts) → element (with .destroy()). opts:
//   autoload  'visible' (prefetch when scrolled into view; review cards),
//             'now' (prefetch at once) or false
//   fkey      prefix for data-fkey (focus survives re-renders)
//   label     accessible name of the slider (default common.audio.slider)
export function audioBar(spec, opts = {}) {
  const key = clipKey(spec);
  const fk = opts.fkey || `abar:${key}`;
  const playBtn = h('button', { type: 'button', class: 'abar-play', dataset: { fkey: `${fk}:play` } });
  const rateBtn = h('button', { type: 'button', class: 'abar-rate', dataset: { fkey: `${fk}:rate` } });
  const played = h('span', { class: 'abar-played' });
  const rail = h('span', { class: 'abar-rail' }, played);
  const head = h('span', { class: 'abar-head' });
  const tip = h('span', { class: 'abar-tip', 'aria-hidden': 'true' });
  const track = h(
    'div',
    {
      class: 'abar-track',
      role: 'slider',
      tabindex: '0',
      'aria-label': opts.label || t('common.audio.slider'),
      'aria-valuemin': '0',
      'aria-keyshortcuts': 'ArrowLeft ArrowRight Home End Shift+ArrowLeft Space',
      dataset: { fkey: `${fk}:track` },
    },
    rail,
    head,
    tip,
  );
  const cur = h('span', { class: 'abar-cur' });
  const dur = h('span', { class: 'abar-dur' });
  const timeEl = h('span', { class: 'abar-time' }, cur, h('span', { class: 'abar-sep' }, ' / '), dur);
  const errEl = h('span', { class: 'abar-err', role: 'alert' });
  const retry = h('button', { type: 'button', class: 'abar-retry', title: t('common.audio.reload_title'), 'aria-label': t('common.audio.reload'), dataset: { fkey: `${fk}:retry` } }, icon('rotate-cw', { size: 13 }));
  const root = h('div', { class: 'abar', dataset: { clip: key } }, playBtn, rateBtn, track, timeEl, errEl, retry);

  let lastIcon = '';
  let dragging = false;
  let resume = null; // rate to resume with after a drag, or null
  let dragRaf = 0;
  let dragX = 0;

  const bar = {
    key,
    spec,
    el: root,
    mounted: true,
    render(posOnly) {
      const c = clips.get(key);
      const ready = !!c && c.status === 'ready';
      const isActive = !!active && active.key === key;
      const total = ready ? c.durMs : null;
      const pos = isActive ? position() : 0;
      const p = total ? clamp(pos / total, 0, 1) : 0;
      root.style.setProperty('--p', `${(p * 100).toFixed(3)}%`);
      cur.textContent = fmtClipTime(ready ? pos : null);
      if (posOnly) return;
      const pending = wantPlay && wantPlay.key === key;
      const loading = !!c && (c.status === 'queued' || c.status === 'loading');
      const playing = isActive && phase === 'playing';
      root.classList.toggle('is-loading', loading);
      root.classList.toggle('is-ready', ready);
      root.classList.toggle('is-error', !!c && c.status === 'error');
      root.classList.toggle('is-active', isActive);
      root.classList.toggle('is-playing', playing);
      root.classList.toggle('is-ended', isActive && phase === 'ended');
      if (ready && c.regionStart != null) {
        root.style.setProperty('--s', `${((c.regionStart / total) * 100).toFixed(3)}%`);
        root.style.setProperty('--e', `${((c.regionEnd / total) * 100).toFixed(3)}%`);
        root.classList.add('has-region');
      } else {
        root.classList.remove('has-region');
      }
      dur.textContent = fmtClipTime(total);
      const ic = loading || pending ? 'loader-circle' : playing ? 'pause' : 'play';
      if (ic !== lastIcon) {
        playBtn.replaceChildren(icon(ic, { size: 14, class: ic === 'loader-circle' ? 'spin' : '' }));
        lastIcon = ic;
      }
      playBtn.disabled = loading; // A13.3: not usable before the clip has loaded
      playBtn.setAttribute('aria-label', loading ? t('common.audio.loading') : playing ? t('common.audio.pause') : t('common.audio.play'));
      playBtn.title = loading ? t('common.audio.loading_title') : playing ? t('common.audio.pause') : t('common.audio.play_title');
      const r = isActive || pending ? (pending ? wantPlay.rate : rate) : 1;
      rateBtn.textContent = rateText(r);
      rateBtn.classList.toggle('is-slow', r !== 1);
      rateBtn.setAttribute('aria-label', t('common.audio.rate_label', { rate: rateText(r), next: rateText(r === 1 ? SLOW_RATE : 1) }));
      rateBtn.title = t('common.audio.rate_title', { rate: rateText(r) });
      rateBtn.disabled = !ready;
      errEl.textContent = c && c.status === 'error' ? t('common.audio.load_failed', { error: c.error }) : '';
      track.setAttribute('aria-disabled', ready ? 'false' : 'true');
      track.setAttribute('aria-busy', loading ? 'true' : 'false');
      track.setAttribute('aria-valuemax', total ? (total / 1000).toFixed(1) : '0');
      track.setAttribute('aria-valuenow', (pos / 1000).toFixed(1));
      track.setAttribute(
        'aria-valuetext',
        ready
          ? c.regionStart != null
            ? t('common.audio.valuetext_region', { pos: fmtClipTime(pos), total: fmtClipTime(total), from: fmtClipTime(c.regionStart), to: fmtClipTime(c.regionEnd) })
            : t('common.audio.valuetext', { pos: fmtClipTime(pos), total: fmtClipTime(total) })
          : loading
            ? t('common.audio.loading')
            : t('common.audio.not_loaded'),
      );
    },
    destroy() {
      if (!bar.mounted) return;
      bar.mounted = false;
      bars.delete(bar);
      if (io) io.unobserve(root);
      cancelAnimationFrame(dragRaf);
    },
  };
  root._abar = bar;
  root.destroy = bar.destroy;

  const isReady = () => {
    const c = clips.get(key);
    return !!c && c.status === 'ready';
  };
  const fracAt = (x) => {
    const r = rail.getBoundingClientRect();
    return r.width ? clamp((x - r.left) / r.width, 0, 1) : 0;
  };
  const seekToX = (x) => {
    const c = clips.get(key);
    if (c && c.durMs) seekTo(spec, fracAt(x) * c.durMs);
  };

  // The button keeps the speed shown next to it; Space / Shift+Space choose
  // 1× / 0.75× explicitly (SPEC §9.7).
  playBtn.addEventListener('click', (e) => toggle(spec, e.shiftKey ? SLOW_RATE : active && active.key === key ? rate : 1));
  rateBtn.addEventListener('click', () => {
    const c = clips.get(key);
    if (!c || c.status !== 'ready') return;
    const next = (active === c ? rate : 1) === 1 ? SLOW_RATE : 1;
    if (active !== c) activate(c);
    setRate(next);
  });
  retry.addEventListener('click', () => request(ensure(spec), true));

  // Click anywhere to jump; drag the head to scrub in real time, then carry
  // on playing if it was playing (A13.2).
  track.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    track.focus({ preventScroll: true });
    if (!isReady()) {
      request(ensure(spec), true);
      return;
    }
    e.preventDefault();
    dragging = true;
    resume = isPlaying(spec) ? rate : null;
    if (resume != null) pause();
    root.classList.add('is-dragging');
    try {
      track.setPointerCapture(e.pointerId);
    } catch {
      // ignored
    }
    seekToX(e.clientX);
  });
  track.addEventListener('pointermove', (e) => {
    const c = clips.get(key);
    if (c && c.status === 'ready' && c.durMs) {
      const f = fracAt(e.clientX);
      tip.textContent = fmtClipTime(f * c.durMs);
      tip.style.left = `${(f * 100).toFixed(2)}%`;
    }
    if (!dragging) return;
    dragX = e.clientX;
    if (!dragRaf) {
      dragRaf = requestAnimationFrame(() => {
        dragRaf = 0;
        if (dragging) seekToX(dragX);
      });
    }
  });
  const endDrag = (e) => {
    if (!dragging) return;
    dragging = false;
    root.classList.remove('is-dragging');
    try {
      track.releasePointerCapture(e.pointerId);
    } catch {
      // ignored
    }
    if (e.type === 'pointerup') seekToX(e.clientX);
    const c = clips.get(key);
    const atEnd = active === c && position() >= (c.durMs || 0) - 1;
    // dragged to the very end: it stays there, stopped, like playing to the end
    if (atEnd) finish();
    else if (resume != null) play(spec, resume);
    resume = null;
  };
  track.addEventListener('pointerup', endDrag);
  track.addEventListener('pointercancel', endDrag);
  track.addEventListener('lostpointercapture', endDrag);

  // The slider's own keys (ARIA slider pattern); the review view adds the
  // same keys for the focused card (A13.2).
  track.addEventListener('keydown', (e) => {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const c = clips.get(key);
    if (e.key === 'ArrowLeft' || e.key === 'ArrowDown') {
      if (e.shiftKey && e.key === 'ArrowLeft') seekSentence(spec);
      else seekBy(spec, -STEP_MS);
    } else if (e.key === 'ArrowRight' || e.key === 'ArrowUp') seekBy(spec, STEP_MS);
    else if (e.key === 'Home') seekTo(spec, 0);
    else if (e.key === 'End') seekTo(spec, c && c.durMs ? c.durMs : 0);
    else if (e.key === ' ' || e.key === 'Enter') toggle(spec, e.shiftKey ? SLOW_RATE : 1);
    else return;
    e.preventDefault();
  });

  bars.add(bar);
  bar.render(false);
  if (opts.autoload === 'now') prefetch(spec);
  else if (opts.autoload === 'visible') observer().observe(root);
  return root;
}

export { STEP_MS };

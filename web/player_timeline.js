// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The player's timeline (SPEC-v2 P6.1): played and buffered parts, hover
// preview (the time and the English sentence of that moment, ~60
// characters; no thumbnails), marks for chapter starts, modified sentences
// (thin grey) and pending review changes (amber) merged where they are too
// dense, and dragging that updates the subtitles live and seeks on release.
// ext web3 (user request 2026-10-27 no. 5): with chapters the track is
// split into one segment per chapter (a small gap at each chapter start)
// and the segment under the pointer is lifted; the preview names its
// chapter. Without chapters the track is one piece, as before.

import { h, clear } from './dom.js';
import { t } from './i18n.js';
import { fmtPlayerTime } from './player_util.js';

const MERGE_PX = 3; // marks closer than this are drawn as one

// opts: {duration() → ms, onScrub(ms|null), onSeek(ms), preview(ms) → {text, chapter}}
export function createTimeline(opts) {
  const buffered = h('div', { class: 'pl-tl-buffered' });
  const played = h('div', { class: 'pl-tl-played' });
  const marks = h('div', { class: 'pl-tl-marks', 'aria-hidden': 'true' });
  const segHover = h('div', { class: 'pl-tl-seg', hidden: true, 'aria-hidden': 'true' });
  const gaps = h('div', { class: 'pl-tl-gaps', 'aria-hidden': 'true' });
  const head = h('div', { class: 'pl-tl-head' });
  const hoverLine = h('div', { class: 'pl-tl-hoverline', hidden: true });
  const track = h('div', { class: 'pl-tl-track' }, buffered, marks, played, segHover, gaps, hoverLine);
  const tipTime = h('span', { class: 'pl-tip-time' });
  const tipChapter = h('span', { class: 'pl-tip-chapter' });
  const tipText = h('span', { class: 'pl-tip-text', lang: 'en' });
  const tip = h('div', { class: 'pl-tl-tip', hidden: true, 'aria-hidden': 'true' }, h('div', { class: 'pl-tip-top' }, tipTime, tipChapter), tipText);
  const el = h(
    'div',
    {
      class: 'pl-tl',
      role: 'slider',
      tabindex: '0',
      'aria-label': t('player.timeline'),
      'aria-valuemin': '0',
      'aria-valuemax': '0',
      'aria-valuenow': '0',
    },
    track,
    head,
    tip,
  );

  let cur = 0;
  let dur = 0;
  let scrub = null; // ms while dragging
  let data = { chapters: [], sentences: [] };
  let lastAria = -1;
  let hoverX = null;

  const ro = new ResizeObserver(() => drawMarks());
  ro.observe(el);

  function frac(ms) {
    return dur > 0 ? Math.max(0, Math.min(1, ms / dur)) : 0;
  }

  function msAt(clientX) {
    const r = track.getBoundingClientRect();
    if (r.width <= 0 || dur <= 0) return 0;
    return Math.max(0, Math.min(1, (clientX - r.left) / r.width)) * dur;
  }

  function paint() {
    const ms = scrub != null ? scrub : cur;
    const f = frac(ms);
    played.style.transform = `scaleX(${f})`;
    head.style.left = `${f * 100}%`;
    const sec = Math.floor(ms / 1000);
    if (sec !== lastAria) {
      lastAria = sec;
      el.setAttribute('aria-valuenow', String(sec));
      el.setAttribute('aria-valuetext', `${fmtPlayerTime(ms, dur)} / ${fmtPlayerTime(dur, dur)}`);
    }
  }

  // ---- marks (P6.1) ----

  function drawMarks() {
    clear(marks);
    clear(gaps);
    const w = track.getBoundingClientRect().width;
    if (w <= 0 || dur <= 0) return;
    // sentences: one bucket per MERGE_PX; pending wins over modified
    const buckets = new Map();
    for (const s of data.sentences) {
      const kind = s.pending ? 2 : s.modified ? 1 : 0;
      if (!kind) continue;
      const b = Math.floor((frac(s.start_ms) * w) / MERGE_PX);
      if ((buckets.get(b) || 0) < kind) buckets.set(b, kind);
    }
    // neighbouring buckets of the same kind become one wider mark
    const keys = [...buckets.keys()].sort((a, b) => a - b);
    let run = null;
    const flush = () => {
      if (!run) return;
      const left = run.from * MERGE_PX;
      const width = Math.max(2, (run.to - run.from + 1) * MERGE_PX - 1);
      marks.append(h('span', { class: `pl-tl-mark ${run.kind === 2 ? 'is-pending' : 'is-mod'}`, style: { left: `${left}px`, width: run.to > run.from ? `${width}px` : '' } }));
      run = null;
    };
    for (const k of keys) {
      const kind = buckets.get(k);
      if (run && run.kind === kind && k === run.to + 1) run.to = k;
      else {
        flush();
        run = { from: k, to: k, kind };
      }
    }
    flush();
    // chapters: a gap at every chapter start but the first (segments)
    let lastX = 0;
    for (const c of data.chapters) {
      const x = frac(c.start_ms) * w;
      if (x - lastX < MERGE_PX * 2 || x > w - MERGE_PX * 2) continue;
      lastX = x;
      gaps.append(h('span', { class: 'pl-tl-gap', style: { left: `${x}px` } }));
    }
  }

  // The chapter segment under ms: [start, end) of its chapter.
  function segmentAt(ms) {
    const list = data.chapters;
    if (!list.length || dur <= 0) return null;
    let k = -1;
    for (let i = 0; i < list.length && list[i].start_ms <= ms; i++) k = i;
    const start = k < 0 ? 0 : list[k].start_ms;
    const end = k + 1 < list.length ? list[k + 1].start_ms : dur;
    return end > start ? { start, end } : null;
  }

  function showSegment(ms) {
    const seg = ms == null ? null : segmentAt(ms);
    segHover.hidden = !seg;
    if (seg) Object.assign(segHover.style, { left: `${frac(seg.start) * 100}%`, width: `${(frac(seg.end) - frac(seg.start)) * 100}%` });
  }

  // ---- hover preview ----

  function showTip(clientX) {
    const ms = msAt(clientX);
    const r = el.getBoundingClientRect();
    const p = opts.preview(ms) || {};
    tipTime.textContent = fmtPlayerTime(ms, dur);
    tipChapter.textContent = p.chapter || '';
    tipChapter.hidden = !p.chapter;
    tipText.textContent = p.text || '';
    tipText.hidden = !p.text;
    tip.hidden = false;
    const tw = tip.offsetWidth;
    const x = clientX - r.left;
    tip.style.left = `${Math.max(0, Math.min(r.width - tw, x - tw / 2))}px`;
    hoverLine.hidden = false;
    hoverLine.style.left = `${frac(ms) * 100}%`;
    showSegment(ms);
  }

  function hideTip() {
    tip.hidden = true;
    hoverLine.hidden = true;
    showSegment(null);
  }

  el.addEventListener('pointermove', (e) => {
    hoverX = e.clientX;
    if (scrub != null) {
      scrub = msAt(e.clientX);
      paint();
      opts.onScrub(scrub);
    }
    showTip(e.clientX);
  });
  el.addEventListener('pointerleave', () => {
    hoverX = null;
    if (scrub == null) hideTip();
  });
  el.addEventListener('pointerdown', (e) => {
    if (e.button !== 0 || dur <= 0) return;
    e.preventDefault();
    el.focus({ preventScroll: true });
    el.setPointerCapture(e.pointerId);
    el.classList.add('is-dragging');
    scrub = msAt(e.clientX);
    paint();
    opts.onScrub(scrub);
    showTip(e.clientX);
  });
  const release = (e, commit) => {
    if (scrub == null) return;
    const ms = scrub;
    scrub = null;
    el.classList.remove('is-dragging');
    if (el.hasPointerCapture(e.pointerId)) el.releasePointerCapture(e.pointerId);
    opts.onScrub(null);
    if (commit) opts.onSeek(ms);
    paint();
    if (hoverX == null) hideTip();
  };
  el.addEventListener('pointerup', (e) => release(e, true));
  el.addEventListener('pointercancel', (e) => release(e, false));
  el.addEventListener('keydown', (e) => {
    // ← / → (5 s) are the player's shortcuts (P6.3); the slider adds Home / End
    if (e.key === 'Home' || e.key === 'End') {
      e.preventDefault();
      opts.onSeek(e.key === 'Home' ? 0 : Math.max(0, dur - 500));
    } else if (e.key === 'PageUp' || e.key === 'PageDown') {
      e.preventDefault();
      opts.onSeek(Math.max(0, Math.min(dur, cur + (e.key === 'PageUp' ? 60000 : -60000))));
    }
  });

  return {
    el,
    get scrubbing() {
      return scrub != null;
    },
    setTime(ms) {
      cur = ms;
      paint();
    },
    setDuration(ms) {
      if (Math.abs(ms - dur) < 1) return;
      dur = ms;
      el.setAttribute('aria-valuemax', String(Math.floor(ms / 1000)));
      lastAria = -1;
      paint();
      drawMarks();
    },
    setBuffered(ranges) {
      clear(buffered);
      for (const [a, b] of ranges) {
        const l = frac(a);
        const r = frac(b);
        if (r > l) buffered.append(h('span', { style: { left: `${l * 100}%`, width: `${(r - l) * 100}%` } }));
      }
    },
    // chapters: [{start_ms}], sentences: [{start_ms, modified, pending}]
    setMarks(d) {
      data = d;
      drawMarks();
    },
    destroy() {
      ro.disconnect();
    },
  };
}

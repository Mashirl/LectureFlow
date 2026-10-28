// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The second camera angle in the player (ext web3; user request 2026-10-27
// no. 4): picture in picture. The lecture's main video (GET /media, with
// the sound) stays the clock of everything (subtitles, transcript,
// chapters, resume position); the second angle is a muted <video> on
// GET /angles/{name}, which has no sound.
//
//   Angle 1 in the picture (the default): the second angle sits in a small
//   box in a corner of the picture — bottom right when the subtitles are a
//   band below the picture, top right when they are drawn over it — and
//   does not play. It is paused and moved to the clock about every
//   REFRESH_MS (one decoded frame each, about 1.6 frames a second), at once
//   after a seek, so it costs little decoding.
//   A click on the box swaps the two: the second angle fills the picture
//   and plays continuously, kept on the clock (play, pause, rate, the main
//   video waiting; a drift above DRIFT_SEEK s is corrected by a seek, a
//   smaller one by nudging the rate until it is within DRIFT_OK s). The
//   main video, which keeps playing for the sound, is shown in the box.
//   The box can be closed (only the main picture); the bar's button opens
//   it again. Whether it is open is remembered (localStorage, try/catch).
//   A second angle that is not on screen (box closed, angle 1 in the
//   picture) stays paused and is not refreshed.
//
// Only in video mode (the main video plays): in audio mode there is no
// angle 1 to swap with. When the second angle cannot be loaded, a toast
// says so once and the player goes back to angle 1 without the box.
// Nothing here changes server state.

import { h } from './dom.js';
import { t } from './i18n.js';
import { pIcon } from './player_icons.js';
import { readPref, writePref } from './player_util.js';

const REFRESH_MS = 600; // the box's frame interval while it does not play
const TICK_MS = 200; // the sync loop
const DRIFT_SEEK = 0.3; // s: corrected by a seek
const DRIFT_OK = 0.08; // s: close enough (no rate nudge)
const NUDGE = 0.05; // ±5 % playback rate while catching up
const STILL = 0.04; // s: a paused frame this close to the clock is the right one
const PREF_PIP = 'lectureflow.player.pip';

// The angles of a lecture as the server lists them ([{name}]); the first
// one is the second angle (the main video is angle 1).
export function lectureAngles(lec) {
  return ((lec && Array.isArray(lec.angles) && lec.angles) || []).filter((a) => a && typeof a.name === 'string' && a.name);
}

// opts: {url(name) → URL, osd(text), toast(text, kind), onLayout(), tip(name, key)}
export function createAngles(opts) {
  let name = ''; // the second angle's name ('' = none)
  let a2 = null; // its <video>
  let main = null; // the main media's <video> while attached
  let frame = null; // the picture's container while attached
  let primary = 1; // the angle in the picture
  let pipOn = readPref(PREF_PIP, true) !== false;
  let failed = false;
  let mainWaiting = false;
  let lastRefresh = 0;
  let refreshAgain = false;
  let timer = 0;
  let offMain = [];

  const media = h('div', { class: 'pl-pip-media' });
  const label = h('span', { class: 'pl-pip-label', 'aria-hidden': 'true' });
  const swapBtn = h('button', { type: 'button', class: 'pl-pip-swap', onclick: () => swap() }, h('span', { class: 'pl-pip-swap-icon', 'aria-hidden': 'true' }, pIcon('arrow-left-right', { size: 18 })));
  const closeBtn = h('button', { type: 'button', class: 'pl-pip-close', 'aria-label': t('player.angle.close'), title: t('player.angle.close'), onclick: () => setPip(false) }, pIcon('x', { size: 14 }));
  const box = h('div', { class: 'pl-pip', hidden: true }, media, swapBtn, label, closeBtn);
  const button = h(
    'button',
    { type: 'button', class: 'pl-ctl pl-ctl-pip', hidden: true, 'aria-label': t('player.angle.pip'), title: t('player.angle.pip_tip'), 'aria-pressed': 'false', onclick: () => setPip(!pipOn) },
    pIcon('picture-in-picture-2', { size: 18 }),
  );

  const angleName = (n) => t(n === 1 ? 'player.angle.name_1' : 'player.angle.name_2');

  function available() {
    return !!(name && a2 && main && !failed);
  }

  function newVideo(url) {
    const v = document.createElement('video');
    v.className = 'pl-video pl-angle';
    v.preload = 'metadata'; // auto once it plays in the picture (place)
    v.muted = true; // the second angle has no sound; the clock's video has it
    v.playsInline = true;
    v.disablePictureInPicture = true;
    v.setAttribute('aria-hidden', 'true');
    v.addEventListener('error', onError);
    v.addEventListener('loadedmetadata', () => {
      refresh(true);
      opts.onLayout();
    });
    v.addEventListener('seeked', () => {
      if (refreshAgain) {
        refreshAgain = false;
        refresh(true);
      }
    });
    v.src = url;
    return v;
  }

  function onError() {
    if (failed || !a2) return;
    failed = true;
    stopLoop();
    try {
      a2.pause();
    } catch {
      // nothing to stop
    }
    if (primary === 2) primary = 1;
    place();
    sync();
    opts.toast(t('player.angle.failed'), 'warn');
    opts.onLayout();
  }

  // setAngles(list): the lecture's angles (from GET /api/lectures/{id}).
  function setAngles(list) {
    const next = (list[0] && list[0].name) || '';
    if (next === name) return;
    dropVideo();
    name = next;
    failed = false;
    primary = 1;
    if (name && main) a2 = newVideo(opts.url(name));
    place();
    sync();
    opts.onLayout();
  }

  function dropVideo() {
    if (!a2) return;
    a2.removeEventListener('error', onError);
    try {
      a2.pause();
      a2.removeAttribute('src');
      a2.load();
    } catch {
      // already gone
    }
    a2.remove();
    a2 = null;
  }

  // attach(mediaSource, frameEl): the main video started (video mode).
  function attach(src, frameEl) {
    detach();
    if (!src || !src.el || src.kind !== 'video') return;
    main = src.el;
    frame = frameEl;
    mainWaiting = false;
    const on = (type, fn) => {
      main.addEventListener(type, fn);
      offMain.push(() => main && main.removeEventListener(type, fn));
    };
    on('seeking', () => refresh(true));
    on('ratechange', () => {
      if (a2) a2.playbackRate = main.playbackRate;
    });
    on('pause', () => {
      if (a2 && primary === 2) {
        a2.pause();
        refresh(true);
      }
    });
    on('play', () => sync());
    on('waiting', () => {
      mainWaiting = true;
      if (a2 && primary === 2) a2.pause();
    });
    on('playing', () => {
      mainWaiting = false;
      sync();
    });
    if (name && !a2 && !failed) a2 = newVideo(opts.url(name));
    place();
    sync();
  }

  // detach(): the main media is torn down (audio mode, leaving).
  function detach() {
    for (const off of offMain) off();
    offMain = [];
    stopLoop();
    if (a2) a2.pause();
    if (main && primary === 2 && frame && main.parentNode !== frame) frame.append(main);
    main = null;
    frame = null;
    box.hidden = true;
    syncButton();
  }

  // Puts each video where it belongs: the one in the picture into the
  // frame, the other into the box (appending moves; a playing video moved
  // in the same task keeps playing).
  function place() {
    if (!main || !frame) {
      box.hidden = true;
      syncButton();
      return;
    }
    if (!available()) {
      if (main.parentNode !== frame) frame.append(main);
      if (a2 && a2.parentNode) a2.remove();
      box.hidden = true;
      syncButton();
      return;
    }
    const inPicture = primary === 2 ? a2 : main;
    const inBox = primary === 2 ? main : a2;
    if (inPicture.parentNode !== frame) frame.append(inPicture);
    if (inBox.parentNode !== media) media.append(inBox);
    inPicture.classList.remove('is-pip');
    inBox.classList.add('is-pip');
    a2.preload = primary === 2 ? 'auto' : 'metadata';
    const other = primary === 2 ? 1 : 2;
    label.textContent = angleName(other);
    const swapName = t('player.angle.swap', { name: angleName(other) });
    swapBtn.setAttribute('aria-label', swapName);
    swapBtn.title = opts.tip(swapName, 'V');
    box.hidden = !pipOn;
    box.dataset.angle = String(other);
    syncButton();
  }

  function syncButton() {
    button.hidden = !available();
    button.setAttribute('aria-pressed', String(pipOn));
    button.classList.toggle('is-on', pipOn && available());
  }

  function setPip(on, quiet = false) {
    pipOn = !!on;
    writePref(PREF_PIP, pipOn);
    place();
    sync();
    opts.onLayout();
    if (!quiet) opts.osd(t(pipOn ? 'player.angle.osd_pip_on' : 'player.angle.osd_pip_off'));
    if (!pipOn && box.contains(document.activeElement)) button.focus({ preventScroll: true });
  }

  function swap() {
    if (!available()) return false;
    primary = primary === 2 ? 1 : 2;
    if (a2) {
      a2.playbackRate = main.playbackRate;
      a2.preservesPitch = true;
      if (Math.abs(a2.currentTime - main.currentTime) > STILL) a2.currentTime = main.currentTime;
    }
    place();
    sync();
    opts.onLayout();
    opts.osd(t('player.angle.osd_now', { name: angleName(primary) }));
    return true;
  }

  // ---- keeping the second angle on the clock ----

  // refresh(now): the box's frame (paused) or the picture's position
  // (playing) moves to the clock; now = a seek happened (no throttle).
  function refresh(now) {
    if (!available() || !a2.readyState) return;
    const clock = main.currentTime;
    if (primary === 2) {
      if (Math.abs(a2.currentTime - clock) > (main.paused ? STILL : DRIFT_SEEK) || now) {
        if (a2.seeking) refreshAgain = true;
        else a2.currentTime = clock;
      }
      return;
    }
    if (!pipOn) return;
    const tNow = performance.now();
    if (!now && tNow - lastRefresh < REFRESH_MS) return;
    if (Math.abs(a2.currentTime - clock) <= STILL) return;
    if (a2.seeking) {
      refreshAgain = now || refreshAgain;
      return;
    }
    lastRefresh = tNow;
    a2.currentTime = clock;
  }

  function step() {
    if (!available()) return;
    if (primary === 1) {
      if (!a2.paused) a2.pause();
      refresh(false);
      return;
    }
    const playing = !main.paused && !mainWaiting && !main.ended;
    if (playing && a2.paused) a2.play().catch(() => {});
    if (!playing && !a2.paused) a2.pause();
    if (a2.seeking || a2.readyState < 2) return;
    const rate = main.playbackRate;
    const drift = a2.currentTime - main.currentTime;
    if (Math.abs(drift) > DRIFT_SEEK || (!playing && Math.abs(drift) > STILL)) {
      a2.playbackRate = rate;
      a2.currentTime = main.currentTime + (playing ? 0.04 : 0);
    } else if (playing && Math.abs(drift) > DRIFT_OK) {
      a2.playbackRate = rate * (drift > 0 ? 1 - NUDGE : 1 + NUDGE);
    } else if (a2.playbackRate !== rate) {
      a2.playbackRate = rate;
    }
  }

  function sync() {
    const run = available() && (primary === 2 || pipOn);
    if (run && !timer) timer = setInterval(step, TICK_MS);
    if (run) step();
    else {
      stopLoop();
      if (a2 && !a2.paused) a2.pause(); // not on screen: nothing to decode
    }
  }

  function stopLoop() {
    clearInterval(timer);
    timer = 0;
  }

  // ---- layout ----

  // layout(fr, {W, H, overlay}): the box in a corner of the picture rect fr
  // (stage pixels) that the subtitles do not use.
  function layout(fr, { W, H, overlay }) {
    if (box.hidden || !fr || !fr.width) return;
    const inBox = primary === 2 ? main : a2;
    const aspect = inBox && inBox.videoWidth && inBox.videoHeight ? inBox.videoWidth / inBox.videoHeight : 16 / 9;
    const w = Math.round(Math.max(120, Math.min(360, fr.width * 0.24)));
    const hgt = Math.round(w / aspect);
    const margin = Math.max(8, Math.round(Math.min(fr.width, fr.height) * 0.02));
    const right = W - (fr.left + fr.width) + margin;
    const style = { width: `${w}px`, height: `${hgt}px`, right: `${right}px`, left: '', top: '', bottom: '' };
    if (overlay) style.top = `${Math.max(fr.top + margin, 52)}px`; // below the OSD; the subtitles are at the bottom
    else style.bottom = `${Math.max(0, H - (fr.top + fr.height)) + margin}px`;
    Object.assign(box.style, style);
  }

  return {
    box,
    button,
    setAngles,
    attach,
    detach,
    swap,
    setPip,
    layout,
    available,
    // the element showing the picture (clicks on it play / pause) and its aspect
    pictureEl: () => (available() && primary === 2 ? a2 : main),
    aspect() {
      const v = available() && primary === 2 ? a2 : null;
      return v && v.videoWidth && v.videoHeight ? v.videoWidth / v.videoHeight : 0;
    },
    // the main video is not on screen (angle 2 in the picture, the box
    // closed): its frames are not presented, the player's clock must not
    // wait for them (requestVideoFrameCallback)
    mainHidden: () => available() && primary === 2 && !pipOn,
    get primary() {
      return primary;
    },
    get pipOn() {
      return pipOn;
    },
    destroy() {
      detach();
      dropVideo();
      box.remove();
    },
  };
}

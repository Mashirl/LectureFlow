// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Against clickjacking (src/lib/embed.js): the controls that confirm
// something — send, "仍然发送", "跳过这 N 节" (sends the rest), "对这个平台不
// 再提醒", trusting a site — act only once they have been shown (present and
// enabled) for a while, and, where the browser can tell (IntersectionObserver
// v2 on Chrome), only while the card is really visible. A click that comes
// too early is dropped; one on a card that is covered, transparent or
// otherwise not visible hands over to the extension's own window.
//
// The cards re-render by replacing their children, so a control is tracked
// by its id: shown since the first time an element with that id was present
// and enabled, forgotten when it goes away.

import { guardDecision, ARM_DELAY_MS } from '../lib/embed.js';

/** Confirming controls by id; a label line counts as its check box. */
const GUARDED = { primary: 'primary', 'override-confirm': 'override-confirm', 'override-skip': 'override-skip', 'override-remember': 'override-remember', 'override-remember-line': 'override-remember', trust: 'trust', 'trust-line': 'trust' };
const SELECTOR = Object.keys(GUARDED).map((id) => `#${id}`).join(',');
const TRACKED = [...new Set(Object.values(GUARDED))];

const now = () => (globalThis.performance && performance.now ? performance.now() : Date.now());

/** Whether this browser can tell whether an element is really visible (IntersectionObserver v2). */
export function canCheckVisibility() {
  return typeof IntersectionObserver === 'function' && typeof IntersectionObserverEntry === 'function' && 'isVisible' in IntersectionObserverEntry.prototype;
}

/**
 * Watches whether `target` is really visible (not covered, not transparent,
 * not transformed or filtered, in view). Returns { get() → true | false |
 * null (no answer yet), stop() }, or null where the browser cannot tell.
 */
export function watchVisibility(target) {
  if (!canCheckVisibility()) return null;
  let visible = null;
  let io;
  try {
    io = new IntersectionObserver(
      (entries) => {
        for (const e of entries) visible = e.isVisible === true;
      },
      { trackVisibility: true, delay: 100 },
    );
    io.observe(target);
  } catch {
    return null;
  }
  return { get: () => visible, stop: () => io.disconnect() };
}

/**
 * Guards the confirming controls inside `root`. opts: delayMs; visibility
 * (watchVisibility's handle, or null); onInvisible() when a click lands on
 * a card that is not visible. Returns { stop() }.
 */
export function guardClicks(root, { delayMs = ARM_DELAY_MS, visibility = null, onInvisible = () => {} } = {}) {
  const shown = new Map(); // id → when it was first present and enabled
  function scan() {
    const t = now();
    const present = new Set();
    for (const id of TRACKED) {
      const node = root.querySelector(`#${id}`);
      if (node && !node.disabled) present.add(id);
    }
    for (const id of present) if (!shown.has(id)) shown.set(id, t);
    for (const id of [...shown.keys()]) if (!present.has(id)) shown.delete(id);
  }
  const mo = new MutationObserver(scan);
  mo.observe(root, { subtree: true, childList: true, attributes: true, attributeFilter: ['disabled'] });
  scan();
  const onClick = (ev) => {
    const target = ev.target && typeof ev.target.closest === 'function' ? ev.target.closest(SELECTOR) : null;
    if (!target || !root.contains(target)) return;
    scan(); // a control that has just appeared counts from now
    const decision = guardDecision({
      shownAt: shown.get(GUARDED[target.id]),
      now: now(),
      delayMs,
      checksVisibility: !!visibility,
      visible: visibility ? visibility.get() : null,
    });
    if (decision === 'allow') return;
    // A check box must not change either: preventDefault undoes the tick.
    ev.preventDefault();
    ev.stopImmediatePropagation();
    if (decision === 'window') onInvisible();
  };
  root.addEventListener('click', onClick, true);
  return {
    stop() {
      mo.disconnect();
      root.removeEventListener('click', onClick, true);
      if (visibility) visibility.stop();
    },
  };
}

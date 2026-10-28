// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Registered only on sites the user has authorized (src/background/sites.js),
// at document_start in every frame. It remembers the media and manifest
// requests the page makes — the resource-timing buffer keeps only 250
// entries, a playing video fills it with segments — and whether a media
// element reported encrypted media. It changes nothing on the page and sends
// nothing anywhere; the scan (src/content/scan.js) reads what it kept when
// detection runs. A classic script: content scripts cannot be modules.
//
// Page-response observation (profile `observe`, src/lib/observe.js): it asks
// the background whether this frame's address belongs to a profile that
// observes a response; if so it hands the request pattern and pointer names
// to the main-world part (src/content/observe-main.js) and keeps, in memory
// only, the values that part reports — after checking them again here:
// declared request, declared names, allowed value shapes. Nothing else of a
// response ever reaches the extension.

(() => {
  if (globalThis.__lectureflowObserver) return;
  const store = { resources: [], eme: { encrypted: 0 }, observed: [] };
  globalThis.__lectureflowObserver = store;
  const MAX = 500;
  // A coarse pre-filter to bound memory; src/lib/classify.js decides.
  const INTERESTING = /\.(m3u8?|mpd|mp4|m4a|m4v|mp3|aac|webm|weba|mov|ogg|oga|opus|wav|flac)(?:$|[?#])|manifest\(format=/i;
  try {
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) {
        if (store.resources.length >= MAX) return;
        const path = e.name.split('?')[0];
        if (e.initiatorType === 'video' || e.initiatorType === 'audio' || INTERESTING.test(path)) {
          store.resources.push({
            url: e.name,
            initiatorType: e.initiatorType || '',
            responseStatus: typeof e.responseStatus === 'number' ? e.responseStatus : null,
          });
        }
      }
    }).observe({ type: 'resource', buffered: true });
  } catch {
    // no PerformanceObserver
  }
  // "encrypted" does not bubble; a capturing listener still sees it.
  document.addEventListener(
    'encrypted',
    () => {
      store.eme.encrypted++;
    },
    true,
  );

  // ------------------------------------------------ response observation
  const CONFIG_EVENT = 'lectureflow-observe-config';
  const READY_EVENT = 'lectureflow-observe-ready';
  const RESULT_EVENT = 'lectureflow-observed';
  const MAX_STRING = 2048;
  const MAX_ARRAY = 16;
  let config = null; // { key, specs: [{ urlPattern, hosts, pointers }] } from the background
  let compiled = [];
  let answer = null; // the JSON string handed to the main world

  const send = () => {
    if (answer === null) return;
    try {
      document.dispatchEvent(new CustomEvent(CONFIG_EVENT, { detail: answer }));
    } catch {
      // nothing to tell
    }
  };
  // The main-world part may start after this script: it says so, and gets
  // the answer again.
  document.addEventListener(READY_EVENT, send);

  const hostMatches = (pattern, host) => {
    const p = String(pattern).toLowerCase();
    const h = String(host).toLowerCase();
    if (p.indexOf('*.') === 0) return h === p.slice(2) || h.endsWith(p.slice(1));
    return h === p;
  };
  const scalar = (v) =>
    v === null || typeof v === 'boolean' || (typeof v === 'number' && isFinite(v)) || (typeof v === 'string' && v.length <= MAX_STRING);

  document.addEventListener(RESULT_EVENT, (ev) => {
    if (!config) return;
    try {
      const r = JSON.parse(String(ev.detail));
      if (!r || r.key !== config.key || !Number.isInteger(r.index) || !compiled[r.index]) return;
      const spec = compiled[r.index];
      const u = new URL(String(r.url));
      if (u.protocol !== 'http:' && u.protocol !== 'https:') return;
      const bare = `${u.protocol}//${u.host}${u.pathname}`;
      if (!spec.hosts.some((h) => hostMatches(h, u.hostname)) || bare.length > 2048 || !spec.re.test(bare)) return;
      const values = {};
      const given = r.values && typeof r.values === 'object' ? r.values : {};
      for (const name of spec.names) {
        if (!Object.prototype.hasOwnProperty.call(given, name)) continue;
        const v = given[name];
        if (Array.isArray(v)) values[name] = v.filter(scalar).slice(0, MAX_ARRAY);
        else if (scalar(v)) values[name] = v;
      }
      store.observed[r.index] = { key: config.key, index: r.index, url: bare, values };
    } catch {
      // not ours, or malformed
    }
  });

  try {
    const rt = (globalThis.browser || globalThis.chrome).runtime;
    // the origins this frame is embedded in: none of them may be a page
    // LectureFlow does not handle (src/lib/blocklist.js)
    let ancestors = null;
    try {
      if (window.top !== window && location.ancestorOrigins) ancestors = [...location.ancestorOrigins].slice(0, 64).map(String);
    } catch {
      ancestors = null;
    }
    Promise.resolve(rt.sendMessage({ cmd: 'observeConfig', ancestors }))
      .then((c) => {
        if (c && typeof c.key === 'string' && Array.isArray(c.specs) && c.specs.length) {
          config = c;
          compiled = c.specs.map((s) => ({ re: new RegExp(s.urlPattern, 'i'), hosts: s.hosts, names: Object.keys(s.pointers) }));
          answer = JSON.stringify(c);
        } else answer = 'null';
        send();
      })
      .catch(() => {
        answer = 'null';
        send();
      });
  } catch {
    answer = 'null';
    send();
  }
})();

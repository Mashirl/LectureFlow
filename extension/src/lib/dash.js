// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// DASH manifest (MPD) parsing and splitting into tracks (SPEC-ext 通用轨道
// 探测: AdaptationSets counted by contentType as audio or video). Pure.

import { parseXml, localName, getAttr, childElements, textOf } from './xml.js';
import { kindOfCodecs, KIND_AUDIO, KIND_VIDEO, KIND_UNKNOWN, FORMAT_DASH } from './classify.js';
import { parseHttpUrl, urlKey } from './url.js';

/** ISO 8601 duration ("PT1H2M3.5S", "P1DT2H") to seconds; null if bad. */
export function parseIsoDuration(s) {
  const str = String(s || '').trim();
  const m = /^P(?:(\d+(?:\.\d+)?)Y)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)W)?(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$/.exec(
    str,
  );
  if (!m || str === 'P' || str.endsWith('T')) return null;
  const [, y, mo, w, d, h, mi, se] = m.map((x) => (x === undefined ? 0 : Number(x)));
  return y * 365 * 86400 + mo * 30 * 86400 + w * 7 * 86400 + d * 86400 + h * 3600 + mi * 60 + se;
}

function kindOfMime(mime) {
  const t = String(mime || '').toLowerCase();
  if (t.startsWith('audio/')) return KIND_AUDIO;
  if (t.startsWith('video/')) return KIND_VIDEO;
  if (t.startsWith('text/') || t.startsWith('image/') || t.startsWith('application/ttml')) return 'text';
  return '';
}

function kindOfAdaptationSet(as) {
  const ct = (getAttr(as, 'contentType') || '').toLowerCase();
  if (ct === 'audio' || ct === 'video') return ct;
  if (ct === 'text' || ct === 'image') return 'text';
  for (const cc of childElements(as, 'ContentComponent')) {
    const c = (getAttr(cc, 'contentType') || '').toLowerCase();
    if (c === 'video') return KIND_VIDEO;
    if (c === 'audio') return KIND_AUDIO;
  }
  const byMime = kindOfMime(getAttr(as, 'mimeType'));
  if (byMime) return byMime;
  const reps = childElements(as, 'Representation');
  for (const r of reps) {
    const k = kindOfMime(getAttr(r, 'mimeType'));
    if (k) return k;
  }
  const codecs = getAttr(as, 'codecs') || reps.map((r) => getAttr(r, 'codecs') || '').join(',');
  if (/^(stpp|wvtt)/i.test(codecs)) return 'text';
  const k = kindOfCodecs(codecs);
  return k;
}

function baseUrlOf(el, parentBase) {
  const b = childElements(el, 'BaseURL')[0];
  if (!b) return parentBase;
  const u = parseHttpUrl(textOf(b).trim(), parentBase);
  return u ? u.href : parentBase;
}

function hasProtection(el) {
  return childElements(el, 'ContentProtection').length > 0;
}

/** Turns a SegmentTemplate media/initialization string into a RegExp source. */
export function templateToRegex(template, repId, bandwidth) {
  let out = '';
  const re = /\$([A-Za-z]*)(%0\d+d)?\$/g;
  let last = 0;
  let m;
  while ((m = re.exec(template))) {
    out += escapeRe(template.slice(last, m.index));
    const id = m[1];
    if (id === '') out += '\\$';
    else if (id === 'RepresentationID') out += escapeRe(repId || '');
    else if (id === 'Bandwidth') out += bandwidth ? String(bandwidth) : '\\d+';
    else out += '\\d+'; // Number, Time, SubNumber
    last = re.lastIndex;
  }
  out += escapeRe(template.slice(last));
  return out;
}

/**
 * A RegExp source matching the absolute URLs of a template relative to
 * base: each $…$ variable is swapped for a marker, the URL resolved, and
 * the markers turned into their patterns.
 */
export function resolvedTemplateRegex(template, base, repId, bandwidth) {
  const vars = [];
  const marked = template.replace(/\$([A-Za-z]*)(%0\d+d)?\$/g, (m) => {
    vars.push(m);
    return `lfvar${vars.length - 1}lf`;
  });
  const abs = parseHttpUrl(marked, base);
  if (!abs) return null;
  abs.hash = '';
  let out = escapeRe(abs.href);
  vars.forEach((v, i) => {
    out = out.replace(`lfvar${i}lf`, () => templateToRegex(v, repId, bandwidth));
  });
  return '^' + out + '$';
}

function escapeRe(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Parses an MPD. Returns {
 *   type: 'static' | 'dynamic' | 'invalid', durationSec,
 *   adaptationSets: [{ period, ordinal, kind, lang, protected, maxHeight,
 *     maxBandwidth, representations: [{ id, bandwidth, width, height, codecs }],
 *     childUrls: [url], childPatterns: [regexSource] }]
 * }
 */
export function parseMpd(text, baseUrl) {
  let root;
  try {
    root = parseXml(text);
  } catch {
    return { type: 'invalid', durationSec: null, adaptationSets: [], drm: false };
  }
  if (localName(root.name) !== 'MPD') return { type: 'invalid', durationSec: null, adaptationSets: [], drm: false };
  const type = (getAttr(root, 'type') || 'static').toLowerCase() === 'dynamic' ? 'dynamic' : 'static';
  let durationSec = parseIsoDuration(getAttr(root, 'mediaPresentationDuration') || '');
  const mpdBase = baseUrlOf(root, baseUrl);
  const periods = childElements(root, 'Period');
  if (durationSec === null) {
    const sum = periods.reduce((acc, p) => acc + (parseIsoDuration(getAttr(p, 'duration') || '') || 0), 0);
    durationSec = sum > 0 ? sum : null;
  }
  const sets = [];
  periods.forEach((period, pIndex) => {
    const periodBase = baseUrlOf(period, mpdBase);
    const ordinals = new Map();
    for (const as of childElements(period, 'AdaptationSet')) {
      const kind = kindOfAdaptationSet(as);
      if (kind === 'text') continue;
      const lang = (getAttr(as, 'lang') || '').toLowerCase();
      const ordKey = `${kind}\u0000${lang}`;
      const ordinal = ordinals.get(ordKey) || 0;
      ordinals.set(ordKey, ordinal + 1);
      const asBase = baseUrlOf(as, periodBase);
      const asTemplate = childElements(as, 'SegmentTemplate')[0];
      const reps = [];
      const childUrls = [];
      const childPatterns = [];
      let prot = hasProtection(as);
      for (const r of childElements(as, 'Representation')) {
        const id = getAttr(r, 'id') || '';
        const bandwidth = Number(getAttr(r, 'bandwidth')) || null;
        const height = Number(getAttr(r, 'height') || getAttr(as, 'height')) || null;
        const width = Number(getAttr(r, 'width') || getAttr(as, 'width')) || null;
        reps.push({ id, bandwidth, width, height, codecs: getAttr(r, 'codecs') || getAttr(as, 'codecs') || '' });
        if (hasProtection(r)) prot = true;
        const repBase = baseUrlOf(r, asBase);
        const template = childElements(r, 'SegmentTemplate')[0] || asTemplate;
        const list = childElements(r, 'SegmentList')[0] || childElements(as, 'SegmentList')[0];
        if (template) {
          for (const a of ['media', 'initialization']) {
            const t = getAttr(template, a);
            if (!t) continue;
            const re = resolvedTemplateRegex(t, repBase, id, bandwidth);
            if (re) childPatterns.push(re);
          }
        } else if (list) {
          for (const su of childElements(list, 'SegmentURL')) {
            const media = getAttr(su, 'media');
            const abs = media ? parseHttpUrl(media, repBase) : null;
            if (abs) childUrls.push(urlKey(abs.href));
          }
          const init = childElements(list, 'Initialization')[0];
          const src = init && getAttr(init, 'sourceURL');
          const abs = src ? parseHttpUrl(src, repBase) : null;
          if (abs) childUrls.push(urlKey(abs.href));
        } else if (childElements(r, 'BaseURL').length > 0) {
          // SegmentBase or a bare BaseURL: the representation is one file
          // the player reads with byte ranges.
          childUrls.push(urlKey(repBase));
        }
      }
      sets.push({
        period: pIndex,
        ordinal,
        kind: kind || KIND_UNKNOWN,
        lang,
        protected: prot,
        maxHeight: Math.max(0, ...reps.map((r) => r.height || 0)) || null,
        maxBandwidth: Math.max(0, ...reps.map((r) => r.bandwidth || 0)) || null,
        representations: reps,
        childUrls,
        childPatterns,
      });
    }
  });
  return { type, durationSec, adaptationSets: sets, drm: sets.some((s) => s.protected) };
}

/**
 * Tracks of one MPD: one per AdaptationSet, merged across Periods by kind,
 * language and order. The track url is the MPD's; `selector` tells which
 * AdaptationSet (the bridge protocol has no field for it yet — see NOTES).
 */
export function dashTracks(parsed, manifestUrl) {
  const url = urlKey(manifestUrl);
  if (parsed.type === 'invalid') return [];
  const merged = new Map();
  for (const s of parsed.adaptationSets) {
    const key = `${s.kind}\u0000${s.lang}\u0000${s.ordinal}`;
    const cur = merged.get(key);
    if (!cur) {
      merged.set(key, { ...s, representations: [...s.representations], childUrls: [...s.childUrls], childPatterns: [...s.childPatterns] });
    } else {
      cur.protected = cur.protected || s.protected;
      cur.maxHeight = Math.max(cur.maxHeight || 0, s.maxHeight || 0) || null;
      cur.maxBandwidth = Math.max(cur.maxBandwidth || 0, s.maxBandwidth || 0) || null;
      cur.childUrls.push(...s.childUrls);
      cur.childPatterns.push(...s.childPatterns);
    }
  }
  const tracks = [];
  for (const s of merged.values()) {
    const sorted = [...s.representations].sort((a, b) => (b.height || 0) - (a.height || 0) || (b.bandwidth || 0) - (a.bandwidth || 0));
    tracks.push({
      url,
      selector: `${s.kind}:${s.lang || '-'}:${s.ordinal}`,
      kind: s.kind,
      format: FORMAT_DASH,
      quality: s.kind === KIND_VIDEO && s.maxHeight ? `${s.maxHeight}p` : '',
      bandwidth: s.maxBandwidth,
      variants: sorted.map((r) => ({ id: r.id, bandwidth: r.bandwidth, width: r.width, height: r.height, codecs: r.codecs })),
      protected: s.protected,
      childUrls: [...new Set(s.childUrls)],
      childPatterns: [...new Set(s.childPatterns)],
      probeUrl: null,
      durationSec: parsed.durationSec,
      name: '',
      language: s.lang,
    });
  }
  return tracks;
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Generic track detection (SPEC-ext 通用轨道探测). Pure: the network is
// reached only through the two functions in `io`, so the same code runs in
// the background and in the fixture replay (src/lib/replay.js).

import { buildCandidates, mergeCandidates, collapseSegmentRuns, SOURCE_ORDER } from './merge.js';
import { C_HLS, C_DASH, C_FILE, KIND_AUDIO, KIND_VIDEO, KIND_UNKNOWN, FORMAT_FILE, FORMAT_HLS, FORMAT_DASH, mediaType } from './classify.js';
import { parseHls, hlsTracks, estimateSize, mediaPlaylistKind } from './hls.js';
import { parseMpd, dashTracks } from './dash.js';
import { urlKey } from './url.js';

export const LIMITS = Object.freeze({
  manifests: 12, // manifests fetched per detection
  probes: 8, // media playlists fetched for duration and DRM signals
  files: 8, // single files probed for size and type
});

/**
 * input: { frames, webRequests } (see buildCandidates)
 * io: {
 *   fetchText(url, { frameIds, purpose }) → Promise<{ ok, status, contentType, text, error }>
 *   probeFile(url, { frameIds }) → Promise<{ ok, status, contentType, size, error }>
 * }
 * Returns { tracks, evidence, segmentRuns, fetches, ignored }.
 * fetches is the log of every io call in call order; the report keeps it
 * so a replay can answer the same calls.
 */
export async function detectTracks(input, io, limits = LIMITS) {
  const { candidates, evidence } = buildCandidates(input);
  const merged = mergeCandidates(candidates);
  const segmentRuns = collapseSegmentRuns(merged);
  const fetches = [];
  const textCache = new Map();

  const fetchText = async (url, ctx) => {
    if (textCache.has(url)) return textCache.get(url);
    let res;
    try {
      res = await io.fetchText(url, ctx);
    } catch (e) {
      res = { ok: false, status: 0, contentType: '', text: '', error: String(e && e.message ? e.message : e) };
    }
    res = normalizeFetch(res);
    fetches.push({ purpose: ctx.purpose, url, ...res });
    textCache.set(url, res);
    return res;
  };

  // 1. Manifests: fetch and parse.
  const manifestCands = merged.filter((m) => m.cls === C_HLS || m.cls === C_DASH);
  const parsedManifests = [];
  for (const m of manifestCands.slice(0, limits.manifests)) {
    const res = await fetchText(m.url, { frameIds: m.frameIds, purpose: 'manifest' });
    let parsed = null;
    let cls = m.cls;
    if (res.ok) {
      // The body decides when the server's type and the name disagree.
      const body = res.text.replace(/^﻿/, '').trimStart();
      if (body.startsWith('#EXTM3U')) cls = C_HLS;
      else if (body.startsWith('<')) cls = C_DASH;
      parsed = cls === C_HLS ? parseHls(res.text, m.url) : parseMpd(res.text, m.url);
      if (parsed.type === 'invalid') parsed = null;
    }
    parsedManifests.push({ cand: m, cls, parsed, fetched: res.ok });
  }
  const unread = manifestCands.slice(limits.manifests);

  // 2. Tracks from manifests. Masters and MPDs first, so that media
  //    playlists the player loaded on their behalf are absorbed.
  const absorbed = new Map(); // child url → owning track
  const patterns = []; // [RegExp, track]
  const tracks = [];
  const order = [...parsedManifests].sort((a, b) => rank(a) - rank(b));
  for (const pm of order) {
    if (pm.parsed && pm.cls === C_HLS && pm.parsed.type === 'media' && absorbed.has(pm.cand.url)) {
      const owner = absorbed.get(pm.cand.url);
      addSources(owner, pm.cand.sources);
      pm.parsed.segments.forEach((s) => absorbed.set(s, owner));
      if (pm.parsed.drm) owner.protected = true;
      if (!owner.durationSec && pm.parsed.durationSec) owner.durationSec = pm.parsed.durationSec;
      continue;
    }
    if (!pm.parsed) {
      tracks.push(baseTrack({
        url: pm.cand.url,
        kind: KIND_UNKNOWN,
        format: pm.cls === C_HLS ? FORMAT_HLS : FORMAT_DASH,
        sources: pm.cand.sources,
        frameIds: pm.cand.frameIds,
        unreadable: true,
      }));
      continue;
    }
    const derived = pm.cls === C_HLS ? hlsTracks(pm.parsed, pm.cand.url) : dashTracks(pm.parsed, pm.cand.url);
    for (const d of derived) {
      const t = baseTrack({ ...d, sources: pm.cand.sources, frameIds: pm.cand.frameIds });
      tracks.push(t);
      for (const c of d.childUrls || []) absorbed.set(c, t);
      for (const p of d.childPatterns || []) {
        try {
          patterns.push([new RegExp(p), t]);
        } catch {
          // a template we could not turn into a pattern; nothing absorbed
        }
      }
    }
    if (pm.cls === C_HLS && pm.parsed.type === 'media') {
      pm.parsed.segments.forEach((s) => absorbed.set(s, tracks[tracks.length - 1]));
      pm.parsed.initUris.forEach((s) => absorbed.set(s, tracks[tracks.length - 1]));
    }
  }
  for (const m of unread) {
    tracks.push(baseTrack({
      url: m.url,
      kind: KIND_UNKNOWN,
      format: m.cls === C_HLS ? FORMAT_HLS : FORMAT_DASH,
      sources: m.sources,
      frameIds: m.frameIds,
      unreadable: true,
    }));
  }

  // 3. Probe one media playlist per HLS track for duration and DRM.
  let probes = 0;
  for (const t of tracks) {
    if (t.format !== FORMAT_HLS || !t.probeUrl || probes >= limits.probes) continue;
    if (t.durationSec && t.probeUrl === t.url) continue;
    probes++;
    const res = await fetchText(t.probeUrl, { frameIds: t.frameIds, purpose: 'probe' });
    if (!res.ok) continue;
    const p = parseHls(res.text, t.probeUrl);
    if (p.type !== 'media') continue;
    if (p.drm) t.protected = true;
    if (p.durationSec) t.durationSec = p.durationSec;
    if (t.kind === KIND_UNKNOWN) {
      const k = mediaPlaylistKind(p);
      if (k !== KIND_UNKNOWN) t.kind = k;
    }
    p.segments.forEach((s) => absorbed.set(s, t));
    p.initUris.forEach((s) => absorbed.set(s, t));
  }

  // 4. Single files that no manifest owns.
  const ignored = [];
  let fileProbes = 0;
  for (const m of merged) {
    if (m.cls !== C_FILE) continue;
    const owner = absorbed.get(m.url) || patterns.find(([re]) => re.test(m.url))?.[1];
    if (owner) {
      addSources(owner, m.sources);
      continue;
    }
    const t = baseTrack({
      url: m.url,
      kind: m.kind,
      format: FORMAT_FILE,
      sources: m.sources,
      frameIds: m.frameIds,
      mime: mediaType(m.hints.contentType || m.hints.sourceType || ''),
      sizeBytes: m.hints.size > 0 ? m.hints.size : -1,
      durationSec: m.hints.duration || null,
    });
    // What the media element knows: an <audio>, or a <video> with no
    // picture once its metadata is in, is audio.
    if (m.hints.tag === 'audio') t.kind = KIND_AUDIO;
    else if (m.hints.tag === 'video' && m.hints.readyState >= 1) {
      if (m.hints.videoWidth === 0 && m.hints.videoHeight === 0) t.kind = KIND_AUDIO;
      else if (m.hints.videoHeight > 0) {
        t.kind = KIND_VIDEO;
        t.quality = `${m.hints.videoHeight}p`;
      }
    }
    if ((t.sizeBytes < 0 || t.kind === KIND_UNKNOWN || !t.mime) && fileProbes < limits.files) {
      fileProbes++;
      let res;
      try {
        res = await io.probeFile(m.url, { frameIds: m.frameIds });
      } catch (e) {
        res = { ok: false, status: 0, contentType: '', size: -1, error: String(e && e.message ? e.message : e) };
      }
      res = normalizeProbe(res);
      fetches.push({ purpose: 'file', url: m.url, ...res });
      if (res.ok) {
        if (res.size > 0 && t.sizeBytes < 0) t.sizeBytes = res.size;
        const type = mediaType(res.contentType);
        if (type && !t.mime) t.mime = type;
        if (t.kind === KIND_UNKNOWN) {
          if (type.startsWith('audio/')) t.kind = KIND_AUDIO;
          else if (type.startsWith('video/')) t.kind = KIND_VIDEO;
        }
      }
    }
    tracks.push(t);
  }
  for (const m of merged) {
    if (m.cls !== C_FILE && m.cls !== C_HLS && m.cls !== C_DASH) ignored.push({ url: m.url, cls: m.cls, sources: m.sources });
  }

  // 5. EME: a media element with MediaKeys protects what it plays. With
  //    MSE (blob: source) we cannot tell which manifest feeds it, so every
  //    adaptive track is marked.
  const keyed = new Set(evidence.mediaKeysUrls);
  for (const t of tracks) {
    if (keyed.has(t.url)) t.protected = true;
    if (evidence.mediaKeysOnStream && t.format !== FORMAT_FILE) t.protected = true;
  }

  // 6. Durations and size estimates.
  const pageDuration = evidence.mediaDurations.length ? Math.max(...evidence.mediaDurations) : null;
  for (const t of tracks) {
    if (!t.durationSec && pageDuration && t.format !== FORMAT_FILE) t.durationSec = pageDuration;
    if (t.sizeBytes < 0 && t.format !== FORMAT_FILE) t.sizeBytes = estimateSize(t.bandwidth, t.durationSec);
    if (t.format !== FORMAT_FILE && t.sizeBytes > 0) t.sizeEstimated = true;
  }

  const final = dedupeTracks(tracks).map(finalize).sort(compareTracks);
  return { tracks: final, evidence, segmentRuns, fetches, ignored };
}

function rank(pm) {
  if (!pm.parsed) return 2;
  if (pm.cls === C_DASH || pm.parsed.type === 'master') return 0;
  return 1;
}

function addSources(track, sources) {
  const set = new Set([...track.sources, ...sources]);
  track.sources = SOURCE_ORDER.filter((s) => set.has(s));
}

function baseTrack(d) {
  return {
    url: d.url,
    selector: d.selector || '',
    kind: d.kind || KIND_UNKNOWN,
    format: d.format,
    quality: d.quality || '',
    bandwidth: d.bandwidth || null,
    sizeBytes: d.sizeBytes ?? -1,
    sizeEstimated: false,
    durationSec: d.durationSec || null,
    sources: SOURCE_ORDER.filter((s) => (d.sources || []).includes(s)),
    frameIds: d.frameIds || [0],
    protected: !!d.protected,
    mime: d.mime || '',
    variants: d.variants || [],
    probeUrl: d.probeUrl || null,
    name: d.name || '',
    language: d.language || '',
    unreadable: !!d.unreadable,
  };
}

// The same rendition can come from two manifests (e.g. a master and a
// second master that shares an audio playlist).
function dedupeTracks(tracks) {
  const seen = new Map();
  for (const t of tracks) {
    const id = trackId(t);
    const cur = seen.get(id);
    if (!cur) seen.set(id, t);
    else {
      addSources(cur, t.sources);
      cur.protected = cur.protected || t.protected;
    }
  }
  return [...seen.values()];
}

export function trackId(t) {
  return t.selector ? `${t.url}#lf=${t.selector}` : t.url;
}

function finalize(t) {
  return {
    id: trackId(t),
    url: t.url,
    selector: t.selector || undefined,
    kind: t.kind,
    format: t.format,
    quality: t.quality || undefined,
    sizeBytes: t.sizeBytes > 0 ? t.sizeBytes : -1,
    sizeEstimated: t.sizeBytes > 0 ? t.sizeEstimated : false,
    durationSec: t.durationSec || undefined,
    sources: t.sources,
    frameIds: t.frameIds,
    protected: t.protected,
    mime: t.mime || undefined,
    variants: t.variants.length ? t.variants : undefined,
    name: t.name || undefined,
    language: t.language || undefined,
    unreadable: t.unreadable || undefined,
  };
}

const KIND_ORDER = { [KIND_VIDEO]: 0, [KIND_AUDIO]: 1, [KIND_UNKNOWN]: 2 };

function compareTracks(a, b) {
  return (
    (KIND_ORDER[a.kind] ?? 3) - (KIND_ORDER[b.kind] ?? 3) ||
    heightOf(b) - heightOf(a) ||
    b.sizeBytes - a.sizeBytes ||
    (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)
  );
}

function heightOf(t) {
  const m = /^(\d+)p$/.exec(t.quality || '');
  return m ? Number(m[1]) : 0;
}

function normalizeFetch(r) {
  return {
    ok: !!r.ok,
    status: Number.isFinite(r.status) ? r.status : 0,
    contentType: r.contentType || '',
    text: typeof r.text === 'string' ? r.text : '',
    error: r.error || undefined,
  };
}

function normalizeProbe(r) {
  return {
    ok: !!r.ok,
    status: Number.isFinite(r.status) ? r.status : 0,
    contentType: r.contentType || '',
    size: Number.isFinite(r.size) && r.size > 0 ? r.size : -1,
    error: r.error || undefined,
  };
}

/** Counts by kind, for the badge and the popup header. */
export function summarize(tracks) {
  const s = { total: tracks.length, audio: 0, video: 0, unknown: 0, protected: 0 };
  for (const t of tracks) {
    s[t.kind] = (s[t.kind] || 0) + 1;
    if (t.protected) s.protected++;
  }
  return s;
}

export { urlKey };

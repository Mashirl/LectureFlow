// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The made-up recording platform of the native end-to-end test, on two
// local servers:
//
//   pages  https://127.0.0.1:P (self-signed): what the test browser reaches
//          as https://tenant.cloud.panopto.eu:P (a Panopto-like viewer,
//          embed player, folder list in the main and the LMS layouts, and
//          DeliveryInfo.aspx) and https://canvas.lf-e2e.test:P (LMS pages
//          that embed the list and the embed player in an iframe);
//   media  http://127.0.0.1:M: the "CDN" — HLS in Panopto's single-file
//          fMP4 shape, the full MP4, and a generic page whose MP4 needs a
//          cookie and a signed query (like a signed CDN URL), plus an EME
//          page and an HLS page with SAMPLE-AES.
//
// LectureFlow downloads from the media server only: the browser maps the
// platform's names to 127.0.0.1 (--host-resolver-rules), LectureFlow's
// aria2c cannot, so the platform's answers name the media by 127.0.0.1 —
// as the real Panopto names its CDN. Every name, id, title and value here
// is made up; the cookie and signature values are random per run.

import http from 'node:http';
import https from 'node:https';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';

export const PANOPTO_HOST = 'tenant.cloud.panopto.eu';
export const CANVAS_HOST = 'canvas.lf-e2e.test';
// What DeliveryInfo says about the viewer: must never leave the page.
export const VIEWER_EMAIL = 'lf-native-student@example.edu';
export const VIEWER_NAME = 'LfNativeStudentName';

const hex12 = (n) => n.toString(16).padStart(12, '0');

function sessionIds(n) {
  const media = `5e550000-0000-4000-8000-${hex12(n)}`;
  return {
    deliveryId: `d0000000-0000-4000-8000-${hex12(n)}`,
    media, // the media path's session id (not the page's id, as on Panopto)
    stream: `57000000-0000-4000-8000-${hex12(n)}`,
    screen: `5c000001-0000-4000-8000-${hex12(n)}`,
    screen2: `5c000002-0000-4000-8000-${hex12(n)}`,
  };
}

const AUDIO_MASTER = '#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=70000,AVERAGE-BANDWIDTH=66000,CODECS="mp4a.40.2"\nindex.m3u8\n';
const SCREEN_MASTER = '#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-STREAM-INF:BANDWIDTH=300000,AVERAGE-BANDWIDTH=260000,RESOLUTION=320x180,CODECS="avc1.42c01e"\nindex.m3u8\n';
const DRM_INDEX =
  '#EXTM3U\n#EXT-X-VERSION:5\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n' +
  '#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://lf-native-test-key",KEYFORMAT="com.apple.streamingkeydelivery",KEYFORMATVERSIONS="1"\n' +
  '#EXTINF:2.0,\nseg0.ts\n#EXTINF:2.0,\nseg1.ts\n#EXT-X-ENDLIST\n';

function send(res, status, type, body, extra = {}) {
  const buf = Buffer.from(body);
  res.writeHead(status, { 'content-type': type, 'content-length': buf.length, 'cache-control': 'no-store', ...extra });
  res.end(buf);
}

/** A file with Range support; hold(), when given, is awaited after the first 16 KiB. */
async function serveFile(req, res, file, type, extra = {}, hold = null) {
  const total = fs.statSync(file).size;
  const m = /^bytes=(\d+)-(\d*)$/.exec(req.headers.range || '');
  let start = 0;
  let end = total - 1;
  const headers = { 'content-type': type, 'accept-ranges': 'bytes', 'cache-control': 'no-store', ...extra };
  if (m) {
    start = Number(m[1]);
    end = m[2] ? Math.min(Number(m[2]), total - 1) : total - 1;
    if (start >= total) {
      res.writeHead(416, { 'content-range': `bytes */${total}` });
      res.end();
      return;
    }
    res.writeHead(206, { ...headers, 'content-range': `bytes ${start}-${end}/${total}`, 'content-length': end - start + 1 });
  } else res.writeHead(200, { ...headers, 'content-length': total });
  if (req.method === 'HEAD') {
    res.end();
    return;
  }
  const body = fs.readFileSync(file).subarray(start, end + 1);
  if (!hold) {
    res.end(body);
    return;
  }
  const first = Math.min(16384, body.length);
  res.write(body.subarray(0, first));
  await hold();
  if (!res.destroyed) res.end(body.subarray(first));
}

function playerPage({ embed }) {
  const titleHtml = embed ? '<h1 id="title"><span class="title-text"></span></h1>' : '<h1 id="deliveryTitle"></h1>';
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>Panopto Viewer</title>
<style>body{font-family:sans-serif;margin:0} #viewerHeader{display:flex;align-items:center;justify-content:space-between;gap:12px;height:56px;padding:0 16px;border-bottom:1px solid #ddd} #header-right-react{display:flex;gap:8px} h1#title{font-size:16px;margin:8px 12px;display:flex;align-items:center;gap:8px}</style>
</head>
<body>
${embed ? '' : '<div id="viewerHeader"><div id="header-left"><span class="brand">ReCap</span></div><div id="header-right-react"><button class="share">Share</button><button class="help">?</button></div></div>'}
${titleHtml}
<div id="player" style="height:180px;background:#111"></div>
<script>
(async () => {
  const q = new URLSearchParams(location.search);
  const body = 'deliveryId=' + encodeURIComponent(q.get('id')) + '&isEmbed=${embed ? 'true' : 'false'}&responseType=json';
  const info = await new Promise((resolve, reject) => {
    const x = new XMLHttpRequest();
    x.open('POST', '/Panopto/Pages/Viewer/DeliveryInfo.aspx');
    x.setRequestHeader('content-type', 'application/x-www-form-urlencoded');
    x.onload = () => resolve(JSON.parse(x.responseText));
    x.onerror = reject;
    x.send(body);
  });
  document.querySelector('${embed ? 'h1#title .title-text' : '#deliveryTitle'}').textContent = info.Delivery.SessionName;
  // The player loads the streams it shows (from the "CDN").
  for (const s of info.Delivery.Streams) {
    const master = await (await fetch(s.StreamUrl)).text();
    const child = master.trim().split('\\n').pop();
    await (await fetch(new URL(child, s.StreamUrl))).text();
  }
  document.body.dataset.ready = '1';
})();
</script>
</body></html>
`;
}

function listPage({ embedded, rows }) {
  const tr = rows
    .map(
      (s, i) =>
        `<tr class="list-view-row" id="${s.deliveryId}"><td class="thumb"><div class="thumbnail">${8 + i}:00</div></td><td><div class="item-title"><a class="detail-title" href="/Panopto/Pages/Viewer.aspx?id=${s.deliveryId}" target="_blank">${s.title}</a></div><div class="item-meta">Lecturer · ${i + 1} views</div></td></tr>`,
    )
    .join('\n');
  const header = embedded
    ? '<div class="css-embed-toolbar"><select class="folder-select"><option>Folder (test)</option></select><div class="css-7abc12"><button class="subscribe-button">+ Subscribe</button></div></div>'
    : '<div id="headerWrapper"><div class="css-3evg5k"><div class="bg-layer"></div><div class="folder-name">Folder (test)</div><div class="css-hdr99"><button class="subscribe-button headerButton">+ Subscribe</button></div></div></div>';
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>Sessions</title>
<style>
body{font-family:sans-serif;margin:0;background:#fafafa}
.css-3evg5k{position:relative;display:flex;align-items:center;justify-content:space-between;height:56px;padding:0 16px;background:#00264d;color:#fff}
.css-3evg5k .bg-layer{position:absolute;inset:0;z-index:2;background:transparent}
.css-hdr99{position:relative;z-index:3;display:flex;align-items:center;gap:8px}
.css-embed-toolbar{display:flex;align-items:center;gap:12px;padding:8px 16px;background:#fff;border-bottom:1px solid #ddd}
.css-7abc12{margin-left:auto;display:flex;align-items:center;gap:8px}
table{border-collapse:collapse;width:100%} td{padding:10px 16px;vertical-align:top}
.thumbnail{width:120px;height:60px;background:#333;color:#fff}
.item-title{font-size:16px;font-weight:600}
.item-meta{color:#777;font-size:13px}
</style></head>
<body>
${header}
<table><tbody>
${tr}
</tbody></table>
<script>document.body.dataset.ready = '1';</script>
</body></html>
`;
}

// small: a small embedded player (about 380 × 210, as on a user's Canvas
// page), with no room for the in-page card.
function lmsPage(src, { small = false } = {}) {
  const size = small ? 'width:380px;height:214px' : 'width:100%;height:640px';
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>LMS (test)</title>
<style>body{margin:0;font-family:sans-serif} .nav{height:40px;background:#394b58;color:#fff;padding:8px} iframe{border:0;${size};display:block}</style></head>
<body><div class="nav">Course (test LMS page)</div>
<iframe name="tool_content" title="ReCap" src="${src}"></iframe>
</body></html>
`;
}

const GENERIC_PAGE = (videoSrc) => `<!doctype html>
<html><head><meta charset="utf-8"><title>Lecture (generic test page)</title></head>
<body><h1>Lecture recording (test page)</h1>
<video id="v" src="${videoSrc}" preload="metadata" muted controls width="320"></video>
</body></html>
`;

const EME_PAGE = `<!doctype html>
<html><head><meta charset="utf-8"><title>EME</title></head>
<body><h1>EME (test page)</h1>
<video id="v" src="/g/protected.mp4" preload="none" muted></video>
<script>
(async () => {
  try {
    const access = await navigator.requestMediaKeySystemAccess('org.w3.clearkey', [{ initDataTypes: ['cenc'], videoCapabilities: [{ contentType: 'video/mp4; codecs="avc1.42E01E"' }] }]);
    await document.getElementById('v').setMediaKeys(await access.createMediaKeys());
    document.title = 'eme-ready';
  } catch (e) {
    document.title = 'eme-failed: ' + e.name;
  }
})();
</script></body></html>
`;

const DRM_PAGE = `<!doctype html>
<html><head><meta charset="utf-8"><title>DRM HLS</title></head>
<body><h1>SAMPLE-AES HLS (test page)</h1>
<script>
(async () => {
  await (await fetch('/g/drm/master.m3u8')).text();
  await (await fetch('/g/drm/index.m3u8')).text();
  document.body.dataset.ready = '1';
})();
</script></body></html>
`;

/**
 * Starts both servers. tls: { cert, key }. media: makeMedia()'s paths.
 * Returns the handles: origins, ports, the made-up credentials, switches
 * and the request log (no header values are kept, only whether they were
 * right).
 */
export async function startSite({ tls, media }) {
  const state = {
    sessions: new Map(), // deliveryId → session
    byMedia: new Map(), // media session id → session
    list: [], // deliveryIds on the folder list, in order
    stallGeneric: false,
    released: null,
    requests: [],
  };
  let nextSession = 1;
  const creds = {
    cookieName: 'lf_native_auth',
    cookie: `LF-NATIVE-COOKIE-${crypto.randomBytes(8).toString('hex')}`,
    signature: `LfNativeSig${crypto.randomBytes(12).toString('base64url')}`,
    panoptoCookie: `LF-NATIVE-PANOPTO-${crypto.randomBytes(8).toString('hex')}`,
  };
  let releaseStall = () => {};
  let stallGate = Promise.resolve();
  const site = { state, creds };

  // ----------------------------------------------------------- media
  const mediaServer = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1');
    const ua = String(req.headers['user-agent'] || '');
    const cookieOk = String(req.headers.cookie || '').split(/;\s*/).includes(`${creds.cookieName}=${creds.cookie}`);
    let ref = null;
    try {
      ref = req.headers.referer ? new URL(req.headers.referer) : null;
    } catch {
      ref = null;
    }
    const entry = {
      server: 'media', method: req.method, path: url.pathname, query: [...url.searchParams.keys()],
      cookieOk, hasCookie: !!req.headers.cookie, signatureOk: url.searchParams.get('Signature') === creds.signature,
      // the Referer's scheme, host and path, and whether it has a query (its
      // value is never kept): strict-origin-when-cross-origin gives a
      // cross-origin request "https://host:port/", a same-origin one the page
      referer: ref ? ref.host : '', refererScheme: ref ? ref.protocol : '', refererPath: ref ? ref.pathname : '', refererQuery: !!(ref && ref.search),
      aria2: /aria2/i.test(ua), range: !!req.headers.range,
    };
    state.requests.push(entry);
    const cors = { 'access-control-allow-origin': '*' };

    const sm = /^\/sessions\/([0-9a-f-]{36})\/(.+)$/.exec(url.pathname);
    if (sm) {
      const s = state.byMedia.get(sm[1]);
      if (!s) return send(res, 404, 'text/plain', 'no such session', cors);
      const rest = sm[2];
      const hm = /^([0-9a-f-]+)(\.object)?\.hls\/(master\.m3u8|index\.m3u8|fragmented\.mp4)$/.exec(rest);
      if (hm) {
        const which = hm[2] ? (hm[1] === s.ids.screen2 ? 'screen2' : 'screen') : 'audio';
        if (hm[3] === 'master.m3u8') return send(res, 200, 'application/vnd.apple.mpegurl', which === 'audio' ? AUDIO_MASTER : SCREEN_MASTER, cors);
        if (hm[3] === 'index.m3u8') return send(res, 200, 'application/vnd.apple.mpegurl', fs.readFileSync(path.join(media[which], 'index.m3u8')), cors);
        return serveFile(req, res, path.join(media[which], 'fragmented.mp4'), 'video/mp4', cors);
      }
      if (rest === `${s.ids.media}-${s.ids.stream}.mp4`) return serveFile(req, res, media.full, 'video/mp4', cors);
      return send(res, 404, 'text/plain', 'not here', cors);
    }
    // The generic page: its MP4 needs the cookie the page set and the signed query.
    if (url.pathname === '/g/lecture.html') {
      const src = `/g/lecture.mp4?Expires=1924992000&Signature=${encodeURIComponent(creds.signature)}&Key-Pair-Id=LFNATIVEKEYPAIRID`;
      return send(res, 200, 'text/html; charset=utf-8', GENERIC_PAGE(src), { 'set-cookie': `${creds.cookieName}=${creds.cookie}; Path=/; HttpOnly; SameSite=Lax` });
    }
    if (url.pathname === '/g/lecture.mp4') {
      if (!cookieOk || !entry.signatureOk) return send(res, 403, 'text/plain', 'forbidden');
      const hold = state.stallGeneric && entry.aria2 && req.method === 'GET' ? () => stallGate : null;
      if (hold) entry.stalled = true;
      return serveFile(req, res, media.full, 'video/mp4', {}, hold);
    }
    if (url.pathname === '/g/eme.html') return send(res, 200, 'text/html; charset=utf-8', EME_PAGE);
    if (url.pathname === '/g/protected.mp4') return serveFile(req, res, media.full, 'video/mp4');
    if (url.pathname === '/g/drm.html') return send(res, 200, 'text/html; charset=utf-8', DRM_PAGE);
    if (url.pathname === '/g/drm/master.m3u8') return send(res, 200, 'application/vnd.apple.mpegurl', SCREEN_MASTER);
    if (url.pathname === '/g/drm/index.m3u8') return send(res, 200, 'application/vnd.apple.mpegurl', DRM_INDEX);
    if (url.pathname.startsWith('/g/drm/seg')) return send(res, 200, 'video/mp2t', Buffer.alloc(1880));
    send(res, 404, 'text/plain', 'not found');
  });

  // ----------------------------------------------------------- pages
  const pageServer = https.createServer({ cert: tls.cert, key: tls.key }, (req, res) => {
    const url = new URL(req.url, 'https://127.0.0.1');
    const host = String(req.headers.host || '').replace(/:\d+$/, '');
    const port = String(req.headers.host || '').split(':')[1] || '';
    const entry = { server: 'pages', method: req.method, host, path: url.pathname, panoptoCookie: String(req.headers.cookie || '').includes(creds.panoptoCookie) };
    state.requests.push(entry);
    const html = (body, extra) => send(res, 200, 'text/html; charset=utf-8', body, extra);
    // The platform's own sign-in cookie: the Panopto profile reads none (cookieUrls: []).
    const panoptoCookie = { 'set-cookie': `.ASPXAUTH=${creds.panoptoCookie}; Path=/; Secure; HttpOnly; SameSite=None` };

    if (host === PANOPTO_HOST) {
      if (url.pathname === '/Panopto/Pages/Viewer.aspx') return html(playerPage({ embed: false }), panoptoCookie);
      if (url.pathname === '/Panopto/Pages/Embed.aspx') return html(playerPage({ embed: true }), panoptoCookie);
      if (url.pathname === '/Panopto/Pages/Sessions/List.aspx') {
        const rows = state.list.map((id) => state.sessions.get(id));
        return html(listPage({ embedded: url.searchParams.get('embedded') === '1', rows }), panoptoCookie);
      }
      if (url.pathname === '/Panopto/Pages/Viewer/DeliveryInfo.aspx' && req.method === 'POST') {
        let body = '';
        req.on('data', (c) => (body += c));
        req.on('end', () => {
          const form = new URLSearchParams(body);
          entry.deliveryId = form.get('deliveryId') || '';
          const s = state.sessions.get(entry.deliveryId);
          if (!s) return send(res, 404, 'application/json', '{"ErrorCode":1}');
          const base = `${site.mediaOrigin}/sessions/${s.ids.media}`;
          const streams = [
            { Tag: 'AUDIO', StreamUrl: `${base}/${s.ids.media}-${s.ids.stream}.hls/master.m3u8` },
            { Tag: 'OBJECT', StreamUrl: `${base}/${s.ids.screen}.object.hls/master.m3u8` },
          ];
          if (s.twoScreens) streams.push({ Tag: 'OBJECT', StreamUrl: `${base}/${s.ids.screen2}.object.hls/master.m3u8` });
          send(
            res,
            200,
            'application/json; charset=utf-8',
            JSON.stringify({
              Delivery: {
                SessionName: s.title,
                Duration: 8,
                IsPrimaryAudioOnly: true,
                Streams: streams,
                PodcastStreams: [{ StreamUrl: `${base}/${s.ids.media}-${s.ids.stream}.mp4`, Name: VIEWER_NAME }],
              },
              DownloadUrl: s.downloads ? `https://${PANOPTO_HOST}/Panopto/Podcast/Download/${s.deliveryId}.mp4` : null,
              UserEmail: VIEWER_EMAIL,
              UserName: VIEWER_NAME,
              UserKey: `${VIEWER_EMAIL}-key`,
            }),
          );
        });
        return undefined;
      }
    }
    if (host === CANVAS_HOST) {
      if (url.pathname === '/canvas/list.html') return html(lmsPage(`https://${PANOPTO_HOST}:${port}/Panopto/Pages/Sessions/List.aspx?embedded=1`));
      if (url.pathname === '/canvas/embed.html') return html(lmsPage(`https://${PANOPTO_HOST}:${port}/Panopto/Pages/Embed.aspx?id=${encodeURIComponent(url.searchParams.get('id') || '')}`, { small: url.searchParams.get('small') === '1' }));
    }
    send(res, 404, 'text/plain', 'not found');
  });

  const listen = (srv) => new Promise((resolve) => srv.listen(0, '127.0.0.1', () => resolve(srv.address().port)));
  const mediaPort = await listen(mediaServer);
  const pagesPort = await listen(pageServer);
  Object.assign(site, {
    mediaPort,
    pagesPort,
    mediaOrigin: `http://127.0.0.1:${mediaPort}`,
    panoptoOrigin: `https://${PANOPTO_HOST}:${pagesPort}`,
    canvasOrigin: `https://${CANVAS_HOST}:${pagesPort}`,
    genericUrl: `http://127.0.0.1:${mediaPort}/g/lecture.html`,
    /** A new platform session: { deliveryId, title, downloads, twoScreens, ids }. */
    addSession({ title, downloads = true, twoScreens = false, list = false }) {
      const n = nextSession++;
      const ids = sessionIds(n);
      const s = { deliveryId: ids.deliveryId, title, downloads, twoScreens, ids };
      state.sessions.set(s.deliveryId, s);
      state.byMedia.set(ids.media, s);
      if (list) state.list.push(s.deliveryId);
      return s;
    },
    showInList(s) {
      if (!state.list.includes(s.deliveryId)) state.list.push(s.deliveryId);
    },
    viewerUrl: (s) => `https://${PANOPTO_HOST}:${pagesPort}/Panopto/Pages/Viewer.aspx?id=${s.deliveryId}`,
    listUrl: () => `https://${PANOPTO_HOST}:${pagesPort}/Panopto/Pages/Sessions/List.aspx`,
    /** Holds aria2c's GETs of the generic MP4 after 16 KiB until release(). */
    stall() {
      state.stallGeneric = true;
      stallGate = new Promise((r) => (releaseStall = r));
    },
    release() {
      state.stallGeneric = false;
      releaseStall();
    },
    /** Every value a test must never find on disk or in a message. */
    secrets() {
      const out = [creds.cookie, creds.signature, creds.panoptoCookie, VIEWER_EMAIL, VIEWER_NAME];
      // Panopto's media paths are credentials on their own (lead ruling 3):
      // the media session and stream ids appear nowhere else.
      for (const s of state.sessions.values()) out.push(s.ids.media, s.ids.stream, s.ids.screen, s.ids.screen2);
      return out;
    },
    close: () => Promise.all([mediaServer, pageServer].map((srv) => new Promise((r) => { srv.closeAllConnections?.(); srv.close(r); }))),
  });
  return site;
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A local test site on 127.0.0.1 for the end-to-end tests: a page with
// plain media files (and a same-origin iframe), a page whose "player"
// fetches an HLS master with an audio group, a page using EME (Clear Key)
// plus a DRM-signalled DASH manifest, an empty page, and Panopto-like
// pages: the viewer (its player asks DeliveryInfo.aspx — a JSON answer that
// also carries the viewer's e-mail and name, as the real one does — then
// loads the HLS masters it names), the embed player, and a folder list in
// the main site's layout and in the layout Panopto uses inside an LMS
// iframe; plus Canvas-like pages embedding the list and the embed player.
// The test browser maps the test host names (*.lf-e2e.test,
// *.cloud.panopto.eu) to this server; the server answers whatever the Host
// header says. Media bodies are filler bytes: detection never needs to
// decode them. Every name, id and title here is made up.

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const site = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'testdata', 'site');

const FILES = {
  '/media/lecture.mp4': { type: 'video/mp4', size: 123456 },
  '/media/talk.m4a': { type: 'audio/mp4', size: 54321 },
  '/media/frame-audio.mp3': { type: 'audio/mpeg', size: 4321 },
  '/media/protected.mp4': { type: 'video/mp4', size: 99999 },
  '/hls/v720/seg0.ts': { type: 'video/mp2t', size: 1880 },
};

const TYPES = { '.html': 'text/html; charset=utf-8', '.m3u8': 'application/vnd.apple.mpegurl', '.mpd': 'application/dash+xml', '.js': 'text/javascript' };

const g = (n) => `a0000000-0000-4000-8000-0000000000${n}`;

// The Panopto-like viewer: made-up GUIDs (the page id is the delivery id,
// the media paths use the session id, as on Panopto).
export const PANOPTO = Object.freeze({
  host: 'tenant.cloud.panopto.eu',
  deliveryId: '1d2e3f40-0000-4000-8000-0000000000e1',
  session: '5e6f7a8b-0000-4000-8000-0000000000e2',
  primary: '5e6f7a8b-0000-4000-8000-0000000000e2-9c8d7e6f-0000-4000-8000-0000000000e3',
  screen: 'abcdef01-0000-4000-8000-0000000000e4',
  screen2: 'abcdef02-0000-4000-8000-0000000000e5',
  title: 'CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026',
  // what DeliveryInfo says about the viewer: must never reach the extension
  userEmail: 'lf-e2e-student@example.edu',
  userName: 'LfE2eStudentName',
});

// The folder list: six sessions (row id = the id Viewer.aspx takes).
export const LIST = Object.freeze([
  { id: g('a1'), title: 'CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026', downloads: true }, // new
  { id: g('a2'), title: 'CSC1003/L01/01 Wed Oct 14 09:35:00 BST 2026', downloads: true }, // imported (session)
  { id: g('a3'), title: 'CSC1003/L01/01 Wed Oct 07 09:35:00 BST 2026', downloads: true }, // being fetched
  { id: g('a4'), title: 'CSC1003/L01/01 Wed Sep 30 09:35:00 BST 2026', downloads: true }, // imported (title)
  { id: g('a5'), title: 'CSC1003/L02/01 Tue Sep 29 12:35:00 BST 2026', downloads: true, fail: 500 }, // DeliveryInfo fails
  { id: g('a6'), title: 'CSC1003/L02/01 Mon Sep 28 12:35:00 BST 2026', downloads: false }, // restricted
]);

function playerPage({ embed }) {
  const titleHtml = embed ? '<h1 id="title"><span class="title-text"></span></h1>' : '<h1 id="deliveryTitle"></h1>';
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>Panopto Viewer</title>
<style>body{font-family:sans-serif;margin:0} #viewerHeader{display:flex;align-items:center;justify-content:space-between;gap:12px;height:56px;padding:0 16px;border-bottom:1px solid #ddd} #header-right-react{display:flex;gap:8px} h1#title{font-size:16px;margin:8px 12px;display:flex;align-items:center;gap:8px}</style>
</head>
<body>
${embed ? '' : '<div id="viewerHeader"><div id="header-left"><span class="brand">ReCap</span></div><div id="header-right-react"><button class="share">Share</button><button class="help">?</button></div></div>'}
${titleHtml}
<div id="player" style="height:200px;background:#111"></div>
<script>
(async () => {
  const q = new URLSearchParams(location.search);
  const body = 'deliveryId=' + encodeURIComponent(q.get('id')) + '&isEmbed=${embed ? 'true' : 'false'}&responseType=json';
  const url = '/Panopto/Pages/Viewer/DeliveryInfo.aspx';
  let info;
  if (q.get('via') === 'fetch') {
    const r = await fetch(url, { method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded' }, body });
    info = await r.json();
  } else {
    info = await new Promise((resolve, reject) => {
      const x = new XMLHttpRequest();
      x.open('POST', url);
      x.setRequestHeader('content-type', 'application/x-www-form-urlencoded');
      x.onload = () => resolve(JSON.parse(x.responseText));
      x.onerror = reject;
      x.send(body);
    });
  }
  document.querySelector('${embed ? 'h1#title .title-text' : '#deliveryTitle'}').textContent = info.Delivery.SessionName;
  for (const s of info.Delivery.Streams) {
    const master = await (await fetch(s.StreamUrl)).text();
    const child = master.trim().split('\\n').pop();
    await (await fetch(new URL(child, s.StreamUrl))).text();
  }
  // The page itself still gets the whole answer, untouched.
  window.__pageSawUser = info.UserName;
  document.body.dataset.ready = '1';
})();
// What Panopto's React header does when it re-renders.
window.__rerenderHeader = () => {
  const old = document.getElementById('header-right-react');
  const fresh = document.createElement('div');
  fresh.id = 'header-right-react';
  fresh.innerHTML = '<button class="share">Share</button>';
  old.replaceWith(fresh);
};
</script>
</body></html>
`;
}

function listPage({ embedded }) {
  const rows = LIST.map(
    (s, i) => `<tr class="list-view-row" id="${s.id}"><td class="thumb"><div class="thumbnail">${55 - i}:00</div></td><td><div class="item-title"><a class="detail-title" href="/Panopto/Pages/Viewer.aspx?id=${s.id}" target="_blank">${s.title}</a></div><div class="item-meta">Lecturer · ${i + 1} views</div></td></tr>`,
  ).join('\n');
  // The main site's dark folder bar has a background layer above its
  // content; inside an LMS Panopto uses a light toolbar instead.
  const header = embedded
    ? `<div class="css-embed-toolbar"><select class="folder-select"><option>2026/27 - Cryptography</option></select><div class="css-7abc12"><button class="subscribe-button">+ Subscribe</button></div><a class="open-new" href="#" target="_blank">⧉</a></div>`
    : `<div id="headerWrapper"><div class="css-3evg5k"><div class="bg-layer"></div><div class="folder-name">2026/27 - Cryptography</div><div class="css-hdr99"><button class="subscribe-button headerButton">+ Subscribe</button></div></div></div>`;
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>Sessions</title>
<style>
body{font-family:sans-serif;margin:0;background:#fafafa}
.css-3evg5k{position:relative;display:flex;align-items:center;justify-content:space-between;height:56px;padding:0 16px;background:#00264d;color:#fff}
.css-3evg5k .bg-layer{position:absolute;inset:0;z-index:2;background:transparent}
.css-hdr99{position:relative;z-index:3;display:flex;align-items:center;gap:8px}
.css-embed-toolbar{display:flex;align-items:center;gap:12px;padding:8px 16px;background:#fff;border-bottom:1px solid #ddd}
.css-7abc12{margin-left:auto;display:flex;align-items:center;gap:8px}
table{border-collapse:collapse;width:100%} td{padding:12px 16px;vertical-align:top}
.thumbnail{width:160px;height:90px;background:#333;color:#fff}
.item-title{font-size:18px;font-weight:600}
.item-meta{color:#777;font-size:13px}
</style></head>
<body>
${header}
<table><tbody>
${rows}
</tbody></table>
<script>
// A row click opens the session (the page's own handler).
document.querySelectorAll('tr.list-view-row').forEach((tr) => tr.addEventListener('click', () => { window.__rowClicks = (window.__rowClicks || 0) + 1; }));
document.body.dataset.ready = '1';
</script>
</body></html>
`;
}

// An LMS-like page embedding a Panopto page. Served on any host name: on
// canvas.lf-e2e.test (a site the e2e build may access), or on
// *.lf-e2e-x.test (one it may not: a page trying to clickjack the buttons).
// hidden: the iframe is transparent (opacity 0), as a clickjacking page
// would make it.
// small: a small embedded player (about the 380 × 210 of a user's Canvas
// page), too small for the in-page card.
// Do-not-handle sites (blocklist.test.mjs): a course page whose video is
// on a general "CDN" host, and an LMS page embedding one frame (with a
// lecture of its own unless own is false).
function coursePage(media) {
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>Course (test)</title></head>
<body><h1>Course lecture (test page)</h1>
<video id="v" src="${media}" preload="auto" muted></video>
</body></html>
`;
}

function lmsPage(src, own) {
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>LMS (test)</title>
<style>body{margin:0;font-family:sans-serif} iframe{border:0;width:100%;height:640px}</style></head>
<body><div class="nav">Course (test LMS page)</div>
${own ? '<audio id="a" src="/media/talk.m4a" preload="auto"></audio>' : ''}
<iframe name="tool_content" title="Tool" src="${src}"></iframe>
</body></html>
`;
}

function canvasPage(src, { tall, hidden = false, small = false }) {
  const size = small ? 'width:380px;height:214px' : `width:100%;height:${tall ? '3000px' : '640px'}`;
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>Canvas (test)</title>
<style>body{margin:0;font-family:sans-serif} .nav{height:40px;background:#394b58;color:#fff;padding:8px} iframe{border:0;${size}${hidden ? ';opacity:0' : ''}}</style></head>
<body><div class="nav">Course (test LMS page)</div>
<iframe name="tool_content" title="ReCap" src="${src}"></iframe>
</body></html>
`;
}

const AUDIO_MASTER = '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=135000,AVERAGE-BANDWIDTH=128000,CODECS="mp4a.40.2"\nindex.m3u8\n';
const SCREEN_MASTER = '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=1920x1080,CODECS="avc1.640028"\nindex.m3u8\n';
const INDEX = '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:600\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:600.0,\nseg0.ts\n#EXTINF:600.0,\nseg1.ts\n#EXTINF:600.0,\nseg2.ts\n#EXT-X-ENDLIST\n';

function serveFile(req, res, spec) {
  const total = spec.size;
  const range = /^bytes=(\d+)-(\d*)$/.exec(req.headers.range || '');
  const headers = { 'content-type': spec.type, 'accept-ranges': 'bytes' };
  if (range) {
    const start = Number(range[1]);
    const end = range[2] ? Math.min(Number(range[2]), total - 1) : total - 1;
    res.writeHead(206, { ...headers, 'content-range': `bytes ${start}-${end}/${total}`, 'content-length': end - start + 1 });
    res.end(req.method === 'HEAD' ? undefined : Buffer.alloc(end - start + 1));
    return;
  }
  res.writeHead(200, { ...headers, 'content-length': total });
  res.end(req.method === 'HEAD' ? undefined : Buffer.alloc(total));
}

function send(res, status, type, body, extra = {}) {
  const buf = Buffer.from(body);
  res.writeHead(status, { 'content-type': type, 'content-length': buf.length, 'cache-control': 'no-store', ...extra });
  res.end(buf);
}

/** DeliveryInfo's answer for a delivery id, or null (unknown). */
function deliveryInfo(id, origin, panopto) {
  let sess;
  let title;
  let downloads;
  if (id === PANOPTO.deliveryId) {
    sess = PANOPTO.session;
    title = PANOPTO.title;
    downloads = !!panopto.downloadUrl;
  } else {
    const s = LIST.find((x) => x.id === id);
    if (!s) return null;
    if (s.fail) return { fail: s.fail };
    sess = s.id;
    title = s.title;
    downloads = s.downloads;
  }
  const primary = id === PANOPTO.deliveryId ? PANOPTO.primary : `${sess}-9c8d7e6f-0000-4000-8000-0000000000e3`;
  const base = `${origin}/sessions/${sess}`;
  const streams = [
    { Tag: 'AUDIO', StreamUrl: `${base}/${primary}.hls/master.m3u8` },
    { Tag: 'OBJECT', StreamUrl: `${base}/${PANOPTO.screen}.object.hls/master.m3u8` },
  ];
  if (panopto.twoScreens) streams.push({ Tag: 'OBJECT', StreamUrl: `${base}/${PANOPTO.screen2}.object.hls/master.m3u8` });
  return {
    Delivery: {
      SessionName: title,
      Duration: 3300,
      IsPrimaryAudioOnly: true,
      Streams: streams,
      PodcastStreams: [{ StreamUrl: `${base}/${primary}.mp4`, Name: PANOPTO.userName }],
    },
    DownloadUrl: downloads ? `${origin}/Panopto/Podcast/Download/${sess}.mp4` : null,
    UserEmail: PANOPTO.userEmail,
    UserName: PANOPTO.userName,
    UserKey: `${PANOPTO.userEmail}-key`,
  };
}

export function startServer() {
  const requests = [];
  // Test switches: whether the viewer's DeliveryInfo allows downloads, and
  // whether it lists a second screen capture.
  const panopto = { downloadUrl: null, twoScreens: false };
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, 'http://127.0.0.1');
    const host = String(req.headers.host || '').replace(/:\d+$/, '');
    const entry = { method: req.method, host, path: url.pathname, cookie: req.headers.cookie || '' };
    requests.push(entry);
    const origin = `http://${req.headers.host}`;
    const port = String(req.headers.host || '').split(':')[1] || '';

    // Panopto-like pages
    if (url.pathname === '/Panopto/Pages/Viewer.aspx') return send(res, 200, 'text/html; charset=utf-8', playerPage({ embed: false }));
    if (url.pathname === '/Panopto/Pages/Embed.aspx') return send(res, 200, 'text/html; charset=utf-8', playerPage({ embed: true }));
    if (url.pathname === '/Panopto/Pages/Sessions/List.aspx') return send(res, 200, 'text/html; charset=utf-8', listPage({ embedded: url.searchParams.get('embedded') === '1' }));
    if (url.pathname === '/Panopto/Pages/Viewer/DeliveryInfo.aspx' && req.method === 'POST') {
      let body = '';
      req.on('data', (c) => (body += c));
      req.on('end', () => {
        const form = new URLSearchParams(body);
        entry.deliveryId = form.get('deliveryId') || '';
        entry.form = Object.fromEntries(form);
        const info = deliveryInfo(entry.deliveryId, origin, panopto);
        if (!info) return send(res, 404, 'application/json', '{"error":"no such delivery"}');
        if (info.fail) return send(res, info.fail, 'application/json', '{"error":"server error"}');
        send(res, 200, 'application/json; charset=utf-8', JSON.stringify(info));
      });
      return;
    }
    if (url.pathname.startsWith('/sessions/')) {
      if (url.pathname.endsWith('/master.m3u8')) return send(res, 200, 'application/vnd.apple.mpegurl', url.pathname.includes('.object.hls') ? SCREEN_MASTER : AUDIO_MASTER);
      if (url.pathname.endsWith('/index.m3u8')) return send(res, 200, 'application/vnd.apple.mpegurl', INDEX);
      return send(res, 404, 'text/plain', 'not here');
    }
    // Canvas-like pages embedding Panopto
    const hidden = url.searchParams.get('hidden') === '1';
    if (url.pathname === '/canvas/list.html') return send(res, 200, 'text/html; charset=utf-8', canvasPage(`http://${PANOPTO.host}:${port}/Panopto/Pages/Sessions/List.aspx?embedded=1`, { tall: url.searchParams.get('tall') === '1', hidden }));
    if (url.pathname === '/canvas/embed.html') return send(res, 200, 'text/html; charset=utf-8', canvasPage(`http://${PANOPTO.host}:${port}/Panopto/Pages/Embed.aspx?id=${PANOPTO.deliveryId}`, { tall: false, hidden, small: url.searchParams.get('small') === '1' }));

    // Do-not-handle sites (www.youtube-nocookie.com, www.udemy.com,
    // www.edx.org, www.linkedin.com: mapped here by the test browser, so
    // nothing reaches the real ones) and the LMS page that embeds one.
    if (url.pathname === '/blocked/course.html' || url.pathname.startsWith('/learning/') || url.pathname.startsWith('/feed/')) {
      return send(res, 200, 'text/html; charset=utf-8', coursePage(`http://cdn.lf-e2e.test:${port}/media/lecture.mp4`));
    }
    if (url.pathname === '/blocked/lms.html') {
      const src = url.searchParams.get('frame') || '';
      if (!/^http:\/\/[a-z0-9.-]+:\d+\/[A-Za-z0-9/._?=&%-]*$/.test(src)) return send(res, 400, 'text/plain', 'bad frame');
      return send(res, 200, 'text/html; charset=utf-8', lmsPage(src.replace(/&/g, '&amp;'), url.searchParams.get('own') !== '0'));
    }

    if (FILES[url.pathname]) return serveFile(req, res, FILES[url.pathname]);
    const file = path.join(site, path.normalize(url.pathname).replace(/^([/\\])+/, ''));
    if (!file.startsWith(site) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) {
      res.writeHead(404, { 'content-type': 'text/plain' });
      res.end('not found');
      return;
    }
    const type = TYPES[path.extname(file)] || 'application/octet-stream';
    const body = fs.readFileSync(file);
    // The "CDN" answers cross-origin requests from the page's origin.
    const cors = url.pathname.startsWith('/cdn/') && req.headers.origin ? { 'access-control-allow-origin': req.headers.origin, vary: 'Origin' } : {};
    res.writeHead(200, { 'content-type': type, 'content-length': body.length, 'cache-control': 'no-store', ...cors });
    res.end(req.method === 'HEAD' ? undefined : body);
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({ origin: `http://127.0.0.1:${port}`, port, requests, panopto, close: () => new Promise((r) => server.close(r)) });
    });
  });
}

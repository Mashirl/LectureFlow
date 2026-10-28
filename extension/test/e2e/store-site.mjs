// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The demo recording platform of the store screenshots
// (test/e2e/store-shots.mjs): a neutral player page, its embed player, a
// folder list, and a learning platform's pages embedding the player, in
// English or Simplified Chinese. It is our own design and does not imitate
// any real platform; the pages carry the elements the demo profile
// (store-shots.mjs: the Panopto profile's rules on the demo host) reads,
// the JSON answer the player asks for, and HLS playlists. The browser uses
// this server as its proxy, so the pages have plain host names without a
// port (lectures.example.edu, learn.example.edu); any other host gets 404
// and CONNECT is refused, so nothing reaches the internet. Every name, id,
// title and number here is made up.

import http from 'node:http';

export const HOST = 'lectures.example.edu';
export const LMS = 'learn.example.edu';

const g = (n) => `d0e1f2a3-0000-4000-8000-0000000000${n}`;

// The course's lectures, newest first (the folder list); the first one is the
// recording the player page shows.
const LECTURES = [
  { id: g('b8'), week: 8, en: 'Elliptic curves', zh: '椭圆曲线', date: '2026-10-21T09:35', min: 55 },
  { id: g('b7'), week: 7, en: 'Key exchange', zh: '密钥交换', date: '2026-10-14T09:35', min: 52 },
  { id: g('b6'), week: 6, en: 'Digital signatures', zh: '数字签名', date: '2026-10-07T09:35', min: 54 },
  { id: g('b5'), week: 5, en: 'Public-key encryption', zh: '公钥加密', date: '2026-09-30T09:35', min: 51 },
  { id: g('b4'), week: 4, en: 'Hash functions', zh: '哈希函数', date: '2026-09-23T09:35', min: 49 },
  { id: g('b3'), week: 3, en: 'Block ciphers', zh: '分组密码', date: '2026-09-16T09:35', min: 53 },
];
export const SESSIONS = Object.freeze(LECTURES.map((l) => l.id));

const PRIMARY = '5f6e7d8c-0000-4000-8000-0000000000c1';
const SCREEN = 'abcdef11-0000-4000-8000-0000000000c2';
const SCREEN2 = 'abcdef12-0000-4000-8000-0000000000c3';

const T = {
  en: {
    lang: 'en',
    brand: 'Lecture Capture',
    course: 'CSC1003 Cryptography',
    title: (l) => `CSC1003 Week ${l.week}: ${l.en}`,
    listTitle: (l) => `Week ${l.week}: ${l.en}`,
    when: (l) => {
      const d = new Date(`${l.date}:00`);
      const day = d.toLocaleDateString('en-GB', { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' });
      return `${day} · ${l.date.slice(11)}`;
    },
    minutes: (n) => `${n} min`,
    share: 'Share',
    help: 'Help',
    contents: 'Contents',
    notes: 'Notes',
    chapters: [
      ['00:00', 'Recap: groups and fields'],
      ['06:12', 'Curves over the reals'],
      ['14:40', 'Adding points'],
      ['23:05', 'Curves over a finite field'],
      ['31:50', 'Scalar multiplication'],
      ['40:18', 'Elliptic-curve Diffie–Hellman'],
      ['49:30', 'Summary and reading'],
    ],
    screen2: 'Screen 2',
    folder: 'Recordings',
    subscribe: 'Subscribe',
    sort: 'Newest first',
    views: (n) => `${n} views`,
    slide: {
      title: 'Elliptic curves over a finite field',
      eq: 'E : y² ≡ x³ + 2x + 3  (mod 97)',
      bullets: ['Points (x, y) with x, y in {0, …, 96}', 'plus the point at infinity O', 'Same addition rule, arithmetic mod p'],
      count: (n) => `#E = ${n} points`,
      foot: 'CSC1003 · Week 8',
    },
    board: ['P = (3, 6)', 'λ = (3x² + a) / 2y', '2P = (80, 10)'],
    lms: {
      name: 'Learn',
      nav: ['Home', 'Announcements', 'Modules', 'Lecture recordings', 'Assignments', 'Grades'],
      crumbs: ['CSC1003 Cryptography', 'Lecture recordings'],
      heading: 'Week 8: Elliptic curves',
      intro: 'Watch the recording before Thursday’s tutorial. Problem sheet 8 is in Modules.',
      moduleTitle: 'Week 8 · Elliptic curves',
      items: ['Lecture recording', 'Slides (PDF)', 'Problem sheet 8', 'Tutorial: point addition by hand'],
      modulesNav: 'Modules',
    },
  },
  'zh-CN': {
    lang: 'zh-CN',
    brand: '课程录播',
    course: 'CSC1003 密码学',
    title: (l) => `CSC1003 第 ${l.week} 周：${l.zh}`,
    listTitle: (l) => `第 ${l.week} 周：${l.zh}`,
    when: (l) => {
      const d = new Date(`${l.date}:00`);
      const wd = '日一二三四五六'[d.getDay()];
      return `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日 周${wd} ${l.date.slice(11)}`;
    },
    minutes: (n) => `${n} 分钟`,
    share: '分享',
    help: '帮助',
    contents: '目录',
    notes: '笔记',
    chapters: [
      ['00:00', '回顾：群与域'],
      ['06:12', '实数上的椭圆曲线'],
      ['14:40', '点的加法'],
      ['23:05', '有限域上的椭圆曲线'],
      ['31:50', '标量乘法'],
      ['40:18', '椭圆曲线 Diffie–Hellman'],
      ['49:30', '小结与阅读'],
    ],
    screen2: '画面 2',
    folder: '录播',
    subscribe: '订阅',
    sort: '最新在前',
    views: (n) => `${n} 次观看`,
    slide: {
      title: '有限域上的椭圆曲线',
      eq: 'E : y² ≡ x³ + 2x + 3（mod 97）',
      bullets: ['点 (x, y)，x、y ∈ {0, …, 96}', '再加上无穷远点 O', '加法规则不变，运算都取模 p'],
      count: (n) => `共 #E = ${n} 个点`,
      foot: 'CSC1003 · 第 8 周',
    },
    board: ['P = (3, 6)', 'λ = (3x² + a) / 2y', '2P = (80, 10)'],
    lms: {
      name: '学习平台',
      nav: ['首页', '公告', '单元', '课程录播', '作业', '成绩'],
      crumbs: ['CSC1003 密码学', '课程录播'],
      heading: '第 8 周：椭圆曲线',
      intro: '请在周四辅导课前看完录播。第 8 周习题在“单元”里。',
      moduleTitle: '第 8 周 · 椭圆曲线',
      items: ['课程录播', '课件（PDF）', '第 8 周习题', '辅导：手算点加法'],
      modulesNav: '单元',
    },
  },
};

const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

// The points of y² = x³ + 2x + 3 over F_97, for the slide's plot.
const P = 97;
const POINTS = [];
for (let x = 0; x < P; x++) for (let y = 0; y < P; y++) if ((y * y) % P === (x * x * x + 2 * x + 3) % P) POINTS.push([x, y]);

const FONT = "'Segoe UI', system-ui, -apple-system, 'Microsoft YaHei UI', 'Microsoft YaHei', 'PingFang SC', sans-serif";

/** A slide as one SVG (viewBox 1600 × 900), so it scales with any player. */
function slideSvg(t, { week = 8, title = null } = {}) {
  const s = t.slide;
  if (title) {
    // a thumbnail: the week's title only
    return `<svg viewBox="0 0 1600 900" preserveAspectRatio="xMidYMid slice" aria-hidden="true"><rect width="1600" height="900" fill="#f7f8fb"/><rect width="1600" height="16" fill="#1f6feb"/><text x="110" y="380" font-family="${FONT}" font-size="78" fill="#6b7383">CSC1003 · ${week}</text><text x="110" y="520" font-family="${FONT}" font-size="104" font-weight="700" fill="#161a21">${esc(title)}</text></svg>`;
  }
  const px = (x) => 930 + (x / (P - 1)) * 560;
  const py = (y) => 790 - (y / (P - 1)) * 560;
  const dots = POINTS.map(([x, y]) => `<circle cx="${px(x).toFixed(1)}" cy="${py(y).toFixed(1)}" r="6.5" fill="#1f6feb"/>`).join('');
  const bullets = s.bullets.map((b, i) => `<text x="130" y="${520 + i * 74}" font-family="${FONT}" font-size="44" fill="#353b47">• ${esc(b)}</text>`).join('');
  return `<svg viewBox="0 0 1600 900" preserveAspectRatio="xMidYMid meet" aria-hidden="true">
<rect width="1600" height="900" fill="#ffffff"/>
<rect width="1600" height="14" fill="#1f6feb"/>
<text x="110" y="160" font-family="${FONT}" font-size="76" font-weight="700" fill="#161a21">${esc(s.title)}</text>
<text x="130" y="330" font-family="'Cambria Math', Cambria, 'Times New Roman', serif" font-size="58" fill="#161a21">${esc(s.eq)}</text>
${bullets}
<text x="130" y="${520 + s.bullets.length * 74 + 30}" font-family="${FONT}" font-size="44" font-weight="600" fill="#175fd3">${esc(s.count(POINTS.length + 1))}</text>
<rect x="915" y="215" width="590" height="590" fill="none" stroke="#d5d9e0" stroke-width="3"/>
<line x1="915" y1="805" x2="1505" y2="805" stroke="#7d8492" stroke-width="3"/>
<line x1="915" y1="215" x2="915" y2="805" stroke="#7d8492" stroke-width="3"/>
${dots}
<text x="1505" y="850" text-anchor="end" font-family="${FONT}" font-size="30" fill="#7d8492">x</text>
<text x="880" y="230" font-family="${FONT}" font-size="30" fill="#7d8492">y</text>
<text x="110" y="868" font-family="${FONT}" font-size="30" fill="#7d8492">${esc(s.foot)}</text>
<text x="1490" y="868" text-anchor="end" font-family="${FONT}" font-size="30" fill="#7d8492">14 / 32</text>
</svg>`;
}

/** The second picture: a board, written by hand. */
function boardSvg(t) {
  const lines = t.board.map((l, i) => `<text x="120" y="${300 + i * 190}" font-family="'Ink Free', 'Segoe Print', 'Comic Sans MS', cursive" font-size="110" fill="#f4f1e6">${esc(l)}</text>`).join('');
  return `<svg viewBox="0 0 1600 900" preserveAspectRatio="xMidYMid slice" aria-hidden="true"><rect width="1600" height="900" fill="#23443a"/><rect x="0" y="860" width="1600" height="40" fill="#6b5a45"/>${lines}<path d="M1100 700 Q 1250 380 1480 300" stroke="#f4f1e6" stroke-width="7" fill="none" stroke-linecap="round"/><circle cx="1250" cy="470" r="14" fill="#f4f1e6"/></svg>`;
}

const ICON = {
  play: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 4v16l13-8z" fill="currentColor"/></svg>',
  cc: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="5" width="18" height="14" rx="2" fill="none" stroke="currentColor" stroke-width="2"/><path d="M10 10.5a2 2 0 1 0 0 3M16 10.5a2 2 0 1 0 0 3" fill="none" stroke="currentColor" stroke-width="2"/></svg>',
  gear: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3.2" fill="none" stroke="currentColor" stroke-width="2"/><path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>',
  full: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>',
  vol: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 9h4l5-4v14l-5-4H4z" fill="currentColor"/><path d="M16 9a4 4 0 0 1 0 6" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>',
  mark: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="2" y="2" width="20" height="20" rx="6" fill="currentColor"/><path d="M10 8v8l6-4z" fill="#fff"/></svg>',
};

function controls({ compact = false } = {}) {
  return `<div class="controls${compact ? ' compact' : ''}">
<button class="cbtn" aria-label="Play">${ICON.play}</button>
<span class="time">23:41 / 55:00</span>
<div class="track"><div class="fill" style="width:43%"></div><div class="knob" style="left:43%"></div></div>
${compact ? '' : `<button class="cbtn">${ICON.vol}</button><span class="rate">1×</span><button class="cbtn">${ICON.cc}</button><button class="cbtn">${ICON.gear}</button>`}
<button class="cbtn">${ICON.full}</button>
</div>`;
}

const BASE_CSS = `
*{box-sizing:border-box}
html,body{margin:0;height:100%}
body{font-family:${FONT};color:#1d2330;background:#f3f5f8;-webkit-font-smoothing:antialiased}
button{font:inherit}
.controls{display:flex;align-items:center;gap:14px;height:52px;padding:0 16px;background:#111722;color:#e8ecf3}
.controls.compact{height:40px;gap:10px;padding:0 10px}
.cbtn{display:inline-flex;align-items:center;justify-content:center;width:30px;height:30px;padding:0;border:0;background:none;color:inherit}
.cbtn svg{width:20px;height:20px}
.controls.compact .cbtn svg{width:16px;height:16px}
.time{font-size:13px;font-variant-numeric:tabular-nums;color:#c5ccd8;white-space:nowrap}
.controls.compact .time{font-size:11px}
.track{position:relative;flex:1;height:5px;border-radius:3px;background:#3a4354}
.track .fill{position:absolute;inset:0 auto 0 0;border-radius:3px;background:#4c8dff}
.track .knob{position:absolute;top:50%;width:13px;height:13px;margin:-6.5px 0 0 -6.5px;border-radius:50%;background:#fff}
.rate{font-size:13px;color:#c5ccd8}
.stage{position:relative;background:#0b0f16;display:flex;gap:6px}
.stage svg{display:block;width:100%;height:100%}
.view{position:relative;flex:2;aspect-ratio:16/9}
.view.second{flex:1;align-self:center}
.view .tag{position:absolute;left:10px;top:10px;padding:2px 8px;border-radius:6px;background:rgb(0 0 0 / 0.55);color:#fff;font-size:12px}
`;

/** The player script of the demo platform: it asks for the delivery JSON, then loads the playlists it names. */
const PLAYER_SCRIPT = (embed, titleSel) => `
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
  ${titleSel ? `document.querySelector(${JSON.stringify(titleSel)}).textContent = info.Delivery.SessionName;` : ''}
  for (const s of info.Delivery.Streams) {
    const master = await (await fetch(s.StreamUrl)).text();
    const child = master.trim().split('\\n').pop();
    await (await fetch(new URL(child, s.StreamUrl))).text();
  }
  document.body.dataset.ready = '1';
})();`;

function viewerPage(t, lec, state) {
  const chapters = t.chapters
    .map(([at, name], i) => `<li class="${i === 3 ? 'now' : ''}"><span class="at">${at}</span><span>${esc(name)}</span></li>`)
    .join('');
  const second = state.twoScreens ? `<div class="view second">${boardSvg(t)}<span class="tag">${esc(t.screen2)}</span></div>` : '';
  return `<!doctype html>
<html lang="${t.lang}"><head><meta charset="utf-8"><title>${esc(t.title(lec))}</title>
<style>${BASE_CSS}
#viewerHeader{display:flex;align-items:center;gap:12px;height:56px;padding:0 20px;background:#fff;border-bottom:1px solid #e1e5ec}
#header-left{display:flex;align-items:center;gap:10px;margin-right:auto;font-weight:600;font-size:16px;color:#1d2330}
#header-left svg{width:26px;height:26px;color:#0f766e}
#header-left .sep{color:#aab1bd;font-weight:400}
#header-left .crs{font-weight:500;color:#4b5261}
#header-right-react{display:flex;gap:8px}
.hbtn{height:30px;padding:0 12px;border:1px solid #d5d9e0;border-radius:8px;background:#fff;color:#2b3240;font-size:13px}
.wrap{display:flex;gap:20px;padding:18px 20px 0}
.main{flex:0 1 860px;min-width:0}
h1#deliveryTitle{margin:0 0 4px;font-size:22px;font-weight:650;color:#161a21}
.meta{margin:0 0 14px;font-size:13px;color:#69707d}
.player{border-radius:10px;overflow:hidden;background:#0b0f16}
.side{width:300px;flex:none;background:#fff;border:1px solid #e1e5ec;border-radius:10px;overflow:hidden;align-self:flex-start}
.tabs{display:flex;border-bottom:1px solid #e1e5ec}
.tabs span{flex:1;padding:11px 0;text-align:center;font-size:13px;font-weight:600;color:#69707d}
.tabs span.on{color:#0f766e;box-shadow:inset 0 -2px 0 #0f766e}
.side ul{list-style:none;margin:0;padding:6px 0}
.side li{display:flex;gap:12px;padding:10px 16px;font-size:13.5px;color:#2b3240}
.side li .at{width:42px;flex:none;color:#8a919d;font-variant-numeric:tabular-nums}
.side li.now{background:#e9f6f4;color:#0b5d56;font-weight:600}
</style></head>
<body>
<div id="viewerHeader"><div id="header-left">${ICON.mark}<span>${esc(t.brand)}</span><span class="sep">/</span><span class="crs">${esc(t.course)}</span></div><div id="header-right-react"><button class="hbtn">${esc(t.share)}</button><button class="hbtn">${esc(t.help)}</button></div></div>
<div class="wrap">
<div class="main">
<h1 id="deliveryTitle">${esc(t.title(lec))}</h1>
<p class="meta"><time id="recorded" datetime="${lec.date}">${esc(t.when(lec))}</time> · ${esc(t.minutes(lec.min))}</p>
<div class="player"><div class="stage">${second}<div class="view">${slideSvg(t)}</div></div>${controls()}</div>
</div>
<aside class="side"><div class="tabs"><span class="on">${esc(t.contents)}</span><span>${esc(t.notes)}</span></div><ul>${chapters}</ul></aside>
</div>
<script>${PLAYER_SCRIPT(false, '')}</script>
</body></html>`;
}

function embedPage(t, lec) {
  return `<!doctype html>
<html lang="${t.lang}"><head><meta charset="utf-8"><title>${esc(t.title(lec))}</title>
<style>${BASE_CSS}
body{display:flex;flex-direction:column;background:#0b0f16;color:#e8ecf3}
h1#title{display:flex;align-items:center;gap:8px;margin:0;height:38px;padding:0 10px;font-size:13.5px;font-weight:600;background:#111722;white-space:nowrap;overflow:hidden}
h1#title .title-text{overflow:hidden;text-overflow:ellipsis}
.stage{flex:1;min-height:0}
.view{flex:1;aspect-ratio:auto}
</style></head>
<body>
<h1 id="title"><span class="title-text">${esc(t.title(lec))}</span></h1>
<time id="recorded" datetime="${lec.date}" hidden></time>
<div class="stage"><div class="view">${slideSvg(t)}</div></div>
${controls({ compact: true })}
<script>${PLAYER_SCRIPT(true, '')}</script>
</body></html>`;
}

function listPage(t) {
  const rows = LECTURES.map(
    (l, i) => `<tr class="list-view-row" id="${l.id}">
<td class="thumb"><div class="thumbnail">${slideSvg(t, { week: l.week, title: t.lang === 'en' ? l.en : l.zh })}<span class="dur">${l.min}:00</span></div></td>
<td><div class="item-title"><a class="detail-title" href="/Panopto/Pages/Viewer.aspx?id=${l.id}">${esc(t.listTitle(l))}</a></div><div class="item-meta">${esc(t.when(l))} · ${esc(t.views(40 + i * 7))}</div></td></tr>`,
  ).join('\n');
  return `<!doctype html>
<html lang="${t.lang}"><head><meta charset="utf-8"><title>${esc(t.course)}</title>
<style>${BASE_CSS}
#headerWrapper{background:#fff;border-bottom:1px solid #e1e5ec}
.top{display:flex;align-items:center;gap:10px;height:56px;padding:0 20px;font-weight:600;font-size:16px}
.top svg{width:26px;height:26px;color:#0f766e}
.bar{position:relative;display:flex;align-items:center;gap:10px;height:58px;padding:0 20px;border-top:1px solid #eef0f4}
.folder-name{margin-right:auto;font-size:19px;font-weight:650;color:#161a21}
.folder-name small{margin-left:8px;font-size:13px;font-weight:500;color:#69707d}
.subscribe-button,.sort{height:30px;padding:0 12px;border:1px solid #d5d9e0;border-radius:8px;background:#fff;color:#2b3240;font-size:13px}
table{border-collapse:collapse;width:100%;max-width:1280px}
td{padding:12px 20px;vertical-align:top;border-bottom:1px solid #e6e9ee}
tr{background:#fff}
td.thumb{width:170px;padding-right:0}
.thumbnail{position:relative;width:160px;height:90px;border-radius:8px;overflow:hidden;border:1px solid #e1e5ec}
.thumbnail .dur{position:absolute;right:6px;bottom:6px;padding:1px 6px;border-radius:5px;background:rgb(0 0 0 / 0.65);color:#fff;font-size:11.5px}
.item-title{font-size:16px;font-weight:600;line-height:1.5}
.item-title a{color:#123a7a;text-decoration:none}
.item-meta{margin-top:2px;color:#69707d;font-size:13px}
</style></head>
<body>
<div id="headerWrapper"><div class="top">${ICON.mark}<span>${esc(t.brand)}</span></div>
<div class="bar"><div class="folder-name">${esc(t.course)}<small>${esc(t.folder)}</small></div><button class="sort">${esc(t.sort)}</button><button class="subscribe-button">+ ${esc(t.subscribe)}</button></div></div>
<table><tbody>
${rows}
</tbody></table>
<script>document.body.dataset.ready = '1';</script>
</body></html>`;
}

/** The learning platform's page: the recording big (recordings) or small among a module's items (module). */
function lmsPage(t, kind) {
  const L = t.lms;
  const nav = L.nav
    .map((n) => `<li class="${(kind === 'module' ? n === L.modulesNav : n === L.nav[3]) ? 'on' : ''}">${esc(n)}</li>`)
    .join('');
  const embed = `http://${HOST}/Panopto/Pages/Embed.aspx?id=${LECTURES[0].id}`;
  const content =
    kind === 'module'
      ? `<h2>${esc(L.moduleTitle)}</h2>
<div class="mod">
<div class="item rec"><div class="it-title">${esc(L.items[0])}</div><iframe name="tool_content" title="${esc(L.items[0])}" src="${embed}" style="width:380px;height:214px"></iframe></div>
${L.items.slice(1).map((x) => `<div class="item"><span class="doc"></span>${esc(x)}</div>`).join('')}
</div>`
      : `<h2>${esc(L.heading)}</h2><p class="intro">${esc(L.intro)}</p>
<iframe name="tool_content" title="${esc(L.heading)}" src="${embed}" style="width:780px;height:470px"></iframe>`;
  return `<!doctype html>
<html lang="${t.lang}"><head><meta charset="utf-8"><title>${esc(L.crumbs.join(' › '))}</title>
<style>${BASE_CSS}
body{display:flex;background:#fff}
.rail{width:72px;flex:none;background:#1e2a3a;display:flex;flex-direction:column;align-items:center;gap:18px;padding-top:16px}
.rail .logo{width:40px;height:40px;border-radius:10px;background:#3e7bd6;color:#fff;font-weight:700;display:flex;align-items:center;justify-content:center;font-size:18px}
.rail i{display:block;width:26px;height:26px;border-radius:7px;background:#3a4a60}
.col{flex:1;min-width:0}
.crumbs{display:flex;align-items:center;gap:8px;height:56px;padding:0 24px;border-bottom:1px solid #e3e6eb;font-size:15px;color:#3e7bd6}
.crumbs span:last-child{color:#2b3240}
.page{display:flex}
.cnav{width:200px;flex:none;list-style:none;margin:0;padding:18px 0 0 24px}
.cnav li{padding:7px 12px;margin-bottom:2px;font-size:14px;color:#3e7bd6;border-left:3px solid transparent}
.cnav li.on{color:#1d2330;font-weight:600;border-left-color:#1d2330}
.content{flex:1;padding:22px 28px}
h2{margin:0 0 10px;font-size:22px;font-weight:650}
.intro{margin:0 0 16px;font-size:14px;color:#4b5261}
iframe{display:block;border:1px solid #d5d9e0;border-radius:6px}
.mod{border:1px solid #d5d9e0;border-radius:8px;overflow:hidden;max-width:760px}
.item{display:flex;align-items:center;gap:12px;padding:12px 16px;border-top:1px solid #e3e6eb;font-size:14px}
.item:first-child{border-top:0}
.item.rec{flex-direction:column;align-items:flex-start}
.it-title{font-weight:600}
.doc{width:16px;height:20px;border:2px solid #8a919d;border-radius:3px}
</style></head>
<body>
<nav class="rail"><div class="logo">L</div><i></i><i></i><i></i><i></i></nav>
<div class="col">
<div class="crumbs"><span>${esc(L.crumbs[0])}</span><span>›</span><span>${esc(kind === 'module' ? L.modulesNav : L.crumbs[1])}</span></div>
<div class="page"><ul class="cnav">${nav}</ul><div class="content">${content}</div></div>
</div>
</body></html>`;
}

const AUDIO_MASTER = '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=135000,AVERAGE-BANDWIDTH=128000,CODECS="mp4a.40.2"\nindex.m3u8\n';
const SCREEN_MASTER = '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=1920x1080,CODECS="avc1.640028"\nindex.m3u8\n';
const INDEX = '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:600\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:600.0,\nseg0.ts\n#EXTINF:600.0,\nseg1.ts\n#EXTINF:600.0,\nseg2.ts\n#EXT-X-ENDLIST\n';

function deliveryInfo(id, state, t) {
  const lec = LECTURES.find((l) => l.id === id);
  if (!lec) return null;
  const base = `http://${HOST}/sessions/${lec.id}`;
  const primary = `${lec.id}-${PRIMARY}`;
  const streams = [
    { Tag: 'AUDIO', StreamUrl: `${base}/${primary}.hls/master.m3u8` },
    { Tag: 'OBJECT', StreamUrl: `${base}/${SCREEN}.object.hls/master.m3u8` },
  ];
  if (state.twoScreens && lec === LECTURES[0]) streams.push({ Tag: 'OBJECT', StreamUrl: `${base}/${SCREEN2}.object.hls/master.m3u8` });
  return {
    Delivery: {
      SessionName: t.title(lec),
      Duration: lec.min * 60,
      IsPrimaryAudioOnly: true,
      Streams: streams,
      PodcastStreams: [{ StreamUrl: `${base}/${primary}.mp4` }],
    },
    DownloadUrl: `http://${HOST}/podcast/${lec.id}.mp4`,
  };
}

function send(res, status, type, body) {
  const buf = Buffer.from(body);
  res.writeHead(status, { 'content-type': type, 'content-length': buf.length, 'cache-control': 'no-store' });
  res.end(buf);
}

/** Starts the demo site; lang: "en" | "zh-CN". state.twoScreens: the player shows (and lists) a second screen. */
export function startStoreSite({ lang = 'en' } = {}) {
  const t = T[lang];
  const state = { twoScreens: false };
  const requests = [];
  const server = http.createServer((req, res) => {
    // As a proxy the browser sends absolute URLs; the Host header names the site.
    const url = new URL(req.url, 'http://placeholder.invalid');
    const host = String(req.headers.host || url.host || '').replace(/:\d+$/, '').toLowerCase();
    requests.push({ method: req.method, host, path: url.pathname });
    const page = (html) => send(res, 200, 'text/html; charset=utf-8', html);
    if (host === HOST) {
      const id = url.searchParams.get('id') || '';
      const lec = LECTURES.find((l) => l.id === id) || LECTURES[0];
      if (url.pathname === '/Panopto/Pages/Viewer.aspx') return page(viewerPage(t, lec, state));
      if (url.pathname === '/Panopto/Pages/Embed.aspx') return page(embedPage(t, lec));
      if (url.pathname === '/Panopto/Pages/Sessions/List.aspx') return page(listPage(t));
      if (url.pathname === '/Panopto/Pages/Viewer/DeliveryInfo.aspx' && req.method === 'POST') {
        let body = '';
        req.on('data', (c) => (body += c));
        req.on('end', () => {
          const info = deliveryInfo(new URLSearchParams(body).get('deliveryId') || '', state, t);
          if (!info) return send(res, 404, 'application/json', '{"error":"no such delivery"}');
          send(res, 200, 'application/json; charset=utf-8', JSON.stringify(info));
        });
        return;
      }
      if (url.pathname.startsWith('/sessions/')) {
        if (url.pathname.endsWith('/master.m3u8')) return send(res, 200, 'application/vnd.apple.mpegurl', url.pathname.includes('.object.hls') ? SCREEN_MASTER : AUDIO_MASTER);
        if (url.pathname.endsWith('/index.m3u8')) return send(res, 200, 'application/vnd.apple.mpegurl', INDEX);
      }
    }
    if (host === LMS) {
      if (url.pathname === '/course/recordings') return page(lmsPage(t, 'recordings'));
      if (url.pathname === '/course/module') return page(lmsPage(t, 'module'));
    }
    send(res, 404, 'text/plain', 'not found');
  });
  // No tunnels: HTTPS (and anything else) through this proxy fails.
  server.on('connect', (req, socket) => {
    requests.push({ method: 'CONNECT', host: String(req.url || ''), path: '' });
    socket.on('error', () => {});
    socket.end('HTTP/1.1 403 Forbidden\r\n\r\n');
  });
  // the browser drops connections as it likes
  server.on('connection', (socket) => socket.on('error', () => {}));
  server.on('clientError', (_err, socket) => socket.destroy());
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({ port, state, requests, t, lectures: LECTURES, close: () => new Promise((r) => server.close(r)) });
    });
  });
}

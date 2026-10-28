# AGENTS.md — adding a recording platform profile with an AI agent

> 给贡献者的 AI agent（Claude、Codex 等）看的操作说明：借助连接到贡献者自己浏览器的工具，观察录播平台，生成对策文件与脱敏 fixtures。给人看的说明在根目录的 `CONTRIBUTING-profiles.md`。

You are an AI coding agent helping a student add support for their university's lecture-recording platform to the LectureFlow browser extension. The result is a pull request with one **platform profile** (`extension/profiles/<id>.json`, declarative JSON, no code) and one or more **redacted detection reports** (`extension/fixtures/<id>/*.report.json`). The human-facing guide is `CONTRIBUTING-profiles.md` at the repository root; read it too. The format is defined by `extension/schema/profile.schema.json`; `extension/profiles/panopto.json` is the reference profile to copy from.

## 0. Ground rules (non-negotiable)

1. **The human's own session, the human's own recordings.** Work only in the browser the contributor is already signed in to, on recordings they can already watch. They sign in themselves. Never type, read, store or ask for passwords, MFA codes or session tokens.
2. **Observe, don't fetch media.** Never download or save media files. Never crawl: open only the one or two pages the human points you to. Do not send requests the page would not send, with one exception in step 2.D (one header-only request to learn whether media needs cookies).
3. **Nothing secret or personal reaches a file, a commit, a PR, or your chat output**:
   - cookies, `Authorization` headers, CSRF tokens;
   - values of signed or secret query parameters (`Signature`, `Policy`, `Key-Pair-Id`, `token`, `ks`, `X-Amz-*`, `hdnts`, `sig`, `auth`, …): keep parameter **names** only;
   - names, e-mail addresses, student or staff IDs, avatars;
   - real session, folder or user GUIDs and long numeric ids: replace them with fake values of the same shape;
   - the institution's tenant hostname: write `tenant.<platform-domain>` (for example `tenant.cloud.panopto.eu`) unless the human explicitly wants their institution named.
4. **No course information.** The schema deliberately has no fields for course names, course codes or folders, and the extension never reads them. Do not collect them, not even "for context".
5. **DRM means stop.** If playback uses Encrypted Media Extensions (`video.mediaKeys` set, `encrypted` events) or the manifests carry DRM signals (`#EXT-X-KEY` with a method other than `AES-128`, `<ContentProtection>`, Widevine / PlayReady / FairPlay URIs), record that the tracks are protected and stop there. Never try to work around it.
6. **Rules and terms.** If the platform's terms or the institution's rules clearly forbid this, stop and tell the human.
7. **Not for the sites on the do-not-handle list.** Never write a profile for YouTube or for a MOOC or online / paid course platform (Coursera, edX, Udemy, LinkedIn Learning's `/learning`, 中国大学 MOOC, B 站课堂 `/cheese` and the rest of [`extension/src/lib/blocklist.js`](src/lib/blocklist.js)); there the popup says "LectureFlow does not work on this site". The CI check and the extension's "Import local profile" refuse a profile whose hosts cover such a site, or whose `pathPattern` could reach a refused path. LectureFlow is only for the lecture recordings a student's own school gives them; if the human asks for one of these sites, stop and tell them.

## 1. What the human does first

Tell the human to do these, and wait until they confirm. You cannot click browser UI (toolbar, extension popups, permission prompts):

1. Install LectureFlow, set the user environment variable `LECTUREFLOW_DEV_EXTENSION=1` and restart LectureFlow and the browser (an installed LectureFlow lets only the store extension connect otherwise), then load the development build of the extension (`docs/install.md`, section "Browser extension").
2. Sign in to the recording platform in that browser and open **one recording page**. If the platform has a list or folder page, and if the university embeds it in an LMS (Canvas, Moodle, Blackboard…), keep those pages handy too.
3. Click the LectureFlow toolbar icon → **在此网站自动探测 / Detect automatically on this site** → allow the site when the browser asks. Reload the page and let the recording play for a few seconds.
4. In the popup (or the extension's settings page), **导出探测报告 / Export detection report**, and save the file. This report is already redacted by the extension; it is the main fixture.
5. Connect your browser tool to that browser (for example Claude in Chrome, or your harness's browser control over the Chrome DevTools Protocol). Work in a **new tab**, and close the tabs you opened when you are done.

## 2. What to capture

Work through this list on the recording page (and the list / LMS pages where relevant). Run read-only JavaScript in the page and look at the network requests the page has already made. Write down **patterns**, not values.

### A. Which pages the profile applies to (`match`)
- Hostnames: the tenant pattern and every regional domain the platform uses (check the vendor's public documentation; Panopto has five regions). Use wildcards: `*.example-video.com`.
- `pathPattern`: a regular expression for the recording page path (and its embed variant), for example `^/Panopto/Pages/(Viewer|Embed)\.aspx$`.
- If media comes from another host (a CDN), note it: `mediaHosts` is needed only when the extension must read cookies there.

### B. The session id (`session.idFrom`)
Where the page's own id for this recording lives: a query parameter (`{"query": "id"}`), a path segment (`{"pathPattern": "^/video/([^/]+)"}`), or an element attribute (`{"selector": "…", "attr": "…"}`). Check that it stays the same after a reload.

### C. Lecture fields (`lecture`): title, recordedAt, durationSec only
- Prefer stable selectors: ids, `data-*` attributes, semantic tags, ARIA labels. **Never** use generated class names such as `css-1a2b3c` or `sc-xyz`; they change between deployments and between standalone and embedded layouts.
- `recordedAt`: often only inside the title text. Use a selector with a `pattern` whose first group captures the date, and check that the extension parses it (see `extension/src/lib/dates.js`).
- `durationSec`: often from the media element (`video.duration`) or an observed field (see E).
- Each field may list several candidates; the first that yields a value wins.

```js
// Elements whose text equals the visible lecture title (tells you where the title lives).
const t = document.title.trim();
[...document.querySelectorAll('body *')].filter(e => e.children.length === 0 && e.textContent.trim() === t)
  .map(e => e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + [...e.classList].map(c => '.' + c).join(''));
```

### D. Media tracks (`tracks`, `presets`, `request`)
List what the player loaded, already redacted:

```js
// Media-looking resources the page loaded: host masked, ids masked, query reduced to parameter names.
[...new Set(performance.getEntriesByType('resource').map(e => e.name)
  .filter(u => /\.(m3u8|mpd|mp4|m4a|m4s|m4v|ts|aac|mp3|webm)(\?|$)|manifest|playlist/i.test(u))
  .map(u => { const x = new URL(u);
    return x.host.replace(/^[^.]+/, 'tenant') +
      x.pathname.replace(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, '{guid}')
                .replace(/[0-9a-f]{24,}/gi, '{hex}').replace(/\d{5,}/g, '{n}') +
      (x.search ? ' ?' + [...x.searchParams.keys()].join(',') : ''); }))]
```

For each track, decide:
- **kind** (`audio` / `video`): for HLS, read the master playlist's `CODECS` (`mp4a` only → audio); for DASH, the AdaptationSet `contentType` / `mimeType`.
- **format**: `file`, `hls` (single-file byte-range HLS counts as hls) or `dash`.
- **role**: `primary-audio`, `screen`, `camera`, `podcast` (a complete MP4 the platform offers), and a human label in zh-CN and en. A `urlPattern` that tells the roles apart (matched against the URL **without** its query).
- **presets**: which roles make up "main audio and video" (`full`) and "audio only" (`audio`).
- **Do the media URLs need cookies?** Make **one** header-only request from the page with credentials omitted, for one media or manifest URL the page already loaded: `await fetch(url, {method: 'HEAD', credentials: 'omit'}).then(r => r.status)`. 200 → `request.cookieUrls: []`; 401/403 → `["track"]`. Do not print the URL.
- `request.maxConnectionsPerFile`: keep it modest (4 or less).

### E. JSON the page already fetches (`observe`)
Many players fetch a JSON "delivery" or "media info" document that holds the full-MP4 URL, a download-allowed flag, the duration or the title. If so, describe it with `observe`: `urlPattern` for that request and JSON `pointers` (for example `"/Delivery/PodcastStreams/*/StreamUrl"`) for **only** the fields you need. Look at the response in the network log, write down **key paths and value types only**, and never paste the response anywhere: such responses usually contain the viewer's name and e-mail.

### F. Restriction flag (`downloadPolicy`)
Where the page says the instructor or university restricted saving: an element (`disabledWhen.selector`) or an observed field (`{"observed": "…", "empty": true}`). If it cannot be read, leave it out and say so in the PR. Also say whether the recording you tested is restricted, so the reviewer can check both cases.

### G. List / folder pages and LMS embedding (`inpage`, `sessionInfo`)
- `inpage.player`: a stable anchor in the recording page's header for the "发送至 LectureFlow" button.
- `inpage.list`: the row element, how to read each row's session id and title, and a stable anchor for "更新这门课程到 LectureFlow" next to the folder's own controls.
- Check **both** layouts: the platform's own page and the same page embedded in the LMS iframe (open the LMS page; layouts often differ, and generated class names differ too). You may not be able to run scripts inside a cross-origin iframe from the top page; open the iframe's address in its own tab, or describe what you can see and ask the human.
- If the media needs cookies (`"cookieUrls": ["track"]`) and the player is embedded in an LMS, ask the human to send once from inside the LMS page as well. In Firefox (and in Chrome with third-party cookies blocked) the player's cookies live in the LMS site's partition; the extension reads only the partition of the tab it sends from, which it identifies from the top page's site (`location.ancestorOrigins`), so the in-page button works there without allowing the LMS site. The first send from a page embedded in a site the human has not trusted yet opens a small confirmation window (protection against click-jacking); that is expected.
- If list pages do not carry track information, describe in `sessionInfo` the same-origin request the recording page makes to get it (method, path, form fields with `{sessionId}`). The extension sends it only after the user clicks, only for the lectures being sent.

## 3. Write the profile

1. Copy `extension/profiles/panopto.json` to `extension/profiles/<id>.json` (`id`: lowercase letters, digits and dashes; the file name must equal the id).
2. Fill it from section 2. Set `"version": "0.1.0"`, `"draft": true` until the human has tried it, and `"maintainers"` to the contributor's GitHub handle (ask them).
3. Labels in zh-CN and en (`en` is required); add `zh-TW`, `yue-Hant-HK`, `ru` and `hi` too if you can, as `panopto.json` does (the extension's six interface languages; a missing one falls back to English, Cantonese to `zh-TW` first). Keep the extension's wording: lectures are **sent** to LectureFlow and **fetched** (刮削); do not write "download" in any language (擷取 / получение / फ़ेच, see `web/i18n/glossary.md` §9).
4. Validate against the schema while you write: `cd extension && npm ci && npm test` runs the same checks as the extension's "Import local profile".

## 4. Fixtures

1. Put the report from step 1.4 in `extension/fixtures/<id>/<scenario>.report.json` (for example `viewer.report.json`, `restricted.report.json`, `lms-embedded.report.json`).
2. Read it line by line before committing. The CI leak scan (`node scripts/profilecheck/check.mjs` from the repository root) rejects cookies, auth headers, e-mail addresses, JWTs, real-looking ids and non-placeholder query values, but you are responsible for what it cannot know (a person's name in a title, an institution name in a host).
3. `npm test` in `extension/` replays every fixture through the generic detection and your profile, and checks track count and names. Make it pass.

## 5. Try it for real

Ask the human to open the extension's settings → **导入本地对策文件 / Import local profile** → choose your file. On the recording page the card should show the right title, date, tracks and labels, marked **未审核 / unreviewed**. Ask them to send one short lecture to LectureFlow and confirm that it is fetched and transcribed. Fix what is wrong, then set `"draft": false`.

## 6. Pull request

Follow `CONTRIBUTING-profiles.md` and the PR template (`?template=profile.md`). Profiles and fixtures are released under CC0 (`extension/profiles/LICENSE`); a PR that only touches `extension/profiles/` and `extension/fixtures/` does not need the CLA. In the description, state the platform and its version or region, whether an LMS embedding was tested, which fields you could not verify, and how the restriction flag behaved, without naming the institution unless the human wants to.

## 7. Report back to the human

Finish with a short summary: what you captured, what you could not verify and why, which files you changed, and anything they still have to click or decide.

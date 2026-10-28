# 添加录播系统 / Adding a recording platform

[简体中文](#简体中文) · [English](#english)

> 底稿（2026-10-26），将整理为 M3 的 `CONTRIBUTING-profiles.md`。扩展里的菜单名尚未定稿，文中用〔方括号〕标出，以扩展实际界面为准。

## 简体中文

LectureFlow 浏览器扩展认识一个录播系统，靠的是一份**对策文件**（profile）：一个只含声明的 JSON，告诉扩展哪些网页属于这个平台、这节课的 ID 在哪、标题在哪，以及怎样给扩展已经探测到的媒体轨道起名字。对策文件里不能写脚本。唯一能让扩展多发的请求是 `sessionInfo`：用户在文件夹列表上点了发送之后，扩展为要发送的每节课向该页同源的平台地址请求讲座信息，与平台自己的播放页发出的请求相同。写错了，最多是轨道名字不对或读不到讲座信息，不会让扩展去请求别的网站。

本指南面向会用浏览器开发者工具（Elements、Network、Console）的同学。你需要：

- 在这个平台上至少有一节你有权观看的录播（最好两节：一节受限、一节不受限；一节有摄像头、一节没有）；
- 一个 GitHub 账号；
- 大约一小时。

### 第 0 步：先看通用探测能不能找到轨道

打开一节录播，**开始播放**，点工具栏上的扩展图标。

- 显示“探测到 N 条轨道”：可以继续。对策文件只负责命名和推荐，不负责“找到”。
- 显示 0 条：先确认已经开始播放；如果播放器在另一个网站的 iframe 里（例如嵌在 Canvas、Moodle 页面中），在扩展〔站点权限〕里把播放器所在的网站也加上。
- 轨道标着“受版权保护（DRM）”，弹窗显示“这条媒体流受数字版权保护（DRM），LectureFlow 不处理受保护的内容。”：页面用了 DRM（加密媒体扩展）。LectureFlow 不支持这种平台，请不要提交对策文件。

### 第 1 步：导出脱敏的探测报告

在扩展〔设置 › 导出探测报告〕导出当前页面的报告，得到 `lectureflow-report-<平台>-<日期>.json`。

报告**只**包含：

- 地址的协议、域名和路径；路径里像 ID 的长串（UUID、长数字、长字母数字串）换成形状相同的假值，同一个 ID 总是换成同一个假值；
- 查询参数只留**名字**，值一律换成占位符（如 `{uuid-1}`、`{tok120-3}`）；
- 清单（m3u8、mpd）按理解重建：地址同上处理，数字和枚举属性保留，自由文本只留形状；
- 页面上被对策文件选择器命中的节点的**结构**（标签、id、class、属性名），文字只留形状（字母变 x / X，数字变 0）。

报告**从不**读取 Cookie。只要报告里出现 `@`、看起来像学号或姓名的字段名，扩展会拒绝导出并告诉你在哪里。**不要手工改报告来绕过**：手改会让回放失败，也很容易漏掉东西。遇到这种情况，请开一个 issue，只描述是哪一类发现、出现在报告的哪个位置。

导出之后自己再读一遍，在文件里搜索：你的姓名、学号、邮箱用户名、学校缩写、课程代码、讲师姓名。

### 第 2 步：用开发者工具找齐对策文件要的信息

| 对策文件字段 | 去哪里找 | 注意 |
|---|---|---|
| `match.hosts` | 地址栏的域名 | 写平台的域名，并用 `*.` 通配覆盖其他学校与区域（如 `*.hosted.panopto.com`）。`*.example.com` 同时包含 `example.com` 本身。不要写到只适用于你们学校的子域名；也不能通配整个公共后缀或共享托管域（`*.co.uk`、`*.edu.au`、`*.github.io`、`*.cloudfront.net` 之类会被拒绝） |
| `match.pathPattern` | 播放页的路径 | 正则，只测路径（不含查询），不区分大小写。只匹配**单节课的播放页**；课程列表、文件夹页一律不要匹配 |
| `session.idFrom` | 地址里的课次 ID | 三选一：`{"query": "id"}`（查询参数）、`{"pathPattern": "^/lesson/([^/]+)"}`（路径里恰好一个捕获组）、`{"selector": …}`。必须每次打开同一节课都一样；不要用会过期的会话令牌 |
| `lecture.title` / `recordedAt` / `durationSec` | Elements 面板：在标题上右键 › 检查 | 选择器尽量用 `id` 或语义稳定的 class；不要用 `css-1x2y3z` 这类自动生成的 class。可以给出多个选择器按顺序尝试，可以用 `pattern` 截取一部分。**只有这三个字段**：课程名、课程代码、文件夹、讲师姓名都不能读 |
| `tracks[].detect` | Network 面板：筛选 `m3u8`、`mpd`、`mp4`，开始播放后看请求 | `urlPattern` 是正则，测的是**去掉查询参数**的完整地址；只按路径规律写，不要写 CDN 的随机域名。配上 `kind`（audio / video）和 `format`（hls / dash / file）。规则按顺序“先到先得” |
| `tracks[].recommended` | — | 一次最多送入一条音频、两条视频（双机位）；推荐转写最需要的那条（通常是主讲音频，或带声音的主讲视频） |
| `downloadPolicy.disabledWhen` | 在一节**受限**和一节**不受限**的课上，比较表示限制的元素（例如讲师关闭了平台自己的下载选项时，页面上哪个元素不同） | 只有页面上确实有可靠的标记时才写；命中即“该媒体流可能受讲师或大学政策限制”，弹窗显示受限提示。拿不准就不写，弹窗不会提示 |
| `request.cookieUrls` | 见下 | `[]` 或 `["track"]` |
| `request.maxConnectionsPerFile` | — | 默认 4；平台限流（429、频繁断开）时调低 |
| `match.mediaHosts` | Network 面板里媒体请求的域名 | 只在媒体在另一个域名、**并且**需要 Cookie 时才写；不要写 `*.cloudfront.net` 这种公共 CDN 的整个域 |

**判断要不要 Cookie**：在 Network 面板对一个媒体清单请求右键 › Copy › Copy as cURL，粘到你自己电脑的终端里，**删掉带 Cookie 的那一项**（`-b '…'` 或 `-H 'cookie: …'`）再运行。仍然返回 200：`cookieUrls` 写 `[]`；返回 401 / 403：写 `["track"]`。复制出来的 cURL 含有你的登录凭据，用完立即清掉，**不要贴到 issue、PR 或聊天里**。

### 第 3 步：照着 panopto.json 写

从仓库里的 `extension/profiles/panopto.json` 复制一份改名为 `extension/profiles/<id>.json`。各字段：

```json
{
  "schemaVersion": 1,
  "id": "<小写字母开头，字母数字和连字符，如 my-platform>",
  "version": "0.1.0",
  "name": "<平台名称>",
  "maintainers": ["@<你的 GitHub 账号>"],
  "match": { "hosts": ["*.<平台域名>"], "pathPattern": "^/<播放页路径>$" },
  "session": { "idFrom": { "query": "<参数名>" } },
  "lecture": { "title": { "selector": "<选择器>" } },
  "tracks": [
    {
      "role": "primary-audio",
      "label": { "zh-CN": "主讲音频", "en": "Lecture audio" },
      "detect": { "urlPattern": "<路径正则>", "kind": "audio", "format": "hls" },
      "recommended": true
    }
  ],
  "request": { "sendReferer": true, "cookieUrls": [], "maxConnectionsPerFile": 4 },
  "fixtures": "fixtures/<id>/"
}
```

- `label` 至少要有 `en`；能写几种语言就写几种。
- 新平台从 `0.1.0` 开始。以后：只改选择器或正则修 bug，升补丁号；加轨道规则或字段，升次版本号；改 `match` 或 `session`（会影响去重），升主版本号。
- 还没在真实页面上验证完的，加 `"draft": true`：会做校验，但不会打包进扩展。

### 第 4 步：在扩展里试用

1. 扩展〔设置 › 导入本地对策文件〕，选你的 JSON。有错误会逐条列出（字段、哪个正则编译不了、`pathPattern` 捕获组不是一个等）。
2. 打开录播页并开始播放，点扩展图标。弹窗里会用你的轨道名称，并标着“未审核”。要改进扩展自带的平台（文件的 `id` 与自带的相同）时，导入时会先问你是否代替自带版本，确认后才使用你的文件。
3. 逐项核对：轨道名称和数量、推荐勾选、标题、录制日期、受限提示；换一节课再看一次。
4. 同一节课点两次“发送至 LectureFlow”，第二次应提示“这节课已经导入 LectureFlow。”——这说明会话 ID 是稳定的。
5. 修改 JSON 后重新导入即可覆盖。
6. 最后**再导出一次报告**：这次报告里会带上对策文件的命名结果和选择器命中的节点结构，CI 要用它。

### 第 5 步：提 PR

放这两样东西：

- `extension/profiles/<id>.json`
- `extension/fixtures/<id>/<场景>.report.json`：第 4 步最后导出的报告，每个场景一份，最好两份以上（例如有摄像头 / 没摄像头、受限 / 不受限、两个不同区域）。文件名写场景，不写学校。

PR 描述里写：平台名称、你测试的区域域名（不写学校名）、每个报告对应的场景、是否需要 Cookie、受限标记是怎么确认的。

**许可**：对策文件与 fixtures 以 CC0 1.0（公有领域贡献）发布，提交 PR 即表示你同意以 CC0 发布它们，并确认其中没有别人的受版权保护内容。扩展代码本身沿用项目的 AGPL-3.0-or-later。只含对策文件与 fixtures 的 PR 不需要签署 CLA，CLA 检查会自动通过；改动其他文件的 PR 照常需要（见 [CONTRIBUTING-profiles.md](../../CONTRIBUTING-profiles.md)）。

### CI 会检查什么

1. **Schema 校验**：字段与类型符合 `extension/schema/profile.schema.json`；所有正则都能编译，而且不会回溯失控（不用反向引用；重复的分组里不能再有 `*`、`+`、`{n,}` 或 `|`；整条正则最多三个 `*` / `+` / `{n,}`；正则只用在最长 2048 个字符的内容上）；`sessionInfo.path` 是该页同源的普通路径，并且匹配它所指 `observe` 条目的 `urlPattern`；`session.idFrom.pathPattern` 恰好一个捕获组；`role` 不重复；`id` 不是保留的 `generic`。
2. **fixtures 回放**：用报告里记录的输入重跑通用探测，轨道数量、类型、格式必须和报告一致；再套用你的对策文件，轨道命名、推荐勾选、会话 ID 来源、是否受限的结论必须和报告里的期望一致。
3. **脱敏扫描**：fixtures 里出现 `@`、像身份字段的查询参数名或属性名，直接失败。

### 脱敏要求（硬性）

- **任何包含 Cookie、个人姓名、学号的 fixture 一律不合并。**
- 同样不接受：邮箱、校内账号、讲师姓名、课程名或课程代码、签名参数或令牌的值、截图、录屏、HAR 文件。
- 只交扩展导出的报告，不要手改；宁可只交对策文件、不交 fixture。
- 维护者合并前会再查一遍；发现问题会要求你重新导出，必要时从 PR 历史里删掉。

### 不接受的内容

- 脚本、`eval`、远程地址或任何“动态规则”；
- 读取课程、文件夹、讲师信息的选择器；
- 绕过 DRM、登录或付费墙的任何做法；
- 要求扩展去请求页面没有请求过的地址（对策文件做不到，也不会加这种能力）；
- 扩展不处理的网站的对策文件：YouTube，以及 MOOC 与在线 / 付费课程平台（名单见 `extension/src/lib/blocklist.js`；这些网站上弹窗显示“这个网站不在 LectureFlow 的支持范围内”，CI 与“导入本地对策文件”都会拒绝）。

### 已有草稿、等人验证的平台

`docs/extension/drafts/` 里有根据公开资料写的草稿（目前是 Echo360），标着 `draft` 与“未经真实账号验证”。如果你有这个平台的账号，最有用的贡献是：按第 1、4 步在课次播放页上导出报告，核对草稿里的标题选择器、轨道文件名和是否需要 Cookie，然后连同修正后的对策文件一起提 PR。

---

## English

The LectureFlow browser extension recognises a recording platform through a **profile**: a declarations-only JSON file that says which pages belong to the platform, where the lecture's ID and title are, and how to name the media tracks the extension has already detected. A profile cannot contain scripts. The one request it can add is `sessionInfo`: after the user clicks to send from a folder list, the extension asks the platform, at the page's own origin, for the details of each lecture being sent, as the platform's own player page does. A mistake in a profile can at worst give a track the wrong name or fail to read a lecture's details; it cannot make the extension request another site.

This guide is for students who can use the browser's developer tools (Elements, Network, Console). You need:

- at least one recording on the platform that you are allowed to watch (two is better: one restricted and one not; one with a camera and one without);
- a GitHub account;
- about an hour.

### Step 0: check that generic detection finds the tracks

Open a recording, **start playback**, and click the extension's toolbar icon.

- "N tracks detected": carry on. A profile only names and recommends tracks; it does not find them.
- 0 tracks: make sure playback has started. If the player sits in an iframe from another site (for example inside a Canvas or Moodle page), add the player's site under the extension's 〔Site permissions〕 too.
- Tracks marked "Copyright protected (DRM)", with the popup saying "This stream is protected by digital rights management (DRM). LectureFlow does not handle protected content.": the page uses DRM (Encrypted Media Extensions). LectureFlow does not support such platforms; please do not submit a profile.

### Step 1: export a redacted detection report

Use the extension's 〔Settings › Export detection report〕 on the page. You get `lectureflow-report-<platform>-<date>.json`.

The report contains **only**:

- the scheme, host and path of addresses; long ID-like runs in a path (UUIDs, long numbers, long alphanumeric tokens) are replaced by fakes of the same shape, the same ID always by the same fake;
- the **names** of query parameters; every value becomes a placeholder such as `{uuid-1}` or `{tok120-3}`;
- manifests (m3u8, mpd) rebuilt from what is understood: addresses as above, numeric and enumerated attributes kept, free text reduced to its shape;
- the **structure** of the page nodes your profile's selectors hit (tag, id, classes, attribute names); their text reduced to its shape (letters become x / X, digits 0).

A report **never** reads cookies. If anything in it contains `@` or a field name that looks like a student number or a name, the extension refuses to export and tells you where. **Do not edit the report by hand to get around this**: edited reports fail the replay, and it is easy to miss something. Open an issue instead, saying only what kind of finding it was and where in the report.

After exporting, read the file yourself and search it for your name, student number, e-mail user name, university abbreviation, course codes and lecturers' names.

### Step 2: collect what the profile needs with the developer tools

| Profile field | Where to look | Notes |
|---|---|---|
| `match.hosts` | The host in the address bar | Write the platform's domain with a `*.` wildcard so it covers other universities and regions (for example `*.hosted.panopto.com`). `*.example.com` includes `example.com` itself. Do not write a sub-domain that only your university uses; nor a wildcard over a whole public suffix or shared host (`*.co.uk`, `*.edu.au`, `*.github.io`, `*.cloudfront.net` and the like are refused) |
| `match.pathPattern` | The player page's path | A regular expression, tested against the path only (no query), case-insensitive. Match **single-lecture player pages** only; never course lists or folder pages |
| `session.idFrom` | The lecture's ID in the address | One of `{"query": "id"}` (a query parameter), `{"pathPattern": "^/lesson/([^/]+)"}` (exactly one capture group on the path), or `{"selector": …}`. It must be the same every time the same lecture is opened; never use a session token that expires |
| `lecture.title` / `recordedAt` / `durationSec` | Elements panel: right-click the title › Inspect | Prefer an `id` or a meaningful, stable class; avoid generated classes such as `css-1x2y3z`. You may list several selectors to try in order and use `pattern` to keep part of the text. **These three fields are all there is**: course names, course codes, folders and lecturers' names must not be read |
| `tracks[].detect` | Network panel: filter for `m3u8`, `mpd`, `mp4` and watch the requests after playback starts | `urlPattern` is a regular expression tested against the full address **without its query**; describe the path, never a CDN's random host name. Add `kind` (audio / video) and `format` (hls / dash / file). Rules apply in order, first match wins |
| `tracks[].recommended` | — | One send takes at most one audio track and two video tracks (“Two cameras”); recommend what transcription needs most (usually the lecture audio, or the lecture video when it carries the sound) |
| `downloadPolicy.disabledWhen` | Compare a **restricted** lecture with one that is **not**, and find the element that shows the restriction (for example what changes when the lecturer turns off the platform's own download option) | Only when the page has a reliable marker; a match means "this stream may be restricted by the lecturer's or university's policy", and the popup shows the restriction notice. If unsure, leave it out and the popup says nothing |
| `request.cookieUrls` | See below | `[]` or `["track"]` |
| `request.maxConnectionsPerFile` | — | 4 by default; lower it if the platform throttles (429s, dropped connections) |
| `match.mediaHosts` | The host of the media requests in the Network panel | Only when the media is on another domain **and** needs cookies; never a whole public CDN domain such as `*.cloudfront.net` |

**Does it need cookies?** In the Network panel, right-click a manifest request › Copy › Copy as cURL, paste it into a terminal on your own computer, **delete the cookie option** (`-b '…'` or `-H 'cookie: …'`) and run it. Still 200: write `[]`. 401 / 403: write `["track"]`. The copied cURL contains your sign-in credentials: clear it as soon as you are done and **never paste it into an issue, a pull request or a chat**.

### Step 3: write the profile, using panopto.json as the model

Copy `extension/profiles/panopto.json` to `extension/profiles/<id>.json` and change it:

```json
{
  "schemaVersion": 1,
  "id": "<starts with a lower-case letter; letters, digits and hyphens, e.g. my-platform>",
  "version": "0.1.0",
  "name": "<platform name>",
  "maintainers": ["@<your GitHub handle>"],
  "match": { "hosts": ["*.<platform domain>"], "pathPattern": "^/<player page path>$" },
  "session": { "idFrom": { "query": "<parameter name>" } },
  "lecture": { "title": { "selector": "<selector>" } },
  "tracks": [
    {
      "role": "primary-audio",
      "label": { "zh-CN": "主讲音频", "en": "Lecture audio" },
      "detect": { "urlPattern": "<path regular expression>", "kind": "audio", "format": "hls" },
      "recommended": true
    }
  ],
  "request": { "sendReferer": true, "cookieUrls": [], "maxConnectionsPerFile": 4 },
  "fixtures": "fixtures/<id>/"
}
```

- `label` needs at least `en`; add as many languages as you can.
- A new platform starts at `0.1.0`. Later: a selector or pattern fix bumps the patch number; new track rules or fields bump the minor number; changes to `match` or `session` (they affect de-duplication) bump the major number.
- Not yet verified on real pages? Add `"draft": true`: it is validated but not packaged into the extension.

### Step 4: try it in the extension

1. Use 〔Settings › Import local profile〕 and pick your JSON. Errors are listed one by one (the field, a pattern that does not compile, a `pathPattern` without exactly one capture group, and so on).
2. Open a recording, start playback and click the icon. The popup uses your track names and marks the profile "Unreviewed". To improve a bundled platform (your file has the bundled profile's id), the import first asks whether it should replace the bundled version; only after you confirm is your file used.
3. Check everything: track names and count, recommended ticks, title, recording date, the restriction notice; then try another lecture.
4. Send the same lecture to LectureFlow twice: the second time it should say "This lecture is already in LectureFlow.", which shows the session ID is stable.
5. Re-import after each change; it replaces the previous import.
6. Finally **export the report again**: this one includes your profile's naming and the structure of the nodes your selectors hit, which CI needs.

### Step 5: open a pull request

Include:

- `extension/profiles/<id>.json`
- `extension/fixtures/<id>/<scenario>.report.json`: the report from the end of step 4, one per scenario, preferably two or more (with / without a camera, restricted / not, two regions). Name files after the scenario, never after a university.

In the description, give the platform's name, the regional domain you tested on (not your university), the scenario of each report, whether cookies are needed, and how you confirmed the restriction marker.

**Licence**: profiles and fixtures are published under CC0 1.0 (public domain dedication). By opening the pull request you agree to release them under CC0 and confirm they contain nobody else's copyrighted material. The extension's code stays under the project's AGPL-3.0-or-later. A pull request that contains only profiles and fixtures needs no CLA, and the CLA check passes it by itself; a pull request that touches anything else needs the CLA as usual (see [CONTRIBUTING-profiles.md](../../CONTRIBUTING-profiles.md)).

### What CI checks

1. **Schema validation**: fields and types match `extension/schema/profile.schema.json`; every regular expression compiles and cannot backtrack out of control (no backreferences; no repeated group holding `*`, `+`, `{n,}` or `|`; at most three `*` / `+` / `{n,}` in one expression; expressions only ever run on at most 2048 characters); `sessionInfo.path` is a plain path on the page's own origin and matches the `urlPattern` of the `observe` entry it names; `session.idFrom.pathPattern` has exactly one capture group; no `role` appears twice; the `id` is not the reserved `generic`.
2. **Fixture replay**: generic detection runs again on the inputs recorded in each report and must find the same number, kinds and formats of tracks; then your profile is applied and the track names, recommendations, session ID source and restriction verdict must equal what the report expects.
3. **Redaction scan**: any `@`, or a query parameter or attribute name that looks like an identity field, fails the check.

### Redaction rules (non-negotiable)

- **A fixture containing cookies, a person's name or a student number is never merged.**
- Also not accepted: e-mail addresses, university account names, lecturers' names, course names or codes, values of signatures or tokens, screenshots, screen recordings, HAR files.
- Submit only reports exported by the extension, unedited; a profile without fixtures is better than a hand-edited fixture.
- Maintainers check again before merging; if something slips through, you will be asked to re-export, and it will be removed from the pull request's history if necessary.

### What we do not accept

- scripts, `eval`, remote addresses or any "dynamic rules";
- selectors that read course, folder or lecturer information;
- anything that gets around DRM, sign-in or paywalls;
- requests for the extension to fetch addresses the page did not fetch (profiles cannot do this, and that ability will not be added);
- profiles for the sites the extension leaves alone: YouTube, and MOOCs and online or paid course platforms (the list is `extension/src/lib/blocklist.js`; on those sites the popup says "LectureFlow does not work on this site", and CI and "Import local profile" refuse such a profile).

### Platforms with a draft waiting for verification

`docs/extension/drafts/` holds drafts written from public sources (Echo360 for now), marked `draft` and "not verified with a real account". If you have an account on such a platform, the most useful contribution is to export reports on lecture player pages (steps 1 and 4), check the draft's title selector, track file names and whether cookies are needed, and open a pull request with the corrected profile.

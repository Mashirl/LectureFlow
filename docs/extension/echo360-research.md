# Echo360 调研（只记事实）与其他录播平台摘要

调研：research（LectureFlow 浏览器扩展），2026-10-26。

**没有用任何真实 Echo360 账号验证。** 下面的事实全部来自 GitHub 上的开源下载器、播放器增强脚本，以及 Echo360 官方帮助中心。这些项目的许可证各不相同（有的没有声明许可证，即保留所有权利），本文**不含它们的任何代码**，只用自己的话复述可以观察到的事实（地址形态、文件名、字段名、行为）。每条事实标了来源编号；来源的许可证与日期见第 1 节。Echo360 会改版（2026 年年中就改过一次课程列表），这些事实可能已经过时。

对策文件草稿：[`drafts/echo360.json`](drafts/echo360.json)；合成 fixtures 的设计：[`drafts/echo360-fixtures.md`](drafts/echo360-fixtures.md)。

## 1. 来源

| 编号 | 来源 | 许可证 | 引用文件的最近提交 / 页面更新 | 性质 |
|---|---|---|---|---|
| S1 | [soraxas/echo360](https://github.com/soraxas/echo360) | MIT | `echo360/course.py` 2026-07-30；`echo360/videos.py` 2024-12-17；PR #101（2026-07-28） | Python + Selenium 命令行下载器，348 星 |
| S2 | [mohsen-ameli/echo360-downloader](https://github.com/mohsen-ameli/echo360-downloader) | 未声明（保留所有权利） | `src/main.tsx`、`public/manifest.json` 2026-07-21（v4.5.0） | Chrome MV3 下载扩展 |
| S3 | [zhenban/lite-player-for-echo360](https://github.com/zhenban/lite-player-for-echo360) | GPL-3.0-or-later | `src/10-adapter-echo360.js`、`src/36-session.js` 2026-10-17；`src/03-common.js` 2026-10-19 | 替换播放器的用户脚本 / 扩展 |
| S4 | [Tchanwangsa/oculus](https://github.com/Tchanwangsa/oculus) | Apache-2.0 | `docs/auth.md` 2026-10-20；`app/src-tauri/src/sources/echo360/embed/` 2026-10-12 | 桌面学习助手，含 Echo360 嵌入视频下载 |
| S5 | [C0D3D3V/Moodle-DL](https://github.com/C0D3D3V/Moodle-DL) `moodle_dl/downloader/extractors/echo360.py` | GPL-3.0 | 2023-03-20 | Moodle 下载器里的 Echo360 公开链接提取器 |
| E1 | Echo360 帮助中心 [EchoVideo: Recommended Browsers](https://support.echo360.com/hc/en-us/articles/11062438116237-EchoVideo-Recommended-Browsers) | — | 2026-09-25 | 官方 |
| E2 | Echo360 帮助中心 [EchoVideo: Allow or Disable Downloading Media from Classes](https://support.echo360.com/hc/en-us/articles/11077312875789) | — | 2024-01-22 | 官方 |

可信度：两个以上独立来源一致或官方 = 高；单一来源 = 中；推断 = 低。

## 2. 区域域名

| 事实 | 来源 | 可信度 |
|---|---|---|
| EchoVideo 按区域分站：`echo360.org`（美国）、`echo360.net.au`（澳新、亚洲、太平洋）、`echo360.org.uk`（英国、冰岛、欧洲）、`echo360.ca`（加拿大） | E1；S3 的站点表一致；S2 的宿主权限一致 | 高 |
| `echo360.org.au` 也被当作区域站 | S4、S5（官方文章未列） | 中 |
| 页面在**裸域名**上（如 `https://echo360.org/lesson/...`），不是学校子域名 | S3 的匹配规则只写裸域名；S1、S5 的示例地址 | 高 |
| 媒体在 `content.<区域域名>`；字幕在 `captions.<区域域名>`；播放统计 API 在 `api.<区域域名>` | 媒体：S2 的 CSP 与请求过滤、S4 的测试数据、S1 把清单主机改写为 `content.<页面主机>`；字幕：S2；API：S3 | 媒体高，其余中 |
| `echo360.com` 是公司网站与帮助中心，不是播放站 | E1 只列上面四个；S2 把它放进权限，但没有证据表明有播放页 | 中 |
| 旧版自建 ESS（EchoSystem Server）用学校自己的域名，路径 `/ess/portal/section/<UUID>`、`/ess/client/...`，有的带 8443 端口 | S1 | 中（旧系统，正在淘汰） |

## 3. 页面地址与 ID

| 页面 | 地址形态 | 来源 | 可信度 |
|---|---|---|---|
| 课次播放页（classroom，需要登录） | `/lesson/<lessonId>/classroom` | S1、S3 | 高 |
| 课程首页（班级列表） | `/section/<sectionId>/home`；旧的 JSON 接口 `/section/<sectionId>/syllabus` 在 2026 年年中的新界面上只返回网页外壳 | S1（PR #101） | 高 |
| 公开链接（不需要登录） | `/media/<mediaId>/public`，`mediaId` 为 UUID（S5 的测试地址在 `echo360.org.uk` 与 `echo360.net.au` 上） | S5 | 中高（2023 年的资料） |
| LMS 嵌入 | 先 POST 到 `/lti/<UUID>?linkId=…`，再重定向到 `/ui/player/<mediaId>?sessionId=…&secureLinkAccessDataId=…` | S4 | 中 |

- `lessonId`：旧接口里是 UUID；S1 记录了 2026 年年中新界面课程列表里的 lesson ID 末尾带着这节课的开始与结束时间（`…_<开始 ISO 时间>_<结束 ISO 时间>`），并用它拼出 `/lesson/<id>/classroom`。两种形态可能并存。可信度：中。
- 课程首页和课次页都带课程信息（课程首页的 `h1` 就是课程名，见 S1）。按规格，扩展**只匹配课次页**，不碰课程首页。

## 4. 页面里的数据（对策文件读不到）

- 课次页用一段内联脚本启动播放器，调用 `Echo["echoPlayerV2FullApp"](<JSON 字符串>)`。JSON 里有标题、`lesson.id` / `lesson.name`、`video.mediaId`、`video.duration`（ISO 8601，如 `PT1358.393S`）、`video.playableMedias[]`（`uri`、`isHls`、`trackType`、`sourceIndex`、`isLive`）、`cookieRenewalIntervalMillis`，**也有课程名、课程代码、用户 ID 与角色**。（S3）
- 公开链接页用 `Echo["mediaPlayerBootstrapApp"](…)`，带 `sessionId`、`publicLinkId` / `shareLinkId`、`mediaId`。（S5）
- 接口：`/api/ui/sessions/<sessionId>` 在响应头 `Token` 里给一个 JWT；`/api/ui/echoplayer/public-links/<id>/media/<mediaId>/player-properties`（S5）与 `/api/ui/echoplayer/secure-link-access-datas/<id>/media/<mediaId>/player-properties`（S4）用 `Authorization: Bearer` 调用，返回 `playableAudioVideo.playableMedias`、`sourceQueryStrings`、`mediaName`、`copyrightData.restrictDownload`。
- 这些是脚本数据和 API，不是 DOM。对策文件只能用 CSS 选择器读元素文本或属性，读不到；而且启动 JSON 里混着课程信息，**也不应该**为了取一个字段去读整段脚本。

## 5. 媒体地址

| 事实 | 来源 | 可信度 |
|---|---|---|
| 目录形态：`https://content.<区域域名>/0000.<机构标识>/<mediaId>/1/<文件名>` | S4 测试数据；S1 的主机改写 | 中高 |
| `s0_a.m3u8`：只有音频的主清单 | S4 | 高 |
| `s1_v.m3u8`：第一路画面，只有视频；`s1_av.m3u8`：第一路画面与声音合成 | S1（只下载 `av`）、S4（优先 `v` + `a`） | 高 |
| `s2_v.m3u8` / `s2_av.m3u8`：第二路画面（第二个摄像头或屏幕，哪路是哪路取决于教室配置） | S1、S2、S4 | 高 |
| 清晰度清单：`s<n>q0.m3u8`（标清）、`s<n>q1.m3u8`（高清）；`s0q*` 为音频 | S2 | 中 |
| 同一目录下还有整段文件 `s<n>q<m>.mp4` 或 `.m4s`；更早的命名是 `hd1` / `hd2` / `sd1` / `sd2.mp4`（2026-07 仍有学校在用） | S2（HEAD 探测这些文件）、S1 | 中 |
| 还有 `captions.vtt`、`poster1.jpg` | S4 | 中 |
| 网页播放器只播 HLS；没见到 DASH | S3（只取 `isHls`） | 中 |

扩展不能主动请求页面没请求过的地址（对策文件只能筛选、改名），所以即使整段 MP4 存在，也只有播放器自己请求过它时才会被探测到。

## 6. 凭据

| 场景 | 事实 | 来源 | 可信度 |
|---|---|---|---|
| 课次页（登录后） | 媒体靠 **CloudFront 签名 Cookie**（HttpOnly，覆盖该机构的全部媒体），有效约 2 小时；页面声明的续期间隔 `cookieRenewalIntervalMillis` 为 1 小时，重新 GET 一次课次页就会续签 | S3 | 中高 |
| 课次页 | 登录态 Cookie 名为 `ECHO_JWT` | S1 | 中 |
| 课次页 | 带 Cookie（`credentials: include`）可以直接 HEAD `content.*` 上的文件 | S2 | 中 |
| 公开链接 / LMS 嵌入 | 不用 Cookie（LTI 嵌入只有 `PLAY_SESSION`），CDN 要 **CloudFront 签名查询参数** `Policy`、`Signature`、`Key-Pair-Id`；做法是把地址原有的查询**整个替换**成与 `uriPattern`（`*` 通配）匹配的那一条；追加在原查询后或不带查询都会 403；HLS 的每个分片地址也要带上 | S4、S5 | 高 |

## 7. 标题、日期、下载开关

- **标题**：没有找到任何项目用 CSS 选择器从课次页读标题。S2 用标签页标题，S3 依次用启动 JSON 的 `title`、`lesson.name`、`document.title`。S3 的合成测试数据里，`title` 以课程代码开头——**课次标题本身可能含课程代码**。草稿暂用 `<title>`，必须由有账号的贡献者确认。可信度：低。
- **录制日期**：页面上的位置未知。新界面的 lesson ID 可能自带开始时间（第 3 节），但对策文件只能读 DOM，不能读页面地址。可信度：低。
- **时长**：在启动 JSON 里（ISO 8601），DOM 位置未知；不写，引擎用媒体元素的时长。
- **下载开关**：学生的“Download / Download original”在**课程首页的班级列表**里，每节课的菜单中；讲师关闭后学生与助教看不到；还可以限制为只下载音频（E2）。课次页上没有对应控件；API 里有 `copyrightData.restrictDownload`（S4）。所以**课次页没有可读的 DOM 标记**，草稿不写 `downloadPolicy`（弹窗不提示，不等于允许）。

## 8. 对 LectureFlow 的影响

1. **必须带 Cookie**：`request.cookieUrls: ["track"]`。`match.hosts` 写 `*.echo360.org` 这类形式，既匹配裸域名的页面，也覆盖 `content.echo360.org`（扩展的 `hostMatches` 与浏览器的 `*://*.echo360.org/*` 权限都包含裸域名），不需要 `mediaHosts`。
2. **Cookie 只有约 2 小时**：排队太久会 403。规格的“需要刷新”正好覆盖；建议下载队列对 Echo360 的 401 / 403 直接标 `needs_refresh`，不要重试。一节课的下载通常远短于 2 小时。
3. **公开链接与嵌入页用签名查询**：LectureFlow 的 HLS 下载器拿到的是播放列表地址；分片地址若是相对路径、本身没有查询，直接请求会 403。要支持这两种页面，下载器需要“分片没有查询时沿用其播放列表的查询”（S5 的做法相当于此）。在决定之前，草稿只匹配课次页。
4. **轨道命名**：`s0_a` 是音频；`s1_*`、`s2_*` 是两路画面，不能断定哪一路是屏幕，命名用“画面 1 / 画面 2”。`s1_av` 自带声音，若只探测到它，就推荐它一条。
5. **探测时注意**：只有音频的主清单（`s0_a.m3u8`）里 `#EXT-X-STREAM-INF` 的 `CODECS` 只有 `mp4a`，通用 HLS 拆轨应据此把它判为音频而不是视频（交给 extension-builder 核对）。
6. **脱敏**：媒体路径里的 `0000.<机构标识>` 可能指向学校，报告与 fixtures 必须替换成假值；签名查询参数的值必须变成占位符。

## 9. 顺带：其他常见录播平台

只记开源资料里能看到的事实，不写草稿。

**Kaltura**（来源：yt-dlp `extractor/kaltura.py`，Unlicense，访问于 2026-10-26）
- 播放器与嵌入在 `cdnapisec.kaltura.com` / `www.kaltura.com`：`/html5/html5lib/<版本>/mwEmbedFrame.php/p/<partnerId>/uiconf_id/<id>/entry_id/<entryId>`，或 `/index.php/kwidget/...`、`/index.php/extwidget/preview/...`；学校的 LMS 集成（KAF）站点形如 `<学校>.kaf.kaltura.com`（yt-dlp 测试数据）。MediaSpace 门户常见于 `<学校>.mediaspace.kaltura.com`，这一条未在本次资料中核实。
- 媒体经 `playManifest` 取得，HLS 形式为把 `format/url` 换成 `format/applehttp`；也有按 `flavorId` 的整段 MP4。
- 鉴权票据 KS 可以**放在路径里**（`…/ks/<票据>`），不只在查询里——脱敏规则要覆盖路径段。会话 ID 用 `entryId`（形如 `1_abcd1234`）。

**Microsoft Stream（基于 SharePoint / OneDrive）**（来源：yt-dlp `extractor/sharepoint.py`，Unlicense）
- 页面：`<租户>.sharepoint.com/:v:/<类型>/…/<46 位 ID>` 分享链接，或 `<租户>.sharepoint.com/…/_layouts/15/stream.aspx?id=<文件路径>`。
- 页面脚本里的 `g_fileInfo` 给出 `.transformUrl`；媒体是同一转换服务的 `videomanifest`，`format=dash` 为 DASH，`format=hls` / `hls-vnext` 为 HLS；有权限时还有原文件 `downloadUrl`。
- 需要 Microsoft 365 登录 Cookie；未登录会跳到 `login.microsoftonline.com`。`id=` 参数是文件路径，常含用户名，**做会话 ID 前必须哈希**，报告里必须脱敏。

**YuJa**（来源：[JadedCtrl/yuja-dl](https://github.com/JadedCtrl/yuja-dl)，CC0，最近提交 2024-01-29，可能过时）
- 页面：`<学校>.yuja.com/V/Video?v=<videoPID>`。
- 接口：`/P/Data/VideoJSON`、`/P/Data/VideoSource?video=…&videoPID=…` 返回 `videoSources[]`（m3u8 地址）；字幕在 `/P/DataPage/CaptionFile/<key>`。
- 可选 Cookie 罐用于需要登录的视频。学校子域名各不相同，`*.yuja.com` 可以统一匹配。

**Mediasite**（来源：yt-dlp `extractor/mediasite.py`，Unlicense）
- 页面：`<学校主机>/Mediasite/Play/<34 位十六进制 ID>`，或 `/Mediasite/Showcase/<名称>/Presentation/<ID>`，可带 `catalog=`。学校自建域名，`hosts` 无法统一列出。
- 播放信息来自 `/Mediasite/PlayerService/PlayerService.svc/json/GetPlayerOptions`。

**Opencast**（来源：yt-dlp `extractor/opencast.py`，Unlicense）
- 页面：`/paella/ui/watch.html?id=<UUID>` 或 `/engage/ui/index.html?id=<UUID>`；剧集信息在 `/search/episode.json?id=<UUID>`。学校自建域名。

Mediasite、Opencast、旧版 Echo360 ESS、Panopto 自建安装都在学校自己的域名上：对策文件的 `hosts` 列不全，需要“用户把当前站点绑定到某个对策文件”的机制（见交付报告里的待决事项）。

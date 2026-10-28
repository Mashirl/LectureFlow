# Panopto 端点清单

调研：research（LectureFlow 浏览器扩展），2026-10-26。用途：给 `profiles/panopto.json` 的 `match`、`tracks`、`downloadPolicy` 提供依据，并照顾欧洲以外的租户。

可信度的写法：

- **高**：Panopto 官方资料，或官方公开租户的页面与前端脚本里直接看到，或至少两个独立来源一致。
- **中**：只有一个第三方来源（大学帮助页、开源项目），或从前端脚本推断、没在真实租户上验证。
- **低**：只有间接线索。

前端脚本的结论来自 Panopto 官方公开租户（`howtovideos.hosted.panopto.com`，Panopto 自己的教程视频站，不是学校）在 2026-10-26 加载的 17.14.0.00003 版脚本（`Viewer.js`、`ViewerReact.js`、`Embed.js`）。只读了文本，没有登录，也没有访问任何学校的录播页。Panopto 更新版本后，这些细节可能变化。

## 1. 托管区域与页面域名

Panopto 云服务跑在 AWS 上。官方《Technical Infrastructure and Security》列出的主区域是北美（us-east-1）、欧洲（eu-west-1）、亚太（ap-southeast-1，新加坡）、加拿大（ca-central-1）、澳大利亚（ap-southeast-2）[P1]。每个学校（租户）有一个子域名，形态按区域不同：

| 区域 | AWS 区域 | 页面域名形态 | 证据 | 可信度 |
|---|---|---|---|---|
| 北美 NA | us-east-1 | `<租户>.hosted.panopto.com` | [P1]；官方公开租户 `howtovideos.` / `demo.hosted.panopto.com`；yt-dlp 测试用例 [Y1]；多所美国大学帮助页 [U1] | 高 |
| 欧洲 EU | eu-west-1 | `<租户>.cloud.panopto.eu` | [P1]；yt-dlp 测试用例 [Y1]；英国大学迁区公告 [U1]；lead 实测（欧洲租户） | 高 |
| 亚太 AP | ap-southeast-1 | `<租户>.ap.panopto.com` | [P1]；香港一所大学迁云公告 [U2]；日本大学使用指南 [U3] | 中高 |
| 澳大利亚 AU | ap-southeast-2 | `<租户>.au.panopto.com` | [P1]；yt-dlp 测试用例 [Y1]；新西兰大学的 App 指南 [U4] | 高 |
| 加拿大 CA | ca-central-1 | `<租户>.ca.panopto.com` | [P1]；加拿大一所大学的帮助页 [U5]；Panopto 2019 年开加拿大区的新闻 [P3] | 中 |
| 日本 JP | 未公开 | 未知 | 2024-06 宣布在日本境内托管 [P4]；没找到任何公开的日本区租户地址；已知的日本大学仍在 `*.ap.panopto.com` [U3] | 低 |

其他相关主机：

- `login.panopto.com`：全局登录入口，输入邮箱后跳到所属租户 [U6]。不是播放页，不需要匹配。
- `static-assets-cdn.i.hosted.panopto.com`：北美区前端脚本与样式的 CDN（官方公开租户页面里看到）。不是媒体。
- `services.hosted.panopto.com`：自建（on-prem）安装连回 Panopto 的授权服务 [P2]。
- **自建安装（on-prem）**：用学校自己的域名，路径同样是 `/Panopto/Pages/...` [P2]。任何对策文件的 `hosts` 都覆盖不了，只能靠通用探测，或以后让用户把站点手动绑定到某个对策文件（见第 7 节）。
- **自定义域名**：没找到 Panopto 云服务支持自定义域名的官方说明；社区里有管理员希望支持却得不到的帖子 [P5]。可信度低，暂按“不存在”处理。

## 2. 播放页与嵌入页的路径

| 路径 | 用途 | 会话 ID | 证据 | 可信度 |
|---|---|---|---|---|
| `/Panopto/Pages/Viewer.aspx?id=<GUID>` | 完整播放页 | 查询参数 `id` | [Y1]；lead 实测；官方公开租户 | 高 |
| `/Panopto/Pages/Embed.aspx?id=<GUID>` | 嵌入播放器（iframe，常见于 Canvas、Moodle 等 LMS 页面里） | 查询参数 `id` | [Y1]；大学嵌入指南 [U7]；官方公开租户 | 高 |
| `/Panopto/Pages/Viewer.aspx?pid=<GUID>[&id=<GUID>]`、`Embed.aspx?pid=…` | 播放列表；带 `id` 时是列表里的某一节 | 有 `id` 时用 `id`，只有 `pid` 时退回通用探测的地址哈希 | [Y1] | 高 |
| `/Panopto/Pages/Sessions/List.aspx#folderID=…` | 文件夹列表 | — | [Y1] | 高 |
| `/Panopto/Pages/Auth/Login.aspx` | 登录 | — | 官方公开租户嵌入页配置里的 `LoginUrl` | 高 |

要点：

- 播放页常见的附加参数有 `start`、`autoplay`、`offerviewer`、`showtitle`、`showbrand`、`captions`、`interactivity`、`advance`、`instance`（LMS 集成）[U7][Y1]。都不影响匹配，`pathPattern` 只看路径。
- 服务器是 IIS，路径大小写不敏感；扩展编译对策文件里的正则时用了 `i` 标志，大小写不同的地址也能匹配。
- **`Sessions/List.aspx` 不能匹配**：它是文件夹（课程）结构，规格明确不读。文件夹级的播客 RSS 订阅同理不碰 [U8]。
- **`id` 是 Delivery ID，不是 Session ID**。官方公开租户的嵌入页配置里两者是不同的 GUID，媒体与缩略图路径里用的是 Session ID。去重键用 `id` 即可（一个 delivery 对应一节课），但不要指望在轨道 URL 里找到同一个 GUID。

## 3. 媒体 CDN 的形态

| 事实 | 证据 | 可信度 |
|---|---|---|
| 内容存在 S3，经 Amazon CloudFront 分发 | [P1] | 高 |
| 点播流的地址形态：`https://<分发>.cloudfront.net/sessions/<SessionId>/<流ID>.hls/master.m3u8`；主讲流的流 ID 是 `<SessionId>-<GUID>`，屏幕或对象流是 `<GUID>.object.hls` | lead 实测（欧洲租户）；Panopto 前端自己用来识别点播流的正则：`Sessions/<36 位>/<36 位>(-<36 位>)?…hls/master.m3u8`，大小写不敏感 | 高 |
| 流地址可能带查询参数 `InvocationID=<GUID>` | 同一段前端正则 | 中 |
| 变体播放列表的文件名为 `index.m3u8`、`index2.m3u8` | 前端常量 `HlsLevelFileNames` | 中 |
| 直播流是 `Sessions/_hlsbc/<id>/master.m3u8`（或 `_rtmp`），直播中会改请求 `master2.m3u8` | 同一段前端代码 | 中 |
| 北美区：官方公开租户的缩略图在 `d2y36twrtb17ty.cloudfront.net/sessions/<SessionId>/…`；同一分发是否也承载媒体，未验证 | 官方公开租户嵌入页配置里的 `ThumbUrl` | 中 |
| 各区的 CloudFront 分发名不同（随机子域），没有公开清单；Panopto 不公开白名单，社区里官方回复要走支持渠道 [P5] | — | 高 |
| 也可以接 Akamai 或客户自己的 CDN（在管理后台配置）；中国大陆另有方案 | Panopto 产品页 [P6]；[P1] 脚注 | 中 |
| 播放器是 hls.js（`Scripts/HlsJs/stable/hls.min.js`），清单和分片经 XHR 请求，`performance` 资源条目能看到 | 官方公开租户页面 | 高 |
| 欧洲租户拉媒体不需要 Cookie | lead 实测 | 高（只限该租户） |
| 有 Podcast 合成流（`PodcastStreams`）：普通流没有地址时播放器改播 Podcast 流；Podcast 流通常是各路合成的一条 | 前端代码；yt-dlp 注释 [Y1] | 中 |
| 完整 MP4 下载地址（`DownloadUrl`）只出现在 `DeliveryInfo.aspx` 的响应里，页面 DOM 读不到 | lead 实测；前端代码；[Y1] | 高 |

对对策文件的含义：

1. **轨道只按路径匹配，不按 CDN 域名匹配**。分发名随区域和学校配置变化，还可能是 Akamai 或学校自己的 CDN。
2. **不声明 `match.mediaHosts`**。媒体不需要 Cookie，没必要为 `*.cloudfront.net` 这种范围极大的域名申请权限（商店审核也会质疑）。通用探测的 `performance` 这一路不需要宿主权限就能看到 CDN 地址；拉 `master.m3u8` 拆轨走页面上下文的 `fetch`；播放器本身就是跨源 XHR 拉清单的，CloudFront 必然对播放页的源返回 CORS 头（推断，未单独验证）。
3. 如果以后有贡献者在别的区域发现 CDN 要 Cookie（403），需要单独讨论：不能为整个 `cloudfront.net` 申请权限。
4. 直播（`_hlsbc`）不命名、不推荐，留给通用探测显示。

## 4. “查看者能否下载”的页面标记（DOM）

背景：讲师或管理员可以按文件夹或单个视频决定谁能下载；不能下载的人看不到下载按钮；很多学校默认只有内容所有者能下载 [U9]。前端是否显示下载入口，只取决于 `DeliveryInfo` 响应里的 `DownloadUrl` 是否为空。

### Viewer.aspx（完整播放页）

- **新版右上角的下载入口是 React 渲染的“更多操作”菜单里的一项**（菜单 ID `more-actions-menu`，文案本地化）。菜单关着时这一项不在 DOM 里，“更多操作”按钮本身在有复制、统计、设置等任何一项时都会出现，所以**不能用它判断**。
- **旧版头部元素 `#podcastDownload` 仍在静态 HTML 里**（位于被隐藏的 `#header-right` 中），前端在新头部渲染完之后仍会处理它：`DownloadUrl` 为空时给它写上内联样式 `width: 0px; visibility: hidden;`；不为空时只绑定点击事件，不改样式。
- 因此：**`#podcastDownload[style*="visibility: hidden"]` 命中 = 不能下载**。可信度：中（读前端代码得出，一个版本，未在“允许下载”和“禁止下载”两种真实课上分别验证）。
- 时机：播放器载入会话数据之后才写样式。载入之前没有样式，读到的是“允许”（偏宽松）。扩展是在用户开始播放、探测到轨道后才读，一般已经载入。

### Embed.aspx（嵌入播放器）

- `#downloadButton` 在服务器输出的 HTML 里就带 `style="display: none"`；载入会话后，若 `DownloadUrl` 不为空（且当前设备支持下载），前端**删除整个 `style` 属性**让它显示。
- 因此：**`#downloadButton[style*="none"]` 命中 = 不能下载**。可信度：中高（逻辑简单直接；同样未在两种真实课上验证）。
- 时机：载入之前读到的是“不能下载”（偏保守）。

### 建议写法与验证

```json
"downloadPolicy": {
  "disabledWhen": [
    { "selector": "#podcastDownload[style*=\"visibility: hidden\"]" },
    { "selector": "#downloadButton[style*=\"none\"]" }
  ]
}
```

当前引擎的语义是“有 `disabledWhen` 但都没命中 = 允许”。两个页面各只有自己的那个元素，所以这一组在两页上都成立。

请 lead 在真实租户上各打开一节**允许下载**和一节**不允许下载**的课（各在 Viewer 与 Embed 两种页面），开始播放后在开发者工具控制台执行：

```js
[document.querySelector('#podcastDownload')?.getAttribute('style'),
 document.querySelector('#downloadButton')?.getAttribute('style')]
```

不允许时应分别含 `visibility: hidden` / `none`，允许时不含。四种情况都对上再写进 `panopto.json`；否则保留现状（不写 `downloadPolicy`，弹窗不提示）。

## 5. 标题、录制日期、时长

| 字段 | Viewer.aspx | Embed.aspx | 可信度 |
|---|---|---|---|
| 标题 | `h1#deliveryTitle`（前端填入）；`<title>` 初始为 `Panopto Viewer`，载入后改成会话标题 | `h1#title .title-text`（服务器端输出，元素初始隐藏但有文本）；`<title>` 服务器端即为会话标题 | 高 |
| `og:title` | 没有（只有 `og:site_name`） | 没有（只有 `og:url`，指向对应的 Viewer 地址） | 高（17.14.0 公开租户） |
| 录制日期 | 头部不显示。日期在 React 渲染的“详情”标签页里，类名由 CSS-in-JS 生成、不稳定，旁边就是所有者姓名 | 不显示 | 中 |
| 时长 | 只显示已播放和剩余时间 | 同左 | 高 |

建议：

- 标题依次读 `h1#deliveryTitle`、`h1#title .title-text`、`title`（`title` 加 `"pattern": "^(?!Panopto Viewer$)(.+)$"`，排除载入前的占位标题）。
- 录制日期只在会话标题本身带日期时用 `pattern` 从标题里取（lead 现有写法）；不读“详情”标签页。
- 时长不写，引擎会用媒体元素的时长。
- **不要读** `#parentContext`、`#parentName`：那是文件夹名，也就是课程信息。

## 6. 可以直接放进 panopto.json 的片段

推荐（逐个列出已证实的区域，商店说明和权限提示里一目了然）：

```json
"match": {
  "hosts": [
    "*.hosted.panopto.com",
    "*.cloud.panopto.eu",
    "*.ap.panopto.com",
    "*.au.panopto.com",
    "*.ca.panopto.com"
  ],
  "pathPattern": "^/Panopto/Pages/(Viewer|Embed)\\.aspx$"
}
```

备选（把将来的区域和日本区一起兜住，代价是权限提示变成“所有 panopto.com 与 panopto.eu 网站”）：

```json
"match": {
  "hosts": ["*.panopto.com", "*.panopto.eu"],
  "pathPattern": "^/Panopto/Pages/(Viewer|Embed)\\.aspx$"
}
```

两种写法不要混用：`*.panopto.com` 已经包含所有 `*.xx.panopto.com`，再列具体区域只会让权限清单变长。

轨道规则（按路径，与 CDN 域名无关；规则顺序即“先到先得”）：

```json
"tracks": [
  {
    "role": "primary-audio",
    "label": { "zh-CN": "主讲音频", "en": "Lecture audio" },
    "detect": { "urlPattern": "/sessions/[0-9a-f-]{36}/[0-9a-f-]{36}(-[0-9a-f-]{36})?\\.hls/master\\.m3u8$", "kind": "audio", "format": "hls" },
    "recommended": true
  },
  {
    "role": "primary-video",
    "label": { "zh-CN": "主讲视频", "en": "Lecture video" },
    "detect": { "urlPattern": "/sessions/[0-9a-f-]{36}/[0-9a-f-]{36}(-[0-9a-f-]{36})?\\.hls/master\\.m3u8$", "kind": "video", "format": "hls" },
    "recommended": true
  },
  {
    "role": "screen",
    "label": { "zh-CN": "屏幕录像", "en": "Screen capture" },
    "detect": { "urlPattern": "/sessions/[0-9a-f-]{36}/[0-9a-f-]{36}\\.object\\.hls/master\\.m3u8$", "kind": "video", "format": "hls" },
    "recommended": false
  }
]
```

说明：主讲流的流 ID 按 Panopto 自己的正则可以是一个或两个 GUID；`.object.hls` 不会被前两条吃掉（`.hls` 前必须紧跟 GUID）。屏幕录像不推荐，是因为一次送入最多一条视频；有摄像头时主讲视频带声音，更适合转写。

## 7. 与当前 `extension/profiles/panopto.json` 的差异（供 lead 参考）

对照 2026-10-26 读到的版本（extension-builder 负责，research 没有改动）：

1. `hosts` 同时列了 `*.panopto.com`、`*.panopto.eu` 和具体区域，后者多余；具体区域里缺 `*.au.panopto.com`（被 `*.panopto.com` 兜住了，所以不影响匹配）。建议二选一，见第 6 节。
2. 标题与日期的备选读 `meta[property="og:title"]`；在 17.14.0 的公开租户里两种页面都没有这个标签。可以换成 `h1#title .title-text`（Embed）和带占位排除的 `title`。
3. 没写 `downloadPolicy`。第 4 节给出了两种页面的标记，需要先在真实租户上验证。
4. 轨道正则用 `[^/]+`，比按 GUID 宽，问题不大。直播地址 `Sessions/_hlsbc/<id>/master.m3u8` 不会命中现有规则（`<id>` 后面没有 `.hls`），符合“直播不命名”的预期。
5. 自建安装与将来的“站点绑定”：on-prem 的学校域名不在 `hosts` 里。是否在设置里提供“把这个站点按 Panopto 处理”（用户手动授权该域名后套用 panopto.json 的其余规则），属于 M3 的产品决定。

## 来源

访问日期均为 2026-10-26。

- [P1] Panopto, *Technical Infrastructure and Security — Panopto Cloud Video Platform*（PDF，未注明日期）：<https://support.panopto.com/servlet/servlet.FileDownload?file=015Pb00000D7Ssy>；同主题帮助文章：<https://support.panopto.com/s/article/Panopto-Cloud-Architecture>
- [P2] Panopto, *Learn About On-Prem System Requirements and Prerequisites*：<https://support.panopto.com/s/article/On-Prem-Installation-Guide>
- [P3] Panopto 新闻，加拿大区（2019）：<https://panopto.com/?p=2079>
- [P4] Panopto 新闻，日本区（2024-06-13）：<https://www.panopto.com/news/panopto-launches-japan-cloud-hosting-first-video-content-management-solution-to-offer-local-hosting/>
- [P5] Panopto Community，白名单 URL 的提问与官方回复（2025-11）：<https://community.panopto.com/discussion/2799/list-of-urls-to-whitelist-for-panopto-videos>；自定义域名的愿望帖：<https://community.panopto.com/discussion/comment/1387>
- [P6] Panopto 产品页（HLS、Akamai 与 CloudFront、自有 CDN）：<https://www.panopto.com/features/video-cloud/>
- [Y1] yt-dlp `yt_dlp/extractor/panopto.py`（Unlicense；该文件最近提交 2026-10-21）：<https://github.com/yt-dlp/yt-dlp/blob/master/yt_dlp/extractor/panopto.py>
- 官方公开租户（Panopto 自己的教程站）的页面与 17.14.0.00003 版前端脚本：<https://howtovideos.hosted.panopto.com/Panopto/Pages/Viewer.aspx?id=5fa74e93-3d87-4694-b60e-aaa4012214ed>、同一 `id` 的 `Embed.aspx`；脚本在 `static-assets-cdn.i.hosted.panopto.com/Panopto/Cache/17.14.0.00003/Scripts/Panopto/Bundles/`
- [U1] 牛津大学 Replay 迁移到欧洲服务器的博客：<https://blogs.it.ox.ac.uk/lecture-capture/514>
- [U2] 香港大学迁移到 Panopto 云的公告：<https://its.hku.hk/news/migrating-panopto-on-premises-to-panopto-cloud-platform/>
- [U3] 日本大学的 Panopto 指南（例：明治大学）：<https://www.meiji.ac.jp/isys/doc/faq/Panopto_ViewingStatus.pdf>
- [U4] 怀卡托大学 Panopto App 指南：<https://www.waikato.ac.nz/students/teaching-and-learning/student/help-with-technology/panopto/the-panopto-app/>
- [U5] 约克大学（加拿大）Panopto 页面：<https://www.yorku.ca/glendon/outilstechnologiques/panopto/>
- [U6] Panopto 学生支持页（login.panopto.com）：<https://www.panopto.com/student-support/>
- [U7] 北卡大学嵌入指南（Embed.aspx 与参数）：<https://www.med.unc.edu/webguide/userguide/iframes>
- [U8] 莫纳什大学播客说明：<https://www.monash.edu/learning-teaching/teachhq/learning-technologies/panopto/using-podcasts>
- [U9] 牛津大学导出与下载说明：<https://help.web.ox.ac.uk/replay/exporting>；另一所大学的按文件夹 / 视频开下载的说明：<https://su-support.atlassian.net/wiki/spaces/IDD/pages/79168026/How+to+Enable+Podcast+Downloads+in+the+Viewer+per+Folder+or+Video+on+Panopto>

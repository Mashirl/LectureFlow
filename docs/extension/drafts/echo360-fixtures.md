# Echo360 合成 fixtures 的设计（草稿）

配套：[`echo360.json`](echo360.json)（`draft: true`，未经真实账号验证）、事实依据见 [`../echo360-research.md`](../echo360-research.md)。

目的：在没有 Echo360 账号的情况下，先用**完全合成**的数据把对策文件的 schema 校验、轨道命名和脱敏扫描跑通；等有账号的贡献者交来真实页面导出的脱敏报告，再替换或补充。合成 fixtures 只能证明“对策文件与我们对 Echo360 的理解一致”，不能证明“我们的理解与 Echo360 一致”。

## 1. 文件与生成方式

- 位置：`extension/fixtures/echo360/synthetic-<场景>.report.json`，CC0。文件名带 `synthetic-`，CI 与评审一眼能看出不是真实导出。
- **不要手写报告 JSON**。手写容易和 `buildReport()` 的输出格式（`inputs.frames`、`inputs.webRequests`、`inputs.fetches`、`dom`、`expected`）对不上，也绕过了脱敏代码。做法：在测试辅助里写一份合成的**输入**（页面地址、媒体元素、`performance` 资源条目、webRequest 记录、每个清单的文本），交给扩展的通用探测与 `buildReport()` 生成报告，再写入 fixtures 目录。这样同时测到了脱敏。
- 生成脚本与合成输入归 extension-builder（`extension/test/`），本文只定内容。

## 2. 合成数据的约定（全部场景通用）

| 项目 | 合成值 | 说明 |
|---|---|---|
| 页面主机 | `echo360.org`（另一场景用 `echo360.org.uk`） | 平台的区域域名不是学校信息，可以用真值 |
| 媒体主机 | `content.echo360.org` | 同上 |
| 机构路径段 | `0000.00000000-0000-4000-8000-00000000000a` | 真实值可能指向学校，一律用假 UUID |
| mediaId / lessonId / sectionId | `00000000-0000-4000-8000-00000000000N`（N 依次编号） | 形状与真实 UUID 相同，脱敏后仍能被路径正则匹配 |
| 带时间的 lessonId | `G_<假 UUID>_<假 UUID>_2026-01-05T09:00:00.000_2026-01-05T10:00:00.000` | 覆盖第二种 ID 形态；日期用虚构的 |
| 页面标题 | `Lecture 1` | **不带课程代码、课程名、讲师姓名** |
| Cookie | 无 | 报告本来就不读 Cookie；合成输入里也不要出现 `Cookie` 头或 `CloudFront-*` 字样 |
| 签名查询 | 只在场景 D 出现：`Policy=AAAA…`、`Signature=BBBB…`、`Key-Pair-Id=K0000000000000` | 全是占位字符，长度接近真实值，用来确认它们被替换成 `{tok…}` 占位 |
| 清单文本 | 自己写的最小 HLS，见第 4 节 | 不复制任何真实清单 |

## 3. 场景与期望

| 场景 | 页面 | 播放器请求了什么 | 期望（`expected.profile`） |
|---|---|---|---|
| A 两路画面 | `/lesson/<id>/classroom` | `s0_a.m3u8`（只有音频）、`s1_v.m3u8`、`s2_v.m3u8`，以及它们的 `s?q1.m3u8` 与若干分片 | 3 条轨道：`primary-audio`（推荐）、`view-1`（推荐）、`view-2`；会话 ID 来源 `profile`；`downloadPolicy` 为 `unknown` |
| B 只有合成流 | 同上，lessonId 用带时间的形态，主机 `echo360.org.uk` | 只有 `s1_av.m3u8` | 1 条轨道：`view-1`（推荐）；会话 ID 是整个带时间的 lessonId |
| C 旧命名整段文件 | 同上 | `<video src>` 直接指向 `hd1.mp4` | 1 条 `whole-file` 轨道（不推荐）；单文件探测的 HEAD 结果在 `inputs.fetches` 里 |
| D 公开链接（不匹配） | `/media/<id>/public` | `s1_av.m3u8?Policy=…&Signature=…&Key-Pair-Id=…` | `profile` 为 `null`（草稿不匹配公开链接，走通用探测）；报告里查询参数只剩名字，值是占位；`scanReport` 没有发现 |

每个场景再加两条反向检查：

- 课程首页 `/section/<id>/home` 不匹配对策文件。
- `captions.vtt`、`poster1.jpg` 不被任何轨道规则命中（不是媒体轨道）。

## 4. 合成清单示例（自己写的最小内容）

只有音频的主清单（场景 A 的 `s0_a.m3u8`），用来确认通用探测按 `CODECS` 把它判为音频：

```
#EXTM3U
#EXT-X-VERSION:6
#EXT-X-STREAM-INF:BANDWIDTH=96000,CODECS="mp4a.40.2"
s0q0.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=160000,CODECS="mp4a.40.2"
s0q1.m3u8
```

只有视频的主清单（`s1_v.m3u8`）：

```
#EXTM3U
#EXT-X-VERSION:6
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=1280x720,CODECS="avc1.64001f"
s1q0.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1920x1080,CODECS="avc1.640028"
s1q1.m3u8
```

媒体清单（`s1q1.m3u8`），三个分片足够；分片扩展名在真实系统里是 `.ts` 还是 fMP4（`#EXT-X-MAP` + `.m4s`）未知，场景 A 用 fMP4，场景 B 用 `.ts`，两种都覆盖：

```
#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="s1q1_init.mp4"
#EXTINF:6.000,
s1q1_00001.m4s
#EXTINF:6.000,
s1q1_00002.m4s
#EXTINF:4.500,
s1q1_00003.m4s
#EXT-X-ENDLIST
```

分片文件名是虚构的，只为让清单合法。

## 5. 有账号的贡献者来了之后

1. 在课次页导出真实报告（扩展会脱敏），按 `adding-a-platform.md` 自查后提交为 `fixtures/echo360/<区域>-<场景>.report.json`。
2. 对照真实报告修正草稿：标题选择器（`<title>` 是否就是课次标题、是否带站点后缀或课程代码）、录制日期是否有可读的 DOM 元素、轨道文件名是否与 `echo360-research.md` 第 5 节一致。
3. 确认后去掉 `draft`、版本改为 `1.0.0`、`maintainers` 换成贡献者自己的账号；合成 fixtures 保留，作为格式回归测试。

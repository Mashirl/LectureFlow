<!--
Platform profile pull request / 对策文件 PR.
Guide / 说明: CONTRIBUTING-profiles.md
Do not paste cookies, cURL commands, HAR files, screenshots, names, student
numbers or your university's name here. 不要在这里粘贴 Cookie、cURL 命令、HAR、截图、姓名、学号或学校名称。
-->

## Platform / 平台

- Name / 名称:
- Profile / 对策文件: `extension/profiles/<id>.json`, version / 版本:
- Regional domain tested on (not your university) / 测试用的区域域名（不写学校）:
- New platform or change to an existing one / 新平台还是修改已有平台:

## Fixtures / 报告

| File / 文件 | Scenario / 场景 (camera, screen only, downloads off, region …) |
|---|---|
| `extension/fixtures/<id>/….json` | |

- Cookies needed (`cookieUrls`) / 是否需要 Cookie: `[]` / `["track"]`, checked how / 如何确认:
- Download marker (`downloadPolicy`) / 下载开关标记: not used / 未使用 · confirmed on a downloadable and a non-downloadable lecture / 已在能下载与不能下载的课上确认

## Checklist / 清单

- [ ] The profile only declares; no script, no remote address. / 对策文件只含声明，没有脚本和远程地址。
- [ ] `lecture` reads only title, recording date and duration; nothing about courses, folders, lecturers or students. / `lecture` 只读标题、录制日期、时长，不读课程、文件夹、讲师或学生信息。
- [ ] `match.hosts` are the platform's own domains (no university-only sub-domain, no shared CDN). / `match.hosts` 是平台自己的域名（没有学校专用子域名、没有公共 CDN）。
- [ ] The platform is a lecture-recording system schools use for their own recordings, not YouTube and not a MOOC or online / paid course platform (`extension/src/lib/blocklist.js`; CI refuses those). / 平台是学校用来录自己课堂的录播系统，不是 YouTube，也不是 MOOC 或在线 / 付费课程平台（`extension/src/lib/blocklist.js`；CI 会拒绝）。
- [ ] Tested in the extension after Settings › Import a local profile (popup showed "Not reviewed"): track names, recommended ticks, title, date, download notice. / 已在设置里导入本地对策文件并试用（弹窗显示“未审核”）：轨道名称、推荐勾选、标题、日期、下载提示。
- [ ] Sending the same lecture twice says it is already imported. / 同一节课送入两次时提示已导入。
- [ ] Every fixture was exported by the extension with this profile imported, and **not edited by hand**. / 每份 fixture 都是导入本对策文件后由扩展导出的，**没有手改**。
- [ ] I read every fixture: no cookies, names, student numbers, e-mail addresses, account names, course codes, lecturer names or university names. / 我读过每份 fixture：没有 Cookie、姓名、学号、邮箱、账号、课程代码、讲师姓名或学校名称。
- [ ] `node scripts/profilecheck/check.mjs` and `npm test` (in `extension/`) pass on my computer. / 本机 `node scripts/profilecheck/check.mjs` 与 `extension/` 里的 `npm test` 都通过。
- [ ] I release the profile and fixtures under CC0 1.0 (`extension/profiles/LICENSE`) and they contain nobody else's protected material. / 我以 CC0 1.0 发布这份对策文件与 fixtures（`extension/profiles/LICENSE`），其中没有他人受保护的内容。

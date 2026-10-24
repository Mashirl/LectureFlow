# LectureFlow v3 验收指南

[1. GitHub CI](#1-github-ci) · [2. macOS 真机验收](#2-macos-真机验收apple-silicon) · [3. Colab 验收](#3-colab-验收) · [附录：Colab 抓包](#附录可选用官方-google-colab-cli-录制一次借卡) · [回报模板](#回报模板--report-template)

第 1、2 部分附英文版（English versions follow each Chinese section）。第 3 部分由项目负责人自己在应用里完成。

通用约定：

- **不要**在回报、截图、日志里出现 API key、令牌、Cookie、邮箱或讲座里的个人信息（例如讲师姓名）。截图前先看一眼。
- 每步写的是“应该看到什么”。看到的不一样，就记下实际看到的内容并截图，然后继续后面能做的步骤。
- 界面文字以应用实际显示为准；下面给出简体中文与英文界面的写法。
- 安装、更新相关的系统提示，见 [docs/install.md](install.md)。Windows 本机验收的结果可作参照：[docs/benchmarks/v3-acceptance.md](benchmarks/v3-acceptance.md)。

---

## 1. GitHub CI

**你需要准备什么**：对 GitHub 仓库有写权限的账号；把本地 `master` 推送到仓库（由负责人或你完成）。

### 推送后（每次推送都会运行）

1. 打开仓库的 **Actions** 页面，找到这次推送触发的运行。
   应该看到两个工作流：**CI** 与 **License audit**（**CLA** 只在 Pull Request 上运行，推送时不出现是正常的）。
2. 打开 **CI**，应该看到 3 个作业，全部为绿色：

   | 作业 | 内容 | 大约用时 |
   |---|---|---|
   | `ffmpeg-windows` | 在 Linux 上交叉编译 Windows 用的 LGPL ffmpeg（第一次较慢，之后命中缓存） | 首次 20–40 分钟 |
   | `test (windows-latest)` | Go vet、Go 测试、Python worker 测试、桌面外壳单元测试、许可证审计 | 10–20 分钟 |
   | `test (macos-14)` | Go vet、Go 测试、`GOOS=darwin` 构建、Python 测试、桌面外壳单元测试 | 10–20 分钟 |

3. 打开 **License audit**，作业 `audit` 应为绿色（依赖许可证审计 + SPDX 文件头两步）。

**哪些失败要回报**：任何一个红色作业都要回报。在回报里写作业名、失败的步骤名（例如 `Go tests`、`Python worker tests`、`Desktop unit tests`），并附上该步骤日志里**第一处**报错前后约 30 行（复制文本即可，不要整份日志）。`test (macos-14)` 的结果最重要：macOS 部分此前从未在真机或 CI 上运行过。

### 打 tag 后（生成草稿 Release）

1. tag 必须与应用版本一致：当前是 **`v3.0.0`**（见 `desktop/package.json` 的 `version`）。只推送这一个 tag：`git push origin v3.0.0`。
   **不要用 `git push --tags`**：仓库里已有 `v2`、`v3` 等旧 tag，它们也会触发发布流程，并在 “Tag matches the app version” 一步失败（这种失败可以忽略，但会造成干扰）。
2. Actions 中应该出现工作流 **Release**，作业：`ffmpeg-windows` → `build (windows-latest)`、`build (macos-14)` → `draft-release`，全部为绿色。
   `build` 作业里有一步 “Smoke test the packaged app”，它启动打包好的应用做冒烟测试，失败要回报。
3. 打开仓库的 **Releases** 页面，应该看到一个标为 **Draft** 的 `LectureFlow 3.0.0`（草稿只有有写权限的人能看到，不会自动发布）。附件应包括：
   - `LectureFlow-Setup-3.0.0-x64.exe`、`LectureFlow-3.0.0-x64-portable.exe`
   - `LectureFlow-3.0.0-arm64.dmg` 与一个 `.zip`
   - 若干 `.blockmap`
   - `size-report-Windows.md/.json`、`size-report-macOS.md/.json`
   Release 正文是两份大小报告。安装包应**不超过 250 MB**；超过时报告里会列出最大的几项，把这部分抄进回报。
4. 不要点 “Publish release”，由负责人决定。

### 可选：开启自动更新

默认构建出来的安装包**不检查更新**（日志里会写 automatic updates are off），这是预期行为。要开启：

1. 仓库 **Settings → Secrets and variables → Actions → Variables**，新建变量 `LECTUREFLOW_UPDATE_REPO`，值为 `owner/仓库名`。
2. 有 Windows 代码签名证书时，可再设 `LECTUREFLOW_PUBLISHER_NAME`（不设则取证书的名称）。
3. 重新打一个**新版本号**的 tag（版本号要先在 `desktop/package.json` 里改好）。之后的草稿 Release 里应多出 `latest.yml`、`latest-mac.yml`。

### English: GitHub CI

**What you need**: a GitHub account with write access to the repository; the local `master` branch pushed to it.

**After a push** (runs on every push)

1. Open the repository's **Actions** tab. You should see two workflows for the push: **CI** and **License audit** (**CLA** runs only on pull requests; its absence is expected).
2. **CI** has three jobs, all green:
   - `ffmpeg-windows`: cross-builds the LGPL ffmpeg for Windows on Linux (slow the first time, 20–40 min; cached afterwards);
   - `test (windows-latest)`: Go vet, Go tests, Python worker tests, desktop unit tests, licence audit;
   - `test (macos-14)`: Go vet, Go tests, a `GOOS=darwin` build, Python tests, desktop unit tests.
3. **License audit**: job `audit` is green (dependency licences and SPDX headers).

**Report any red job**: the job name, the failing step name, and about 30 lines around the **first** error in that step's log (text, not the whole log). `test (macos-14)` matters most: the macOS parts have never run on a Mac or in CI before.

**After a tag** (draft release)

1. The tag must match the app version: **`v3.0.0`** (`version` in `desktop/package.json`). Push only that tag: `git push origin v3.0.0`. **Do not use `git push --tags`**: older tags such as `v2` and `v3` would also start the release workflow and fail at "Tag matches the app version" (harmless, but noisy).
2. Workflow **Release** runs `ffmpeg-windows` → `build (windows-latest)` and `build (macos-14)` → `draft-release`, all green. Report a failure of the step "Smoke test the packaged app".
3. **Releases** shows a **Draft** `LectureFlow 3.0.0` (visible only to people with write access; nothing is published automatically) with the Windows installer and portable `.exe`, `LectureFlow-3.0.0-arm64.dmg` plus a `.zip`, `.blockmap` files, and `size-report-Windows.*` / `size-report-macOS.*`. Installers should be **250 MB or less**; if not, copy the "largest items" part of the size report into your report.
4. Do not click "Publish release"; the maintainer decides.

**Optional: turn on automatic updates.** By default the installers never check for updates (expected). To turn updates on, add the Actions variable `LECTUREFLOW_UPDATE_REPO` = `owner/name` (Settings → Secrets and variables → Actions → Variables), optionally `LECTUREFLOW_PUBLISHER_NAME` for signed Windows builds, then tag a **new** version. The draft release then also contains `latest.yml` and `latest-mac.yml`.

---

## 2. macOS 真机验收（Apple Silicon）

**你需要准备什么**：一台 Apple 芯片（M1 或更新）的 Mac，macOS 12 以上，可用磁盘至少 10 GB，能接电源也能用电池（笔记本）；第 1 部分生成的 `LectureFlow-3.0.0-arm64.dmg`；一段约 10 分钟的英文讲座录音或视频（自己的、或公开授权的，**不要**用含他人隐私的素材）；可选：一个修复与翻译用的模型服务 API key（例如 DeepSeek）。没有 key 也能验收，选“只转写”档位即可。准备一个计时器，记下每步开始和结束的时间。

### 安装与启动

1. **下载 dmg**：从草稿 Release 下载；或在 Release 工作流那次运行的页面底部 **Artifacts** 里下载 `installers-macOS`（zip，解压后得到 dmg）。
   应该看到：文件 `LectureFlow-3.0.0-arm64.dmg`，约 150–250 MB。
2. **安装**：打开 dmg，把 LectureFlow 拖进“应用程序”。
3. **第一次打开**：双击 LectureFlow。安装包未签名，系统会提示无法验证开发者：点 **完成**（不要点“移到废纸篓”），然后按 [docs/install.md](install.md#macos隐私与安全性) 在 **系统设置 → 隐私与安全性** 里点 **仍要打开**。
   应该看到：短暂的启动画面后，打开引导页（第一步“欢迎”）。截图：隐私与安全性里的“仍要打开”。
4. **数据目录**：这台 Mac 没用过 LectureFlow 时，直接用默认目录 `~/Library/Application Support/LectureFlow/`。
   用过、想用新目录时，先退出 LectureFlow，在“终端”里运行：
   `LECTUREFLOW_DATA_DIR="$HOME/lf-acceptance" /Applications/LectureFlow.app/Contents/MacOS/LectureFlow`
   （之后每次都用这条命令启动；验收完删掉 `~/lf-acceptance` 即可。）

### 引导（7 步）

5. **欢迎 / 隐私**：界面语言按系统预选。点“开始”“知道了”。
6. **你的电脑**：等待检测（几秒）。
   应该看到：检测结果里写着 “你的 Mac 使用 Apple M… 芯片……”（英文界面：“Your Mac uses the Apple M… chip…”）；推荐安装的组件包括 **Apple 芯片转写（MLX）**（Apple silicon transcription (MLX)）与一个 MLX 模型（标注“用于 Apple 芯片转写”）；**不应该**出现 CUDA 或 OpenVINO。内存小于 16 GB 时可能推荐较小的模型，并说明原因。截图这一页。记下页面上显示的下载大小与安装后大小。
7. 点 **按推荐安装**（Install suggested）。
8. **翻译与修复**：有 key 时选“标准”并粘贴 key，点“测试连接”，应该看到“连接正常，用时 … 毫秒”；没有 key 时选 **只转写**。点“使用这个档位”。
9. **下载**：下载在进入这一步时开始。
   应该看到：每个组件的进度条与总进度，结束时全部就绪、没有红色错误。记下开始与完成的时间。可以点“在后台继续，先去看看”，但请等下载完成再做下一步。
10. **测试设备**：点 **测试这台设备**（Test this device），它会用应用附带的示例音频（约 1 分钟）。
    应该看到：设备名包含 Apple 芯片 / GPU，结果表格里有 RTF（越小越快）和准确度。记下 RTF。可以点“使用推荐的组合”，也可以保持不变（在回报里写明选了哪个）。
11. **第一门课**：点“添加第一门课”，填课程名（例如 `Acceptance`），其余可以跳过；在最后一步把讲座文件**拖进窗口**或用文件选择按钮添加，然后完成并点“启动”。
    应该看到：讲座进入处理，流水线依次经过预处理、转写（有 key 时还有摘要、修复、翻译）、导出。记下开始与完成的时间。

### 处理完成后

12. **播放器**：在课程页点讲座标题，再点“播放”。
    应该看到：视频 / 音频正常播放，字幕与讲话同步；拖动进度条到 3 个不同位置，字幕仍然对得上；右侧文稿高亮当前句。截图一张（确认画面里没有个人信息）。
13. **资源栏**：看窗口里的资源栏（转写设备一节）。
    应该看到：芯片型号（如 Apple M2）与内存使用情况；**GPU 占用率读不到时显示 `--`**，不应该弹出要求管理员密码的窗口。截图。
14. **电池模式**：拔掉电源，等资源栏的电源状态变成“电池”（On battery）。再添加两段短录音（各 1–2 分钟即可）同时启动。
    应该看到：两节**不会同时**转写，第二节等第一节转写完才开始；鼠标悬停在“ASR 槽位”上，提示里“上限”为 **1**（英文：“Transcription slots: … limit 1”）。转写仍然使用 Apple 芯片，而不是停下来等电源。接回电源后上限恢复。
15. **快捷键与菜单**：
    - 屏幕左上角的应用菜单 **LectureFlow** 里应该有：关于 LectureFlow、设置…（`Cmd+,`）、隐藏、退出（`Cmd+Q`）。逐个点一下：关于显示版本 3.0.0；设置… 打开设置页。
    - `Cmd+C` / `Cmd+V` 能在输入框里复制粘贴；`Cmd+=` / `Cmd+-` / `Cmd+0` 缩放界面。
    - 窗口左上角绿色按钮（或“视图 → 进入全屏幕”、`Ctrl+Cmd+F`）进入**原生全屏**（独占一个桌面空间），再退出。
    - 界面里任何提示快捷键的地方应该写 `Cmd` 而不是 `Ctrl`。
16. **组件大小**：打开 **设置 → 组件**。
    应该看到：已安装的组件、版本与占用空间。记下每个组件的大小。
17. **诊断信息**（有问题时）：**设置 → 应用 → 日志与诊断 → 导出诊断信息**，得到一个 zip（不含讲座内容与 API key）。随回报一起交给负责人。
18. **退出**：应用菜单 → 退出。应该在几秒内完全退出（活动监视器里没有残留的 LectureFlow 或 python 进程）。

**要记录**：每步的耗时；讲座时长与转写用时，RTF = 转写用时 ÷ 音频时长；第 10 步自测的 RTF；各组件大小；点击次数（大概即可）。

### English: macOS acceptance (Apple silicon)

**What you need**: an Apple silicon Mac (M1 or newer, macOS 12+), at least 10 GB free; the `LectureFlow-3.0.0-arm64.dmg` from part 1; an English lecture recording of about 10 minutes (your own or openly licensed; nothing with other people's private data); optionally an API key for a repair/translation service (e.g. DeepSeek). Without a key, choose the "Transcription only" tier. Note the start and end time of each step.

**Install and start**

1. Download the dmg from the draft release, or the `installers-macOS` artifact at the bottom of the Release workflow run. Expect `LectureFlow-3.0.0-arm64.dmg`, about 150–250 MB.
2. Open it and drag LectureFlow to Applications.
3. Open LectureFlow. It is unsigned: click **Done** (not "Move to Bin"), then **System Settings → Privacy & Security → Open Anyway** (see [docs/install.md](install.md#english)). Expect a short splash screen, then the setup guide ("Welcome"). Screenshot the "Open Anyway" step.
4. Data folder: on a Mac that never ran LectureFlow, use the default `~/Library/Application Support/LectureFlow/`. Otherwise quit the app and start it from Terminal with a fresh folder: `LECTUREFLOW_DATA_DIR="$HOME/lf-acceptance" /Applications/LectureFlow.app/Contents/MacOS/LectureFlow` (delete `~/lf-acceptance` afterwards).

**Setup guide**

5. **Welcome / Privacy**: the language follows the system.
6. **Your computer**: expect "Your Mac uses the Apple M… chip…", and the suggested components include **Apple silicon transcription (MLX)** plus an MLX model ("For Apple silicon transcription"). No CUDA or OpenVINO. With less than 16 GB memory a smaller model may be chosen, with a reason. Screenshot; note the download and installed sizes shown.
7. Click **Install suggested**.
8. **Translation**: with a key, choose "Standard", paste the key, "Test connection" should say it works; without one choose **Transcription only**. Apply the tier.
9. **Download**: expect progress per component and no red errors at the end. Note the times. Wait for it to finish.
10. **Test your devices**: click **Test this device** (uses the bundled sample audio, about 1 minute). Expect an Apple chip / GPU device and a result table with RTF (lower is faster) and accuracy. Note the RTF and whether you chose "Use the recommended combination".
11. **First course**: "Add first course", enter a name, skip the rest; at the last step drag the lecture file into the window (or use the file button), finish and start it. Expect the pipeline to run (prep, transcription, and with a key summary, repair, translation), then export. Note the times.

**After processing**

12. **Player**: on the course page click the lecture title, then play. Expect working video/audio, subtitles in sync at three different positions, and the transcript highlighting the current sentence. One screenshot (no personal data in it).
13. **Resource bar**: expect the chip model (e.g. Apple M2) and memory use; GPU utilisation shows `--` when it cannot be read, and **no administrator password prompt**. Screenshot.
14. **Battery**: unplug the power cable and wait until the power status says "On battery". Start two short recordings (1–2 minutes each). Expect only one to transcribe at a time; hovering "Transcription slots" shows "limit 1". Transcription keeps using the Apple chip. Plug in again: the limit goes back up.
15. **Shortcuts and menus**: the **LectureFlow** application menu has About, Settings… (`Cmd+,`), Hide and Quit (`Cmd+Q`); About shows version 3.0.0, Settings… opens settings. `Cmd+C`/`Cmd+V` work in text fields, `Cmd+=`/`Cmd+-`/`Cmd+0` zoom. The green window button (or `Ctrl+Cmd+F`) enters **native full screen** (its own Space) and leaves it. Shortcut hints in the UI say `Cmd`, not `Ctrl`.
16. **Component sizes**: **Settings → Components** lists installed components, versions and sizes. Note them.
17. **Diagnostics** (if something went wrong): **Settings → App → Logs and diagnostics → Export diagnostics** creates a zip (no lecture content, no API keys). Send it to the maintainer with your report.
18. **Quit** from the application menu. The app should exit within a few seconds, with no LectureFlow or python processes left in Activity Monitor.

**Record**: time per step; lecture length and transcription time (RTF = transcription time ÷ audio length); the self-test RTF; component sizes; roughly how many clicks it took.

---

## 3. Colab 验收

**你需要准备什么**：有计算单元余额的付费 Colab 账号（Colab Pro / Pay As You Go）；在这台 Windows 电脑上安装 Google 官方的 gcloud（Google Cloud CLI）；已经能正常使用的 LectureFlow（开发版或 v3 桌面版）；CSC1003 的**一节**讲座。先在 Colab 网页上记下当前的计算单元余额。

### 授权

1. 打开 **设置**，在左侧目录选 **Colab 远端转写**。
   应该看到：顶部一段“录音会上传到 Google 的服务器”的说明，一段“每次借卡最多使用 4 小时”的公平使用说明；“授权”一节写着 **未授权**。
2. 按页面上的第 1 步安装 gcloud（已安装可跳过）。
3. 第 2 步：点 **复制命令**，在终端（PowerShell）粘贴运行页面上显示的命令（以 `gcloud auth application-default login --scopes=` 开头，**以页面为准，不要自己改范围**）。在打开的浏览器页面登录 Google 账号并同意授权。
   应该看到：终端提示凭据已保存。
4. 回到设置页，点 **检测授权**。
   应该看到：“已找到 gcloud 的授权文件，可以导入”。
5. 选择 **授权有效期**：“N 天后失效”（填天数）/“仅本次运行”/“直到我手动断开”。验收建议选 **1 天后失效**。点 **导入授权**。
   应该看到：“已导入授权”，状态变为 **已授权**，并显示“… 到期（剩余 …）”。
6. 在“使用方式”一节打开 **允许把讲座派往 Colab**；**加速器** 选 `T4`（或你账户可用的其他型号）；**付费 / 免费层** 选“付费（推荐）”；其余保持默认，保存。

### 派发一节讲座

7. 在课程页找到 CSC1003 的一节讲座（还没转写的、或可以重新转写的），**右键 → 改用 Colab 处理**，在对话框里点 **改用 Colab**。
   应该看到：提示“已交给 Colab，正在排队”；讲座流水线的转写段带远端标记，悬停显示子步骤 **等卡 → 上传音频 → 远端转写 → 下载结果 → 校验**。
8. 看资源栏的 **远端 GPU（Colab）** 卡片，按顺序应该看到：
   - 状态依次为 **排队等卡 → 开机中 → 运行中**；
   - **使用时限**：“已占用 X / 4 小时”在增长；
   - **当前任务** 与“预计还需 …”；“本次还能再处理约 N 节”；
   - **计算单元**：“余额 …”“本次预计消耗 …”；
   - **授权**：“剩余 …”。
   工作台顶部也应有一行 Colab 状态。处理中截图 2–3 张（开机中、运行中、下载结果）。
9. 等转写完成，讲座继续本机的后续阶段（修复、翻译、导出）。
   应该看到：转写结果完整（打开文稿抽查开头、中间、结尾），播放器里字幕同步。

### 结束与检查

10. 队列里没有其他要派往 Colab 的讲座时，应用会**自动关机**。
    应该看到：卡片状态变为 **关机中 → 已释放**，随后收起为一行“远端 GPU：已释放（队列已清空）”或“远端 GPU：未使用”。
11. 打开 Colab 网页（colab.research.google.com），在 **运行时 → 管理会话**（Runtime → Manage sessions）里查看。
    应该看到：**没有**活动会话。记下新的计算单元余额。
12. （可选）再派发一节，在运行中点资源栏的 **立即关机** 并确认。应该看到：那节讲座回到队列，Colab 会话释放；设置页显示“已暂停自动派往 Colab”。
13. **断开连接**：设置 → Colab 远端转写 → **断开连接**，确认对话框里的三点说明后点 **断开连接**。
    应该看到：“已断开与 Colab 的连接”，状态回到 **未授权**。注意：这会同时让本机 gcloud 的 application-default 登录失效，以后要再用时重新运行第 3 步的命令。

**要记录**：讲座时长；从点击“改用 Colab”到状态变为“运行中”的时间（等卡 + 开机）；远端转写用时；从开始到结果下载完成的总耗时；关机是否自动发生；Colab 网页上是否有残留会话；计算单元消耗（前后余额之差，与资源栏“本次已用约 …”对照）。

---

## 附录（可选）：用官方 google-colab-cli 录制一次借卡

目的：给开发者一份真实交互的样本，用来校准回放测试。**只在你愿意时做**，需要一台 Linux / macOS（或 WSL）。

1. 按官方说明安装 google-colab-cli 与 mitmproxy；用一个专门的、可以随时删除的 gcloud 登录（做完后执行 `gcloud auth application-default revoke`）。
2. 启动 `mitmdump -w colab-raw.flow`，按 mitmproxy 的说明让 CLI 走这个代理（设置 `HTTPS_PROXY` 并信任 mitmproxy 的证书）。
3. 用 CLI 完整借一次卡：申请 T4 → 等待开机 → 上传一个**公共领域的短音频**（例如 LectureFlow 附带的示例音频；不要用课堂录音）→ 运行一条简单命令 → 下载一个文件 → 释放运行时。
4. 停止 mitmdump。**脱敏**后再交给开发者：删除或替换所有 `Authorization` 头、Cookie、`access_token` / `refresh_token` / `id_token`、XSRF 令牌、邮箱和账号 ID；音频内容可以删掉，只保留请求与响应的结构。
5. 把脱敏后的文件私下交给开发者，**不要提交到仓库、不要发到 Issue 或公开渠道**。含令牌的原始抓包 `colab-raw.flow` 用完立即删除，并撤销那次 gcloud 登录。

---

## 回报模板 / Report template

复制下面的内容，填空后发给负责人。没做的步骤写“未做”，不要删掉。
Copy, fill in, and send to the maintainer. Write "not done" for skipped steps instead of deleting them.

```text
# LectureFlow v3 验收回报 / acceptance report
部分 / Part: [1 CI] [2 macOS] [3 Colab]（删去不适用的 / keep what applies）
验收人 / Tester:
日期 / Date:

## 环境 / Environment
- 提交或 tag / Commit or tag:
- 机器与芯片 / Machine and chip:            （例 / e.g. MacBook Air M2, 16 GB）
- 系统版本 / OS version:
- 数据目录 / Data folder:                   默认 / default | LECTUREFLOW_DATA_DIR=...
- 网络 / Network:                           （大致下载速度 / rough download speed）
- 档位 / Tier (macOS):                      标准 / Standard | 只转写 / Transcription only | ...
- Colab 加速器与授权有效期 / accelerator and lifetime:

## 每步结果 / Result per step
| 部分-步骤 / Part-step | 结果 / Result (通过 pass / 失败 fail / 未做 not done) | 用时 / Time | 实际看到 / What you saw (仅在不同时 / only if different) |
|---|---|---|---|
| 1-CI / ffmpeg-windows | | | |
| 1-CI / test (windows-latest) | | | |
| 1-CI / test (macos-14) | | | |
| 1-License audit | | | |
| 1-Release / build (windows-latest) | | | |
| 1-Release / build (macos-14) | | | |
| 1-Release / draft-release | | | |
| 2-1 … 2-18 | | | |
| 3-1 … 3-13 | | | |

## 数字 / Numbers
- 安装包大小 / Installer sizes (Windows exe, portable, dmg):
- 组件大小 / Component sizes:
- 下载总量与用时 / Total download and time:
- 自测 RTF / Self-test RTF:
- 讲座时长 / Lecture length:
- 转写用时与 RTF / Transcription time and RTF:
- 端到端用时 / End-to-end time:
- 点击次数（约）/ Clicks (approx.):
- Colab：等卡+开机 / queue+boot:      远端转写 / remote transcription:      总计 / total:
- Colab：计算单元 前 / 后 / 资源栏显示 / compute units before / after / shown in app:
- Colab：自动关机 / auto shutdown (是 yes / 否 no)；残留会话 / leftover session (有 yes / 无 no)

## 截图清单 / Screenshots
- [ ] 2-3 隐私与安全性“仍要打开” / Privacy & Security "Open Anyway"
- [ ] 2-6 硬件检测与推荐 / hardware detection and suggestion
- [ ] 2-10 设备自测结果 / device self-test result
- [ ] 2-12 播放器 / player
- [ ] 2-13 资源栏（芯片、内存、--）/ resource bar (chip, memory, --)
- [ ] 2-14 电池模式下“上限 1” / "limit 1" on battery
- [ ] 2-15 应用菜单 / application menu
- [ ] 2-16 设置 → 组件 / Settings → Components
- [ ] 3-5 已授权 / Authorized
- [ ] 3-8 远端卡片：开机中、运行中 / remote card: Starting, Running
- [ ] 3-10 已释放 / Released
- [ ] 3-11 Colab 网页无活动会话 / no active session on the Colab page
（确认截图里没有 key、令牌、邮箱或个人信息 / no keys, tokens, e-mail or personal data in screenshots）

## 问题 / Problems
| # | 部分-步骤 / Part-step | 现象 / What happened | 能否重现 / Reproducible | 附件 / Attachments (日志片段 log excerpt, 诊断 zip, 截图 screenshot) |
|---|---|---|---|---|
| 1 | | | | |
```

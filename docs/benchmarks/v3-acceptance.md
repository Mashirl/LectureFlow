# v3 真实验收（V12.2，Windows 本机）

执行：desktop-builder，2026-10-24（文中时间均为 UTC；本机时区 BST = UTC+1）。
结论：**引导 → 按推荐安装 → 标准档位（环境变量中的 DeepSeek key）→ 下载 → 设备自测 → 添加课程 → 处理一节完整讲座 → 播放器观看，全部走通；“导入已有数据”核对全部一致。** 过程中发现 1 个会让引导失败的问题（数据目录路径过长时 `asr-cuda` 安装失败，见问题 1）和若干较小问题（见“问题清单”）。

## 1. 环境与方法

| 项 | 内容 |
|---|---|
| 机器 | Windows 11 Pro for Workstations 10.0.26200；NVIDIA GeForce RTX 5070 Ti Laptop GPU 12 GB（驱动 592.00）；Intel Core Ultra 9 386H（核显 + NPU）；内存 63.4 GB；外接电源 |
| 被测程序 | `desktop/dist/win-unpacked`，17:41:35–17:42:13 用 `node scripts/dist.js --skip-audit` 以当前工作区重新构建，然后**复制**到 scratchpad 的 `acc-v3/app/` 再运行（避免与另一个 agent 的构建互相覆盖）。未设置 `LECTUREFLOW_UPDATE_REPO`，日志确认“automatic updates are off”。没有安装到系统、没写注册表、没建快捷方式 |
| ffmpeg | 打包的是 BtbN 构建（`win64 lgpl shared`，autobuild-2026-10-24，n9.0.2，含 `--enable-libzvbi`，即 GPL-2.0-only 的 zvbi）。本次是内部测试、不分发，照常使用；之后换成自建 LGPL 构建 |
| 数据目录 | `LECTUREFLOW_DATA_DIR`：主流程 `…\lfacc\df`，导入测试 `…\lfacc\di`（`lfacc` 是指向 scratchpad `acc-v3` 的目录联接，原因见问题 1）。打包后的程序在设置了 `LECTUREFLOW_DATA_DIR` 时把 Electron 的 userData 放在 `{数据目录}\electron`，所以 `%APPDATA%\LectureFlow` 没有被创建（验收后确认不存在），不需要 `LECTUREFLOW_TEST_APPDATA` |
| DeepSeek key | 启动脚本用 PowerShell 读用户级 `DEEPSEEK_API_KEY`，只放进应用子进程的环境变量；没有打印、没有写文件或日志，也没有在任何输入框里键入 |
| 讲座 | `D:\Lectures\CSC1003\CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4`（54:47，118.8 MB，与 v2 / I12 验收同一节），只读 |
| 驱动方式 | Playwright `_electron` 无法驱动打包后的程序（fuse 关闭了 `--inspect`，`electronApp.evaluate` 不可用；preload 暴露的 `window.lectureflow` 是冻结对象，也不能在页面里替换）。改为：以 `--remote-debugging-port` 启动打包程序，用 Playwright `chromium.connectOverCDP` 操作窗口。**文件选择没有用测试替身**：大纲、课件、讲座用 CDP `Input.dispatchDragEvent` 把真实文件路径“拖入”窗口（走 preload 的 `webUtils.getPathForFile` → 服务端按路径复制，即 V1.4 的真实路径）；“导入已有数据”的系统文件夹对话框是真实弹出的，用 UI Automation 只对 LectureFlow 进程的对话框发 `WM_SETTEXT`（填路径）和 `BM_CLICK`（“选择文件夹”），不发全局按键 |

## 2. 第一次尝试：长数据目录导致安装失败

第一次按任务书用 `<scratchpad>\acc-v3\data-fresh`（144 个字符）作数据目录：引导前 4 步正常，下载 30 秒后整体失败，`asr-cuda` 报 `components.verify_failed`，详情是 Python 的 `ImportError: DLL load failed while importing frame: The filename or extension is too long.`。原因是 Windows 的 260 字符路径上限：
`{数据目录}\components\asr-cuda\.staging-1.0.0-t1792863891255-1\venv\Lib\site-packages\av.libs\libopencore-amrwb-0-<32 位哈希>.dll` 在这个数据目录下约 286 个字符。默认位置 `%APPDATA%\LectureFlow`（约 41 个字符）不受影响；但数据目录超过约 110 个字符时（例如用户在设置里把数据目录换到一个层级很深的文件夹）就会失败。

处理：关闭应用（正常退出），建目录联接 `C:\Users\<user>\AppData\Local\Temp\lfacc` → scratchpad `acc-v3`（数据实际仍在 scratchpad），用全新目录 `…\lfacc\df`（42 个字符）**从头**重做引导。下面的耗时与点击次数都来自第二次。

## 3. 主流程每步耗时与操作次数

第一次启动 17:47:02.9 → 播放器开始播放 18:02:30.7，**共 15 分 28 秒**（包含自动化脚本本身的等待与两次脚本错误重试，约 2 分钟；机器实际在做事的部分见下表）。
操作次数：**20 次点击 + 3 次拖放 + 2 处键入**（课程名、课程代码）。不测设备、不上传大纲和课件、不填课程代码的最短路径约 **17 次点击 + 1 次拖放 + 1 处键入**（欢迎、隐私、按推荐安装、使用环境变量 key、使用档位、后台继续、跳过自测、添加第一门课、课程名、创建、大纲选“先跳过”、下一步、课件“跳过”、完成、启动、打开讲座、播放）。

| 步骤 | 开始 → 结束（UTC） | 用时 | 操作 | 说明 |
|---|---|---|---|---|
| 启动 → 窗口 | 17:47:02.9 → 17:47:03.3 | 0.4 s | 0 | 服务 0.2 s 就绪，直接进入 `#/onboarding`（启动画面一闪而过，没截到） |
| 1 欢迎 | → 17:47:12.7 | | 1 | 语言按系统预选简体中文；“导入已有数据”在桌面版可见 |
| 2 隐私 | → 17:47:13.5 | | 1 | |
| 3 你的电脑 | → 17:47:16.0 | 2.4 s（检测） | 1 | 检测结果：“NVIDIA 显卡（RTX 5070 Ti Laptop GPU，显存 12 GB），转写会很快”“还有 Intel 神经处理器，用电池时可以改用它转写”。**推荐 = python + asr-cuda + large-v3 float16 + asr-openvino + large-v3-turbo fp16（同时推荐了 asr-cuda 与 asr-openvino）**，共需下载 5.88 GB，安装后约 6.96 GB。点“按推荐安装” |
| 4 翻译与修复 | → 17:47:19.0 | | 2 | 默认选中“标准”；点“使用环境变量中的 key（sk-…69ec）”，自动测试连接“连接正常，用时 764 毫秒”；点“使用这个档位” |
| 5 下载 | 17:47:19 → 17:49:42 | **143 s** | 1 | 进入即开始下载；点“在后台继续，先去看看”。全部走默认源（`suggest_mirror=false`，本机系统语言/时区不在中国大陆），没有失败、没有退回 |
| 6 测试设备 | 17:50:22 → 17:50:36 | 14 s | 2 | 用附带的示例音频（65.5 s）：large-v3 RTF 0.072、WER 0.6%；large-v3-turbo RTF 0.026、WER 0%，推荐 large-v3-turbo（**没有点“使用推荐的组合”**，保持组件提供的 large-v3，与 I12 可比；原因见问题 3）。点“继续” |
| 7 第一门课 + 向导 | 17:51:06 → 17:54:20 | （大纲整理 LLM 约 20 s） | 8 点击 + 2 拖放 + 2 键入 | 课程名 “CSC1003 v3 Acceptance”、代码 CSC1003；拖入 SYLLABUS.MD（1,973 字符）→ 整理为 SYLLABUS.md；拖入 4 份课件 PDF（94 / 65 / 73 / 104 页，pypdf 抽取每份 < 1 s）；术语表 275 条 |
| 上传讲座并启动 | 17:54:10 → 17:54:35 | | 2 点击 + 1 拖放 | 在向导最后一步拖入讲座（以草稿加入）→“完成，打开课程”→“启动” |
| 处理 | 17:54:35 → 18:01:49 | **434 s** | 0 | 分阶段见第 4 节 |
| 观看 | 18:02:17 → 18:02:30 | | 2 | 课程页点讲座标题 → “播放” |

## 4. 讲座处理：各阶段耗时、token 与费用

讲座 54:47（3,287 s），CUDA · large-v3 · auto；所有 LLM 角色都是 DeepSeek / deepseek-flash（标准档位）。处理时段为北京时间凌晨，享半价。

| 阶段 | 用时 | 请求 | 输入 token（缓存命中） | 输出 token | 估算费用 |
|---|---:|---:|---:|---:|---:|
| 预处理 prep | 3.0 s | | | | |
| 转写 asr | 160.6 s（RTF 0.049） | | | | |
| 摘要 summary | 7.4 s | 1 | 19,604（0） | 1,177 | ¥0.024 |
| 修复 repair | 14.2 s | 6 | 91,775（53,504） | 8,392 | ¥0.073 |
| 翻译 translate | **248.6 s** | 248 | 865,887（793,344） | 10,132 | ¥0.129 |
| 导出 export | 0.02 s | | | | |
| 合计（讲座） | **434 s** | 255 | 977,266 | 19,701 | ¥0.226 |

课程准备阶段另有：大纲整理、术语种子、4 份课件术语、术语复核、文件名规则、标题，合计约 14 个请求、约 ¥0.10。
**全部 API 用量**（应用的 usage_daily）：269 个请求，输入 1,040,667 token（缓存命中 850,688，81.7%），输出 30,849 token，估算 ¥0.330。

与 I12（同一节，CUDA large-v3，开发版设置）比：转写 RTF 0.049 对 0.050，相同；端到端 434 s 对 248 s，多出的时间几乎全在翻译——新建的 DeepSeek 服务商是 `max_concurrency 4、rpm 60`（开发版是 8 / 300），248 个翻译请求被每分钟 60 个的限速卡在约 4 分钟（问题 2）。

导出文件（`{数据目录}\courses\01M5QGMXF2YFQ3KZJCHSBGD75D\lectures\01M5QGRXVF2BXSD8J7MAN4W1P8\export\`，实际在 scratchpad `acc-v3\df\…`）：
`W__-01M5QG Perfect Secrecy, One-Time Pad, PRGs, Hash Functions.{bilingual.md, bilingual.vtt, zh.vtt, en.txt, cite.json}`（71.7 / 94.8 / 59.3 / 34.1 / 144.3 KB）。文件名里是 `W__`，因为没有设开课日期，文件名规则也没有识别出录制时间（问题 8）。

## 5. 播放器：字幕与音视频同步

`<video>` 通过主进程注入的令牌正常加载 `/api/lectures/{id}/media`（1366×768，时长 3287.39 s）。跳到三个位置各播放约 2.5 秒，画面下方的双语字幕与该时刻的字幕块（`/cues`，708 块）一致：

| 跳到 | 当前时间 | 显示的字幕 | 对应字幕块 |
|---|---|---|---|
| 742 s | 744.44 s | your scheme that's what that's the guarantees perfect secrecy gives us zero chance / 这就是完善保密性给我们的保证——零机会。 | 739.91–745.15 s，第 66 句 |
| 1800 s | 1802.43 s | So, your key has to be truly random to get perfect secrecy. / 所以，你的密钥必须是真正随机的，才能获得完善保密性。 | 1800.86–1804.38 s，第 140 句 |
| 3000 s | 3001.96 s | and then for the LFSR to stand a chance / 而线性反馈移位寄存器要想有可能达到最大周期， | 3000.88–3004.90 s，第 227 句 |

右侧文稿同步高亮当前句。播放器截图的幻灯片上有讲师姓名，只留在 scratchpad（`acc-v3\shots\24-player-*.png`）。

## 6. 大小

| 项 | 大小 |
|---|---:|
| 安装包 `LectureFlow-Setup-3.0.0-x64.exe` | 180.7 MB（预算 250 MB 以内） |
| 便携版 `LectureFlow-3.0.0-x64-portable.exe` | 180.5 MB |
| 解包后的程序 | 573.7 MB（`resources/bin` 202.4 MB，其中 ffmpeg 的 DLL 约 149 MB、uv 30.6 MB、lectureflow.exe 21.9 MB） |

| 组件 | 下载 | 安装后（清单） | 实际占用 |
|---|---:|---:|---:|
| python 1.0.0 | 25.3 MB | 91 MB | 84 MB |
| asr-cuda 1.0.0 | 1.36 GB | 2.31 GB | 2.3 GB |
| model:faster-whisper:large-v3:float16 | 2.88 GB | 2.88 GB | 2.9 GB |
| asr-openvino 1.0.0 | 98 MB | 167 MB | **305 MB**（清单低估） |
| model:openvino:large-v3-turbo:fp16 | 1.52 GB | 1.52 GB | 1.6 GB |
| 合计 | **5.88 GB**，用时 143 s（约 41 MB/s，全部默认源） | 6.96 GB | 7.0 GB |

各组件完成时间：python 17:47:24，asr-cuda 17:48:24，large-v3 17:49:03，asr-openvino 17:49:16，large-v3-turbo 17:49:41。没有写入全局的 uv 缓存（`UV_CACHE_DIR` 在组件目录内）。

## 7. 导入已有数据

步骤：关闭主流程的应用 → 用另一个全新数据目录 `…\lfacc\di` 启动（这次不传 DeepSeek key）→ 引导第 1 步“导入已有数据”→ 系统文件夹对话框选 `C:\Users\<user>\Documents\LectureFlow_NG\data` → 确认框“开始导入”（共 2 次点击 + 对话框里选文件夹）→ 复制 2.38 GB 约 6 s → 服务以退出码 3 请求重启，外壳 0.4 s 内重新拉起服务并刷新窗口（18:05:10）。正式服务（8420）全程在运行，没有停、没有改。

对比用只读 SQLite 连接（`mode=ro`），导入前对开发版、导入后对开发版与新目录各查一次：

| 项 | 开发版 | 导入后 | 结果 |
|---|---:|---:|---|
| 课程 | 5 | 5 | 一致（逐行 id、名称、代码相同） |
| 讲座 | 16 | 16 | 一致（逐行相同） |
| 句子 | 3,368 | 3,368 | 一致（逐行文本相同） |
| 术语表 | 774 | 774 | 一致（逐行 en、zh、tier、enabled、locked 相同） |
| 每门课术语数 | CSC1003 Acceptance 336 / CSC1003 Cryptography 78 / CSC1003 v2 Acceptance 0 / CSC1003 v3b Intel Acceptance 29 / Cryptography 331 | 同左 | 一致 |
| patch 审阅记录 | 538 条：accepted 12、rejected 2、pending 44、none 480 | 同左（逐行 review、edited_text 相同；按课程分组也相同） | 一致 |
| 课件 / 大纲 / 摘要 / 任务 | 9 / 4 / 16 / 158 | 9 / 4 / 16 / 158 | 一致 |
| 大纲 path | 开发版目录下 | 改写为新目录 | 符合预期 |
| 讲座媒体路径 | | 16/16 指向新目录且文件存在，残留旧前缀 0 | 通过 |
| 数据库版本 | user_version 8 | 9（迁移 009） | 符合预期 |
| 开发版数据库 | 导入前后 md5、修改时间、各项统计都不变 | | 没有被修改 |

播放：导入后打开两节导入的讲座（CSC1003 Acceptance 的 Wed Sep 30 等）；先以音频模式播放，`stream.mp4` 按需生成（0.2 s / 0.9 s）后切换到视频，跳到 600 s 正常播放、字幕正确。导入的实例只运行了这两个 stream 任务，没有 API 请求。

## 8. DeepSeek 余额与花费

| 时间 | 余额 | 来源 |
|---|---:|---|
| 17:41:25（验收前） | ¥381.33 | 正式服务 `/api/resources`（只读） |
| 18:01:50（处理刚结束） | ¥381.09 | 验收应用 `/api/resources` |
| 18:06:35（验收后） | ¥381.00 | 正式服务 `/api/resources`（只读） |

花费 **¥0.33**（余额差，与应用估算的 ¥0.330 一致；期间开发版的用量没有变化）。余额远高于 200 元的停止线。真实 GPU 与 DeepSeek 只用于 CSC1003 的这一节讲座。

## 9. 问题清单

desktop/ 下没有改动（按派发要求）；以下都需要 lead 转交。

| # | 级别 | 问题 | 建议 | 归属 |
|---|---|---|---|---|
| 1 | 高 | **数据目录路径超过约 110 个字符时 `asr-cuda` 安装失败**（`DLL load failed … filename or extension is too long`），界面只显示 `components.verify_failed` 和一段 Python 堆栈。暂存目录名 `.staging-1.0.0-t1792863891255-1` 又加了 30 多个字符 | 暂存目录用短名；`Plan` / 更改数据目录的 `CheckMove` 预先检查“数据目录 + 组件内最长相对路径”是否超过 260，超过时给出可理解的错误码与说明（例如建议换一个更短的文件夹） | components-builder（`internal/components`）、core-builder（`internal/relocate`） |
| 2 | 中 | 引导“使用环境变量中的 key”创建的 DeepSeek 服务商是 `max_concurrency 4、rpm 60`（清单的 DeepSeek 预设没有给值，用了通用默认），翻译 248 个请求被限速在约 4 分钟，端到端 434 s，比 I12 的 248 s 慢 75% | 在清单 presets 的 DeepSeek 条目写上 8 / 300（与开发版一致） | components-builder（`packaging/components/manifest.src.json`） |
| 3 | 中 | 设备自测在 CUDA 上测了 `large-v3-turbo`（faster-whisper），这个模型**没有对应的已安装组件**；worker 按名字从全局 Hugging Face 缓存（`%USERPROFILE%\.cache\huggingface`，开发时下载过）加载。真正的新电脑上会绕过组件管理器与镜像设置，从 huggingface.co 下载约 1.6 GB 到数据目录以外；点“使用推荐的组合”后讲座转写也会依赖它 | 自测只测已安装的模型组件，或在测之前提示并通过组件管理器安装 | core-builder / worker-builder / components-builder |
| 4 | 中 | 引导第 6 步的设备列表只在第一个转写后端装好时加载一次：`web/onboarding_download.js` 的 `schedule()` 在 `S.ready` 为真后不再轮询（`else if (!S.ready && S.downloading)`），所以后装好的核显、NPU 不出现，要退回再进入这一步才看得到 | 下载任务仍在进行时继续轮询 | web-builder |
| 5 | 中 | “导入已有数据”重启后：已有课程，所以引导不再自动打开；首次运行检查把用户带到“诊断”页，显示 3 项红色“需要处理”（Python、faster-whisper、pypdf），提示是开发版的“按 README 在项目目录创建 .venv”“在 config.toml 中设置 python 的路径”，路径是 `resources\.venv\Scripts\python.exe`。导入的新安装还没有任何组件，用户不知道该去下载组件 | 导入后继续引导（从“隐私”或“你的电脑”开始）；桌面模式下诊断页的 Python 检查改为指向组件安装 | web-builder（`web/app.js`、引导）、`internal/api/system.go` |
| 6 | 低 | 导入进度显示“已下载 555.2 MB / 2.38 GB”，实际是复制 | 改为“已复制” | web-builder（文案） |
| 7 | 低 | 设备自测期间没有阻止系统休眠（desktop.log 在 17:50:22–36 没有 “preventing system sleep”）；自测通常很短，影响小 | `/api/app/activity` 把自测算作转写 | web-builder / core-builder |
| 8 | 低 | 文件名规则：LLM 生成的正则没有考虑文件名里的星期（`… Wed Oct 07 09-35-00 BST 2026`），0/1 匹配被拒，讲座显示“未确定时间”，导出文件名为 `W__-…` | — | pipeline-builder |
| 9 | 低 | 讲座目录里 `stream.mp4` 与 `media.mp4` 大小相同（118.8 MB），是完整复制 | 能直接播放时用硬链接或直接引用 media | pipeline-builder |
| 10 | 低 | `asr-openvino` 实际占用 305 MB，清单写 167 MB | 更新 install_bytes | components-builder |
| 11 | 说明 | 打包程序接受 Chromium 的 `--remote-debugging-port`（fuse 管不到），同一用户的任何进程都能以此启动 LectureFlow 并通过 CDP 控制窗口、看到注入的令牌请求头。本次验收正是靠它驱动打包程序的 | 是否在打包版中拒绝该开关（例如检测到就退出），由 lead 决定；若拒绝，需要为验收另留一个只在测试构建中可用的入口 | desktop-builder（lead 裁决） |
| 12 | 说明 | 启动非常快（服务 0.2–0.4 s 就绪），启动画面几乎看不到；没有截到 | — | — |

## 10. 文件位置

- 截图（不含个人信息）：`docs/screenshots/v3/acceptance/`（引导 1–7 步、向导、课程页、导入入口，14 张）。含讲师姓名、或含本机用户目录路径的截图只在 scratchpad `acc-v3\shots\`（大纲整理、播放器、导入确认与诊断页等）。
- 验收数据（约 11 GB，含组件与模型）：scratchpad `acc-v3\`：`df\`（主流程）、`di\`（导入测试）、`data-fresh\`（第一次失败的尝试）、`app\`（被测程序副本）、`timeline.jsonl`、`clicks-*.json`、`download*.jsonl`。
- 目录联接 `C:\Users\<user>\AppData\Local\Temp\lfacc` 已在验收后删除（`rmdir` 只删联接本身，数据仍在 scratchpad）。`df`、`di` 数据库中的路径以 `…\lfacc\` 开头，若要再用应用打开它们，先执行 `mklink /J C:\Users\<user>\AppData\Local\Temp\lfacc <scratchpad>\acc-v3`。
- 驱动脚本：scratchpad `acc-v3\drv\`（`launch.js`、`drv.js`、`dropfiles.js`、`pickdialog.ps1` 等）。

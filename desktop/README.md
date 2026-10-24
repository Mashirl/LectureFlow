# LectureFlow 桌面外壳（Electron）

`desktop/` 是 LectureFlow 的 Electron 外壳（SPEC-v3 V1、V2）：拉起并守护 Go 服务，把窗口指向 `http://127.0.0.1:{随机端口}/`，提供文件选择、通知、阻止休眠等原生能力。业务逻辑、组件管理器都在 Go 服务里，这里只有外壳。

## 准备

只用项目内的便携 Node（`tools/node/`，不在 PATH 里），不全局安装任何东西。

```bash
# Git Bash，在仓库根目录
export PATH="$PWD/tools/node:$PATH"
cd desktop
npm ci                                   # 依赖按 package-lock.json 安装
node node_modules/electron/install.js    # Electron 44 首次使用前下载自身的二进制（也会在第一次 require 时自动下载）
```

PowerShell：`$env:Path = "$PWD\tools\node;$env:Path"`，其余相同。

## 开发启动

先构建 Go 服务（在仓库根目录）：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File make.ps1 build   # 生成 lectureflow.exe
```

然后在 `desktop/` 里：

```bash
# 建议给开发用一个单独的数据目录，免得写进 %APPDATA%\LectureFlow
export LECTUREFLOW_DATA_DIR="$PWD/../data-desktop"
npm start
```

| 环境变量 | 作用 | 缺省 |
|---|---|---|
| `LECTUREFLOW_DATA_DIR` | 数据目录（优先于 `location.json` 与默认位置；打包后也生效，供验收用） | `%APPDATA%\LectureFlow\`（macOS：`~/Library/Application Support/LectureFlow/`），或其中 `location.json` 指向的目录 |
| `LECTUREFLOW_SERVER` | 服务程序路径；以 `.js` 结尾时用 Electron 自带的 Node 运行（测试用的假服务） | 仓库根目录的 `lectureflow(.exe)` |
| `LECTUREFLOW_RESOURCES` | 资源目录，传给服务的 `-resources`（`worker/`、`bin/ffmpeg` 等） | 仓库根目录 |

`LECTUREFLOW_SERVER`、`LECTUREFLOW_RESOURCES` 只在未打包时有效；打包后固定用 `resources/bin/lectureflow(.exe)` 与 `resources/`。另有两个只给测试用、同样只在未打包时有效的变量：`LECTUREFLOW_TEST_APPDATA`（替换系统的 appData，用来测试 `location.json`）、`LECTUREFLOW_FAKE_UPDATER`（用一个模块代替 electron-updater）。

服务以桌面模式启动：`-desktop -listen 127.0.0.1:PORT -data-dir … -resources … -app-version … -config {数据目录}/config.toml`，数据目录不是来自 `LECTUREFLOW_DATA_DIR` 时另加 `-location-file`。访问令牌写在服务的标准输入第一行，不出现在命令行、日志或磁盘上。高级用户可以把 `config.toml` 放进数据目录；不存在时用默认值。

开发模式下有隐藏菜单里的快捷键：Ctrl+R 重新加载、Ctrl+Shift+I 开发者工具。打包后没有开发者工具。

窗口的会话是持久化的（`persist:lectureflow`，在 userData 即 `{默认数据根目录}/electron/` 下，HTTP 缓存关闭），上次成功使用的端口记在 userData 的 `port.json`，下次优先使用，所以页面的 localStorage 偏好会保留。令牌只在内存里，只经请求头注入，端到端测试会扫描数据目录下的全部文件确认没有令牌。

日志：外壳写 `{数据目录}/logs/desktop.log`，服务写同目录的 `lectureflow.log`；错误页的“查看日志”打开这个目录。

## 测试

```bash
npm test            # 单元测试（node:test）：判定函数、参数校验、数据目录、服务进程管理（用假服务）
npm run test:e2e    # Playwright 端到端（_electron）
```

端到端测试有三个 project：

- `fake`：`test/fixtures/fake-server.js`，按 v3-brief §1–§2 的契约模拟服务，可以让它启动失败、端口被占、以退出码 3 请求重启、崩溃、报告有任务在运行。全部用例都在这里跑。
- `go-build`：`go build -o desktop/.e2e-tmp/bin/lectureflow.exe ./cmd/lectureflow`（需要 PATH 里有 go）。
- `go`：用真实服务跑 `shell.spec.js`（窗口加载、白名单、导航拦截、CSP、无令牌 401、Host 不符 403）。

只跑一部分：`npx playwright test --project=fake`。每个用例使用自己的临时数据目录（`LECTUREFLOW_DATA_DIR`），所以单实例锁互不影响，也不碰真实数据。

## 目录

```
src/main/main.js         入口：单实例、userData、菜单、退出流程
src/main/shell.js        控制器：窗口、服务生命周期、关窗、托盘、阻止休眠
src/main/server.js       服务进程：端口、令牌、就绪检测、端口重试、优雅停止
src/main/websecurity.js  会话与 webContents 的安全设置（令牌注入、CSP、导航拦截、权限）
src/main/ipc.js          preload 白名单背后的 IPC 处理（逐项校验参数与发送方）
src/main/updates.js      应用更新（electron-updater ↔ Go 的 /api/app/update 中转）
src/main/lib/            纯函数（可单测）：security、validate、datadir、policy、serverutil、i18n、menu、pngicon、portmemo、location
scripts/                 构建：stage、dist（`--out DIR` 输出到别的目录）、smoke（`--dist DIR`）、build-ffmpeg-windows.sh、build-ffmpeg-macos.sh，lib/（固定版本下载、签名、大小报告）
packaging/               third-party.json（固定版本的第三方程序）、entitlements.mac.plist
src/preload.js           contextBridge：window.lectureflow（应用页）、window.lectureflowShell（启动/错误页）
src/splash.html, .js     启动画面与错误页（本地文件）
locales/                 外壳自己的文案（zh-CN、en）
test/unit, test/e2e      测试
```

## 构建安装包

```bash
npm run dist        # 在 Windows 上：NSIS 安装包 + 便携版；在 macOS 上：arm64 dmg（+ 更新用的 zip）
node scripts/smoke.js   # 启动打包后的程序，确认服务起来、无令牌请求被拒（401），关窗后正常退出
```

需要 PATH 里有 go，第一次构建需要联网（下载 Electron 打包工具，以及下面的固定版本第三方程序）。`scripts/dist.js` 依次：

1. 生成占位图标 `build/icon.png`；
2. `scripts/stage.js` 把随程序分发的文件放进 `build/resources/`（electron-builder 的 extraResources）：`bin/lectureflow(.exe)`（`go build -trimpath -ldflags "-s -w -X main.version=<版本>"`）、`bin/ffmpeg`、`bin/ffprobe`（只含 LGPL 部分的精简构建，两个平台都从固定的 ffmpeg-9.0.2.tar.xz 构建：Windows 由 `scripts/build-ffmpeg-windows.sh` 在 Linux 上用 mingw-w64 交叉编译，外部库只有 OpenH264、libvpl（Quick Sync）、NVENC 头文件与 zlib，本机没有现成产物时 stage 经 Docker 自动构建，产物在 `.cache/ffmpeg-windows-<版本>/`；macOS 由 `scripts/build-ffmpeg-macos.sh` 构建）、`bin/uv`、`worker/` 的运行脚本（不含测试）、`components.json` 与 `locks/`、`sample/`、`LICENSE`、`THIRD_PARTY_NOTICES.md`、`licenses/`（ffmpeg、uv 的许可证原文和 ffmpeg 的 configure 参数）。第三方程序的地址与 SHA-256 固定在 `packaging/third-party.json`，下载后校验，缓存在 `.cache/`；ffmpeg 的 configure 参数含 `--enable-gpl`、`--enable-nonfree` 或 `--enable-version3`、`ffmpeg -L` 不是 LGPL、或 `packaging/licenses/binaries.json` 里 ffmpeg 的 bundled 有 GPL 许可的库时构建失败；
3. `go run ./scripts/licaudit -check`，有违规时构建失败；
4. electron-builder（配置在 `electron-builder.config.js`，Electron fuses 在打包时写入）；
5. 更新配置检查：没有 `LECTUREFLOW_UPDATE_REPO` 时不得有 `app-update.yml` / `latest*.yml`，有时必须指向该仓库，不一致即失败；
6. 大小报告 `dist/size-report.md` / `.json`：安装包大小（预算 250 MB，超出只报告不失败）、各项资源、app.asar、解包后最大的十个文件。

签名只从环境变量读取，没有就构建未签名版本（见 `electron-builder.config.js` 开头与 `docs/install.md`）：Windows `CSC_LINK`/`CSC_KEY_PASSWORD`（或 `WIN_CSC_LINK`）；macOS `CSC_LINK`/`CSC_KEY_PASSWORD` 或 `CSC_NAME`，公证另需 `APPLE_API_KEY`+`APPLE_API_KEY_ID`+`APPLE_API_ISSUER`（或 `APPLE_ID`+`APPLE_APP_SPECIFIC_PASSWORD`+`APPLE_TEAM_ID`）。macOS 未签名时用临时签名（ad-hoc），Apple 芯片只运行签过名的代码。

更新：electron-updater，GitHub Releases。**更新仓库只从构建环境变量 `LECTUREFLOW_UPDATE_REPO`（`owner/name`，严格校验，格式不对构建失败）读取**；未设置时 publish 为 null，不生成 `app-update.yml` 与 `latest*.yml`，打包后的主进程不加载更新器（`/api/app/update` 保持 none，界面不显示更新），构建日志、大小报告与 desktop.log 都写明“自动更新已关闭”。有 Windows 签名证书时，`LECTUREFLOW_PUBLISHER_NAME` 可指定更新包必须具有的发布者名称（缺省取证书的通用名称），构建后检查 `app-update.yml` 带有 publisherName。设置了仓库时主进程启动时和每 6 小时检查，渠道取 `/api/app/settings` 的 update_channel；不自动下载，经 Go 的 `/api/app/update*` 中转给网页；只在退出流程里、服务停止之后、且用户选了“退出时安装”时安装。

持续集成：`.github/workflows/ci.yml`（Windows 与 macOS：go vet/test、pytest、desktop 单元测试、许可证审计）与 `release.yml`（打 tag 时两个平台构建、冒烟测试、创建草稿 Release）。

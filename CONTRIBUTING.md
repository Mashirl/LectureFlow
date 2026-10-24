# Contributing to LectureFlow / 参与贡献

[English](#english) · [简体中文](#简体中文)

## English

LectureFlow is free software under the **GNU Affero General Public License
v3.0 or later** (`LICENSE`), with an additional permission under section 7
of that license (`LICENSE-EXCEPTION.md`). Contributions are welcome.

### License of contributions

Contributions are accepted under the GNU AGPL v3.0 or later **together
with the additional permission in [LICENSE-EXCEPTION.md](LICENSE-EXCEPTION.md)**.
That permission allows LectureFlow to be combined with the libraries it
lists (NVIDIA CUDA libraries, CTranslate2, PyAV, regex), which the
component manager downloads from PyPI on the user's computer and whose
licenses are not compatible with the AGPL. By submitting a contribution
you agree that it may be used under both. The permission does not change
the license of those libraries, and LectureFlow does not distribute them.

### Contributor License Agreement (CLA)

Before a pull request from an external contributor can be merged, its
author must sign the [LectureFlow Individual CLA](CLA.md). You keep the
copyright in your work; the CLA lets the project maintainers keep
distributing it under the AGPL and, if needed, also offer LectureFlow to
institutions under other terms (dual licensing). Every accepted
contribution stays available under the AGPL (CLA §5).

**How to sign:** open your pull request. The CLA check posts a comment
asking you to sign. Read [CLA.md](CLA.md), then reply in the pull request
with exactly this sentence:

    I have read the LectureFlow CLA Document and I hereby sign the CLA

The check records your GitHub user name, the pull request and the date in
`signatures/version1/cla.json` on the `cla-signatures` branch of this
repository, and turns green. You sign once; later pull requests are
recognised automatically. If the check does not update, comment `recheck`.
Pull requests whose authors have not signed are not merged.

### Pull requests

1. Open an issue first for larger changes, so that we can agree on the
   approach.
2. Keep each pull request to one topic. Run the tests:
   `powershell -File make.ps1 test` (Windows) or `make test`.
3. Every new source file starts with the two-line license header used
   throughout the repository:

       // SPDX-License-Identifier: AGPL-3.0-or-later
       // Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

   (use the comment syntax of the language). CI checks it. Python files
   of `worker/` (except tests) carry a third line:

       # Additional permission under AGPL-3.0 section 7: see LICENSE-EXCEPTION.md
4. New third-party dependencies must have a license compatible with
   AGPL-3.0 (MIT, BSD, Apache-2.0, MPL-2.0, LGPL, GPL-3.0, ...). Run
   `go run ./scripts/licaudit` (or `make.ps1 audit`) and commit the updated
   `THIRD_PARTY_NOTICES.md`. CI fails on incompatible or unknown licenses.
5. Never commit API keys, personal data or lecture recordings.

## 简体中文

LectureFlow 是自由软件，以 **GNU AGPL-3.0-or-later** 许可发布（见 `LICENSE`），并附有依据该许可证第 7 条的附加许可（见 `LICENSE-EXCEPTION.md`）。欢迎参与贡献。

### 贡献的许可

贡献以 GNU AGPL-3.0-or-later **连同 [LICENSE-EXCEPTION.md](LICENSE-EXCEPTION.md) 中的附加许可**接受。
该附加许可允许 LectureFlow 与其中列出的库（NVIDIA CUDA 库、CTranslate2、PyAV、regex）组合使用；这些库由组件管理器在用户电脑上从 PyPI 下载，其许可证与 AGPL 不兼容。
提交贡献即表示你同意贡献按这两者使用。附加许可不改变这些库的许可证，LectureFlow 也不分发这些库。
（附加许可以 LICENSE-EXCEPTION.md 的英文文本为准。）

### 贡献者许可协议（CLA）

外部贡献者的拉取请求（PR）在合并之前，作者需要签署 [LectureFlow 个人贡献者许可协议](CLA.md)（英文）。
你的贡献的著作权仍归你所有；协议允许项目维护者继续以 AGPL 发布，并在需要时以其他条款向机构提供 LectureFlow（双重许可）。
所有被接受的贡献都始终以 AGPL 公开提供（协议第 5 条）。

**签署方法**：提交 PR 后，CLA 检查会留言请你签署。阅读 [CLA.md](CLA.md) 之后，在该 PR 中回复下面这句话（必须一字不差）：

    I have read the LectureFlow CLA Document and I hereby sign the CLA

检查会把你的 GitHub 用户名、PR 编号与日期记录在本仓库 `cla-signatures` 分支的 `signatures/version1/cla.json` 中，并变为通过。
只需签署一次，以后的 PR 会自动识别。检查没有更新时，回复 `recheck`。未签署的 PR 不会合并。

### PR 流程

1. 较大的改动请先开 issue 讨论做法。
2. 每个 PR 只做一件事，提交前运行测试：Windows 上 `powershell -File make.ps1 test`，其他系统 `make test`。
3. 每个新源文件开头加上仓库统一的两行许可证声明（见上文英文部分，注释符号按语言替换），持续集成会检查。
   `worker/` 中的 Python 文件（测试除外）还有第三行，引用 LICENSE-EXCEPTION.md（见上文英文部分）。
4. 新增的第三方依赖必须使用与 AGPL-3.0 兼容的许可证（MIT、BSD、Apache-2.0、MPL-2.0、LGPL、GPL-3.0 等）。
   运行 `go run ./scripts/licaudit`（或 `make.ps1 audit`），并提交更新后的 `THIRD_PARTY_NOTICES.md`；
   不兼容或无法识别的许可证会让持续集成失败。
5. 不要提交 API key、个人信息或讲座录音。

# Additional permission under GNU AGPL version 3 section 7

[English](#english) · [简体中文（仅供参考）](#简体中文仅供参考)

## English

The English text of this section is the authoritative text of the
additional permission.

LectureFlow is free software: you can redistribute it and/or modify it
under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or (at
your option) any later version (see `LICENSE`). In addition, the
copyright holders of LectureFlow grant the following additional
permission under section 7 of the GNU Affero General Public License.

### Additional permission

If you modify this Program, or any covered work, by linking or combining
it with any of the Libraries listed below (or a modified version of any
of them, or a later version of any of them), containing parts covered by
the terms of the licenses of those Libraries, the licensors of this
Program grant you additional permission to convey the resulting work.
Corresponding Source for a non-source form of such a combination shall
include the source code for the parts of this Program used as well as
that of the covered work, but need not include the source code of the
Libraries.

The licensors of this Program also grant you additional permission:

1. to convey this Program, or a covered work, in non-source form without
   the Libraries, together with means that obtain them on the
   recipient's computer, such as LectureFlow's component manager and the
   component manifests and lock files that name, locate and verify the
   Libraries; and
2. to run a combination described above, including a modified version
   that users interact with remotely through a computer network.

In both cases, the Corresponding Source that the License requires you to
provide or to offer (sections 6 and 13) shall include the source code
for the parts of this Program used as well as that of the covered work,
but need not include the source code of the Libraries.

"This Program" means LectureFlow as a whole: every part of LectureFlow
that its copyright holders license under the GNU Affero General Public
License, including the server, the web front end, the desktop shell, the
component manager and the Python workers.

### Limits of this permission

- It covers only linking or combining with the Libraries listed below.
  It does not extend to any other library or work.
- It does not relax any other term or condition of the License. Except
  for the source code of the Libraries, everything the License requires
  still applies, in particular the conditions for conveying a covered
  work (sections 4 to 6), including providing its Corresponding Source,
  and offering the Corresponding Source to all users who interact with a
  modified version remotely through a computer network (section 13).
- It grants no rights in the Libraries. Each Library remains under its
  own license, and whoever conveys a Library must comply with that
  license.

### The Libraries

Each Library is named by its package on the Python Package Index
(PyPI, https://pypi.org/). A Library includes the libraries bundled in
that package as listed.

1. NVIDIA CUDA libraries published by NVIDIA on PyPI:
   - `nvidia-cublas-cu12`: NVIDIA cuBLAS.
   - `nvidia-cuda-nvrtc-cu12`: NVIDIA CUDA Runtime Compilation (NVRTC).
   - `nvidia-cudnn-cu12`: NVIDIA cuDNN.
2. `ctranslate2`: the CTranslate2 binary package, including the Intel
   OpenMP runtime library (libiomp5) and the NVIDIA cuDNN library bundled
   in it.
3. `av`: the PyAV binary package, including the FFmpeg libraries and the
   OpenSSL libraries bundled in it.
4. `regex`: the regex package, including its parts covered by the CNRI
   Python license (CNRI-Python).

### Removing this permission

When you convey a copy of a covered work, you may at your option remove
this additional permission from that copy, or from any part of it, as
section 7 of the GNU Affero General Public License provides. If you
modify LectureFlow, you may extend this additional permission to your
version, but you are not obligated to do so. If you do not wish to do so,
delete this file and the lines that refer to it from your version.

### Notes (not part of the permission)

- LectureFlow does not distribute the Libraries. Its component manager
  downloads them on the user's computer from PyPI (or a mirror the user
  chose), pinned by the component lock files. They are listed with their
  licenses in section 7 of `THIRD_PARTY_NOTICES.md`. The project's
  license audit fails if a package downloaded this way, whose license is
  not compatible with the AGPL, is not listed in this file.
- The Libraries are loaded by LectureFlow's transcription workers
  (`worker/`), which run in their own process; the headers of those
  source files point to this file. The permission covers LectureFlow as
  a whole, so that it does not depend on whether the workers and the
  rest of LectureFlow form a single work.
- The operating system, its frameworks and the GPU driver (which
  provides, for example, the CUDA driver library) are not listed:
  LectureFlow does not download them, and they are left to the
  provisions of the License on System Libraries (section 1).
- This file is not legal advice.

## 简体中文（仅供参考）

本节中文说明仅供参考，不具法律效力；附加许可以上面的英文文本为准。

LectureFlow 是自由软件，你可以按自由软件基金会发布的 GNU Affero 通用公共许可证第 3 版或（由你选择）任何更高版本的条款再发布和修改它（见 `LICENSE`）。
此外，LectureFlow 的著作权人依据该许可证第 7 条授予下述附加许可。

### 附加许可的内容

如果你修改本程序或任何受保护作品（covered work），使其与下列任一“库”（或其修改版、更新版本）链接或组合，而这些库含有受其各自许可证条款约束的部分，那么本程序的许可人授予你传递（convey）由此得到的作品的附加许可。
这种组合的非源码形式所对应的源码（Corresponding Source），须包括所用到的本程序部分以及受保护作品的源码，但不必包括这些库的源码。

本程序的许可人还授予你以下附加许可：

1. 以非源码形式传递本程序或受保护作品，不附带这些库，而是附带在接收者电脑上获取这些库的手段，例如 LectureFlow 的组件管理器，以及指明、定位和校验这些库的组件清单与锁定文件；
2. 运行上述组合，包括供用户通过计算机网络远程交互的修改版。

在这两种情况下，许可证要求你提供或提供获取机会的对应源码（第 6 条与第 13 条），须包括所用到的本程序部分以及受保护作品的源码，但不必包括这些库的源码。

“本程序”指 LectureFlow 整体：LectureFlow 的著作权人以 GNU AGPL 许可的每一部分，包括服务端、网页前端、桌面外壳、组件管理器和 Python worker。

### 本附加许可的限度

- 它只涵盖与下列库的链接或组合，不延及任何其他库或作品。
- 它不放宽许可证的任何其他条款或条件。除这些库的源码之外，许可证的各项要求照常适用，尤其是：传递受保护作品的各项条件（第 4 至 6 条），包括提供其对应源码；修改版通过计算机网络与用户远程交互时，须向所有这些用户提供获取对应源码的机会（第 13 条）。
- 它不授予与这些库有关的任何权利。每个库仍受其自身许可证约束，传递这些库的人须遵守相应的许可证。

### 被许可组合的库

每个库以其在 Python 软件包索引（PyPI）上的包名标识，并包括下列说明中随该包附带的库：

1. NVIDIA 在 PyPI 上发布的 CUDA 库：
   - `nvidia-cublas-cu12`：NVIDIA cuBLAS。
   - `nvidia-cuda-nvrtc-cu12`：NVIDIA CUDA 运行时编译库（NVRTC）。
   - `nvidia-cudnn-cu12`：NVIDIA cuDNN。
2. `ctranslate2`：CTranslate2 二进制包，包括其中附带的 Intel OpenMP 运行库（libiomp5）与 NVIDIA cuDNN 库。
3. `av`：PyAV 二进制包，包括其中附带的 FFmpeg 库与 OpenSSL 库。
4. `regex`：regex 包，包括其中受 CNRI Python 许可证（CNRI-Python）约束的部分。

### 删除本附加许可

依照 GNU AGPL 第 7 条，你在传递受保护作品的副本时，可以自行决定从该副本或其任何部分中删除本附加许可。
如果你修改 LectureFlow，可以把本附加许可延伸到你的版本，但没有义务这样做；不愿这样做时，请从你的版本中删除本文件以及引用本文件的那些行。

### 说明（不属于附加许可的内容）

- LectureFlow 不分发这些库。组件管理器按组件的锁定文件，在用户电脑上从 PyPI（或用户选择的镜像）下载它们。它们连同各自的许可证列在 `THIRD_PARTY_NOTICES.md` 第 7 节。以这种方式下载、许可证与 AGPL 不兼容的包如果没有列在本文件中，项目的许可证审计会失败。
- 加载这些库的是 LectureFlow 的转写 worker（`worker/`），它们在独立进程中运行，这些源文件的文件头指向本文件。附加许可涵盖 LectureFlow 整体，这样它的效力不取决于 worker 与 LectureFlow 的其余部分是否构成同一个作品。
- 操作系统、系统框架与显卡驱动（例如其中提供的 CUDA 驱动库）没有列出：LectureFlow 不下载它们，它们按许可证关于“系统库”（System Libraries，第 1 条）的规定处理。
- 本文件不构成法律意见。

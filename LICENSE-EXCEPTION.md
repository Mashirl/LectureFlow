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

"This Program" means LectureFlow as a whole: every part of LectureFlow
that its copyright holders license under the GNU Affero General Public
License, including the server, the web front end, the desktop shell and
the Python workers.

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

- This permission does not change the license of any Library. Each
  Library stays under its own license, and whoever conveys a Library must
  comply with that license.
- LectureFlow does not distribute the Libraries. Its component manager
  downloads them on the user's computer from PyPI (or a mirror the user
  chose). They are listed with their licenses in section 7 of
  `THIRD_PARTY_NOTICES.md`.
- This file is not legal advice.

## 简体中文（仅供参考）

本节中文说明仅供参考，不具法律效力；附加许可以上面的英文文本为准。

LectureFlow 是自由软件，你可以按自由软件基金会发布的 GNU Affero 通用公共许可证第 3 版或（由你选择）任何更高版本的条款再发布和修改它（见 `LICENSE`）。
此外，LectureFlow 的著作权人依据该许可证第 7 条授予下述附加许可。

### 附加许可的内容

如果你修改本程序或任何受保护作品（covered work），使其与下列任一“库”（或其修改版、更新版本）链接或组合，而这些库含有受其各自许可证条款约束的部分，那么本程序的许可人授予你传递（convey）由此得到的作品的附加许可。
这种组合的非源码形式所对应的源码（Corresponding Source），须包括所用到的本程序部分以及受保护作品的源码，但不必包括这些库的源码。

“本程序”指 LectureFlow 整体：LectureFlow 的著作权人以 GNU AGPL 许可的每一部分，包括服务端、网页前端、桌面外壳和 Python worker。

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

- 本附加许可不改变任何库的许可证。每个库仍受其自身许可证约束，传递这些库的人须遵守相应的许可证。
- LectureFlow 不分发这些库。它们由组件管理器在用户电脑上从 PyPI（或用户选择的镜像）下载，连同各自的许可证列在 `THIRD_PARTY_NOTICES.md` 第 7 节。
- 本文件不构成法律意见。

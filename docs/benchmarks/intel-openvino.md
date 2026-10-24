# Intel 核显 / NPU 上的 Whisper 转写基准测试（OpenVINO）

SPEC-v2 第三部分 I1–I5 的调研结果。原始数据在 `spikes/openvino/results/`，机器可读的推荐配置在 `spikes/openvino/recommendation.json`，复现方法见文末。

**一句话结论**：本机的 Intel 核显和 NPU 都能用 OpenVINO 跑 Whisper large-v3-turbo，并且能输出词级时间戳：核显 RTF 0.034（10 分钟音频约 20 秒）、NPU RTF 0.071，相对修复后文本的 WER 为 5.7% 和 6.1%，与产品现在的 CUDA large-v2 + 提示词（6.2%）处在同一水平。缺少的是词级置信度、幻觉过滤所需的片段概率和 VAD，按 I7 的方式补齐即可，**值得合入**；NPU 首次编译约 4 分钟，必须使用编译缓存。

---

## 1. 环境（I1）

| 项目 | 值 |
|---|---|
| 机器 | ASUS ROG Zephyrus G16（GU606AR），Windows 11 Pro for Workstations 10.0.26200 |
| CPU | Intel Core Ultra 9 386H，16 核 16 线程 |
| 内存 | 64 GB（系统报告 63.4 GB） |
| Intel 核显 | Intel(R) Graphics，架构 Xe3（OpenVINO 报告 `arch=v30.1.0`，32 EU），驱动 32.0.101.8724（2026-04-16） |
| Intel NPU | Intel(R) AI Boost，NPU 架构 5010，驱动 32.0.100.4841（2026-07-24；OpenVINO 报告 `NPU_DRIVER_VERSION 1004841`），设备管理器状态 OK |
| 独立显卡 | NVIDIA GeForce RTX 5070 Ti Laptop GPU，12 GB，驱动 592.00（32.0.15.9200） |
| OpenVINO | openvino 2026.4.1，openvino-genai 2026.4.1.0，optimum-intel 2.2.0，nncf 3.4.0（`.venv-ov`，Python 3.13.15） |
| CUDA 基线 | `.venv`（只读使用）：faster-whisper 1.2.1，ctranslate2 4.8.3 |
| 磁盘 | C:、D: 各约 3.1–3.2 TB 可用（不需要按 I1 的磁盘规则裁剪模型） |

OpenVINO 识别到的设备（`ov.Core().available_devices`）：

| 设备名 | FULL_DEVICE_NAME | 说明 |
|---|---|---|
| `CPU` | Intel(R) Core(TM) Ultra 9 386H | 能力 BF16/FP32/FP16/INT8 |
| `GPU.0` | Intel(R) Graphics (iGPU) | 能力 FP16/INT8，`GPU_HW_MATMUL`；可用共享内存约 33.5 GB |
| `GPU.1` | NVIDIA GeForce RTX 5070 Ti Laptop GPU (dGPU) | **OpenVINO 通过 OpenCL 把 NVIDIA 显卡也列成了 GPU 设备**（不受官方支持，未测） |
| `NPU` | Intel(R) AI Boost | 能力 FP16/INT8；`NPU_MAX_TILES 3` |

NPU 与核显均可用，未安装或升级任何驱动。

## 2. 测试素材与方法（I2–I4）

- **音频**：CSC1003 已完成修复的讲座 “Wed Oct 07”（55 分钟）中 20:28.0–30:30.0 一段，共 602 秒，16 kHz 单声道 wav（`spikes/openvino/audio/clip10.wav`，不提交）。
  起止点特意选在句子之间的静音里（20:28 位于一段 14 秒的空隙中，30:30 位于一段 5.8 秒的空隙中），所以**没有句子跨越边界**。
- **参考答案**：导出的 `cite.json` 中**完全落在区间内**的 71 句的 `en`（即修复后的 `sentence.text`），按时间拼接，共 1295 个词（规范化后 1302 个）。生成脚本 `make_reference.py`，参考文本存放在 `spikes/openvino/audio/`（不提交）。
- **WER**：小写化；去掉撇号（it's → its）；其余所有非字母数字字符（标点、连字符）视为空格；按词做编辑距离（`common.word_errors`，结果已与 jiwer 4.0.0 交叉核对，全部 45 个结果逐一相同）。
  被测文本与产品一致：OpenVINO 返回的词经过 `worker/asr.py` 的 `remove_repeats` 与 `split_sentences` 处理后再拼接（CUDA 基线同样经过幻觉过滤、`remove_repeats`、`split_sentences`）。
- **注意参考答案的偏向**：参考答案来自 **large-v2 + CUDA + 术语提示词** 的转写再经 LLM 修复，修复会删掉口头禅、重复和口误。所以 WER 里有相当一部分是“逐字转写 vs 修复后文本”的差异（插入错误偏多，见 §6），并且对 large-v2 的输出天然有利。WER 适合用于**相互比较**，不代表绝对错误率。
- **OpenVINO 的转写参数**：`WhisperPipeline` 长音频模式（GenAI 内部按 30 秒窗口滑动），`language=<|en|>`、`task=transcribe`、`return_timestamps=True`、`word_timestamps=True`（构造函数与生成参数都要设），贪心解码（`num_beams=1`，见 §7：核显与 NPU 上 beam search 不可用），不加提示词。核显与 NPU 设 `CACHE_DIR`（每个组合单独目录，首次运行前清空）。
- **模型**：`optimum-cli export openvino --model openai/whisper-<m> --task automatic-speech-recognition-with-past --weight-format fp16|int8|int4`，输出到 `data/models/openvino/whisper-<m>-<精度>/`。int8/int4 为**仅权重量化**（NNCF 无数据压缩）；未做需要校准数据的 W8A8 静态量化。
- **计时**：每个组合一个独立进程：加载（含编译）→ 转写两次取较快者 → 退出；核显与 NPU 再起一个进程测“缓存后加载”。
  峰值内存取进程峰值工作集（`PeakWorkingSetSize`），另记录运行期间系统可用内存的最大降幅（能反映核显/NPU 驱动侧分配，但会混入其他程序的波动）。
- **计时条件**：每组计时前记录 CPU 负载（`Get-Counter`，5 次 × 1 秒）、`go.exe`/`lectureflow.exe`/运行 `asr.py` 的 python 进程的 CPU 占用、NVIDIA 负载与性能状态、电源与电源模式、温度，见 `results/conditions.jsonl`。全部计时都在接通电源、Windows 电源模式“最佳性能”（电源计划 Balanced）、电量 100% 的状态下进行；温度只能读到 ACPI 热区（38–48 °C，不是 CPU 封装温度，CPU 温度需要管理员权限，读不到）。共 20 次检查，19 次 CPU 平均负载 2–27%（单个采样最高 39%），`lectureflow.exe`/`go.exe` 存在但 CPU 占用 0–0.9%，没有运行中的 `asr.py`；1 次（CPU/large-v3 之前）负载 44.5%，等了 2.6 分钟降到 7% 后开始。NVIDIA 显卡空闲时为 P8。

## 3. 结果（I3）

#### Intel 核显（OpenVINO 设备名 GPU.0）

| 模型 | 精度 | 变体 | 转换 s | 体积 MB | 首次加载+编译 s | 缓存后加载 s | 转写 s（两次） | RTF | WER | 句数 | 词级时间戳 | 进程峰值内存 MB | 系统内存增量峰值 MB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| base | fp16 | — | 41.6 | 146 | 2.0 | 0.46 | 17.0 / 15.2 | 0.025 | 14.2% | 93 | 有 | 949 | 1400 |
| base | int8 | — | 22.6 | 79 | 1.9 | 0.45 | 17.4 / 15.5 | 0.026 | 13.9% | 93 | 有 | 914 | 1420 |
| base | int4 | — | 21.9 | 59 | 5.2 | 0.46 | 19.0 / 20.1 | 0.032 | 14.8% | 78 | 有 | 1037 | 1491 |
| small | fp16 | — | 109.1 | 469 | 4.2 | 0.64 | 23.8 / 21.8 | 0.036 | 10.8% | 85 | 有 | 1846 | 2030 |
| small | int8 | — | 31.0 | 243 | 3.2 | 0.62 | 29.3 / 21.8 | 0.036 | 9.4% | 87 | 有 | 1628 | 2225 |
| small | int4 | — | 28.7 | 153 | 6.0 | 0.60 | 22.2 / 22.0 | 0.037 | 10.7% | 80 | 有 | 1602 | 1891 |
| medium | fp16 | — | 42.6 | 1466 | 8.4 | 1.61 | 43.2 / 43.5 | 0.072 | 9.7% | 96 | 有 | 3712 | 4493 |
| medium | int8 | — | 61.3 | 746 | 9.6 | 2.03 | 37.5 / 34.0 | 0.056 | 10.2% | 100 | 有 | 3197 | 3553 |
| medium | int4 | — | 65.6 | 424 | 9.6 | 1.30 | 37.0 / 31.7 | 0.053 | 10.6% | 101 | 有 | 2791 | 3176 |
| large-v3-turbo | fp16 | — | 29.8 | 1551 | 6.2 | 1.38 | 21.2 / 20.3 | 0.034 | 5.7% | 79 | 有 | 2683 | 3449 |
| large-v3-turbo | int8 | — | 37.6 | 784 | 7.8 | 0.67 | 17.6 / 15.6 | 0.026 | 5.8% | 79 | 有 | 2109 | 2635 |
| large-v3-turbo | int4 | — | 35.8 | 450 | 7.8 | 0.58 | 18.3 / 16.3 | 0.027 | 6.1% | 70 | 有 | 1797 | 1843 |
| large-v3 | fp16 | — | 37.4 | 2954 | 10.3 | 2.62 | 70.0 / 69.3 | 0.115 | 9.4% | 80 | 有 | 6297 | 7150 |
| large-v3 | int8 | — | 51.3 | 1489 | 9.6 | 2.19 | 60.9 / 59.3 | 0.099 | 9.3% | 79 | 有 | 4896 | 5622 |
| large-v3 | int4 | — | 59.1 | 818 | 8.7 | 2.06 | 47.0 / 45.5 | 0.075 | 8.2% | 78 | 有 | 4437 | 4604 |
| large-v3-turbo | fp16 | chunk30 | 29.8 | 1551 | 5.1 | — | 25.2 / 22.1 | 0.037 | 6.4% | 94 | 有 | 2493 | 3406 |
| large-v3-turbo | fp16 | nowords | 29.8 | 1551 | 5.7 | — | 16.3 / 15.3 | 0.025 | 5.8% | — | 无 | 2443 | 3514 |
| large-v3-turbo | fp16 | prompt-hotwords | 29.8 | 1551 | 4.7 | — | 24.0 / 22.5 | 0.037 | 7.5% | 55 | 有 | 2716 | 4850 |
| large-v3-turbo | fp16 | prompt-initial | 29.8 | 1551 | 5.1 | — | 24.6 / 24.2 | 0.040 | 7.1% | 79 | 有 | 2635 | 3825 |

#### NPU

| 模型 | 精度 | 变体 | 转换 s | 体积 MB | 首次加载+编译 s | 缓存后加载 s | 转写 s（两次） | RTF | WER | 句数 | 词级时间戳 | 进程峰值内存 MB | 系统内存增量峰值 MB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| base | fp16 | — | 41.6 | 146 | 39.1 | 0.65 | 36.5 / 34.8 | 0.058 | 14.2% | 90 | 有 | 1299 | 1753 |
| base | int8 | — | 22.6 | 79 | 39.9 | 0.66 | 35.1 / 36.5 | 0.058 | 13.9% | 93 | 有 | 1304 | 1611 |
| base | int4 | — | 21.9 | 59 | 49.6 | 0.65 | 37.0 / 37.4 | 0.061 | 13.5% | 87 | 有 | 1446 | 1568 |
| small | fp16 | — | 109.1 | 469 | 113.1 | 1.42 | 49.5 / 51.2 | 0.082 | 10.8% | 85 | 有 | 2560 | 2918 |
| small | int8 | — | 31.0 | 243 | 113.8 | 1.32 | 49.3 / 49.5 | 0.082 | 11.1% | 93 | 有 | 2499 | 2433 |
| small | int4 | — | 28.7 | 153 | 266.6 | 1.27 | 51.8 / 51.6 | 0.086 | 8.8% | 85 | 有 | 2772 | 2645 |
| medium | int8 | — | 61.3 | 746 | 665.6 | 2.93 | 70.9 / 73.1 | 0.118 | 9.4% | 101 | 有 | 5804 | 5974 |
| large-v3-turbo | fp16 | — | 29.8 | 1551 | 255.7 | 2.21 | 43.0 / 42.8 | 0.071 | 6.1% | 79 | 有 | 5004 | 4367 |
| large-v3-turbo | int8 | — | 37.6 | 784 | 271.4 | 1.66 | 44.0 / 44.8 | 0.073 | 6.8% | 61 | 有 | 4748 | 4153 |
| large-v3-turbo | int4 | — | 35.8 | 450 | 失败：NPU compile (WhisperPipeline construction) did not finish within 15 min (killed  |  |  |  |  |  |  |  |  |
| large-v3-turbo | fp16 | nowords | 29.8 | 1551 | 228.0 | — | 16.7 / 16.5 | 0.027 | 6.1% | — | 无 | 4975 | 2746 |

#### CPU

| 模型 | 精度 | 变体 | 转换 s | 体积 MB | 首次加载+编译 s | 缓存后加载 s | 转写 s（两次） | RTF | WER | 句数 | 词级时间戳 | 进程峰值内存 MB | 系统内存增量峰值 MB |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| base | fp16 | — | 41.6 | 146 | 0.4 | — | 18.9 / 19.5 | 0.031 | 15.0% | 90 | 有 | 1028 | 959 |
| base | int8 | — | 22.6 | 79 | 0.6 | — | 16.1 / 15.4 | 0.026 | 13.2% | 93 | 有 | 762 | 1097 |
| base | int4 | — | 21.9 | 59 | 0.6 | — | 15.5 / 17.5 | 0.026 | 17.5% | 82 | 有 | 733 | 1464 |
| small | fp16 | — | 109.1 | 469 | 0.7 | — | 48.5 / 51.3 | 0.081 | 10.4% | 85 | 有 | 2051 | 2910 |
| small | int8 | — | 31.0 | 243 | 0.9 | — | 36.6 / 34.8 | 0.058 | 9.4% | 92 | 有 | 1328 | 1855 |
| small | int4 | — | 28.7 | 153 | 1.0 | — | 35.0 / 35.2 | 0.058 | 10.4% | 86 | 有 | 1112 | 1048 |
| medium | int8 | — | 61.3 | 746 | 2.0 | — | 81.7 / 79.0 | 0.131 | 10.1% | 101 | 有 | 2778 | 3371 |
| large-v3-turbo | fp16 | — | 29.8 | 1551 | 0.8 | — | 153.3 / 159.1 | 0.255 | 5.6% | 78 | 有 | 5260 | 5870 |
| large-v3-turbo | int8 | — | 37.6 | 784 | 1.1 | — | 79.1 / 77.6 | 0.129 | 5.7% | 79 | 有 | 2431 | 2557 |
| large-v3-turbo | int4 | — | 35.8 | 450 | 1.6 | — | 94.5 / 93.1 | 0.155 | 6.1% | 64 | 有 | 1893 | 1947 |
| large-v3 | int8 | — | 51.3 | 1489 | 2.4 | — | 130.5 / 133.3 | 0.217 | 9.1% | 79 | 有 | 4676 | 5009 |

#### CUDA 基线（faster-whisper，RTX 5070 Ti Laptop）

| 模型 | 精度 | beam | 提示词 | 加载 s | 转写 s | RTF | WER | 句数 | 显存峰值 MB |
|---|---|---|---|---|---|---|---|---|---|
| large-v3-turbo | float16 | 5 | none | 2.1 | 44.9（单次） | 0.075 | 6.9% | 49 | 3919 |
| large-v3 | float16 | 5 | none | 4.0 | 173.6 / 171.9 | 0.285 | 7.5% | 47 | 5898 |
| large-v3 | float16 | 1 | none | 3.6 | 157.5（单次） | 0.262 | 5.0% | 75 | 5821 |
| large-v3 | float16 | 5 | initial | 3.4 | 174.2 / 175.2 | 0.289 | 7.4% | 46 | 6045 |
| large-v2 | float16 | 5 | initial | 3.8 | 181.2（单次） | 0.301 | 6.2% | 66 | 5984 |

列说明：“首次加载+编译”= 清空编译缓存后的 `WhisperPipeline(...)` 构造耗时（模型已转换好）；“缓存后加载”= 有缓存时的构造耗时；“转写 s”为两次转写各自耗时（第一次 RTF > 0.5 时只跑一次，见 §9）；RTF = 较快一次 / 602 秒；“系统内存增量峰值”混入了其他程序的波动，只作量级参考。

模型转换（下载与转换分开计时；下载走 Hugging Face，与网速有关）：下载 base 77 秒、medium 107 秒、large-v3-turbo 54 秒、large-v3 159–213 秒；转换（optimum-cli，CPU）每个 22–110 秒，见各表“转换 s”列（同一模型的第一次转换包含把 PyTorch 权重读入内存的时间）。体积：fp16 ≈ 参数量 × 2 字节，int8 约一半，int4 约 30%。

## 4. 每种设备的推荐配置（I5）

规则：长音频模式、带词级时间戳、贪心、无提示词的组合中，RTF ≤ 0.5 的前提下 WER 最低；都不满足时取 RTF 最低者。

| 设备 | 推荐（按规则） | RTF | WER | 词级时间戳 | 首次编译 / 缓存后加载 | 进程峰值内存 | 实用替代（WER 差 ≤ 0.5 个百分点中最快） |
|---|---|---|---|---|---|---|---|
| **Intel 核显**（`GPU.0`） | **large-v3-turbo / fp16** | 0.034 | 5.7% | 有 | 6.2 s / 1.4 s | 2.7 GB | large-v3-turbo / int8：RTF 0.026，WER 5.8%，模型 784 MB，内存 2.1 GB |
| **NPU** | **large-v3-turbo / fp16** | 0.071 | 6.1% | 有 | 256 s / 2.2 s | 5.0 GB | 无（int8 慢一点且 WER 6.8%） |
| **CPU** | **large-v3-turbo / fp16** | 0.255 | 5.6% | 有 | 0.8 s / — | 5.3 GB | large-v3-turbo / int8：RTF 0.129，WER 5.7%，内存 2.4 GB |

三种设备都满足 RTF ≤ 0.5，按规则都选 WER 最低者；但核显与 CPU 上 fp16 与 int8 的 WER 只差 1 个词（1302 词中），int8 快 25–50%、内存省一半。**建议合入时核显与 CPU 默认用 int8**（`recommendation.json` 的 `alternative` 字段），NPU 用 fp16。

补充判断：
- **large-v3-turbo 在三种设备上都是最佳选择**。medium 与 large-v3 在本素材上 WER 都更高（插入错误多，见 §6），而且更慢；small 及以下 WER 为 8.8–17.5%。
- **NPU 比核显慢一倍，慢在词级时间戳**：NPU 编码每个窗口约 0.35 秒（turbo），词级时间戳处理却占了一半以上时间；不要词级时间戳时 NPU 的 RTF 是 0.027（§6）。如果 I8 的电池模式只用 NPU，可以考虑在电池模式下关闭词级时间戳（字幕走 P4.4 插值）。**本次没有测功耗**，“NPU 更省电”尚未验证。首次编译约 4 分钟，必须缓存。NPU 的进程峰值内存（turbo 5.0 GB，medium 5.8 GB）比核显（2.7 GB）高，主要在编译阶段，仍在 CLAUDE.md 的 8 GB 以内。
- **核显是 Intel 设备里的首选**：最快（10 分钟音频 20 秒），首次编译只要几秒。
- CPU 能用但慢（turbo int8 约 RTF 0.13，即 55 分钟的课约 7 分钟，期间会占满 CPU），按 I6 默认禁用、作为兜底是合理的。

## 5. 与 CUDA 基线的对比

同一段音频，faster-whisper 1.2.1 + CUDA，参数与产品一致（VAD、词级时间戳、beam 5、`condition_on_previous_text=False`、float16），后处理一致（幻觉过滤、`remove_repeats`、`split_sentences`）。表见 §3 末尾。

| 配置 | RTF | WER | 句数（参考 71） |
|---|---|---|---|
| **CUDA large-v3，beam 5（I3 指定的基线）** | 0.285 | 7.5% | 47 |
| CUDA large-v3，beam 5，产品提示词 | 0.289 | 7.4% | 46 |
| CUDA large-v3，beam 1（单次） | 0.262 | 5.0% | 75 |
| CUDA large-v2，beam 5，产品提示词（= 本机日常配置，单次） | 0.301 | 6.2% | 66 |
| CUDA large-v3-turbo，beam 5（单次） | 0.075 | 6.9% | 49 |
| **OpenVINO 核显 large-v3-turbo fp16**（推荐） | **0.034** | **5.7%** | 79 |
| **OpenVINO NPU large-v3-turbo fp16**（推荐） | **0.071** | **6.1%** | 79 |
| **OpenVINO CPU** large-v3-turbo fp16（int8：0.129 / 5.7%） | 0.255 | 5.6% | 78 |

**重要：本次测量时 NVIDIA 显卡处于功耗受限状态**。计时期间 `nvidia-smi` 显示 P5、显存频率 810 MHz、约 32 W、`SW Power Cap: Active`（`results/conditions.jsonl` 也记录了空闲时 P8）。PROGRESS.md 里记录过同样的现象：上午 55 分钟的讲座用 large-v2 转写约 197 秒（RTF ≈ 0.06），之后同样配置慢了约 4.5 倍。所以上表的 CUDA RTF（约 0.29）比这块显卡的正常水平慢 4–5 倍；**正常状态下的 CUDA large-v2/large-v3 约 RTF 0.06**。我没有改动任何电源或显卡设置（不在权限内），只如实记录。

结论：
- **速度**：Intel 核显上的 large-v3-turbo（RTF 0.034）即使与 CUDA 的正常水平（约 0.06，beam 5、large-v2）相比也不慢；NPU 上的 turbo（0.071）与 CUDA 正常水平相当。
- **准确度**：相对修复后的参考答案，OpenVINO turbo（5.7–6.1%）与 CUDA large-v2 + 提示词（6.2%，即产品现在的配置）处在同一水平。CUDA beam 1 反而比 beam 5 好（5.0% vs 7.5%），beam 5 的输出句子更长、标点更少（47 句 vs 75 句），插入错误更多；这说明 ±1–2 个百分点的差异主要是“逐字程度与标点风格”，不是听错词。
- **差别在于附加信息**：CUDA 版有 VAD、词级置信度（`low_conf_words`）和幻觉过滤所需的片段概率，OpenVINO 版都没有（§7）。

## 6. 附加实验

均为核显 large-v3-turbo fp16（推荐配置）上的对照，两次取较快。

| 变体 | RTF | WER | 句数 | 说明 |
|---|---|---|---|---|
| 基准（长音频，带词级时间戳） | 0.034 | 5.7% | 79 | — |
| 不要词级时间戳 | 0.026 | 5.8% | — | 快约 25%；文本相同量级。性能计数显示词级时间戳处理占核显转写时间的 18–33%，**占 NPU 转写时间的 48–76%**（NPU 上更明显，见 §3 的 NPU 表；turbo 不带词级时间戳的 NPU 结果 ：RTF 0.071 → **0.027**（快 2.6 倍），WER 6.1% 不变） |
| 按静音切成 ≤30 秒的块逐块转写（I7 的“分块取消”写法） | 0.037 | 6.4% | 94 | 进度精确；慢约 9%，WER +0.7 个百分点，句子被块边界切碎（94 句） |
| 产品提示词作 `initial_prompt`（62 个术语，200 token） | 0.040 | 7.1% | 79 | 第一个窗口的术语拼写与标点明显改善（如 “two-time pad”，无提示词时第一个窗口全小写、无标点并写成 “two time bad”），但整体插入错误从 28 增至 54 |
| 产品提示词作 `hotwords`（每个窗口都带） | 0.037 | 7.5% | 55 | 插入错误 61；句子变长（55 句） |

提示词的结论：对照修复后文本的 WER 反而上升 1.5–2 个百分点，主要是插入（更“逐字”的输出），替换与删除反而略减（S 36→34/32，D 10→5）。CUDA large-v3 上 `initial_prompt` 几乎无影响（7.5% → 7.4%），因为它只作用于第一个窗口。合入时建议保持与 faster-whisper 相同的 `initial_prompt` 语义（G4 规则不变），`hotwords` 暂不启用。

WER 的构成（`results/error_analysis.json`）：所有模型都有约 11–12 处 “OK”（参考）↔ “okay”（模型）的拼写差异（约 0.9 个百分点）；“pad → bad” 3–5 处；large-v3 的插入（核显 fp16 约 70 处、CUDA 约 50 处）多为重复词与口误，修复阶段本来就会删掉。这也是 large-v3 在本表中 WER 不如 turbo 的主要原因。

## 7. ASR 后端契约逐条检查

对照 `worker/asr.py` 的契约（job.json → result.json、断句、进度、取消、原子写入、提示词）与 SPEC-v2 I7。三种设备（CPU、核显、NPU）结果一致，除非特别注明。

| 契约项 | OpenVINO GenAI 能否直接满足 | 变通方式 / 合入建议 |
|---|---|---|
| 句级时间戳 | **能**。`return_timestamps=True` 返回片段（`chunks`，含 start/end）；有词级时间戳时直接用 `split_sentences` 断句 | 断句复用共享的 `split_sentences`（I7），结果结构与 faster-whisper 相同 |
| 词级时间戳（`words`） | **能**，三种设备都能。需要 `WhisperPipeline(path, device, word_timestamps=True)` **并且**生成参数 `word_timestamps=True` | ① OpenVINO 会把一个词拆成不带前导空格的片段（如 `two` + `-time`、`x` + `-ore`），必须先把这类片段并回前一个词，否则断句与字幕会出现 “two -time”（`bench_ov.py` 已实现，约每 10 分钟 10–40 处）；② 没有 VAD，静音前后的词会被拉长（最长的词 10–19 秒），偶有时间倒退 0.1–0.2 秒（每 10 分钟 0–3 处），合入时应把单词时长截断到合理上限并保证单调；③ 代价见 §6：NPU 上词级时间戳占转写时间的大部分 |
| 词级置信度（`low_conf_words`） | **不能**。`WhisperWordTiming` 只有 `word/start_ts/end_ts/token_ids`；`scores` 恒为 1.0，`logprobs` 不返回逐词概率 | 按 I7：`low_conf_words` 留空，修复阶段照常 |
| 幻觉过滤（no_speech_prob、avg_logprob） | **不能**，没有片段级概率 | 只能做 `remove_repeats`（已验证生效）。可选：用 VAD 先切掉长静音（需要额外的 VAD 模型，未测） |
| 进度上报 `PROGRESS x.xx` | **部分**。`generate(..., streamer=cb)` 在长音频 + 时间戳 + 词级时间戳下**仍可用**（文档说只支持 30 秒以内的短音频，实测 2026.4.1 并非如此），但回调**每个 30 秒窗口调用一次，只给文本、不给时间** | 方案 A：按静音把音频切成若干块（建议 2–5 分钟一块），逐块转写，块与块之间上报 `PROGRESS = 块结束时间 / 总时长`，块内用 streamer 心跳；方案 B（I7 的写法）：约 30 秒一块，进度精确但精度与速度略差（§6）|
| 取消 | **能**。① LectureFlow 现在的取消方式是杀进程（`exec.CommandContext` → `Process.Kill`）：三种设备上杀进程后 0.001–0.012 秒退出，**设备立即可再用**（重新加载并转写成功），NPU 不会卡死；② 进程内取消：streamer 回调返回 `StreamingStatus.CANCEL` 后 `generate` 立即返回已完成部分 | 沿用杀进程即可，不需要分块也能“几秒内生效” |
| 先写临时文件再原子替换 | **能**，与后端无关：worker 自己写 `--out`（`result.json.tmp`），Go 侧 rename | 照搬 `write_result` |
| initial_prompt（G4 术语提示词） | **能**，三种设备都接受。语义：只作用于**第一个 30 秒窗口**。这与现在的 faster-whisper 设置一致——`condition_on_previous_text=False` 时 faster-whisper 也只在第一个窗口使用 initial_prompt（见 `faster_whisper/transcribe.py` 中 `prompt_reset_since`） | 另有 `hotwords`：作用于**每一个窗口**。效果见 §6 |
| 200 token 截断 | **能**。`pipe.get_tokenizer().encode(" " + prompt)` 计数，复用 `truncate_prompt` | — |
| beam search（现为 beam 5） | **只有 CPU 能**；核显与 NPU 上 `num_beams=5` 报错（`RuntimeError: Exception from src\core\src\runtime\tensor.cpp`） | 核显 / NPU 只能贪心解码（对 WER 的影响见 §5 中 CUDA beam 1 对照） |
| 语言 | **能**，`language="<|en|>"`（也接受 `en`） | — |
| 模型下载与首次准备 | 需要先用 optimum-cli 转换（需要 torch，`.venv-ov` 约 1.4 GB）；NPU 首次编译很慢（§3） | 必须设 `CACHE_DIR`（建议 `data/models/openvino/cache/<设备>-<模型>-<精度>`）；首次使用时显示“首次使用，正在准备模型”（I7） |
| 设备选择 | 设备名 `GPU` 在本机等于 `GPU.0`（Intel 核显），但 OpenVINO 也把 NVIDIA 显卡列为 `GPU.1` | 探测时按 `FULL_DEVICE_NAME`/`DEVICE_ARCHITECTURE` 中的 vendor `0x8086` 找 Intel 核显，**不要直接写 `GPU`**（在 NVIDIA 先枚举的机器上会选错） |

## 8. 遇到的坑

1. **NPU 首次编译很慢，int4 尤其慢**：清空缓存后构造 `WhisperPipeline` 的时间——base 约 40 秒，small 约 113 秒（int4 为 267 秒），large-v3-turbo 约 228–271 秒，medium int8 666 秒，**turbo int4 编译 15 分钟仍未结束**（为守住时间上限而中止）。有缓存后只要 0.65–2.2 秒。合入时必须设 `CACHE_DIR`，并把首次编译算进“首次使用，正在准备模型”；NPU 不推荐 int4。核显首次编译 2–10 秒，CPU 不到 1.2 秒。
2. **词片段没有前导空格**：OpenVINO 的 `words` 偶尔把一个词拆成几段（`two` + `-time`），后一段没有前导空格。直接交给 `split_sentences` 会得到 “two -time”。需要先合并（每 10 分钟 4–40 处，模型越小越多）。
3. **没有 VAD**：静音不会被跳过，静音前后的词被拉长（片段开头的词从 0.0 秒开始、持续 7 秒以上；最长的词 10–19 秒），偶有 0.1–0.2 秒的时间倒退。字幕切分前应截断异常长的词并保证单调。
4. **streamer 的文档与实际不符**：`generate` 的文档说 streamer 只支持 30 秒以内且 `return_timestamps=False`，实测 2026.4.1 在长音频 + 词级时间戳下也能用，但每个 30 秒窗口回调一次、只给文本。
5. **核显与 NPU 不支持 beam search**：`num_beams=5` 报 `tensor.cpp` 异常，只能贪心；CPU 可以。
6. **设备名**：OpenVINO 把 NVIDIA 显卡也列为 `GPU.1`；本机 `GPU` = `GPU.0` = Intel 核显，但不能假设所有机器都如此。
7. **`hf download` 的 `--exclude`**（huggingface_hub 1.x）：`--exclude a b c` 只有第一个是排除模式，其余被当成文件名（并提示 “Ignoring --exclude”）——一个仓库什么都没下载，另一个把全部 15 GB 都下载了。每个模式要单独写一个 `--exclude`。
8. **Windows 上删除刚用过的编译缓存偶尔报 `PermissionError`**（blob 被短暂占用，推测是杀毒扫描）。产品不要在用完后同步删除缓存目录；按“设备-模型-精度”分目录并容忍删除失败。
9. **NVIDIA 显卡在本次测试中功耗受限**（P5，`SW Power Cap`），CUDA 基线比正常慢 4–5 倍，见 §5。
10. **转换需要 torch**：`optimum-cli export openvino` 依赖 torch 与 transformers，`.venv-ov` 共约 1.4 GB；运行时只需要 openvino、openvino-genai、openvino-tokenizers。Hugging Face 上 `OpenVINO/whisper-<模型>-<fp16|int8|int4>-ov`（含 large-v3-turbo、large-v3、medium、small、base）有官方预转换模型，合入时可以直接下载、省掉 torch——**本次未测这些预转换模型能否输出词级时间戳**，合入前需要验证。
11. **faster-whisper 的 `initial_prompt` 只作用于第一个 30 秒窗口**（`condition_on_previous_text=False` 时每个窗口之后都会重置提示），所以现在的术语提示词对整节课的影响很小（CUDA large-v3：7.5% → 7.4%）。这与 OpenVINO 的 `initial_prompt` 语义一致；这一点与 G4 的选词规则有关，供 lead 参考。
12. **WER 参考答案的口径**：参考答案是修复后的文本（删了重复与口误、“OK” 的写法与模型不同），对逐字程度高的模型不利，见 §2、§6。

## 9. 偏离与未测项

为了守住 3 小时的时间上限（不含等待），做了以下取舍：

- **第一次 RTF > 0.5 的组合只跑一次**（这种组合不可能成为推荐配置）。实际没有任何 OpenVINO 组合触发这一条（最慢的 CPU turbo fp16 也只有 RTF 0.26），所有 OpenVINO 组合都跑了两次。
- **CUDA 基线中的附加对照只跑一次**：large-v3 beam 1、large-v2 + 提示词、large-v3-turbo（主基线 large-v3 与 large-v3 + 提示词各跑两次）。原因是显卡处于功耗受限状态，每次约 3 分钟。
- **NPU 上 medium 与 large-v3 只测 int8**，**CPU 上 medium 与 large-v3 只测 int8**：核显上的结果显示这两个模型在本素材上的 WER 都不如 large-v3-turbo，又更慢，不会改变推荐；NPU 上一个 large 模型的首次编译就要 5–15 分钟以上。未测的组合：NPU 的 medium fp16/int4 与 large-v3 fp16/int4，CPU 的 medium fp16/int4 与 large-v3 fp16/int4（原因同上）。NPU large-v3 int8 已开始但在时间上限前中止（NPU medium int8 首次编译用了 666 秒，large-v3 预计更久），记为“未测”。NPU medium int8 能运行：首次编译 666 秒，RTF 0.118，WER 9.4%。
- **NPU large-v3-turbo int4**：编译 15 分钟未结束，中止并记为“未能运行（编译超时）”。
- int8/int4 只测了**仅权重量化**（optimum-cli 的 `--weight-format`），未测需要校准数据集的 W8A8 静态量化（`--quant-mode int8 --dataset librispeech`），后者可能更适合 NPU，留给合入阶段的自测（I9）。
- 产品提示词、不要词级时间戳、分块转写这三组附加实验只在推荐配置上做（§6）。

## 10. 复现

```powershell
# 环境（只建 .venv-ov，不碰 .venv）
& "C:\Program Files\Python313\python.exe" -m venv .venv-ov
.venv-ov\Scripts\python.exe -m pip install -r worker\requirements-openvino.txt
# 素材与参考答案（只读访问讲座目录）
ffmpeg -i <讲座目录>\audio.wav -ss 1228 -t 602 -ar 16000 -ac 1 -c:a pcm_s16le spikes\openvino\audio\clip10.wav
.venv-ov\Scripts\python.exe spikes\openvino\make_reference.py --lecture-dir <讲座目录>
# 环境记录、下载与转换
.venv-ov\Scripts\python.exe spikes\openvino\env_info.py
.venv-ov\Scripts\python.exe spikes\openvino\convert.py --models base small medium large-v3-turbo large-v3 --precisions fp16 int8 int4
# 契约探测
.venv-ov\Scripts\python.exe spikes\openvino\probe_contract.py --device GPU.0   # 以及 NPU、CPU
.venv-ov\Scripts\python.exe spikes\openvino\probe_kill.py --device NPU
# 矩阵（每批前自动做空闲检查）与 CUDA 基线
.venv-ov\Scripts\python.exe spikes\openvino\run_matrix.py --devices GPU.0 NPU CPU
.venv\Scripts\python.exe spikes\openvino\bench_cuda.py --model large-v3
# 汇总（matrix.csv、recommendation.json、本文表格）
.venv-ov\Scripts\python.exe spikes\openvino\summarize.py --markdown
```

## 11. 端到端（I12，2026-10-24）

在正式服务（e8c4f1e，迁移 008）上用真实设备与 DeepSeek 完整跑流水线（转写 → 摘要 → 修复 → 翻译 → 导出）。素材是 CSC1003 的 Wed Oct 07 一节完整讲座（音频 54:47），上传三份到独立的验收课程“CSC1003 v3b Intel Acceptance”，没有加进原课程（避免改变原课程的编号）。验收课程只上传了大纲 SYLLABUS.MD，没有上传课件，所以术语表只有 29 条（强制 25 条、参考 4 条）。原课程的术语表还包含课件，有 331 条。原始数据与脚本放在 scratchpad 的 `i12/` 目录，不提交。

**一句话结论**：large-v3-turbo（核显 / NPU）经过修复后，与 CUDA large-v3 经过修复后的文本只差 5.7–5.8% 的词（去掉数字写法、复合词拆分、口头禅等格式差异后为 4.4–4.5%）。这个差距比两个大模型之间的差距还小：同一讲座 large-v2 与 large-v3 修复后相差 7.7% / 4.9%。课程术语的错误（pad→bad、XOR→exor、Vernam）都被修复阶段改正，修复后各术语的出现次数在三个版本中基本相同（只有 one-time pad 为 6 / 7 / 8 次）。速度上，核显比 CUDA 快，NPU 约慢一半；费用没有实质差别。核显与 CUDA 能同时各转写一节。

### 11.1 自测（I9）与采用的配置

自测片段由程序自动选取：一节已完成修复的 3 分钟讲座的 50.3–169.4 秒，参考文本 280 词。

| 设备 | 候选 | 加载 s | 转写 s | RTF | WER | 进程峰值内存 MB |
|---|---|---|---|---|---|---|
| 核显 ov:GPU | large-v3-turbo / fp16 | 1.29 | 5.26 | 0.044 | 5.71% | 2289 |
| 核显 ov:GPU | **large-v3-turbo / int8**（推荐） | 1.20 | 4.35 | 0.037 | 5.71% | 1561 |
| NPU ov:NPU | large-v3-turbo / fp16 | 2.55 | 9.85 | 0.083 | 5.71% | 3577 |
| NPU ov:NPU | **large-v3-turbo / int8**（推荐） | 2.40 | 9.77 | 0.082 | 5.71% | 3523 |

两台设备的编译缓存都沿用 3a 的结果（不是首次编译）。按推荐“应用”后，两台都改为 int8。注意：NPU 的两种精度 WER 相同（280 词中错误数相同），RTF 只差 0.8%，结果却选了 int8。而 3a 用 10 分钟片段测得 NPU int8 的 WER 更高（6.8%，fp16 为 6.1%）、速度也略慢，见 11.6 第 1 条。

### 11.2 每节的结果

| | [CUDA large-v3] | [iGPU] | [NPU] |
|---|---|---|---|
| 设备 / 模型 / 精度 | cuda:0 / large-v3 / float16，beam 5 | ov:GPU / large-v3-turbo / int8，贪心 | ov:NPU / large-v3-turbo / int8，贪心 |
| 转写：加载 / 转写 s（`asr_timing`） | 3.6 / 164.1 | 1.3 / 105.3 | 3.4 / 223.5 |
| **转写 RTF** | **0.050** | **0.032** | **0.068** |
| worker 峰值工作集 MB | 3346 | 2448 | 4068 |
| 句数（全部有词级时间戳） | 230 | 359 | 363 |
| 字幕块（起点落在词起点上的比例） | 708（100%） | 721（100%） | 736（99.6%） |
| patch：auto / absorb / decide（合计） | 26 / 22 / 0（48） | 23 / 12 / 5（40） | 28 / 12 / 8（48） |
| 修复前后自身差异（WER） | 1.15% | 0.81% | 1.01% |
| 翻译 qa_failed | 3 | 2 | 3 |
| 摘要 / 修复 / 翻译 s | 9 / 12 / 53 | 8 / 3 / 125 | 10 / 7 / 74 |
| **端到端**（启动 → 导出完成） | **248 s** | **250 s** | **325 s** |
| DeepSeek 费用（估算） | ¥0.61 | ¥0.66 | ¥0.92 |

- 时间说明：[CUDA] 与 [iGPU] 同时运行，两者的翻译阶段共用同一个 provider 的并发上限（8），[iGPU] 的翻译因此慢了一倍。[NPU] 在 [iGPU] 转写结束后才启动。
- 费用：三节加课程级任务（术语提取、术语整理、文件名规则，共 ¥0.04）合计 ¥2.23。余额从 ¥383.56 降到 ¥381.33，与估算一致。费用的差别主要来自句数：句子越多，翻译请求越多（237 / 363 / 369 次）。
- CUDA large-v3 只断出 230 句（平均 28.7 词一句，turbo 约 18 词）。这与 NOTES 中“large-v3 对该讲师大段丢失标点”一致。字幕切分按词级时间戳进行，所以字幕块的长度仍然正常（最长 84 字符、7 秒以内）。
- 播放器：三节都在 1440×900 下打开检查（截图含讲师幻灯片，只放 scratchpad）。字幕带的中英文正常；播放时字幕按词级时间切换；核显与 NPU 两版每句都有 `words`，没有长于 3 秒的词，也没有时间倒退。控制台唯一的错误是 `GET /chapters` 返回 404，这是“从未生成过章节”时设计上的返回值。

### 11.3 修复后文本的差异（以 CUDA large-v3 为参照）

WER 的计算口径与第 2 节相同。“去掉格式差异”指：数字与英文数词视为相同；cipher text / pseudo random / key stream / x or 分别合并为一个词；gonna → going to；去掉 um、uh、okay、ok、so、yeah、right、oh。

| 对比 | 转写原文 | 修复后 | 修复后，去掉格式差异 |
|---|---|---|---|
| 核显 turbo vs CUDA large-v3 | 6.20% | **5.77%** | **4.38%** |
| NPU turbo vs CUDA large-v3 | 6.17% | **5.73%** | **4.51%** |
| NPU vs 核显（同一模型、两台设备） | 3.89% | 3.74% | 2.86% |
| 原课程 CUDA large-v2 vs CUDA large-v3（术语表不同，2026-10-23 运行） | 8.06% | 7.74% | 4.85% |

差异的构成（核显 vs CUDA，修复后共 381 处词级编辑）：
- 约 104 处是口头禅与重复词的增删（okay、so、you、it 等）；
- 约 37 处是数字写法（one / 1、zero / 0）；
- 约 20 处是复合词拆分（ciphertext / cipher text、pseudorandom / pseudo random）；
- 其余是 gonna / going to、use / used、will / would、PRG / PRNG 等措辞差异；
- 改变意思的只看到 can / can't 两处。核显那一处已被修复阶段标为 decide（等待用户审阅）。

关键术语（修复后正文中的出现次数，CUDA / 核显 / NPU；括号内为转写原文中的次数）：

| 术语 | CUDA large-v3 | 核显 | NPU |
|---|---|---|---|
| one-time pad | 6（1） | 7（3） | 8（2） |
| two-time pad | 4（1） | 4（1） | 4（1） |
| “time bad”（pad→bad 误识别） | 1（7） | 0（6） | 0（8） |
| Vernam | 1（0，原文是 Furnham） | 1（0） | 1（0） |
| XOR | 38（30） | 39（34；另有 exor 4 处被改正） | 36（34） |
| LFSR | 11（9） | 11（9） | 11（10） |
| perfect secrecy / Caesar | 27 / 2 | 27 / 2 | 27 / 2 |

译文中的“一次性密码本”在三个版本中都是 8 次。**Vigenère 在这一节讲座中没有出现**（原课程的 large-v2 版本中也没有），这一节没法抽查它。

**回答 I12 的问题“小模型 + 修复，最终质量与大模型差多少”**：在这节讲座上，turbo 加修复与 large-v3 加修复的差距，小于 large-v2 与 large-v3 之间的差距。剩下的差异大多是口头禅、数字写法和复合词写法，课程术语没有差别。修复阶段对两种模型同样有效：OpenVINO 没有 `low_conf_words`，但 patch 数与 CUDA 版相当（40、48 对 48）。另外，turbo 在这位讲师的讲座上断句明显更好。
保留意见：没有人工逐字稿，上面只是两两比较，不能说明哪一版更准；large-v3-turbo 有 8.09 亿参数（解码器只有 4 层），并不算真正的小模型；样本只有一节讲座。

### 11.4 多设备同时转写（I8）

设备模式为 auto，先启动 [CUDA]；它开始转写后再启动 [iGPU]。[iGPU] 的 asr 快照绑定到 `ov:GPU`（`auto: true`），[CUDA] 绑定到 `cuda:0`。`GET /api/asr/devices` 与资源栏同时显示 cuda:0 和 ov:GPU 的槽位都是 1/1，各跑一节（重叠 108 秒，即 [iGPU] 的整个转写过程）。[NPU] 在设备模式设为 ov:NPU 时启动，快照固定为 `ov:NPU`（不是 auto）；启动后设备模式已改回 auto。整个过程中没有出现 needs_model_choice。

内存（裁决 8）：同时运行时，两个 worker 采样得到的工作集合计最高 3.3 GB，按各自峰值相加约 5.8 GB（CUDA worker 的提交内存约 6.2 GB，不常驻）。NPU worker 峰值 4.1 GB。如果三台设备同时运行，各自峰值相加约 9.9 GB，会超过 8 GB 的常驻内存预算。本次没有测三台同时运行。

### 11.5 显示器常亮 vs 关闭（用户的假设：长时间不用、显示器关闭后独显变慢）

**结论**：变慢确实发生，但原因不是显示器关闭本身，而是空闲超时关屏后系统进入 Modern Standby（本机只支持 S0 低功耗待机），进入后进程仍在运行，独显却被限制在低功耗状态。**建议**：长时间的 CUDA 任务期间，不要让系统因空闲进入 Modern Standby。可以让显示器保持常亮（用户 10-23 23:20 起已这样做），或者由产品在有 GPU 任务时持有电源请求（哪一种请求在本机的 Modern Standby 下有效，需要实测）。系统设置一律不改，只作建议。

依据：
- 10 分钟片段（与第 2 节相同的 20:28–30:30），用 `worker/asr.py` 直接跑 CUDA large-v2 float16，显示器常亮：
  - 3 次 RTF 分别为 0.0665、0.0630、0.0623；
  - 全程 P0、SM 约 2.7 GHz、功耗 66–82 W、功耗上限 115–125 W，没有任何降频原因（`clocks_event_reasons.active = 0`）；
  - 显示状态用 `GUID_CONSOLE_DISPLAY_STATE` 记录，确认是开着的。
- 系统日志（Kernel-Power 506 / 507）显示，10-23 10:36:22 系统因 Idle Timeout 进入 Modern Standby，直到 22:27:51 才因键盘输入退出，这期间没有其他电源事件。数据库中的 CUDA 整节转写：

| 时段 | 次数 | RTF |
|---|---|---|
| 这一时段之外（10-23 05:20–05:41，10-24 05:58） | 5 | 0.050–0.062 |
| 这一时段之内（10-23 10:48、10:52、21:27） | 3 | 0.287–0.324 |

  3a 的 CUDA 基线（17:49 起，P5、约 32 W、`SW Power Cap`）也在这一时段之内。
- **无法直接测量“显示器关闭”**：在这台机器上，`SC_MONITORPOWER` 会让系统立即进入 Modern Standby（Reason: SC_MONITORPOWER）。本机待机时断开网络，所有进程都被挂起。
  - 本次实验因此在 06:21–06:24 引起了 27 次进入待机，用户按键盘或触控板唤醒后，系统在 0–3 秒内又重新进入待机；
  - 原因推测：广播的 `SC_MONITORPOWER` 由各窗口依次处理，被挂起的应用在每次唤醒后才处理自己那份；这一点未完全证实；
  - 计划中的“关屏”那次运行开始时，系统已被用户唤醒，显示器实际是开着的（RTF 0.0623），不能算作关屏数据；
  - **以后不要用 SC_MONITORPOWER 做这类测试**。如需补测，应等显示器按空闲超时自然关闭后再测，并接受系统会进入 Modern Standby。

### 11.6 发现的问题与建议

1. **自测在两个候选打平时偏向更快的那个，哪怕只快一点点**：NPU 的 fp16 与 int8 在 280 词的片段上 WER 相同，RTF 只差 0.8%，`Pick` 选了 int8；而 3a 用 10 分钟片段测得 NPU int8 的 WER 更高、速度也不快。核显选 int8 符合 3a 的建议，但同样是在 WER 打平时靠 RTF 决出的。建议：WER 相差不超过 1 个错误词、且 RTF 相差不到 10% 时，保留设备的推荐默认值（`devices.Recommended`，fp16）；或者让 NPU 自测用更长的片段（5 分钟以上）。
2. **三台设备同时运行可能超过 8 GB 内存预算**：按各自峰值相加约 9.9 GB（CUDA worker 3.3 GB、核显 2.4 GB、NPU 4.1 GB）。内存准入比较的是系统可用内存（本机约 38 GB）减去 `mem_reserve_mb`，CUDA worker 占用的主机内存也不在预估之内，所以 8 GB 的常驻预算实际上没有被约束。建议：如果要守住 8 GB，准入时按各 worker 的观测峰值（`asr_timing.peak_mem_mb`，CUDA worker 也算在内）累计判断；或者在 cuda:0 与 ov:GPU 都在运行时让 NPU 等待。
3. **播放器控制台出现 404**：`GET /api/lectures/{id}/chapters` 在从未生成过章节时返回 404（设计如此），但浏览器会把它记为控制台错误。建议改为返回 200 和空章节列表（`running: false`），或者前端改用不报错的方式探测。这只是外观问题。
4. **decide 类 patch 等待审阅**：核显版 5 条、NPU 版 8 条（CUDA 版 0 条），需要用户在审阅页处理。这是 OpenVINO 版没有 `low_conf_words` 之后正常的结果，不是缺陷。
5. **独显变慢与 Modern Standby**：见 11.5。产品端可以考虑在有 CUDA 任务运行时持有电源请求；需要先实测：在本机由空闲超时触发的 Modern Standby 下，哪一种请求能让独显保持 P0。

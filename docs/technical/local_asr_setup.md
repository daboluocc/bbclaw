# 本地 ASR 部署指南

本文档记录在 Mac mini (M4) 上部署本地 ASR 的过程。

## 环境

- **设备**: Mac mini (Apple M4)
- **系统**: macOS
- **包管理**: Homebrew

## 安装 Whisper.cpp

### 1. 安装 whisper-cpp

```bash
brew install whisper-cpp
```

### 2. 下载模型

模型存放路径：`/opt/homebrew/share/whisper-cpp/`

```bash
# 下载 base 模型 (141MB)
cd /opt/homebrew/share/whisper-cpp
curl -L -o ggml-base.bin "https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-base.bin"

# 下载 small 模型 (465MB) - 推荐中文使用
curl -L -o ggml-small.bin "https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-small.bin"
```

### 3. 模型对比

| 模型 | 大小 | 中文效果 | 推荐场景 |
|------|------|----------|----------|
| tiny | 39M | 一般 | 快速测试 |
| base | 141M | 一般 | 英文为主 |
| small | 466MB | 较好 | **中文推荐** |
| medium | 1.5GB | 好 | 精度优先 |

## 测试

### 命令行测试

```bash
# 使用 small 模型识别英文音频
/opt/homebrew/bin/whisper-cli \
  -m /opt/homebrew/share/whisper-cpp/ggml-small.bin \
  -f /opt/homebrew/share/whisper-cpp/jfk.wav \
  -l auto --no-timestamps

# 使用 small 模型识别中文音频
/opt/homebrew/bin/whisper-cli \
  -m /opt/homebrew/share/whisper-cpp/ggml-small.bin \
  -f your_audio.wav \
  -l zh --no-timestamps
```

### 测试结果

**英文音频 (JFK 演讲)**:
```
And so my fellow Americans, ask not what your country can do for you, ask what you can do for your country.
```

**中文音频 (测试文本: "你好，我是阿虾助手")**:
```
你好 我是阿瞎助手
```
(声母 "x" 和 "j" 发音接近，存在轻微识别误差)

## OpenClaw 集成配置

在 OpenClaw 环境中配置本地 ASR（Whisper.cpp 示例；**中英双语**优先见上文 **「启用双语支持（中英）」** 中的 FunASR SenseVoice 配置）。

```bash
ASR_PROVIDER=local
ASR_LOCAL_BIN=whisper-cli
ASR_LOCAL_ARGS=-m /opt/homebrew/share/whisper-cpp/ggml-small.bin -l zh -f {wav} --no-timestamps -otxt -of {wav}
ASR_LOCAL_TEXT_PATH={wav}.txt
```

### 参数说明

- `-m`: 模型文件路径
- `-l`: 语言代码 (`zh`=中文, `auto`=多语/自动；**双语场景见「启用双语支持」**)
- `-f`: 输入音频文件路径
- `--no-timestamps`: 不输出时间戳
- `-otxt`: 输出文本文件
- `-of`: 输出文件路径（不含扩展名）

## 性能

### Mac mini M4 推理时间

| 模型 | 加载时间 | 推理时间 (11s音频) |
|------|----------|-------------------|
| base | ~22ms | ~246ms |
| small | ~160ms | ~562ms |

推理使用 Apple Metal (MPS) 加速。

## 启用双语支持（中英）

**目标**：同一段语音里同时识别 **中文与英文**（例如说「你好」或 **hello**、中英混说），而不是只优化纯中文。

### 推荐：FunASR SenseVoice（`asr/funasr_cli.py` 默认）

仓库内 **`funasr_cli.py`** 在未指定 **`-m paraformer-zh`** 时加载 **`iic/SenseVoiceSmall`**，支持 **zh / en / yue / ja / ko** 等；与 **`language=auto`** 配合最适合中英夹杂。

**启用步骤：**

1. 安装依赖并完成 `asr/` 下虚拟环境（见下节「FunASR 部署」）。
2. 在 **`src/.env`** 中配置本地 ASR（路径按本机修改）：

```bash
ASR_PROVIDER=local
ASR_LOCAL_BIN=/path/to/bbclaw/asr/funasr_wrapper.sh
ASR_LOCAL_ARGS=-m dummy -f {wav} -l auto --no-timestamps -otxt -of {wav}
ASR_LOCAL_TEXT_PATH={wav}.txt
```

**要点**

| 配置 | 说明 |
|------|------|
| **`-l auto`** | 必传（经 `ASR_LOCAL_ARGS` 传给 `funasr_cli.py`），对应 SenseVoice 的自动语种，**双语场景请用 auto，不要用 `-l zh`**。 |
| **`-m dummy`** | 占位即可；非 `paraformer-zh` 时走 SenseVoice。 |
| **仅中文** | 若只要 Paraformer 中文模型，使用 **`-m paraformer-zh`** 且通常 **`-l zh`**。 |

可选环境变量（写入调用 FunASR 的 shell 环境或 `funasr_wrapper.sh` 前 **`export`**）：

- **`FUNASR_DEVICE`**：如 `cpu`、`cuda:0`；不设置则由 FunASR 默认选择。

首次使用 SenseVoice 会从 ModelScope **单独下载**一套模型（与 Paraformer 缓存不同），需联网一次。

### 备选：Whisper.cpp（不跑 Python）

使用 **small** 等多语模型，语言用 **`-l auto`**（以本机 `whisper-cli --help` 为准）：

```bash
ASR_PROVIDER=local
ASR_LOCAL_BIN=whisper-cli
ASR_LOCAL_ARGS=-m /opt/homebrew/share/whisper-cpp/ggml-small.bin -l auto -f {wav} --no-timestamps -otxt -of {wav}
ASR_LOCAL_TEXT_PATH={wav}.txt
```

中英混合场景下，**SenseVoice + `-l auto`** 一般比 **Paraformer 纯中文** 更合适；Whisper 适合希望少依赖、仅用 Homebrew 的环境。

## FunASR 部署

FunASR 是阿里巴巴开源的语音识别框架，中文效果优于 Whisper，支持热词功能。

### 安装 (使用 uv)

```bash
mkdir -p bbclaw/asr
cd bbclaw/asr
uv venv
source .venv/bin/activate
uv pip install funasr torch torchaudio
```

### requirements.txt

```
funasr
torch
torchaudio
```

### SenseVoice 与 Paraformer（与「双语」关系）

- **双语（中英）**：见上文 **「启用双语支持（中英）」**；使用 SenseVoice + **`-l auto`**。
- **`funasr_cli.py` 默认**：非 **`-m paraformer-zh`** 时加载 **`iic/SenseVoiceSmall`**。
- **仅中文**：`-m paraformer-zh`，通常 **`-l zh`**。

### 使用示例（Paraformer 纯中文）

```python
from funasr import AutoModel

model = AutoModel(model="paraformer-zh", model_revision="v2.0.4")
result = model.generate("audio.wav")
print(result[0]['text"])
```

### 热词功能

```python
# 创建热词文件
hotword_content = """阿虾
bbclaw
OpenClaw
运维开发
"""
with open("hotword.txt", "w") as f:
    f.write(hotword_content)

# 使用热词模型
model = AutoModel(
    model="paraformer-zh",
    model_revision="v2.0.4",
    hotword="hotword.txt"
)
```

### 测试结果

| 测试 | 结果 |
|------|------|
| 英文识别 | "and so my fellow americans..." |
| 中文识别 | "你好 我是阿虾助手" |
| 热词功能 | ✅ 正常加载 |

### 性能 (Mac mini M4)

- FunASR Paraformer: ~0.3x RTF (实时速度的 3 倍快)
- 模型缓存: `~/.cache/modelscope/hub/models/iic/speech_seaco_paraformer_large_asr_nat-zh-cn-16k-common-vocab8404-pytorch`

## bbclaw 适配器集成

当前已集成到 bbclaw-adapter，使用方式如下：

### 1. 文件结构

```
bbclaw/asr/
├── .venv/
├── funasr_core.py         # 模型单例 + transcribe（CLI 与 server 共用）
├── funasr_cli.py          # 子进程 CLI（ASR_PROVIDER=local）
├── funasr_server.py       # 常驻 HTTP，OpenAI 兼容（ASR_PROVIDER=openai_compatible）
├── funasr_wrapper.sh      # adapter 调用 CLI 的入口
├── run_funasr_server.sh
├── hotword.txt
├── requirements.txt
└── test_*.py
```

### 2. 配置 (src/.env)

```bash
# FunASR — 双语（中英）：SenseVoice + -l auto（详见文档「启用双语支持」）
ASR_PROVIDER=local
ASR_LOCAL_BIN=/path/to/bbclaw/asr/funasr_wrapper.sh
ASR_LOCAL_ARGS=-m dummy -f {wav} -l auto --no-timestamps -otxt -of {wav}
ASR_LOCAL_TEXT_PATH={wav}.txt

# 仅中文：Paraformer
# ASR_LOCAL_ARGS=-m paraformer-zh -f {wav} -l zh --no-timestamps -otxt -of {wav}

# Whisper (备用)
# ASR_PROVIDER=local
# ASR_LOCAL_BIN=whisper-cli
# ASR_LOCAL_ARGS=-m /opt/homebrew/share/whisper-cpp/ggml-small.bin -l zh -f {wav} --no-timestamps -otxt -of {wav}
# ASR_LOCAL_TEXT_PATH={wav}.txt
```

### 3. 工作原理

1. 适配器调用 `funasr_wrapper.sh`
2. wrapper 调用 `funasr_cli.py`
3. FunASR 加载模型并识别
4. **干净文本写入** `-of` 对应的 `{wav}.txt`（与 `ASR_LOCAL_TEXT_PATH={wav}.txt` 一致）

### 3.1 为何以前会把「版本 / 下载模型」混进识别结果？

`bbclaw-adapter` 的 `local` 模式会执行子进程，并取**转写结果**。历史上若**优先读 stdout**，而 FunASR / ModelScope 在加载时把 `funasr version`、`Downloading Model...` 打到 **stdout**，就会和真实识别文连在一起。

**当前行为**：优先读 **`{wav}.txt`**（与 whisper `-otxt`、FunASR CLI 写入的文件一致），**仅当没有该文件时才回退到 stdout**。因此只要 `funasr_cli.py` 把最终文本写入 `{of}.txt`，后台日志与发给 OpenClaw 的 `text` 都应是**纯识别文**。

若仍看到杂质，检查子进程是否把结果只打在 stdout 且未生成 `.txt` 文件。

### 4. 注意事项

- 首次下载模型时 ModelScope 可能在控制台输出进度；写入 `.txt` 的仍应为纯文本。
- **`ASR_PROVIDER=local` + 子进程 `funasr_cli`**：每条 `finish` 会拉起一次进程；若进程内**首次**拉 SenseVoice，会在**该次请求内**下载模型，耗时长、占内存，容易触发 **adapter 超时**、**设备端 HTTP 超时** 或 **系统 OOM**，日志里可能出现 **`signal: killed`**（多为 **SIGKILL**）。**并非「识别错了」，而是冷启动 + 下载未在超时内完成。**
- **推荐**：见下一节 **常驻 HTTP 服务**，启动时完成下载与加载，请求只做推理。

### 5. FunASR 常驻 HTTP 服务（推荐）

`asr/funasr_server.py` 在进程内 **warm** 一次模型（含首次 ModelScope 下载），对外提供 **OpenAI 兼容** 接口，与 `bbclaw-adapter` 的 **`ASR_PROVIDER=openai_compatible`** 对接。

**启动（在单独终端执行，直到出现 `Ready`）：**

```bash
cd bbclaw/asr
source .venv/bin/activate
pip install -r requirements.txt   # funasr / torch；HTTP 服务仅用标准库，无需 flask
export FUNASR_LANGUAGE=auto       # 中英混合同一句用 auto；仅普通话可 zn（或 zh/cn，服务端会映射为 zn）
python funasr_server.py --host 127.0.0.1 --port 18081
# 或使用 ./run_funasr_server.sh
```

**适配器 `src/.env`：**

```bash
ASR_PROVIDER=openai_compatible
ASR_BASE_URL=http://127.0.0.1:18081
ASR_API_KEY=dummy
ASR_MODEL=funasr
```

启动顺序：**先**等 FunASR 服务打印 **Ready**（首次可能要几分钟下载），**再**启动 `bbclaw-adapter`。readiness 会对 `GET /v1/models` 探活。

**仅 Paraformer 中文**：`python funasr_server.py -m paraformer-zh`（或环境变量 `FUNASR_SERVER_MODEL=paraformer-zh`）。

实现说明：推理逻辑在 **`funasr_core.py`**，`funasr_cli.py` 与子进程模式共用同一套加载与 `transcribe_file`，避免两套脚本漂移。

## 其他本地 ASR 方案

### Faster-Whisper (Python 版)

```bash
# 需要创建虚拟环境
python3 -m venv ~/whisper-venv
source ~/whisper-venv/bin/activate
pip install faster-whisper
```

```python
from faster_whisper import WhisperModel

model = WhisperModel("small", device="auto")
segments, info = model.transcribe("audio.wav", language="zh")

for seg in segments:
    print(seg.text)
```

## 常见问题

### Q: 如何启用中英文双语识别？

A: 见 **「启用双语支持（中英）」**。FunASR 路线：SenseVoice + **`ASR_LOCAL_ARGS` 含 `-l auto`**；不要用 **`-l zh`** 又期望英文词稳定。若只要中文，用 **`-m paraformer-zh -l zh`**。

### Q: 模型下载失败

A: 尝试使用镜像源:
```bash
curl -L -o ggml-small.bin "https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-small.bin"
```

### Q: 识别结果里混进 FunASR 版本号、下载日志

A: 见上文 **§3.1**；确认 `ASR_LOCAL_ARGS` 含 `-of {wav}` 且 `funasr_cli.py` 写入 `{wav}.txt`。适配器优先读该文件。

### Q: 日志里 `asr_failed` / `signal: killed`，且 stderr 里有 ModelScope Downloading

A: 多为 **首次拉 SenseVoice 时在一次请求里下载模型**，耗时长、占内存，被 **超时或 OOM** 杀掉。请先单独跑 **`funasr_server.py` 直到 Ready**，或改用 **§5 常驻服务**；或本地先手动跑一次识别完成缓存。

### Q: 识别结果为空

A: 检查音频格式，需要 16kHz, mono, WAV 格式:
```bash
ffmpeg -i input.mp3 -ar 16000 -ac 1 output.wav
```

### Q: 内存不足

A: 使用更小的模型 (tiny/base)，或关闭 Metal 加速:
```bash
whisper-cli -m ... --cpu
```

## 相关文档

- [协议规范](./protocol_specs.md)
- [硬件 BOM](./hardware_bom.md)
- [OpenClaw 集成计划](./openclaw_integration_plan.md)
- 中英文双语识别：本文 **「启用双语支持（中英）」**；适配器侧亦见 `src/README.md`（Provider modes 下 bilingual 说明）

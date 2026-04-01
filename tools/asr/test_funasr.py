#!/usr/bin/env python3
"""
FunASR 测试脚本
"""

from funasr import AutoModel

# 使用 Paraformer 模型 (中文效果最好)
# 首次运行会自动下载模型
model = AutoModel(
    model="paraformer-zh",
    model_revision="v2.0.4"
)

# 识别音频
audio_file = "/opt/homebrew/share/whisper-cpp/jfk.wav"

result = model.generate(audio_file)
print("识别结果:", result)

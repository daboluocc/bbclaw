#!/usr/bin/env python3
"""
FunASR 热词测试
"""

from funasr import AutoModel

# 创建热词文件
hotword_content = """阿虾
bbclaw
OpenClaw
运维开发
智慧园区
"""

with open("/Volumes/1TB/github/bbclaw/asr/hotword.txt", "w") as f:
    f.write(hotword_content)

# 使用热词模型
model = AutoModel(
    model="paraformer-zh",
    model_revision="v2.0.4",
    hotword="/Volumes/1TB/github/bbclaw/asr/hotword.txt"
)

# 测试识别
result = model.generate("/tmp/cn_test.wav")
print("带热词识别结果:", result[0]['text'])

#!/usr/bin/env python3
"""
FunASR CLI — 兼容 bbclaw-adapter 的 whisper 风格参数。
默认使用 SenseVoiceSmall（中英日韩粤等，language=auto 适合「你好 + hello」混合场景）。
指定 -m paraformer-zh 则仅用旧版中文 Paraformer。
用法: funasr_cli.py -m dummy -f audio.wav -l auto --no-timestamps -otxt -of output_prefix
"""

import argparse

from funasr_core import transcribe_file

def main():
    parser = argparse.ArgumentParser(description="FunASR CLI for bbclaw-adapter")
    parser.add_argument("-m", "--model", required=True, help="paraformer-zh 或任意其它(默认走 SenseVoice)")
    parser.add_argument("-f", "--file", required=True, help="Input audio file")
    parser.add_argument("-l", "--language", default="auto", help="SenseVoice: auto|zh|en|yue|ja|ko|nospeech")
    parser.add_argument("--no-timestamps", action="store_true", help="No timestamps")
    parser.add_argument("-otxt", "--output-txt", action="store_true", help="Output text file")
    parser.add_argument("-of", "--output-file", required=True, help="Output path prefix (extension added)")

    args = parser.parse_args()

    text = transcribe_file(args.file, language=args.language, model_arg=args.model)

    output_path = f"{args.output_file}.txt"
    with open(output_path, "w", encoding="utf-8") as f:
        f.write(text)


if __name__ == "__main__":
    main()

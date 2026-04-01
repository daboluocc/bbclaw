"""
FunASR 推理核心：CLI 与常驻 HTTP 服务共用，进程内单例加载模型。
"""

from __future__ import annotations

import os
import sys
from typing import Optional, Tuple

from funasr import AutoModel

try:
    from funasr.utils.postprocess_utils import rich_transcription_postprocess
except ImportError:

    def rich_transcription_postprocess(text: str) -> str:
        return text


_bundle: Optional[Tuple[object, str]] = None  # (AutoModel, "sensevoice"|"paraformer")


def want_paraformer(model_arg: str) -> bool:
    x = (model_arg or "").lower().strip()
    return x in ("paraformer-zh", "paraformer_zh", "paraformer")


def get_model_bundle(model_arg: str = "dummy") -> Tuple[object, str]:
    """单例加载模型；首次调用会触发 ModelScope 下载（SenseVoice 体积较大）。"""
    global _bundle
    if _bundle is not None:
        return _bundle

    if want_paraformer(model_arg):
        print("Loading FunASR: paraformer-zh (Chinese-centric)", file=sys.stderr)
        m = AutoModel(model="paraformer-zh", model_revision="v2.0.4", disable_update=True)
        _bundle = (m, "paraformer")
        return _bundle

    print("Loading FunASR: iic/SenseVoiceSmall (zh/en/yue/ja/ko)", file=sys.stderr)
    device = os.environ.get("FUNASR_DEVICE", "").strip() or None
    kwargs: dict = {"model": "iic/SenseVoiceSmall", "disable_update": True}
    if device:
        kwargs["device"] = device
    m = AutoModel(**kwargs)
    _bundle = (m, "sensevoice")
    return _bundle


def transcribe_file(
    audio_path: str,
    *,
    language: str = "auto",
    model_arg: str = "dummy",
) -> str:
    model, kind = get_model_bundle(model_arg)
    if kind == "sensevoice":
        # SenseVoice 官方 language：auto | zn | en | yue | ja | ko | nospeech（没有 zh，普通话用 zn）
        # 同一段里中英混说请用 auto；仅中文可 zn；仅英文可 en
        lang = (language or "auto").strip().lower()
        if lang in ("zh", "cn"):
            lang = "zn"
        allowed = ("auto", "zn", "en", "yue", "ja", "ko", "nospeech")
        if lang not in allowed:
            print(f"funasr_core: unknown language={lang!r}, fallback to auto", file=sys.stderr)
            lang = "auto"
        res = model.generate(
            input=audio_path,
            cache={},
            language=lang,
            use_itn=True,
        )
    else:
        res = model.generate(audio_path)

    if not res or len(res) == 0:
        return ""
    text = res[0].get("text", "") or ""
    if kind == "sensevoice":
        text = rich_transcription_postprocess(text)
    return text


def warm(model_arg: str = "dummy") -> None:
    """启动时调用，完成下载与加载后再接请求。"""
    get_model_bundle(model_arg)

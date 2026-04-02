#!/usr/bin/env python3
"""
Generate LVGL C font using the same engine as https://lvgl.io/tools/fontconverter
(lv_font_conv npm package). No manual web UI.

Requires: Node.js (npx), curl, Python 3.

Source font: Noto Sans SC (SIL OFL) via @fontsource/noto-sans-sc on unpkg.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import urllib.request
from pathlib import Path

# Pin version for reproducible builds (CLI matches web converter family).
LV_FONT_CONV = "lv_font_conv@1.5.3"
FONTSOURCE_VER = "5.2.9"
WOFF_NAME = "noto-sans-sc-chinese-simplified-400-normal.woff"
WOFF_URL = f"https://unpkg.com/@fontsource/noto-sans-sc@{FONTSOURCE_VER}/files/{WOFF_NAME}"


def repo_root() -> Path:
    return Path(__file__).resolve().parent.parent


def load_symbols(chars_file: Path, subset: str, lite_limit: int) -> str:
    raw = chars_file.read_text(encoding="utf-8")
    lines = [ln for ln in raw.splitlines() if ln.strip() and not ln.strip().startswith("#")]
    text = "".join(lines)
    text = text.replace("\n", "").replace("\r", "")
    if subset == "full":
        return text
    if subset == "lite":
        return text[:lite_limit]
    raise ValueError(subset)


def ensure_font(cache_dir: Path) -> Path:
    cache_dir.mkdir(parents=True, exist_ok=True)
    dest = cache_dir / WOFF_NAME
    if dest.exists() and dest.stat().st_size > 100_000:
        return dest
    print(f"Downloading font -> {dest}", file=sys.stderr)
    urllib.request.urlretrieve(WOFF_URL, dest)  # noqa: S310 — fixed URL
    return dest


def main() -> int:
    root = repo_root()
    ap = argparse.ArgumentParser(description="Generate lv_font_bbclaw_cjk.c via lv_font_conv")
    ap.add_argument(
        "--chars",
        type=Path,
        default=root / "docs" / "lvgl_font_subset_chars.txt",
        help="UTF-8 char list (same as used for manual converter)",
    )
    ap.add_argument(
        "-o",
        "--output",
        type=Path,
        default=root / "generated" / "lv_font_bbclaw_cjk.c",
        help="Output C file path",
    )
    ap.add_argument("--size", type=int, default=int(os.environ.get("LV_FONT_SIZE", "16")))
    ap.add_argument("--bpp", type=int, default=int(os.environ.get("LV_FONT_BPP", "4")))
    ap.add_argument(
        "--subset",
        choices=("full", "lite"),
        default=os.environ.get("LV_FONT_SUBSET", "full"),
        help="full = all chars in file; lite = first N chars (smaller binary)",
    )
    ap.add_argument("--lite-limit", type=int, default=int(os.environ.get("LV_FONT_LITE_LIMIT", "1500")))
    ap.add_argument("--no-compress", action="store_true", help="Pass --no-compress to lv_font_conv")
    args = ap.parse_args()

    symbols = load_symbols(args.chars, args.subset, args.lite_limit)
    if not symbols:
        print("No symbols after parsing --chars file.", file=sys.stderr)
        return 1
    print(f"Symbols: {len(symbols)} codepoints (subset={args.subset})", file=sys.stderr)

    font_path = ensure_font(root / ".cache" / "fonts")
    args.output.parent.mkdir(parents=True, exist_ok=True)

    cmd = [
        "npx",
        "--yes",
        LV_FONT_CONV,
        "--font",
        str(font_path),
        "-r",
        "0x20-0x7F",
        "--symbols",
        symbols,
        "--size",
        str(args.size),
        "--bpp",
        str(args.bpp),
        "--format",
        "lvgl",
        "--lv-font-name",
        "lv_font_bbclaw_cjk",
        "-o",
        str(args.output),
    ]
    if args.no_compress:
        cmd.append("--no-compress")

    print("Running:", " ".join(cmd[:6]), "... --symbols <len=%d> ..." % len(symbols), file=sys.stderr)
    subprocess.run(cmd, check=True)
    print(f"Wrote {args.output} ({args.output.stat().st_size} bytes)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())

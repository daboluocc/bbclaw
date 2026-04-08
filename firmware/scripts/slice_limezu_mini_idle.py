#!/usr/bin/env python3
"""
Slice LimeZu "Free Mini Characters" Idle sheets: top row, 4 frames @ 32x32.

Expected layout (verified for Red/Blue/Green Idle): 128x64 PNG, 2 rows x 4 cols of 32x32 cells;
we export only y=0..31, x=0..127 as four PNGs per color.

Usage:
  python3 scripts/slice_limezu_mini_idle.py --src ~/Downloads/Free\\ Mini\\ Characters \\
      --out ui/lvgl/elements
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path


def repo_firmware_root() -> Path:
    return Path(__file__).resolve().parent.parent


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--src",
        type=Path,
        required=True,
        help="Path to extracted 'Free Mini Characters' folder (contains Red/, Green/, Blue/)",
    )
    ap.add_argument(
        "--out",
        type=Path,
        default=None,
        help="Output directory for PNGs (default: firmware/ui/lvgl/elements)",
    )
    args = ap.parse_args()
    out_dir = args.out if args.out is not None else repo_firmware_root() / "ui" / "lvgl" / "elements"
    out_dir.mkdir(parents=True, exist_ok=True)

    try:
        from PIL import Image
    except ImportError:
        print("Pillow required: pip install Pillow", file=sys.stderr)
        return 1

    colors = ("Red", "Blue", "Green")
    cell = 32
    frames = 4
    prefix_map = {"Red": "red", "Blue": "blue", "Green": "green"}

    src_root = args.src.resolve()
    for folder in colors:
        png_path = src_root / folder / f"{folder}_Idle.png"
        if not png_path.is_file():
            print(f"missing: {png_path}", file=sys.stderr)
            return 1
        im = Image.open(png_path).convert("RGBA")
        w, h = im.size
        if w < cell * frames or h < cell:
            print(f"unexpected size {w}x{h} for {png_path}", file=sys.stderr)
            return 1
        pre = prefix_map[folder]
        for i in range(frames):
            box = (i * cell, 0, (i + 1) * cell, cell)
            tile = im.crop(box)
            out_path = out_dir / f"{pre}_idle_{i}.png"
            tile.save(out_path, "PNG")
            print(f"wrote {out_path}")

    return 0


if __name__ == "__main__":
    sys.exit(main())

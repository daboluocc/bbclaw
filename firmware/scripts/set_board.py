#!/usr/bin/env python3
"""Switch BBClaw board in sdkconfig.

Usage: python3 scripts/set_board.py <board-name>
"""
import pathlib, re, sys

BOARD_MAP = {
    "breadboard": "BREADBOARD",
    "atk-dnesp32s3-box": "ATK_DNESP32S3_BOX",
}

def main():
    if len(sys.argv) < 2 or sys.argv[1] not in BOARD_MAP:
        print(f"Usage: set_board.py <{'|'.join(BOARD_MAP.keys())}>")
        sys.exit(1)
    board = sys.argv[1]
    p = pathlib.Path("sdkconfig")
    t = p.read_text()
    for k, v in BOARD_MAP.items():
        sym = f"CONFIG_BBCLAW_BOARD_{v}"
        if k == board:
            t = re.sub(rf"(?m)^.*{sym}.*$", f"{sym}=y", t)
        else:
            t = re.sub(rf"(?m)^.*{sym}.*$", f"# {sym} is not set", t)
    p.write_text(t)
    print(f"sdkconfig: board={board} — run: make reconfigure && make build")

if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Switch BBClaw board in sdkconfig.

Usage: python3 scripts/set_board.py <board-name>

Flips the Kconfig board choice, then applies boards/<name>/sdkconfig.board
line-by-line onto sdkconfig (ADR-040 §7) so per-board flash size / PSRAM
mode / partition-table differences switch together with the board.
"""
import pathlib, re, sys

BOARD_MAP = {
    "breadboard": "BREADBOARD",
    "bbclaw": "BBCLAW",
    "atk-dnesp32s3-box": "ATK_DNESP32S3_BOX",
    "waveshare-amoled-206": "WS_AMOLED_206",
    "lichuang-szp": "LICHUANG_SZP",
}

def apply_overlay(text: str, overlay_path: pathlib.Path) -> str:
    """Merge a sdkconfig.board overlay into sdkconfig text.

    Handles both `CONFIG_X=v` and `# CONFIG_X is not set` lines: an existing
    line for the same symbol is replaced in place, otherwise the line is
    appended at the end.
    """
    for line in overlay_path.read_text().splitlines():
        stripped = line.strip()
        m = re.match(r"^(CONFIG_[A-Za-z0-9_]+)=", stripped) or \
            re.match(r"^# (CONFIG_[A-Za-z0-9_]+) is not set$", stripped)
        if not m:
            continue
        sym = m.group(1)
        pattern = rf"(?m)^(?:{sym}=.*|# {sym} is not set)$"
        if re.search(pattern, text):
            text = re.sub(pattern, stripped, text)
        else:
            text += ("" if text.endswith("\n") else "\n") + stripped + "\n"
    return text

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
    overlay = pathlib.Path("boards") / board / "sdkconfig.board"
    if overlay.exists():
        t = apply_overlay(t, overlay)
    p.write_text(t)
    print(f"sdkconfig: board={board} (overlay: {overlay if overlay.exists() else 'none'}) — run: make reconfigure && make build")

if __name__ == "__main__":
    main()

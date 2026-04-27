#!/usr/bin/env python3
"""Boot-log verification harness for BBClaw firmware releases.

Reads the device's USB serial port (after triggering a DTR/RTS reset) and
asserts that boot proceeds through a known set of milestone log lines. This
script does NOT flash anything — flashing is reserved as a manual step.

Typical usage (after `make flash`):

    ./scripts/verify_release.py
    ./scripts/verify_release.py --port /dev/cu.usbmodem2112401
    ./scripts/verify_release.py --timeout 45

Exit codes:
    0 — all assertions seen within timeout
    1 — one or more assertions missing (or no port found / serial error)

Requires: pyserial (`pip3 install pyserial`).
"""

import argparse
import glob
import re
import sys
import time

try:
    import serial  # type: ignore
except ImportError:
    sys.stderr.write(
        "error: pyserial not installed.\n"
        "       install with: pip3 install pyserial\n"
    )
    sys.exit(1)


# Substring assertions to look for in boot log. Order is for display only;
# matching is line-independent (any line containing the substring counts).
ASSERTIONS = [
    ("bootloader-second-stage",   "2nd stage bootloader"),
    ("app-init",                  "app_init: Project name:     bbclaw_firmware"),
    ("theme-text-only-registered","bb_agent_theme: register: 'text-only'"),
    ("theme-buddy-registered",    "bb_agent_theme: register: 'buddy-ascii'"),
    ("active-theme-resolved",     "bb_radio_app: boot active theme:"),
    ("nav-init-flipper6",         "bb_nav_input: nav init mode=flipper6"),
    ("wifi-connected",            "bb_wifi: wifi connected"),
    ("transport-health",          "bb_radio_app: transport health ok status=200"),
]

# ANSI CSI escape sequences (color codes etc.) — strip before substring match.
ANSI_RE = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")

# Capture-tail size shown on failure for context.
TAIL_LINES = 20


def strip_ansi(s: str) -> str:
    return ANSI_RE.sub("", s)


def auto_detect_port() -> str | None:
    """Return first matching /dev/cu.usbmodem* (macOS) or None."""
    candidates = sorted(glob.glob("/dev/cu.usbmodem*"))
    if candidates:
        return candidates[0]
    # Linux fallback — never hurts to look.
    candidates = sorted(glob.glob("/dev/ttyUSB*") + glob.glob("/dev/ttyACM*"))
    return candidates[0] if candidates else None


def reset_device(ser: "serial.Serial") -> None:
    """Toggle DTR/RTS to trigger an ESP32 reset.

    Mirrors the bb_radio_app session pattern:
        DTR=False, RTS=True, brief pause, then RTS=False.
    """
    ser.setDTR(False)
    ser.setRTS(True)
    time.sleep(0.1)
    ser.setRTS(False)
    time.sleep(0.05)


def run(port: str, baud: int, timeout_s: float) -> int:
    pending = {aid: substr for aid, substr in ASSERTIONS}
    matched: dict[str, str] = {}
    captured: list[str] = []

    print(f"verify_release: port={port} baud={baud} timeout={timeout_s:.0f}s")
    try:
        ser = serial.Serial(port, baud, timeout=0.5)
    except serial.SerialException as e:
        sys.stderr.write(f"error: cannot open {port}: {e}\n")
        return 1

    with ser:
        reset_device(ser)
        deadline = time.time() + timeout_s
        buf = b""
        while time.time() < deadline and pending:
            chunk = ser.read(4096)
            if not chunk:
                continue
            buf += chunk
            while b"\n" in buf:
                line_b, buf = buf.split(b"\n", 1)
                try:
                    line = line_b.decode("utf-8", errors="replace")
                except Exception:
                    line = repr(line_b)
                line = strip_ansi(line).rstrip("\r")
                captured.append(line)
                # Try to match every still-pending assertion.
                hit = [aid for aid, sub in pending.items() if sub in line]
                for aid in hit:
                    matched[aid] = line
                    pending.pop(aid, None)

    elapsed = timeout_s - max(0.0, deadline - time.time())
    print()
    for aid, substr in ASSERTIONS:
        if aid in matched:
            print(f"  [OK]   {aid:32s} <- {substr!r}")
        else:
            print(f"  [MISS] {aid:32s} <- {substr!r}")

    if pending:
        print()
        print(f"release verification FAILED: {len(pending)}/{len(ASSERTIONS)} missing after {elapsed:.1f}s")
        print(f"missing: {', '.join(sorted(pending.keys()))}")
        print()
        print(f"--- last {min(TAIL_LINES, len(captured))} lines of capture ---")
        for line in captured[-TAIL_LINES:]:
            print(line)
        print("--- end capture ---")
        return 1

    print()
    print(f"release verification PASSED in {elapsed:.1f}s")
    return 0


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="verify_release.py",
        description="Validate BBClaw firmware boot via USB serial log assertions. "
                    "Does NOT flash — run after `make flash`.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    p.add_argument(
        "--port",
        default=None,
        help="Serial port (default: auto-detect /dev/cu.usbmodem* on macOS, "
             "/dev/ttyUSB* or /dev/ttyACM* on Linux).",
    )
    p.add_argument(
        "--baud",
        type=int,
        default=115200,
        help="Serial baud rate (default: 115200).",
    )
    p.add_argument(
        "--timeout",
        type=float,
        default=30.0,
        help="Total wait time in seconds (default: 30).",
    )
    return p.parse_args()


def main() -> int:
    args = parse_args()
    port = args.port or auto_detect_port()
    if not port:
        sys.stderr.write(
            "error: no USB serial port found.\n"
            "       plug in the device, or pass --port /dev/cu.usbmodemXXX\n"
        )
        return 1
    return run(port, args.baud, args.timeout)


if __name__ == "__main__":
    sys.exit(main())

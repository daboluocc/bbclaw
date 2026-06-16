#!/usr/bin/env python3
"""Capture ESP_LOG console output from a BBClaw *bench* board over UART0.

Bench debug boards (CH340 -> UART0, no TinyUSB CDC1) cannot do the binary
screenshot/key protocol of the device-monitor skill. The only debug channel is
plain-text UART0 at 115200, which carries both ESP_LOG output AND the text
command channel (see bb_uart_cmd.c). This script opens the port, reads for N
seconds via a background reader thread, and prints (optionally regex-filtered)
lines. Pairs with uartcmd_inject.py for the inject-and-observe loop.

Usage:
  python3 scripts/uartcmd_capture.py --seconds 10
  python3 scripts/uartcmd_capture.py --seconds 12 --grep "site fetch|adapter|WS"
  python3 scripts/uartcmd_capture.py --port /dev/cu.usbserial-21130 --seconds 8 --raw

Notes:
  - Default port auto-discovers /dev/cu.usbserial-* (the CH340 console).
  - This board has NO working RTS/EN auto-reset; this script does NOT try to
    reboot. To capture from boot t=0, re-flash (`make flash`) or physically
    press RESET, then run this immediately.
  - Idle boards only emit `bb_button_test: gpioX stable raw=N` every ~2s — that
    is the background diagnostic task, not a hang. Connection logs (WiFi/WS/
    adapter) only appear during boot.
"""
import argparse
import glob
import re
import sys
import threading
import time


def pick_port():
    m = sorted(glob.glob("/dev/cu.usbserial-*"))
    if m:
        return m[0]
    # last resort: anything that looks like a usbserial bridge
    m = sorted(glob.glob("/dev/tty.usbserial-*"))
    return m[0] if m else None


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", default=None,
                    help="serial port (default: auto /dev/cu.usbserial-*)")
    ap.add_argument("--baud", type=int, default=115200)
    ap.add_argument("--seconds", type=float, default=10.0,
                    help="how long to capture")
    ap.add_argument("--grep", default=None,
                    help="regex; only matching lines are printed at the end")
    ap.add_argument("--raw", action="store_true",
                    help="stream every line live instead of buffering")
    args = ap.parse_args()

    try:
        import serial
    except ImportError:
        sys.exit("pyserial missing; use the IDF venv python (see CLAUDE.md)")

    port = args.port or pick_port()
    if not port:
        sys.exit("no serial port found (looked for /dev/cu.usbserial-*)")

    pat = re.compile(args.grep) if args.grep else None

    try:
        ser = serial.Serial(port, args.baud, timeout=0.3)
    except Exception as e:
        sys.exit(f"OPEN-FAIL {port}: {e} (device busy? close make monitor / other captures)")

    lines = []
    buf = b""
    stop = threading.Event()

    def reader():
        nonlocal buf
        while not stop.is_set():
            d = ser.read(4096)
            if not d:
                continue
            buf += d
            while b"\n" in buf:
                raw, buf = buf.split(b"\n", 1)
                line = raw.decode("utf-8", "replace").rstrip("\r")
                lines.append(line)
                if args.raw:
                    print(line, flush=True)

    t = threading.Thread(target=reader, daemon=True)
    t.start()

    end = time.time() + args.seconds
    try:
        while time.time() < end:
            time.sleep(0.1)
    except KeyboardInterrupt:
        pass
    stop.set()
    t.join(timeout=1.0)
    ser.close()

    if not args.raw:
        out = [ln for ln in lines if pat.search(ln)] if pat else lines
        for ln in out:
            print(ln)

    sys.stderr.write(
        f"\n[uartcmd_capture] {len(lines)} lines from {port} over {args.seconds}s"
        + (f" ({len([l for l in lines if pat and pat.search(l)])} matched /{args.grep}/)" if pat else "")
        + "\n")
    if not lines:
        sys.stderr.write(
            "[uartcmd_capture] no bytes — device idle with no output / wrong baud / wrong port / "
            "port held by another reader\n")


if __name__ == "__main__":
    main()

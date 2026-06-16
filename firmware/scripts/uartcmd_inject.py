#!/usr/bin/env python3
"""Inject bench-board UART text commands and capture the reply, in one shot.

Bench debug boards expose a newline-terminated text command channel on UART0
(115200) handled by firmware/src/bb_uart_cmd.c (gated by
CONFIG_BBCLAW_DEVICE_MONITOR). This is the ONLY way to drive the UI / trigger
PTT / read heap on a bench board, since it has no TinyUSB CDC1 for the binary
device-monitor protocol.

Supported firmware commands (truth: bb_uart_cmd.c):
  key up|down|left|right|ok|back|ok-long   inject a navigation key event
  ptt down|up|tap [ms]                     inject PTT (tap default 200ms)
  heap                                     print internal/PSRAM heap watermarks
  help                                     list commands

This script opens the port with a background reader thread, sends each command
you pass (newline-terminated, flushed), waits ~0.9s between commands so UI
animations / fetches settle, then prints everything the device echoed back.

Usage:
  # drive from standby into Settings, then read heap
  python3 scripts/uartcmd_inject.py key ok key down key ok heap

  # use ';' to keep multi-word commands together, or quote them
  python3 scripts/uartcmd_inject.py "ptt tap 500" heap
  python3 scripts/uartcmd_inject.py --cmds "key ok; key down; key ok"

  # tune pacing / capture window
  python3 scripts/uartcmd_inject.py --gap 1.2 --tail 3 key ok key ok

There is NO reboot command in bb_uart_cmd (RTS/EN auto-reset is not wired on
this board) — to restart, re-flash or press RESET physically.
"""
import argparse
import glob
import sys
import threading
import time

VALID_KEYS = {"up", "down", "left", "right", "ok", "back", "ok-long"}
VALID_PTT = {"down", "up", "tap"}


def pick_port():
    m = sorted(glob.glob("/dev/cu.usbserial-*"))
    if m:
        return m[0]
    m = sorted(glob.glob("/dev/tty.usbserial-*"))
    return m[0] if m else None


def split_cmds(tokens, cmds_arg):
    """Build the command list from positional tokens and/or --cmds string.

    Positional words are joined and split on ';'. The --cmds string (if given)
    is split on ';' too. This lets both `key ok key down` and
    `--cmds "key ok; key down"` work. To pass a multi-word command as a single
    positional argument, quote it ("ptt tap 500").
    """
    out = []
    if tokens:
        joined = " ".join(tokens)
        out += [c.strip() for c in joined.split(";") if c.strip()]
    if cmds_arg:
        out += [c.strip() for c in cmds_arg.split(";") if c.strip()]
    # collapse: a positional sequence like ["key","ok","key","down"] joined to
    # "key ok key down" has no ';', so re-segment on the known command verbs.
    return resegment(out)


def resegment(items):
    verbs = {"key", "ptt", "heap", "help"}
    result = []
    for item in items:
        words = item.split()
        cur = []
        for w in words:
            if w in verbs and cur:
                result.append(" ".join(cur))
                cur = [w]
            else:
                cur.append(w)
        if cur:
            result.append(" ".join(cur))
    return result


def warn_unknown(cmd):
    w = cmd.split()
    if not w:
        return
    if w[0] == "key" and (len(w) < 2 or w[1] not in VALID_KEYS):
        sys.stderr.write(f"[uartcmd_inject] WARN: '{cmd}' bad key arg (valid: {sorted(VALID_KEYS)})\n")
    elif w[0] == "ptt" and (len(w) < 2 or w[1] not in VALID_PTT):
        sys.stderr.write(f"[uartcmd_inject] WARN: '{cmd}' bad ptt arg (valid: {sorted(VALID_PTT)})\n")
    elif w[0] not in {"key", "ptt", "heap", "help"}:
        sys.stderr.write(f"[uartcmd_inject] WARN: '{cmd}' unknown verb (valid: key/ptt/heap/help)\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", default=None,
                    help="serial port (default: auto /dev/cu.usbserial-*)")
    ap.add_argument("--baud", type=int, default=115200)
    ap.add_argument("--gap", type=float, default=0.9,
                    help="seconds to wait after each command (UI settle)")
    ap.add_argument("--tail", type=float, default=2.0,
                    help="extra seconds to keep reading after the last command")
    ap.add_argument("--cmds", default=None,
                    help="semicolon-separated commands, e.g. \"key ok; key down\"")
    ap.add_argument("cmd", nargs="*",
                    help="command words, e.g. key ok key down heap")
    args = ap.parse_args()

    commands = split_cmds(args.cmd, args.cmds)
    if not commands:
        ap.error("no commands given (e.g. `key ok key down heap` or --cmds \"...\")")

    try:
        import serial
    except ImportError:
        sys.exit("pyserial missing; use the IDF venv python (see CLAUDE.md)")

    port = args.port or pick_port()
    if not port:
        sys.exit("no serial port found (looked for /dev/cu.usbserial-*)")

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
                print(line, flush=True)

    t = threading.Thread(target=reader, daemon=True)
    t.start()

    ser.reset_input_buffer()
    for cmd in commands:
        warn_unknown(cmd)
        sys.stderr.write(f"[uartcmd_inject] >>> {cmd}\n")
        ser.write((cmd + "\n").encode())
        ser.flush()
        time.sleep(args.gap)

    # keep reading a bit so the last command's async log lands
    end = time.time() + args.tail
    while time.time() < end:
        time.sleep(0.1)

    stop.set()
    t.join(timeout=1.0)
    ser.close()
    sys.stderr.write(
        f"[uartcmd_inject] sent {len(commands)} cmd(s), captured {len(lines)} line(s) from {port}\n")


if __name__ == "__main__":
    main()

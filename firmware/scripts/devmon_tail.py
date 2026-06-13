#!/usr/bin/env python3
"""Read raw ESP32 serial logs without idf_monitor's TTY requirement.

idf_monitor refuses to run when stdin isn't a TTY (e.g. an AI agent running
it detached), so `make monitor-log` can't be driven headless. This reads the
serial port directly with pyserial and tees into the SAME standard cache file
(`firmware/.cache/idf-monitor.latest.log`) so the file-based log-viewing
convention still holds.

Usage:
  python3 scripts/devmon_tail.py --port /dev/cu.usbserial-21120 --seconds 15 --reset
  python3 scripts/devmon_tail.py            # auto-pick port, 10s, no reset

--reset pulses the auto-reset lines (RTS->EN, DTR->IO0) to reboot into the
app so the capture starts from boot t=0. No-op if the USB-serial adapter
lacks the auto-reset transistors.
"""
import argparse, glob, os, sys, time

DEFAULT_LOG = os.path.join(os.path.dirname(__file__), "..", ".cache", "idf-monitor.latest.log")


def pick_port():
    for pat in ("/dev/cu.usbserial-*", "/dev/cu.usbmodem*"):
        m = sorted(glob.glob(pat))
        if m:
            return m[0]
    return None


def hard_reset(ser):
    # esptool classic reset: IO0 high (normal boot), pulse EN low->high.
    ser.setDTR(False)   # IO0 = HIGH
    ser.setRTS(True)    # EN  = LOW  (hold in reset)
    time.sleep(0.1)
    ser.setRTS(False)   # EN  = HIGH (release -> boot app)
    time.sleep(0.05)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", default=None)
    ap.add_argument("--baud", type=int, default=115200)
    ap.add_argument("--seconds", type=float, default=10.0)
    ap.add_argument("--reset", action="store_true")
    ap.add_argument("--log", default=DEFAULT_LOG)
    ap.add_argument("--append", action="store_true", help="append instead of overwrite")
    args = ap.parse_args()

    try:
        import serial
    except ImportError:
        sys.exit("pyserial missing; use the IDF venv python (see CLAUDE.md)")

    port = args.port or pick_port()
    if not port:
        sys.exit("no serial port found (looked for /dev/cu.usbserial-* and usbmodem*)")

    log_path = os.path.abspath(args.log)
    os.makedirs(os.path.dirname(log_path), exist_ok=True)

    try:
        ser = serial.Serial(port, args.baud, timeout=0.3)
    except Exception as e:
        sys.exit(f"OPEN-FAIL {port}: {e}")

    if args.reset:
        hard_reset(ser)

    mode = "a" if args.append else "w"
    total = 0
    end = time.time() + args.seconds
    with open(log_path, mode, encoding="utf-8") as f:
        while time.time() < end:
            d = ser.read(4096)
            if d:
                total += len(d)
                txt = d.decode("utf-8", "replace")
                sys.stdout.write(txt)
                sys.stdout.flush()
                f.write(txt)
                f.flush()
    ser.close()
    sys.stderr.write(f"\n[devmon_tail] {total} bytes from {port} -> {log_path}\n")
    if total == 0:
        sys.stderr.write("[devmon_tail] no bytes — device idle / wrong baud / wrong port\n")


if __name__ == "__main__":
    main()

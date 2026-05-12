#!/usr/bin/env python3
"""Reboot the BBClaw device into ROM bootloader (download mode) via the
device monitor protocol — replaces the manual BOOT+RESET button gesture.

Workflow:
    1. Open CDC1 (TinyUSB protocol port)
    2. Send REQ_REBOOT_TO_BOOTLOADER frame (kind=0x07)
    3. Receive RES_REBOOT_ACK (kind=0x08) — best effort, may be cut off
    4. Device reboots; CDC1 disappears within ~200 ms
    5. usbmodem2124401 (USJ via ROM) appears within ~1 s — ready for esptool

Usage:
    ./devmon_reboot.py --port /dev/cu.usbmodem1234563
    ./devmon_reboot.py --port /dev/cu.usbmodem1234563 --wait-for-bootloader

Exits 0 on success, 1 on failure to receive ACK or device didn't enter ROM
bootloader within timeout.
"""

import argparse
import os
import struct
import sys
import time

import serial


MAGIC = b"\xBB\xC1"
KIND_REQ_REBOOT = 0x07
KIND_RES_REBOOT_ACK = 0x08
KIND_ERR = 0xFF
BOOTLOADER_PORT = "/dev/cu.usbmodem2124401"


def crc16_ccitt(data: bytes, seed: int = 0xFFFF) -> int:
    crc = seed
    for byte in data:
        crc ^= byte << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021) & 0xFFFF if crc & 0x8000 else (crc << 1) & 0xFFFF
    return crc


def build_frame(kind: int, seq: int, payload: bytes) -> bytes:
    header_tail = struct.pack("<BBIH", kind, 0, len(payload), seq)
    crc = crc16_ccitt(header_tail + payload)
    return MAGIC + header_tail + payload + struct.pack("<H", crc)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", required=True, help="CDC1 protocol port")
    ap.add_argument("--baud", type=int, default=460800)
    ap.add_argument("--seq", type=int, default=0x4242)
    ap.add_argument("--wait-for-bootloader", action="store_true",
                    help="block until usbmodem2124401 appears (ROM bootloader ready)")
    ap.add_argument("--wait-timeout", type=float, default=10.0)
    args = ap.parse_args()

    frame = build_frame(KIND_REQ_REBOOT, args.seq, b"")
    print(f"[reboot] port={args.port} seq={args.seq}")
    print(f"[reboot] tx REQ_REBOOT_TO_BOOTLOADER ({len(frame)} bytes)")

    try:
        with serial.Serial(args.port, args.baud, timeout=0.5) as ser:
            ser.reset_input_buffer()
            ser.write(frame)
            ser.flush()
            # Try to read ACK before USB tears down. Don't fail if we can't —
            # the device may reset before the ACK reaches us.
            try:
                buf = ser.read(64)
                if buf.startswith(MAGIC):
                    print(f"[reboot] rx ACK ({len(buf)} bytes header+payload)")
            except serial.SerialException:
                pass
    except serial.SerialException as e:
        print(f"[reboot] FAIL: could not open CDC1: {e}", file=sys.stderr)
        return 1

    if not args.wait_for_bootloader:
        print("[reboot] command sent; device should enter ROM bootloader shortly")
        return 0

    deadline = time.time() + args.wait_timeout
    while time.time() < deadline:
        if os.path.exists(BOOTLOADER_PORT):
            print(f"[reboot] OK: {BOOTLOADER_PORT} present, ready for esptool")
            return 0
        time.sleep(0.2)
    print(f"[reboot] FAIL: {BOOTLOADER_PORT} did not appear within {args.wait_timeout}s",
          file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())

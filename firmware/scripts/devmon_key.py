#!/usr/bin/env python3
"""Inject a navigation key event into the BBClaw firmware via the device
monitor protocol (ADR-015). Drives LVGL as if a physical button was pressed.

Usage:
    ./devmon_key.py --port /dev/cu.usbmodem1234563 ok
    ./devmon_key.py --port /dev/cu.usbmodem1234563 up
    ./devmon_key.py --port /dev/cu.usbmodem1234563 ok-long

Key names map to bb_nav_event_t values in firmware/include/bb_nav_input.h.
"""

import argparse
import struct
import sys
import time

import serial


MAGIC = b"\xBB\xC1"
KIND_REQ_INPUT = 0x05
KIND_RES_INPUT_ACK = 0x06
KIND_ERR = 0xFF

KEYS = {
    "up":      0,
    "down":    1,
    "left":    2,
    "right":   3,
    "ok":      4,
    "back":    5,
    "ok-long": 6,
    "pwr": 200,  # ADR-044 录音一键启停(与物理 PWR 键同通路)
}


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


def read_frame(ser: serial.Serial, deadline: float):
    state = 0
    header = b""
    payload = b""
    crc_bytes = b""
    payload_len = 0
    seq = 0
    kind = 0
    while True:
        remaining = deadline - time.time()
        if remaining <= 0:
            raise TimeoutError("no frame within deadline")
        ser.timeout = min(0.3, remaining)
        b = ser.read(1)
        if not b:
            continue
        byte = b[0]
        if state == 0:
            if byte == MAGIC[0]:
                state = 1
        elif state == 1:
            if byte == MAGIC[1]:
                state = 2
                header = b""
            elif byte != MAGIC[0]:
                state = 0
        elif state == 2:
            header += bytes([byte])
            if len(header) == 8:
                kind, _flags, payload_len, seq = struct.unpack("<BBIH", header)
                state = 4 if payload_len == 0 else 3
                payload = b""
        elif state == 3:
            payload += bytes([byte])
            if len(payload) == payload_len:
                state = 4
        elif state == 4:
            crc_bytes += bytes([byte])
            if len(crc_bytes) == 2:
                want = crc16_ccitt(header + payload)
                got = struct.unpack("<H", crc_bytes)[0]
                if want != got:
                    raise ValueError(f"crc mismatch want={want:04x} got={got:04x}")
                return kind, seq, payload


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", required=True)
    ap.add_argument("--baud", type=int, default=460800)
    ap.add_argument("--seq", type=int, default=0x4242)
    ap.add_argument("--timeout", type=float, default=2.0)
    ap.add_argument("key", choices=sorted(KEYS.keys()),
                    help="key name to inject")
    args = ap.parse_args()

    event_id = KEYS[args.key]
    frame = build_frame(KIND_REQ_INPUT, args.seq, bytes([event_id]))
    print(f"[key] port={args.port} key={args.key} event_id={event_id}")

    with serial.Serial(args.port, args.baud, timeout=0.3) as ser:
        ser.reset_input_buffer()
        ser.write(frame)
        ser.flush()
        deadline = time.time() + args.timeout
        try:
            kind, seq, resp = read_frame(ser, deadline)
        except (TimeoutError, ValueError) as e:
            print(f"[key] FAIL: {e}", file=sys.stderr)
            return 1

    if kind == KIND_ERR:
        code = resp[0] if resp else -1
        print(f"[key] FAIL: device returned ERR code={code}", file=sys.stderr)
        return 1
    if kind != KIND_RES_INPUT_ACK:
        print(f"[key] FAIL: expected RES_INPUT_ACK (0x06), got 0x{kind:02x}",
              file=sys.stderr)
        return 1
    status = resp[0] if resp else -1
    print(f"[key] OK: injected {args.key} (status={status})")
    return 0


if __name__ == "__main__":
    sys.exit(main())

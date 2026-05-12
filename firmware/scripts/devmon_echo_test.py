#!/usr/bin/env python3
"""Round-trip echo test for the bb_device_monitor frame protocol (ADR-015).

Sends a REQ_ECHO frame over UART1 (external USB-UART module connected to
GPIO 38/39 on the BBClaw board) and verifies the device responds with a
matching RES_ECHO frame.

Usage:
    pip install pyserial   # one-time
    ./devmon_echo_test.py --port /dev/cu.usbserial-21230
    ./devmon_echo_test.py --port /dev/cu.usbserial-21230 --payload "hello world"

Exits 0 on round-trip OK, 1 otherwise.
"""

import argparse
import struct
import sys
import time

import serial  # pyserial


MAGIC = b"\xBB\xC1"
KIND_REQ_ECHO = 0x01
KIND_RES_ECHO = 0x02
KIND_ERR = 0xFF


def crc16_ccitt(data: bytes, seed: int = 0xFFFF) -> int:
    crc = seed
    for byte in data:
        crc ^= byte << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021) & 0xFFFF if crc & 0x8000 else (crc << 1) & 0xFFFF
    return crc


def build_frame(kind: int, seq: int, payload: bytes) -> bytes:
    if len(payload) > 0xFFFFFFFF:
        raise ValueError("payload too large")
    # Frame v2 (Phase 4): kind, flags, len_u32, seq_u16 — 8 bytes after magic.
    header_tail = struct.pack("<BBIH", kind, 0, len(payload), seq)
    crc = crc16_ccitt(header_tail + payload)
    return MAGIC + header_tail + payload + struct.pack("<H", crc)


def read_frame(ser: serial.Serial, deadline: float) -> tuple[int, int, bytes]:
    """Block until we either get a full frame or run out of time.

    Returns (kind, seq, payload). Raises TimeoutError if no frame arrives in
    time, or ValueError on CRC mismatch / unrecognized magic stream.
    """
    state = 0  # 0=WAIT_M0, 1=WAIT_M1, 2=HEADER, 3=PAYLOAD, 4=CRC
    header = b""
    payload_len = 0
    payload = b""
    crc_bytes = b""
    seq = 0
    kind = 0

    while True:
        remaining = deadline - time.time()
        if remaining <= 0:
            raise TimeoutError("no frame within deadline")
        ser.timeout = min(0.2, remaining)
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
            elif byte == MAGIC[0]:
                pass  # stay
            else:
                state = 0
        elif state == 2:
            header += bytes([byte])
            if len(header) == 8:
                kind, _flags, payload_len, seq = struct.unpack("<BBIH", header)
                if payload_len == 0:
                    state = 4
                    crc_bytes = b""
                else:
                    state = 3
                    payload = b""
        elif state == 3:
            payload += bytes([byte])
            if len(payload) == payload_len:
                state = 4
                crc_bytes = b""
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
    ap.add_argument("--port", required=True, help="UART1 tty (e.g. /dev/cu.usbserial-21230)")
    ap.add_argument("--baud", type=int, default=460800)
    ap.add_argument("--payload", default="ping", help="test payload (will be utf-8 encoded)")
    ap.add_argument("--seq", type=int, default=0x4242)
    ap.add_argument("--timeout", type=float, default=2.0)
    args = ap.parse_args()

    payload = args.payload.encode("utf-8")
    frame = build_frame(KIND_REQ_ECHO, args.seq, payload)

    print(f"[devmon] port={args.port} baud={args.baud}")
    print(f"[devmon] tx kind=REQ_ECHO seq={args.seq} payload={payload!r} ({len(frame)} bytes)")
    print(f"[devmon] tx hex: {frame.hex(' ')}")

    with serial.Serial(args.port, args.baud, timeout=0.2) as ser:
        ser.reset_input_buffer()
        ser.write(frame)
        ser.flush()

        deadline = time.time() + args.timeout
        try:
            kind, seq, resp = read_frame(ser, deadline)
        except (TimeoutError, ValueError) as e:
            print(f"[devmon] FAIL: {e}", file=sys.stderr)
            return 1

    print(f"[devmon] rx kind=0x{kind:02x} seq={seq} payload={resp!r}")

    if kind == KIND_ERR:
        err_code = resp[0] if resp else -1
        print(f"[devmon] FAIL: device returned ERR code={err_code}", file=sys.stderr)
        return 1
    if kind != KIND_RES_ECHO:
        print(f"[devmon] FAIL: expected RES_ECHO (0x02), got 0x{kind:02x}", file=sys.stderr)
        return 1
    if seq != args.seq:
        print(f"[devmon] FAIL: seq mismatch want={args.seq} got={seq}", file=sys.stderr)
        return 1
    if resp != payload:
        print(f"[devmon] FAIL: payload mismatch want={payload!r} got={resp!r}", file=sys.stderr)
        return 1

    print("[devmon] OK: echo round-trip verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())

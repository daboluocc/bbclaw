#!/usr/bin/env python3
"""Capture an LVGL screen snapshot from the BBClaw device over UART1.

Sends a REQ_SCREENSHOT (kind=0x03) frame to the device monitor, reads back
a RES_SCREENSHOT (kind=0x04) carrying [u16 width][u16 height][RGB565 pixels],
converts to PNG and writes to disk.

Requires pyserial and Pillow:
    pip install pyserial Pillow

Usage:
    ./devmon_screenshot.py --port /dev/cu.usbserial-21230
    ./devmon_screenshot.py --port /dev/cu.usbserial-21230 -o /tmp/foo.png
"""

import argparse
import datetime
import os
import struct
import sys
import time

import serial


MAGIC = b"\xBB\xC1"
KIND_REQ_SCREENSHOT = 0x03
KIND_RES_SCREENSHOT = 0x04
KIND_ERR = 0xFF


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


def read_frame(ser: serial.Serial, deadline: float) -> tuple[int, int, bytes]:
    """Block until a complete frame arrives (or deadline). Returns (kind, seq, payload)."""
    state = 0  # 0=M0, 1=M1, 2=HEADER, 3=PAYLOAD, 4=CRC
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
        ser.timeout = min(0.5, remaining)
        # Read in chunks for speed once we know payload_len (big screenshot).
        if state == 3 and payload_len - len(payload) > 1:
            want = min(4096, payload_len - len(payload), int(remaining * 200000) or 1)
            chunk = ser.read(want)
            if not chunk:
                continue
            payload += chunk
            if len(payload) == payload_len:
                state = 4
                crc_bytes = b""
            continue
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
                pass
            else:
                state = 0
        elif state == 2:
            header += bytes([byte])
            if len(header) == 8:
                kind, _flags, payload_len, seq = struct.unpack("<BBIH", header)
                if payload_len == 0:
                    state = 4
                else:
                    state = 3
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


def rgb565_to_png(payload: bytes, out_path: str) -> tuple[int, int]:
    width, height = struct.unpack_from("<HH", payload, 0)
    expected_pixel_bytes = width * height * 2
    pixels = payload[4 : 4 + expected_pixel_bytes]
    if len(pixels) != expected_pixel_bytes:
        raise ValueError(
            f"pixel byte count mismatch: header says {width}x{height}={expected_pixel_bytes}, "
            f"got {len(pixels)}"
        )

    try:
        import numpy as np
        from PIL import Image
    except ImportError as e:
        raise SystemExit(f"need Pillow + numpy: pip install Pillow numpy ({e})")

    arr = np.frombuffer(pixels, dtype="<u2").reshape(height, width)
    r = ((arr >> 11) & 0x1F) << 3
    g = ((arr >> 5) & 0x3F) << 2
    b = (arr & 0x1F) << 3
    # Replicate top bits into low bits for full 8-bit range (sRGB approximation).
    r |= r >> 5
    g |= g >> 6
    b |= b >> 5
    rgb = np.stack([r, g, b], axis=-1).astype("uint8")
    Image.fromarray(rgb).save(out_path)
    return width, height


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", required=True, help="UART1 tty (e.g. /dev/cu.usbserial-21230)")
    ap.add_argument("--baud", type=int, default=460800)
    ap.add_argument("-o", "--output", default=None, help="output PNG path")
    ap.add_argument("--timeout", type=float, default=10.0,
                    help="seconds to wait for the response (default 10)")
    ap.add_argument("--seq", type=int, default=0x4242)
    ap.add_argument("--retries", type=int, default=2,
                    help="retry count on transient device ERR (UI transitions)")
    args = ap.parse_args()

    out_path = args.output
    if out_path is None:
        ts = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
        out_path = f"./screenshot-{ts}.png"

    print(f"[snap] port={args.port} baud={args.baud}")

    kind = seq = 0
    resp = b""
    rtt = 0.0
    for attempt in range(1, args.retries + 2):
        frame = build_frame(KIND_REQ_SCREENSHOT, args.seq + attempt - 1, b"")
        print(f"[snap] attempt {attempt} tx REQ_SCREENSHOT seq={args.seq + attempt - 1}"
              f" ({len(frame)} bytes)")
        t0 = time.time()
        try:
            with serial.Serial(args.port, args.baud, timeout=0.5) as ser:
                ser.reset_input_buffer()
                ser.write(frame)
                ser.flush()
                deadline = time.time() + args.timeout
                kind, seq, resp = read_frame(ser, deadline)
        except (TimeoutError, ValueError) as e:
            print(f"[snap] attempt {attempt} FAIL: {e}", file=sys.stderr)
            if attempt > args.retries:
                return 1
            time.sleep(0.3)
            continue
        rtt = time.time() - t0
        print(f"[snap] attempt {attempt} rx kind=0x{kind:02x} seq={seq}"
              f" {len(resp)} bytes in {rtt:.2f}s")

        if kind == KIND_RES_SCREENSHOT:
            break
        err_code = resp[0] if (kind == KIND_ERR and resp) else -1
        print(f"[snap] attempt {attempt} device returned ERR code={err_code}",
              file=sys.stderr)
        if attempt > args.retries:
            return 1
        time.sleep(0.3)  # give UI a moment to settle (transition animations etc.)

    if kind != KIND_RES_SCREENSHOT:
        print(f"[snap] FAIL: gave up after {args.retries + 1} attempts",
              file=sys.stderr)
        return 1

    try:
        w, h = rgb565_to_png(resp, out_path)
    except (ValueError, SystemExit) as e:
        print(f"[snap] FAIL decoding: {e}", file=sys.stderr)
        return 1

    abs_path = os.path.abspath(out_path)
    print(f"[snap] OK: {w}x{h} → {abs_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

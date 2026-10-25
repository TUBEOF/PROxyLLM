#!/usr/bin/env python3
"""Generate real PNG test images with the Python standard library.

Usage: python3 -I make_png.py OUT.png [--seed N] [--size N]
"""

from __future__ import annotations

import argparse
import math
import struct
import zlib


def chunk(kind: bytes, data: bytes) -> bytes:
    return (
        struct.pack(">I", len(data))
        + kind
        + data
        + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)
    )


def make(path: str, size: int, seed: int) -> None:
    w = h = size
    rows = bytearray()
    for y in range(h):
        rows.append(0)  # filter type 0
        for x in range(w):
            # Dark neon background with a diagonal gradient and shapes.
            t = (x + y) / max(1, (w + h - 2))
            r = int(8 + 30 * t + 10 * math.sin((x + seed) / 37.0))
            g = int(10 + 180 * t + 20 * math.sin((y + seed) / 29.0))
            b = int(18 + 90 * (1 - t) + 15 * math.cos((x - y + seed) / 41.0))
            # A bright lime diagonal band.
            if abs((x - y) - (seed % 97) - 40) < 22:
                r, g, b = 205, 255, 20
            # A magenta/orange square block.
            bx = (seed * 53) % (w - 120) + 20
            by = (seed * 31) % (h - 120) + 20
            if bx <= x < bx + 100 and by <= y < by + 100:
                r, g, b = 255, 70 + (y - by), 140
            # A cyan circle.
            cx, cy = w // 2 + (seed % 60) - 30, h // 2
            if (x - cx) ** 2 + (y - cy) ** 2 < 70 ** 2:
                r, g, b = 20, 220, 230
            rows.extend((max(0, min(255, r)), max(0, min(255, g)), max(0, min(255, b))))
    ihdr = struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)
    png = (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(bytes(rows), 6))
        + chunk(b"IEND", b"")
    )
    with open(path, "wb") as f:
        f.write(png)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("out")
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--size", type=int, default=512)
    args = parser.parse_args()
    make(args.out, max(64, args.size), args.seed)
    print(args.out)


if __name__ == "__main__":
    main()

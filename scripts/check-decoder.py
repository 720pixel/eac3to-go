#!/usr/bin/env python3
"""Check a decoder build against the recorded Windows PCM fixtures."""

import argparse
import hashlib
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("decoder", type=Path)
parser.add_argument("--runner", help="optional Windows executable runner")
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
decoder = str(args.decoder.resolve())
cases = (
    ("ac3", 98304, "d5aba8cc0a2a6effb0796ab2c8020f4e07311480e4ca7fc2edea4a3b93239aa5"),
    ("dts", 81920, "307c35bf452de39e72a9e1116aac12dfd1b1fd54531b920deb0cb23877dfd663"),
)
for codec, size, digest in cases:
    source = root / "internal/app/testdata" / ("delay-stereo48." + codec)
    result = subprocess.run(
        ([args.runner] if args.runner else []) +
        [decoder, "-v", "error", "-nostdin", "-xerror", "-drc_scale", "0",
         "-err_detect", "crccheck+explode", "-i", str(source.relative_to(root)),
         "-c:a", "pcm_f64le", "-f", "f64le", "-"],
        cwd=root, check=True, capture_output=True, timeout=30,
    )
    actual = hashlib.sha256(result.stdout).hexdigest()
    if len(result.stdout) != size or actual != digest:
        raise SystemExit(f"{codec} decoder comparison failed: {len(result.stdout)} bytes, SHA256 {actual}")
    print(codec.upper(), "reference PCM matches", flush=True)

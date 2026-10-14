#!/usr/bin/env python3
"""Build the matched audio decoder beside the CLI, without installing system tools."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import urllib.request
import uuid

root = Path(__file__).resolve().parents[1]
package = root / "third_party/decoder-profile1"
archives = root / "build/archives"
archives.mkdir(parents=True, exist_ok=True)
inputs = json.loads((package / "inputs.json").read_text())


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


for name, record in inputs.items():
    if name.endswith(".patch"):
        continue
    path = archives / name
    if not path.exists() or digest(path) != record["sha256"]:
        temporary = path.with_name(path.name + "." + uuid.uuid4().hex + ".part")
        print("Downloading", name, flush=True)
        try:
            with urllib.request.urlopen(record["url"], timeout=60) as response, temporary.open("wb") as output:
                shutil.copyfileobj(response, output)
            if digest(temporary) != record["sha256"]:
                raise SystemExit("Checksum mismatch for " + name)
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)

build = root / "build" / ("decoder-profile1-" + uuid.uuid4().hex[:12])
subprocess.run([sys.executable, str(package / "build.py"), str(build), str(archives)], check=True)
subprocess.run([sys.executable, str(root / "scripts/check-decoder.py"), str(build / "eac3to-decoder")], check=True)
destination = root / "bin/libexec/eac3to-decoder"
destination.parent.mkdir(parents=True, exist_ok=True)
temporary = destination.with_name(destination.name + "." + uuid.uuid4().hex + ".tmp")
try:
    shutil.copy2(build / "eac3to-decoder", temporary)
    os.replace(temporary, destination)
finally:
    temporary.unlink(missing_ok=True)
print("Created", destination)
print("Source, build logs, and manifest:", build)

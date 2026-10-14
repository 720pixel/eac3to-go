#!/usr/bin/env python3
"""Build Windows amd64 executables with the native audio libraries enabled.

On Linux amd64, the default cross compiler is unpacked into build/reference-tools
from checksum-pinned Ubuntu 24.04 packages. It is never installed system-wide.
A compatible MinGW GCC/G++ pair can instead be selected with --cc and --cxx.
The bundled compiler requires a compatible Linux runtime (Ubuntu 24.04 or newer).
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def bootstrap():
    if platform.system() != "Linux" or platform.machine() not in ("x86_64", "amd64"):
        raise SystemExit("select a MinGW GCC/G++ toolchain with --cc and --cxx on this host")
    if not shutil.which("dpkg-deb"):
        raise SystemExit("dpkg-deb is required to unpack the isolated toolchain, or use --cc and --cxx")
    base = ROOT / "build/reference-tools/mingw"
    downloads = base / "downloads"
    downloads.mkdir(parents=True, exist_ok=True)
    records = json.loads((ROOT / "scripts/windows-toolchain.json").read_text())
    stamp = base / "inputs.json"
    for record in records["packages"]:
        path = downloads / record["file"]
        if not path.exists() or digest(path) != record["sha256"]:
            print("Downloading", record["file"], flush=True)
            with tempfile.NamedTemporaryFile(dir=downloads, delete=False) as f:
                temporary = Path(f.name)
                try:
                    with urllib.request.urlopen(record["url"], timeout=60) as response:
                        shutil.copyfileobj(response, f)
                    f.close()
                    if digest(temporary) != record["sha256"]:
                        raise SystemExit("checksum mismatch: " + record["file"])
                    os.replace(temporary, path)
                finally:
                    temporary.unlink(missing_ok=True)
    if not stamp.exists() or json.loads(stamp.read_text()) != records:
        for record in records["packages"]:
            subprocess.run(["dpkg-deb", "-x", str(downloads / record["file"]), str(base / "root")], check=True)
        stamp.write_text(json.dumps(records, indent=2) + "\n")
    prefix = base / "root/usr/bin"
    return str(prefix / "x86_64-w64-mingw32-gcc-posix"), str(prefix / "x86_64-w64-mingw32-g++-posix")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cc", default=os.environ.get("WINDOWS_CC"))
    parser.add_argument("--cxx", default=os.environ.get("WINDOWS_CXX"))
    parser.add_argument("--output", type=Path, default=ROOT / "bin/windows-amd64")
    parser.add_argument("--test", action="store_true", help="run native library and pipe tests after building")
    parser.add_argument("--test-runner", help="Windows executable runner, such as an explicit Wine path")
    args = parser.parse_args()
    if bool(args.cc) != bool(args.cxx):
        parser.error("--cc and --cxx must be supplied together")
    cc, cxx = (args.cc, args.cxx) if args.cc else bootstrap()
    env = dict(os.environ, GOOS="windows", GOARCH="amd64", CGO_ENABLED="1", CC=cc, CXX=cxx)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    flags = '-s -w -linkmode external -extldflags "-static"'
    manifest = {"target": "windows/amd64", "cgo": True,
                "go": subprocess.check_output(["go", "version"], text=True).strip(),
                "compiler": subprocess.check_output([cc, "--version"], text=True).splitlines()[0],
                "artifacts": {}}
    # Rebuild C/C++ objects because their vendored headers live outside their
    # Go package directories and are not all covered by Go's build cache.
    for name in ("eac3to", "pcmcompare"):
        target = output / (name + ".exe")
        subprocess.run(["go", "build", "-a", "-trimpath", "-buildvcs=false", "-ldflags=" + flags,
                        "-o", str(target), "./cmd/" + name], cwd=ROOT, env=env, check=True)
        manifest["artifacts"][target.name] = {"sha256": digest(target), "size": target.stat().st_size}
        print("Created", target, flush=True)
    if args.test:
        if os.name != "nt" and not args.test_runner:
            parser.error("--test on a non-Windows host requires --test-runner")
        for package in ("ssrc", "aften", "app"):
            binary = ROOT / "build" / ("windows-" + package + ".test.exe")
            subprocess.run(["go", "test", "-c", "-ldflags=" + flags, "-o", str(binary),
                            "./internal/" + package], cwd=ROOT, env=env, check=True)
            command = ([args.test_runner] if args.test_runner else []) + [str(binary), "-test.v"]
            if package == "app":
                command += ["-test.run=TestDemuxPipe"]
            subprocess.run(command, cwd=ROOT / "internal" / package, check=True)
    (output / "build-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print("Media operations need Windows FFmpeg and FFprobe. Select them with --ffmpeg/--ffprobe.")
    print("The optional matched decoder is built separately; this build does not substitute for it.")


if __name__ == "__main__":
    main()

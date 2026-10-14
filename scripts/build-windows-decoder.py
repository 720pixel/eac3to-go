#!/usr/bin/env python3
"""Cross-build the optional matched audio decoder for Windows amd64.

Run on Linux amd64 with Python 3.12+, make and a host C compiler. Downloads,
sources, logs and the cross compiler stay under build/. --runner selects Wine
for the reference PCM checks; without a runner the result stays in build/ and
is not copied beside the CLI.
"""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("windows_build", ROOT / "scripts/build-windows.py")
windows = importlib.util.module_from_spec(spec)
spec.loader.exec_module(windows)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cc", default=os.environ.get("WINDOWS_CC"))
    parser.add_argument("--runner", help="Windows executable runner used for reference checks")
    parser.add_argument("--output", type=Path, default=ROOT / "bin/windows-amd64/libexec/eac3to-decoder.exe")
    args = parser.parse_args()
    cc = args.cc or windows.bootstrap()[0]
    cc = str(Path(shutil.which(cc) or cc).resolve())
    prefix = Path(cc).parent
    package = ROOT / "third_party/decoder-profile1"
    records = json.loads((package / "inputs.json").read_text())
    archives = ROOT / "build/archives"
    archives.mkdir(parents=True, exist_ok=True)
    for name, record in records.items():
        path = (package if name.endswith(".patch") else archives) / name
        if not path.exists() or windows.digest(path) != record["sha256"]:
            if name.endswith(".patch"):
                raise SystemExit("patch checksum differs: " + name)
            temporary = path.with_name(path.name + "." + uuid.uuid4().hex + ".part")
            try:
                with urllib.request.urlopen(record["url"], timeout=60) as response, temporary.open("wb") as output:
                    shutil.copyfileobj(response, output)
                if windows.digest(temporary) != record["sha256"]:
                    raise SystemExit("archive checksum differs: " + name)
                os.replace(temporary, path)
            finally:
                temporary.unlink(missing_ok=True)
    out = ROOT / "build" / ("windows-decoder-profile1-" + uuid.uuid4().hex[:12])
    out.mkdir()
    commands = []

    def run(command, cwd, name):
        commands.append({"command": command, "cwd": str(cwd), "log": name})
        (out / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
        with (out / name).open("wb") as log:
            subprocess.run(command, cwd=cwd, stdout=log, stderr=subprocess.STDOUT, check=True)

    for name in ("ffmpeg-8.0.2.tar.xz", "nasm-2.16.03.tar.xz"):
        with tarfile.open(archives / name) as archive:
            archive.extractall(out, filter="data")
    source = out / "ffmpeg-8.0.2"
    nasm = out / "nasm-2.16.03"
    run(["git", "apply", str(package / "profile1.patch")], source, "patch.log")
    run([str(nasm / "configure"), "--prefix=" + str(out / "unused-prefix")], nasm, "nasm-configure.log")
    run(["make", "-j8", "nasm"], nasm, "nasm-build.log")
    build = out / "build"
    build.mkdir()
    # FFmpeg's src/configure convention avoids absolute source filenames in
    # diagnostics without changing the decoder or compiler arithmetic flags.
    (build / "src").symlink_to("../ffmpeg-8.0.2", target_is_directory=True)
    options = json.loads((package / "configure-options.json").read_text())
    options = [option + " -ffile-prefix-map=src=ffmpeg-8.0.2"
               if option.startswith("--extra-cflags=") else option for option in options]
    relative_cc = os.path.relpath(cc, build)
    options += ["--target-os=mingw32", "--arch=x86_64", "--enable-cross-compile",
                "--cc=" + relative_cc, "--ld=" + relative_cc, "--extra-ldflags=-static",
                "--x86asmexe=../nasm-2.16.03/nasm"]
    for tool in ("ar", "ranlib", "nm", "strip"):
        path = prefix / ("x86_64-w64-mingw32-" + tool)
        if not path.exists():
            raise SystemExit("cross tool not found: " + str(path))
        options.append("--" + tool + "=" + os.path.relpath(path, build))
    run(["src/configure", *options], build, "configure.log")
    run(["make", "-j8", "ffmpeg.exe"], build, "build.log")
    binary = build / "ffmpeg.exe"
    manifest = {"target": "windows/amd64", "profile": "eac3to-decoder-profile1",
                "sha256": windows.digest(binary), "source_inputs": records,
                "reference_pcm_checked": False, "configure": options}
    if args.runner or os.name == "nt":
        command = [sys.executable, str(ROOT / "scripts/check-decoder.py"), str(binary)]
        if args.runner:
            command += ["--runner", args.runner]
        run(command, ROOT, "reference-check.log")
        manifest["reference_pcm_checked"] = True
        destination = args.output.resolve()
        destination.parent.mkdir(parents=True, exist_ok=True)
        temporary = destination.with_name(destination.name + "." + uuid.uuid4().hex + ".tmp")
        try:
            shutil.copy2(binary, temporary)
            os.replace(temporary, destination)
        finally:
            temporary.unlink(missing_ok=True)
        print("Created and checked", destination)
    else:
        print("Built without runtime validation:", binary)
        print("Pass --runner to check the PCM fixtures and publish the decoder beside the CLI.")
    (out / "build-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    if manifest["reference_pcm_checked"]:
        record = args.output.resolve().parent / "build-manifest.json"
        temporary = record.with_name(record.name + "." + uuid.uuid4().hex + ".tmp")
        try:
            shutil.copy2(out / "build-manifest.json", temporary)
            os.replace(temporary, record)
        finally:
            temporary.unlink(missing_ok=True)
    print("Build logs:", out)


if __name__ == "__main__":
    main()

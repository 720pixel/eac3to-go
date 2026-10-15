#!/usr/bin/env python3
"""Package verified native executables, notices and their rebuild sources."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import struct
import subprocess
import tarfile
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]
SOURCE_TOP = {"Makefile", "README", "LICENSE", "COPYING", "go.mod", "go.sum"}
SOURCE_DIRS = {"cmd", "internal", "scripts", "third_party"}
NOTICES = {
    "ulikunitz-xz": ("LICENSE", "NOTICE"),
    "ssrc": ("COPYING", "NOTICE", "README.original.txt"),
    "aften": ("COPYING", "NOTICE"),
    "decoder-profile1": ("COPYING.LGPLv2.1", "COPYING.LGPLv3", "LICENSE.ffmpeg", "NASM-LICENSE", "NOTICE"),
}


def digest(path):
    with Path(path).open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def relative_name(value):
    parts = value.split("/")
    reserved = {"CON", "PRN", "AUX", "NUL"} | {f"{p}{n}" for p in ("COM", "LPT") for n in range(1, 10)}
    if any(not re.fullmatch(r"[A-Za-z0-9_.+-]+", p) or p in (".", "..") or
           p.endswith(".") or p.split(".", 1)[0].upper() in reserved for p in parts):
        raise ValueError("invalid portable path: " + value)
    return PurePosixPath(*parts)


def executable_architecture(path, target):
    with path.open("rb") as source:
        header = source.read(64)
        if target == "linux-amd64":
            valid = len(header) >= 20 and header[:6] == b"\x7fELF\x02\x01"
            valid = valid and struct.unpack_from("<H", header, 18)[0] == 62
        else:
            valid = len(header) == 64 and header[:2] == b"MZ"
            if valid:
                offset = struct.unpack_from("<I", header, 60)[0]
                valid = 64 <= offset <= 16 << 20
                if valid:
                    source.seek(offset)
                    valid = source.read(6) == b"PE\0\0\x64\x86"
    if not valid:
        raise ValueError(path.name + " is not a " + target + " executable")


def public_text(data, label):
    text = data.decode("utf-8", errors="replace")
    if re.search(r"(?:/home/[A-Za-z0-9_.-]+/|(?<![A-Za-z0-9_.-])/root/[A-Za-z0-9_.-]+/|[A-Za-z]:[\\/]Users[\\/][A-Za-z0-9_.-]+[\\/])", text):
        raise ValueError(label + " contains a private build path")


def source_files(root, extra):
    names = subprocess.check_output(["git", "ls-files", "-z"], cwd=root).decode().split("\0")
    names = set(filter(None, names)) | set(extra)
    pending = subprocess.check_output(["git", "ls-files", "-z", "--others", "--exclude-standard"], cwd=root).decode().split("\0")
    for name in filter(None, pending):
        relative = relative_name(name)
        is_source = relative.parts[0] in SOURCE_DIRS or str(relative) in SOURCE_TOP
        if is_source and relative.suffix.lower() != ".md" and name not in names:
            raise ValueError("untracked source needs --source-extra or git add: " + name)
    result = []
    for name in sorted(names):
        relative = relative_name(name)
        if relative.suffix.lower() == ".md":
            continue
        if relative.parts[0] not in SOURCE_DIRS and str(relative) not in SOURCE_TOP:
            continue
        path = root.joinpath(*relative.parts)
        if path.is_symlink() or not path.is_file() or not path.resolve().is_relative_to(root.resolve()):
            raise ValueError("source is not a regular file: " + name)
        public_text(path.read_bytes(), "source " + name)
        result.append((path, relative, bool(path.stat().st_mode & 0o111)))
    required = {"go.mod", "Makefile", "cmd/eac3to/main.go", "cmd/pcmcompare/main.go",
                "scripts/build-decoder.py", "scripts/build-windows-decoder.py",
                "third_party/decoder-profile1/build.py", "third_party/decoder-profile1/profile1.patch"}
    if not required.issubset({str(p) for _, p, _ in result}):
        raise ValueError("source checkout is missing required build files")
    return result


def tar_archive(destination, entries, prefix):
    with destination.open("wb") as output, gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
            for source, relative, executable in sorted(entries, key=lambda entry: str(entry[1])):
                info = tarfile.TarInfo(prefix + "/" + str(relative))
                info.size = source.stat().st_size
                info.mode = 0o755 if executable else 0o644
                info.mtime = 0
                with source.open("rb") as data:
                    archive.addfile(info, data)


def zip_archive(destination, entries, prefix):
    with zipfile.ZipFile(destination, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for source, relative, executable in sorted(entries, key=lambda entry: str(entry[1])):
            info = zipfile.ZipInfo(prefix + "/" + str(relative), date_time=(1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.external_attr = (0o100755 if executable else 0o100644) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            with source.open("rb") as data, archive.open(info, "w", force_zip64=True) as output:
                shutil.copyfileobj(data, output, 1 << 20)


def usage(target):
    command = "eac3to.exe" if target == "windows-amd64" else "./eac3to"
    runtime = ("Windows amd64. Runtime checks were performed with Wine; physical Windows\n"
               "hardware has not been tested for this release.\n" if target == "windows-amd64" else
               "Linux amd64. The main programs are static. The matched decoder requires\n"
               "glibc 2.35 or newer and the normal libc/libm runtime libraries.\n")
    return ("eac3to-go native distribution\n\n" + runtime + "\n"
            "The project is under development and is not a complete replacement for\n"
            "every Windows eac3to operation. Unsupported operations fail explicitly.\n"
            "The native Cgo SSRC and Aften processing libraries are included. The\n"
            "matched decoder has a bounded sample-comparison corpus; it does not\n"
            "establish equivalence for all codecs, extensions, or damaged streams.\n\n"
            "Install a matching FFmpeg/FFprobe pair separately. Blu-ray probing needs\n"
            "FFprobe built with libbluray. This package does not install or replace\n"
            "system tools. Supply --ffmpeg and --ffprobe explicitly, put the pair\n"
            "beside the programs or under tools/, or make it available on PATH.\n"
            "Keep libexec/eac3to-decoder beside the programs; on Windows its suffix\n"
            "is .exe. Explicit options and EAC3TO_* environment overrides take priority.\n\n"
            "  " + command + " --help\n"
            "  " + command + " -test\n"
            "  " + command + ' "path/to/disc/BDMV"\n'
            "  " + command + ' "path/to/disc/BDMV" --playlist=00001 3: audio.dts\n'
            "  " + command + " input.wav output.flac\n"
            "  " + command + " input.wav output.wav -25.000 -changeTo24.000 +100ms\n\n"
            "Terminal use offers interactive selection. Use --non-interactive for\n"
            "scripts, or --interactive to request the prompts explicitly.\n\n"
            "Run pcmcompare --help (pcmcompare.exe on Windows) to compare decoded PCM.\n"
            "Quote paths containing spaces. Existing outputs require --force.\n"
            "Ambiguous disc selections require an explicit playlist. Native Java/menu\n"
            "emulation and universal Windows output equivalence are not claimed.\n"
            "--reference-tail reproduces historical resampler tail handling; normal\n"
            "draining preserves the complete filter tail.\n\n"
            "Licenses and attribution are under licenses/. source/ contains the native\n"
            "source snapshot, decoder inputs and exact decoder build configuration.\n"
            "Extract eac3to-go-source.tar.gz, copy the two .tar.xz files into its\n"
            "build/archives directory, then follow its README and build scripts.\n"
            "The source includes the linked SSRC/Aften libraries and Cgo bridge so\n"
            "they can be modified and the executables rebuilt. Windows cross builds\n"
            "download the pinned toolchain separately. SHA256SUMS checks every file\n"
            "except itself; bundle.json records relative paths, sizes and hashes.\n")


def build(options):
    root = Path(options.source_root).resolve()
    output = Path(options.output).resolve()
    extension = ".zip" if options.target == "windows-amd64" else ".tar.gz"
    if not output.name.endswith(extension):
        raise ValueError("output name must end with " + extension)
    if output.exists() or output.with_name(output.name + ".sha256").exists():
        raise FileExistsError("archive or checksum already exists")
    notices = []
    notice_names = set()
    notice_components = set()
    for value in options.notice:
        name, separator, filename = value.partition("=")
        if not separator or not filename:
            raise ValueError("notice must use COMPONENT/NAME=PATH")
        relative = relative_name(name)
        if len(relative.parts) != 2:
            raise ValueError("notice must use COMPONENT/NAME=PATH")
        if relative.parts[0].casefold() in NOTICES or name.casefold() in notice_names:
            raise ValueError("duplicate or reserved notice name: " + name)
        path = Path(filename)
        if not path.is_file() or path.stat().st_size == 0:
            raise ValueError("missing or empty runtime notice: " + name)
        public_text(path.read_bytes(), "notice " + name)
        notice_names.add(name.casefold())
        notice_components.add(relative.parts[0].casefold())
        notices.append((path, "licenses/" + name))
    required_notices = {"go", "gcc", "mingw" if options.target == "windows-amd64" else "glibc"}
    if not required_notices.issubset(notice_components):
        raise ValueError("runtime notices required for: " + ", ".join(sorted(required_notices - notice_components)))
    source = source_files(root, options.source_extra)
    suffix = ".exe" if options.target == "windows-amd64" else ""
    binaries = [(Path(options.eac3to), "eac3to" + suffix),
                (Path(options.pcmcompare), "pcmcompare" + suffix),
                (Path(options.decoder), "libexec/eac3to-decoder" + suffix)]
    for path, _ in binaries:
        executable_architecture(path, options.target)
        public_text(path.read_bytes(), path.name)
    decoder_manifest = Path(options.decoder_manifest).read_bytes()
    public_text(decoder_manifest, "decoder build manifest")
    decoder_info = json.loads(decoder_manifest)
    if decoder_info.get("sha256", decoder_info.get("binary_sha256")) != digest(options.decoder):
        raise ValueError("decoder build manifest does not match the supplied binary")
    if decoder_info.get("profile") != "eac3to-decoder-profile1" or decoder_info.get("reference_pcm_checked") is not True:
        raise ValueError("decoder manifest does not identify a checked profile1 build")
    inputs = json.loads((root / "third_party/decoder-profile1/inputs.json").read_text())
    if decoder_info.get("source_inputs") != inputs:
        raise ValueError("decoder build inputs differ from the packaged source profile")
    for name, record in inputs.items():
        relative_name(name)
        path = (root / "third_party/decoder-profile1" if name.endswith(".patch") else Path(options.archives)) / name
        if digest(path) != record["sha256"]:
            raise ValueError("decoder source checksum differs: " + name)
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".package-", dir=output.parent) as temporary:
        stage = Path(temporary)
        entries = []

        def add(path, relative, executable=False):
            entries.append((Path(path), relative_name(relative), executable))

        for path, relative in binaries:
            add(path, relative, True)
        for component, names in NOTICES.items():
            for name in names:
                add(root / "third_party" / component / name, "licenses/" + component + "/" + name)
        for path, name in notices:
            add(path, name)
        (stage / "USAGE.txt").write_text(usage(options.target), encoding="utf-8")
        add(stage / "USAGE.txt", "USAGE.txt")
        tar_archive(stage / "source.tar.gz", source, "eac3to-go-source")
        add(stage / "source.tar.gz", "source/eac3to-go-source.tar.gz")
        add(options.decoder_manifest, "source/decoder-build.json")
        for name in inputs:
            if not name.endswith(".patch"):
                add(Path(options.archives) / name, "source/" + name)
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
        dirty = bool(subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=no"], cwd=root))
        manifest = {"format": 1, "target": options.target, "cgo": True,
                    "source_commit": commit, "source_worktree_modified": dirty,
                    "external_dependencies": ["FFmpeg", "FFprobe (libbluray for Blu-ray)"], "files": []}
        for path, relative, _ in sorted(entries, key=lambda entry: str(entry[1])):
            manifest["files"].append({"path": str(relative), "bytes": path.stat().st_size, "sha256": digest(path)})
        (stage / "bundle.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        add(stage / "bundle.json", "bundle.json")
        (stage / "SHA256SUMS").write_text("".join(digest(path) + "  " + str(relative) + "\n"
            for path, relative, _ in sorted(entries, key=lambda entry: str(entry[1]))), encoding="utf-8")
        add(stage / "SHA256SUMS", "SHA256SUMS")
        packed = stage / "archive"
        prefix = "eac3to-" + options.target
        (zip_archive if options.target == "windows-amd64" else tar_archive)(packed, entries, prefix)
        checksum = digest(packed)
        # A hard link publishes the finished archive without replacing a file
        # that another process might have created after the initial check.
        os.link(packed, output)
        checksum_file = output.with_name(output.name + ".sha256")
        with checksum_file.open("x", encoding="utf-8") as record:
            record.write(checksum + "  " + output.name + "\n")
    return {"archive": output.name, "bytes": output.stat().st_size, "sha256": checksum,
            "target": options.target, "files": len(entries)}


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--target", required=True, choices=("linux-amd64", "windows-amd64"))
    result.add_argument("--output", required=True, help="new .tar.gz or .zip archive; never overwritten")
    for name in ("eac3to", "pcmcompare", "decoder", "decoder-manifest"):
        result.add_argument("--" + name, required=True)
    result.add_argument("--source-root", default=ROOT, type=Path)
    result.add_argument("--source-extra", action="append", default=[], help="additional source path relative to checkout")
    result.add_argument("--archives", default=ROOT / "build/archives", type=Path)
    result.add_argument("--notice", action="append", default=[], metavar="COMPONENT/NAME=PATH",
                        help="runtime notice; require go, gcc, and glibc or mingw components")
    return result


if __name__ == "__main__":
    try:
        print(json.dumps(build(parser().parse_args()), indent=2))
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        raise SystemExit("package: " + str(error))

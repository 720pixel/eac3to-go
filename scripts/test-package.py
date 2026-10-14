#!/usr/bin/env python3

import hashlib
import io
import json
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import package


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        files = {
            "go.mod": b"module eac3to\n",
            "Makefile": b"build:\n\tgo build ./cmd/eac3to\n",
            "cmd/eac3to/main.go": b"package main\n",
            "cmd/pcmcompare/main.go": b"package main\n",
            "scripts/build-decoder.py": b"# decoder build fixture\n",
            "scripts/build-windows-decoder.py": b"# Windows build fixture\n",
            "third_party/decoder-profile1/build.py": b"# source build fixture\n",
            "third_party/decoder-profile1/profile1.patch": b"patch fixture\n",
            "internal/PRIVATE.md": b"excluded note\n",
            "bin/old-program": b"excluded binary\n",
        }
        for component, names in package.NOTICES.items():
            for name in names:
                files["third_party/" + component + "/" + name] = b"Preserved upstream license and attribution.\n"
        self.names = sorted(files)
        for name, data in files.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        self.archives = self.root / "archives"
        self.archives.mkdir()
        (self.archives / "source.tar.xz").write_bytes(b"archive fixture")
        records = {}
        for path in (self.archives / "source.tar.xz", self.root / "third_party/decoder-profile1/profile1.patch"):
            records[path.name] = {"sha256": package.digest(path), "bytes": path.stat().st_size}
        (self.root / "third_party/decoder-profile1/inputs.json").write_text(json.dumps(records))
        self.names.append("third_party/decoder-profile1/inputs.json")
        self.git = patch.object(package.subprocess, "check_output", side_effect=self.git_output)
        self.git.start()
        self.addCleanup(self.git.stop)

    def git_output(self, command, **kwargs):
        if command[1] == "ls-files":
            if "--others" in command:
                return b""
            return ("\0".join(self.names) + "\0").encode()
        if command[1] == "rev-parse":
            return "1" * 40 + "\n"
        if command[1] == "status":
            return b""
        self.fail("unexpected git operation")

    def executable(self, name, target):
        data = bytearray(160)
        if target == "windows-amd64":
            data[:2] = b"MZ"
            struct.pack_into("<I", data, 60, 128)
            data[128:134] = b"PE\0\0\x64\x86"
        else:
            data[:6] = b"\x7fELF\x02\x01"
            struct.pack_into("<H", data, 18, 62)
        path = self.root / name
        path.write_bytes(data)
        return path

    def options(self, target="linux-amd64", archive="package"):
        decoder = self.executable("decoder", target)
        manifest = self.root / "decoder.json"
        manifest.write_text(json.dumps({"sha256": package.digest(decoder), "configure": ["src/configure"],
            "profile": "eac3to-decoder-profile1", "reference_pcm_checked": True,
            "source_inputs": json.loads((self.root / "third_party/decoder-profile1/inputs.json").read_text())}))
        args = [
            "--target", target,
            "--output", str(self.root / (archive + (".zip" if target == "windows-amd64" else ".tar.gz"))),
            "--source-root", str(self.root), "--archives", str(self.archives),
            "--eac3to", str(self.executable("main", target)),
            "--pcmcompare", str(self.executable("compare", target)),
            "--decoder", str(decoder), "--decoder-manifest", str(manifest),
        ]
        notice = self.root / "runtime-copyright"
        notice.write_text("Runtime copyright and license fixture.\n")
        for component in ("go", "gcc", "mingw" if target == "windows-amd64" else "glibc"):
            args += ["--notice", component + "/COPYRIGHT=" + str(notice)]
        return package.parser().parse_args(args)

    def unpack(self, output, target):
        if target == "windows-amd64":
            with zipfile.ZipFile(output) as archive:
                self.assertIsNone(archive.testzip())
                return {name.split("/", 1)[1]: archive.read(name) for name in archive.namelist()}
        with tarfile.open(output) as archive:
            return {member.name.split("/", 1)[1]: archive.extractfile(member).read() for member in archive}

    def test_both_formats_sources_notices_and_checksums(self):
        for target in ("linux-amd64", "windows-amd64"):
            with self.subTest(target=target):
                options = self.options(target, target)
                result = package.build(options)
                self.assertEqual(result["sha256"], package.digest(options.output))
                data = self.unpack(options.output, target)
                self.assertIn("licenses/ssrc/README.original.txt", data)
                self.assertIn("licenses/go/COPYRIGHT", data)
                self.assertIn("source/source.tar.xz", data)
                for line in data["SHA256SUMS"].decode().splitlines():
                    expected, name = line.split("  ", 1)
                    self.assertEqual(hashlib.sha256(data[name]).hexdigest(), expected)
                manifest = json.loads(data["bundle.json"])
                self.assertNotIn(str(self.root), data["bundle.json"].decode())
                for entry in manifest["files"]:
                    self.assertEqual(len(data[entry["path"]]), entry["bytes"])
                    self.assertEqual(hashlib.sha256(data[entry["path"]]).hexdigest(), entry["sha256"])
                with tarfile.open(fileobj=io.BytesIO(data["source/eac3to-go-source.tar.gz"])) as source:
                    names = source.getnames()
                    self.assertIn("eac3to-go-source/cmd/eac3to/main.go", names)
                    self.assertFalse(any(name.endswith(".md") or "/bin/" in name for name in names))
                with self.assertRaises(FileExistsError):
                    package.build(options)
                self.assertFalse(list(self.root.glob(".package-*")))

    def test_reproducible_archive_without_absolute_paths(self):
        one = self.options(archive="one")
        two = self.options(archive="two")
        self.assertEqual(package.build(one)["sha256"], package.build(two)["sha256"])

    def test_untracked_source_cannot_be_omitted(self):
        options = self.options()
        pending = self.root / "internal/new.go"
        pending.write_text("package internal\n")
        original = self.git_output

        def git_output(command, **kwargs):
            if "--others" in command:
                return b"internal/new.go\0"
            return original(command, **kwargs)

        with patch.object(package.subprocess, "check_output", side_effect=git_output):
            with self.assertRaisesRegex(ValueError, "untracked source"):
                package.build(options)
            options.source_extra.append("internal/new.go")
            package.build(options)
        data = self.unpack(options.output, options.target)
        with tarfile.open(fileobj=io.BytesIO(data["source/eac3to-go-source.tar.gz"])) as source:
            self.assertIn("eac3to-go-source/internal/new.go", source.getnames())

    def test_mismatched_decoder_and_changed_source_fail_before_publication(self):
        options = self.options()
        (self.root / "decoder").write_bytes((self.root / "decoder").read_bytes() + b"changed")
        with self.assertRaisesRegex(ValueError, "does not match"):
            package.build(options)
        self.assertFalse(Path(options.output).exists())
        options = self.options()
        (self.archives / "source.tar.xz").write_bytes(b"damaged")
        with self.assertRaisesRegex(ValueError, "source checksum differs"):
            package.build(options)
        self.assertFalse(Path(options.output).exists())

    def test_missing_or_colliding_runtime_notices_are_rejected(self):
        options = self.options()
        options.notice = options.notice[:-1]
        with self.assertRaisesRegex(ValueError, "runtime notices required"):
            package.build(options)
        options = self.options()
        options.notice.append(options.notice[0])
        with self.assertRaisesRegex(ValueError, "duplicate or reserved"):
            package.build(options)
        self.assertFalse(Path(options.output).exists())

    def test_private_paths_wrong_architecture_and_symlinks_rejected(self):
        options = self.options()
        self.executable("main", "windows-amd64")
        with self.assertRaisesRegex(ValueError, "not a linux-amd64"):
            package.build(options)
        options = self.options()
        with (self.root / "decoder").open("ab") as output:
            output.write(b"/home/" + b"private-owner/source.c\0")
        with self.assertRaisesRegex(ValueError, "private build path"):
            package.build(options)
        options = self.options()
        source = self.root / "cmd/eac3to/main.go"
        source.unlink()
        source.symlink_to(self.root / "go.mod")
        with self.assertRaisesRegex(ValueError, "regular file"):
            package.build(options)
        self.assertFalse(Path(options.output).exists())

    def test_portable_names_and_relative_tool_root(self):
        for name in ("../file", "/file", "a//b", "a\\b", "NUL.txt", "a/CON", "a/filename."):
            with self.subTest(name=name), self.assertRaises(ValueError):
                package.relative_name(name)
        package.public_text(b"../../toolchain/root/usr/bin/gcc", "relative compiler")
        with self.assertRaises(ValueError):
            package.public_text(b"--cc=/root/" + b"private/bin/gcc", "absolute compiler")


if __name__ == "__main__":
    unittest.main()

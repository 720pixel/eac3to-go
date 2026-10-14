#!/usr/bin/env python3
"""Build an isolated Linux amd64 decoder. Never installs or changes system tools.

Usage: python3 build.py NEW_OUTPUT_DIRECTORY [ARCHIVE_DIRECTORY]
The archive directory defaults to this package's parent. Inputs are verified
against inputs.json; source, logs, libraries, and binary remain in the output.
"""
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile

HERE = Path(__file__).resolve().parent
OUT = Path(sys.argv[1]).resolve()
ARCHIVES = Path(sys.argv[2]).resolve() if len(sys.argv)>2 else HERE.parent
if platform.system() != 'Linux' or platform.machine() not in ('x86_64','amd64'):
    raise SystemExit('profile1 currently supports Linux amd64; other targets are not validated')
if OUT.exists():
    raise SystemExit('output path must not exist: '+str(OUT))


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


inputs = json.loads((HERE/'inputs.json').read_text())
for name, record in inputs.items():
    path = (HERE if name.endswith('.patch') else ARCHIVES)/name
    if sha(path) != record['sha256']:
        raise SystemExit('input checksum differs: '+str(path))

OUT.mkdir(parents=True)
commands = []


def run(command,cwd,log):
    commands.append({'command':command,'cwd':str(cwd),'log':str(log)})
    (OUT/'commands.json').write_text(json.dumps(commands,indent=2)+'\n')
    with log.open('wb') as stream:
        subprocess.run(command,cwd=cwd,stdout=stream,stderr=subprocess.STDOUT,check=True)


for name in ('ffmpeg-8.0.2.tar.xz','nasm-2.16.03.tar.xz'):
    with tarfile.open(ARCHIVES/name) as archive:
        archive.extractall(OUT,filter='data')
SOURCE = OUT/'ffmpeg-8.0.2'
NASM = OUT/'nasm-2.16.03'
run(['git','apply',str(HERE/'profile1.patch')],SOURCE,OUT/'patch.log')
run([str(NASM/'configure'),'--prefix='+str(OUT/'unused-prefix')],NASM,OUT/'nasm-configure.log')
run(['make','-j8','nasm'],NASM,OUT/'nasm-build.log')
BUILD = OUT/'build'
BUILD.mkdir()
options = json.loads((HERE/'configure-options.json').read_text())
command = [str(SOURCE/'configure'),'--cc=gcc','--x86asmexe='+str(NASM/'nasm'),*options]
run(command,BUILD,OUT/'configure.log')
run(['make','-j8','ffmpeg'],BUILD,OUT/'build.log')
binary = OUT/'eac3to-decoder'
shutil.copy2(BUILD/'ffmpeg',binary)
manifest = {'profile':'eac3to-decoder-profile1','binary_sha256':sha(binary),
            'source_inputs':inputs,'configure':command,'platform':platform.platform(),
            'compiler':subprocess.check_output(['gcc','--version'],text=True),
            'nasm':subprocess.check_output([str(NASM/'nasm'),'-v'],text=True),
            'version':subprocess.check_output([str(binary),'-version'],text=True),
            'dynamic_dependencies':subprocess.check_output(['ldd',str(binary)],text=True)}
(OUT/'build-manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
print(json.dumps({'binary':str(binary),'sha256':manifest['binary_sha256']}))

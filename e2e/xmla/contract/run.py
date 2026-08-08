#!/usr/bin/env python3
"""Read ADOMD.NET's declared JSON contracts.

The client matches JSON members against `[DataMember(Name=…)]` on types in the
shipped assembly, so the names are readable rather than guessable. See
README.md for why this beats screening a shape.
"""
import os
import shutil
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
SDK_IMAGE = "mcr.microsoft.com/dotnet/sdk:8.0"

if not shutil.which("docker"):
    sys.exit("docker is not on PATH; the assembly read needs the .NET 8 SDK image")

filt = sys.argv[1] if len(sys.argv) > 1 else ""
# Mount read-only and build inside the container, the way e2e/xmla/run.py
# does: a writable mount leaves bin/ and obj/ (including a 1 MB DLL) in the
# working tree, which is how they nearly got committed.
sys.exit(subprocess.run([
    "docker", "run", "--rm", "--platform", "linux/amd64",
    "-v", f"{DIR}:/src:ro", "-w", "/work",
    "-e", f"CONTRACT_FILTER={filt}",
    SDK_IMAGE, "sh", "-c",
    "set -e; cp -r /src/. /work/; dotnet run --nologo",
]).returncode)

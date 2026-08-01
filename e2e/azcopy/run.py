#!/usr/bin/env python3
"""e2e: Microsoft's real `azcopy` binary transfers files through
fabric-emulator's OneLake Blob surface, authenticated by an entra-emulator
Storage-audience token.

Self-contained and OS-aware (Linux, macOS): builds fabric-emulator from this
repo, installs entra-emulator if missing, downloads the real azcopy, then runs
the driver. stdlib-only orchestrator — azcopy is a static binary and the driver
needs no Python packages.

Why this suite is Linux-first in CI: azcopy is a Go binary, so it honours
SSL_CERT_FILE for the emulator's self-signed CA on Linux. The CI job runs on
Linux — same weight class as the other real-engine suites (spark, livy,
notebook-run).

On macOS neither half of that holds: Go builds `crypto/x509/root_unix.go` with
`unix && !darwin`, so SSL_CERT_FILE is ignored and the system trust store is
consulted instead, and Darwin additionally refuses any leaf valid for more than
825 days — the emulator's own cert is good for ten years. **mkcert** satisfies
both, since its CA lives in the keychain and its leaves are short-lived, so this
suite runs on macOS too once `mkcert -install` has been done. No emulator change
was needed: fabric already serves `{data-dir}/tls/cert.pem` when present and
entra takes `TLS_CERT_DIR`."""

import os
import platform
import shutil
import ssl
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
import zipfile

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
WORK = os.path.join(tempfile.gettempdir(), "azcopy-e2e")

# The emulators' own certs are self-signed with a 10-year life, and Darwin's
# verifier rejects both properties: SSL_CERT_FILE is ignored there (Go builds
# crypto/x509/root_unix.go with `unix && !darwin`, so a Go binary like azcopy
# reads the system trust store instead), and a leaf valid for more than 825 days
# is refused outright. The failure was also silent and expensive — azcopy
# retries a rejected TLS handshake indefinitely, so the run sat at
# "0.0 %, 0 Done, 1 Pending" until an external timeout killed it.
#
# mkcert fixes it properly rather than skipping: it keeps a local CA *in the
# system keychain* — which is what Darwin actually consults — and issues leaves
# inside the 825-day limit. Both emulators already accept a supplied cert
# (fabric reads {data-dir}/tls/cert.pem, entra takes TLS_CERT_DIR), so this
# needs no code change in either: seed the files before they start.
MKCERT_HOSTS = ["localhost", "127.0.0.1", "fabric-emulator",
                "api.fabric.microsoft.com",
                "onelake.dfs.fabric.microsoft.com",
                "onelake.blob.fabric.microsoft.com"]


def mkcert_available():
    """True when mkcert is installed AND its CA is actually trusted.

    Both halves matter: `mkcert` on PATH without `mkcert -install` issues certs
    nothing trusts, which would reproduce the original hang.
    """
    if not shutil.which("mkcert"):
        return False
    if platform.system() != "Darwin":
        return True
    # -install puts the CA in the login and System keychains; either is enough
    # for the system verifier azcopy consults.
    for kc in ("/Library/Keychains/System.keychain",
               os.path.expanduser("~/Library/Keychains/login.keychain-db")):
        found = subprocess.run(["security", "find-certificate", "-c", "mkcert", kc],
                               capture_output=True)
        if found.returncode == 0:
            return True
    return False


def seed_mkcert(tls_dir):
    """Issue a trusted cert.pem/key.pem pair into tls_dir."""
    os.makedirs(tls_dir, exist_ok=True)
    subprocess.run(["mkcert", "-cert-file", os.path.join(tls_dir, "cert.pem"),
                    "-key-file", os.path.join(tls_dir, "key.pem")] + MKCERT_HOSTS,
                   check=True, capture_output=True)


USE_MKCERT = platform.system() == "Darwin" and mkcert_available()
if platform.system() == "Darwin" and not USE_MKCERT:
    print("SKIPPED: azcopy on macOS needs mkcert, because Darwin ignores "
          "SSL_CERT_FILE and rejects the emulator's 10-year dev cert. Install "
          "it once and this suite runs here as it does on Linux:\n"
          "    brew install mkcert && mkcert -install")
    raise SystemExit(0)
ENTRA_PORT = os.environ.get("ENTRA_PORT", "18543")
FABRIC_PORT = os.environ.get("FABRIC_PORT", "19543")
TENANT = "11111111-1111-1111-1111-111111111111"
EXE = ".exe" if os.name == "nt" else ""


def log(msg):
    print(f"==> {msg}", flush=True)


def wait_healthy(url, deadline=60):
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # self-signed harness certs
    end = time.time() + deadline
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, context=ctx, timeout=2) as r:
                if r.status == 200:
                    return
        except OSError:
            pass
        time.sleep(0.2)
    raise RuntimeError(f"health never came up at {url}")


def azcopy_url():
    """The official aka.ms redirect for this OS/arch. azcopy ships as a
    single binary inside a tar.gz (Linux) or zip (macOS/Windows)."""
    sysname = platform.system()
    arm = platform.machine().lower() in ("arm64", "aarch64")
    if sysname == "Linux":
        return "https://aka.ms/downloadazcopy-v10-linux-arm64" if arm else "https://aka.ms/downloadazcopy-v10-linux"
    if sysname == "Darwin":
        return "https://aka.ms/downloadazcopy-v10-mac-arm64" if arm else "https://aka.ms/downloadazcopy-v10-mac"
    if sysname == "Windows":
        return "https://aka.ms/downloadazcopy-v10-windows"
    raise RuntimeError(f"unsupported OS for azcopy: {sysname}")


def fetch_azcopy(dest_dir):
    url = azcopy_url()
    log(f"downloading azcopy from {url}")
    arch_path = os.path.join(dest_dir, "azcopy_dl")
    urllib.request.urlretrieve(url, arch_path)
    extract = os.path.join(dest_dir, "azcopy_extract")
    os.makedirs(extract, exist_ok=True)
    if zipfile.is_zipfile(arch_path):
        with zipfile.ZipFile(arch_path) as z:
            z.extractall(extract)
    else:
        with tarfile.open(arch_path) as t:
            t.extractall(extract)
    for root, _dirs, files in os.walk(extract):
        for f in files:
            if f == "azcopy" + EXE:
                p = os.path.join(root, f)
                os.chmod(p, 0o755)
                return p
    raise RuntimeError("azcopy binary not found in archive")


shutil.rmtree(WORK, ignore_errors=True)
os.makedirs(os.path.join(WORK, "data"))

entra_bin = shutil.which("entra-emulator")
if not entra_bin:
    log("installing entra-emulator")
    subprocess.run(
        ["go", "install", "github.com/calvinchengx/entra-emulator/cmd/entra-emulator@latest"],
        check=True, env={**os.environ, "GOBIN": WORK})
    entra_bin = os.path.join(WORK, "entra-emulator" + EXE)

log("building fabric-emulator")
fabric_bin = os.path.join(WORK, "fabric-emulator" + EXE)
subprocess.run(["go", "build", "-C", REPO, "-o", fabric_bin, "./cmd/fabric-emulator"], check=True)

azcopy_bin = fetch_azcopy(WORK)
subprocess.run([azcopy_bin, "--version"], check=True)

procs = []
logfiles = {}


def start(name, cmd, env):
    path = os.path.join(WORK, name + ".log")
    with open(path, "wb") as f:
        procs.append(subprocess.Popen(cmd, stdout=f, stderr=subprocess.STDOUT, env=env))
    logfiles[name] = path


try:
    entra_tls = os.path.join(WORK, "entra-tls")
    if USE_MKCERT:
        # Before either process starts: both only generate their own cert when
        # the files are absent, so seeding first is what makes them serve a
        # system-trusted chain.
        log("seeding mkcert certificates (macOS system trust)")
        seed_mkcert(entra_tls)
        seed_mkcert(os.path.join(WORK, "data", "tls"))
    log(f"starting entra-emulator on :{ENTRA_PORT} (TLS)")
    start("entra", [entra_bin], {
        **os.environ, "ORIGIN_MODE": "compat", "PORT": ENTRA_PORT, "TLS_ENABLED": "true",
        "DB_PATH": os.path.join(WORK, "entra.sqlite"), "TLS_CERT_DIR": entra_tls})

    log(f"starting fabric-emulator on :{FABRIC_PORT} (TLS)")
    start("fabric", [
        fabric_bin, "-addr", f"127.0.0.1:{FABRIC_PORT}",
        "-data-dir", os.path.join(WORK, "data"),
        "-entra-issuer", f"https://localhost:{ENTRA_PORT}/{TENANT}/v2.0",
        "-entra-tls-insecure"], os.environ.copy())

    wait_healthy(f"https://localhost:{ENTRA_PORT}/health")
    wait_healthy(f"https://127.0.0.1:{FABRIC_PORT}/health")

    # azcopy (a Go binary) trusts the emulators' self-signed certs via a CA
    # bundle of both leaves, passed through SSL_CERT_FILE by the driver.
    ca_bundle = os.path.join(WORK, "ca-bundle.pem")
    with open(ca_bundle, "wb") as out:
        for cert in (os.path.join(entra_tls, "cert.pem"),
                     os.path.join(WORK, "data", "tls", "cert.pem")):
            with open(cert, "rb") as f:
                out.write(f.read())

    log("running azcopy against the emulator")
    subprocess.run([sys.executable, "-u", os.path.join(DIR, "driver.py")], check=True, env={
        **os.environ, "ENTRA_PORT": ENTRA_PORT, "FABRIC_PORT": FABRIC_PORT,
        "AZCOPY_BIN": azcopy_bin, "AZCOPY_CA_BUNDLE": ca_bundle, "AZCOPY_WORK": WORK})
except Exception:
    for name, path in logfiles.items():
        sys.stderr.write(f"\n==== {name} log ====\n")
        with open(path, errors="replace") as f:
            sys.stderr.write(f.read())
    raise
finally:
    for p in procs:
        p.terminate()
    for p in procs:
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()

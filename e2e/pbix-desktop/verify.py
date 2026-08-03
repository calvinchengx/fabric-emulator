"""The parts of the Desktop probe that are ordinary code, kept testable.

WHY THIS IS A SEPARATE MODULE. The interesting half of this suite can only run
on Windows with Power BI Desktop installed, which means it cannot be exercised
on a contributor's machine and only reaches CI. Everything in that position
tends to be written once, shipped, and debugged by watching CI fail — so the
logic that does NOT need Desktop lives here, where `test_verify.py` runs it in
milliseconds anywhere.

What remains untestable off Windows is exactly three things: does Desktop
install, does it launch, does it host an Analysis Services instance. Those are
the findings the suite exists to produce, and they are the only things it should
be able to get wrong in CI.
"""

from __future__ import annotations

import re


class ProbeError(Exception):
    """A stage failed with something the caller should read, not a traceback."""


def parse_port_file(raw: bytes) -> int:
    """Read the port Power BI Desktop wrote for its Analysis Services instance.

    `msmdsrv.port.txt` is written as UTF-16 with a BOM, and reading it as UTF-8
    yields a string full of NULs that `int()` rejects with a message naming
    neither the encoding nor the file. Both encodings are accepted here, and a
    file that decodes to something that is not a port is an error that says what
    it actually contained.
    """
    if not raw:
        raise ProbeError("msmdsrv.port.txt is empty — Desktop wrote the file but not the port")
    for enc in ("utf-16", "utf-16-le", "utf-8-sig", "utf-8"):
        try:
            text = raw.decode(enc)
        except (UnicodeDecodeError, UnicodeError):
            continue
        digits = text.strip().strip("﻿").strip("\x00").strip()
        if digits.isdigit():
            port = int(digits)
            # 0 is what the file holds mid-write; a real listener is never there.
            if not (1 <= port <= 65535):
                raise ProbeError(f"port out of range: {port}")
            return port
    raise ProbeError(f"could not read a port from {raw[:40]!r}")


# ROW Customer[Country]=GB [Revenue]=25227674.7 [PerUnit]=101.72941714921689
_ROW = re.compile(r"(\S+?)=(\S*)")


def parse_rows(stdout: str) -> list[dict[str, str]]:
    """Pull the ROW lines out of the probe's output.

    Only lines beginning `ROW` are data. The probe also prints STAGE lines, and
    a parser that scraped every line would silently turn a connection failure
    into an empty result set — which reads as "the model has no rows" rather
    than "nothing connected".
    """
    rows = []
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("ROW"):
            continue
        rows.append({k: v for k, v in _ROW.findall(line[3:].strip())})
    return rows


def stage_results(stdout: str) -> dict[str, str]:
    """Map each `STAGE <name> :: <outcome>` line to its outcome.

    Stages are reported separately because "installed but never listened" and
    "listened but the query failed" are different findings, and a single
    pass/fail would hide which one happened — the thing this whole spike is
    trying to learn.
    """
    out = {}
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("STAGE "):
            continue
        _, rest = line.split(" ", 1)
        name, _, outcome = rest.partition(" :: ")
        out[name.strip()] = outcome.strip()
    return out


def compare(expected: dict[str, dict[str, float]],
            rows: list[dict[str, str]],
            key_field: str,
            fields: dict[str, str],
            rel_tol: float = 1e-9) -> tuple[bool, list[str]]:
    """Diff Desktop's answer against our emulator's, relatively.

    RELATIVE, not absolute, for the reason Phase 0 measured: two engines summing
    the same doubles in a different order differ in the last bit. On a revenue
    of 25 million an absolute epsilon loose enough to tolerate that would also
    wave through a real error in a per-unit rate.

    Returns (ok, lines) rather than asserting, so the caller reports every
    divergence instead of the first.
    """
    lines, ok = [], True
    got = {r.get(key_field): r for r in rows}

    missing = sorted(set(expected) - set(got))
    if missing:
        ok = False
        lines.append(f"Desktop returned no row for: {', '.join(missing)}")
    extra = sorted(k for k in got if k is not None and k not in expected)
    if extra:
        ok = False
        lines.append(f"Desktop returned rows we did not expect: {', '.join(extra)}")

    for key in sorted(expected):
        row = got.get(key)
        if row is None:
            continue
        for ours_name, probe_name in fields.items():
            if probe_name not in row:
                ok = False
                lines.append(f"{key}: Desktop returned no {probe_name}")
                continue
            try:
                theirs = float(row[probe_name])
            except ValueError:
                ok = False
                lines.append(f"{key}: {probe_name} is not a number: {row[probe_name]!r}")
                continue
            mine = expected[key][ours_name]
            rel = abs(mine - theirs) / max(abs(mine), abs(theirs), 1e-12)
            if rel >= rel_tol:
                ok = False
            lines.append(f"{key:6s} {ours_name:18s} ours={mine!r} desktop={theirs!r} rel={rel:.3e}")
    return ok, lines

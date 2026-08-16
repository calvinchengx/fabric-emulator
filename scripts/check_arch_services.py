#!/usr/bin/env python3
"""Every always-on service must appear in the architecture diagram.

WHY THIS EXISTS. docs/03-architecture.md opened with "The two-system model" and
a mermaid diagram showing entra-emulator and fabric-emulator — for as long as
`keyvault-emulator` had been a default service in docker-compose.yml. Key Vault
appeared *zero* times in the file, while `internal/akv` existed to call it and
docs/parity.md carried a correct row for it. Nothing failed. The document was
simply describing a system with one fewer component than the one that ships, and
an architecture audit built by reading it inherited the gap wholesale.

The general shape of the bug: a diagram's TOPOLOGY is a design decision and
stays true for years, but its CAST OF SERVICES is a list, and lists drift
silently. Hand-drawing the topology is right. Leaving the roster unasserted is
not — nothing about adding a service to compose reminds anyone to redraw.

SCOPE, deliberately narrow. Only services in docker-compose.yml with no
`profiles:` key: the set that starts unconditionally, for everyone, with no flag.
Profiled services (governance, rti, terminal) are opt-in and documented in their
own files. The engine sidecars in docker-compose.override.yml are excluded too —
they appear in the diagram under their ROLES ("Spark agent (Livy)", "SQL Server")
rather than their compose names, and a check that demanded literal service names
there would fail on correct prose. A narrow check that always passes honestly
beats a broad one that gets disabled the first time it is wrong.

Usage:
    check_arch_services.py           exit non-zero naming any missing service
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
COMPOSE = ROOT / "docker-compose.yml"
ARCH = ROOT / "docs" / "03-architecture.md"


def core_services(compose_text):
    """Service names under `services:` that carry no `profiles:` key.

    Parsed rather than yaml-loaded so this runs in any CI job without pulling a
    dependency in for eight lines of structure. Compose files are 2-space
    indented by convention and this one is; a service is a 2-space key inside
    the top-level `services:` block.
    """
    services, current, in_services = {}, None, False
    for line in compose_text.splitlines():
        if re.match(r"^[a-zA-Z]", line):  # a top-level key: services:, volumes:, …
            in_services = line.startswith("services:")
            current = None
            continue
        if not in_services:
            continue
        m = re.match(r"^  ([a-zA-Z0-9._-]+):\s*$", line)
        if m:
            current = m.group(1)
            services[current] = False
            continue
        if current and re.match(r"^\s+profiles:", line):
            services[current] = True
    return [name for name, profiled in services.items() if not profiled]


def main():
    compose = COMPOSE.read_text()
    arch = ARCH.read_text()

    mermaid = "\n".join(re.findall(r"```mermaid\n(.*?)```", arch, re.S))
    if not mermaid:
        print("check_arch_services: docs/03-architecture.md has no mermaid block")
        return 1

    services = core_services(compose)
    if not services:
        # A parse that finds nothing must fail rather than vacuously pass —
        # otherwise a compose reformat silently disarms this check.
        print("check_arch_services: parsed no core services from docker-compose.yml")
        return 1

    missing = []
    for name in services:
        # Substring, so the doc may name it more fully than compose does
        # (`keyvault-emulator` → `azure-keyvault-emulator`). Both the prose and
        # the diagram must carry it: prose alone leaves the picture wrong, which
        # is the failure this was written for.
        where = []
        if name not in arch:
            where.append("the document")
        if name not in mermaid:
            where.append("the mermaid diagram")
        if where:
            missing.append((name, " and ".join(where)))

    if missing:
        print("check_arch_services: docker-compose.yml starts services that")
        print("docs/03-architecture.md does not describe.\n")
        for name, where in missing:
            print(f"  {name:24s} missing from {where}")
        print(
            # Deliberately no count here: the numbered model in that doc grew
            # from two to three to four, and a hardcoded number in the fix
            # instruction is the same drift this check exists to catch.
            "\nAdd it to the numbered system model and to the diagram, or give\n"
            "it a `profiles:` key if it is genuinely opt-in."
        )
        return 1

    print(f"check_arch_services: {len(services)} core services, all described "
          f"({', '.join(sorted(services))})")
    return 0


if __name__ == "__main__":
    sys.exit(main())

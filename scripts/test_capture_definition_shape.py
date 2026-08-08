#!/usr/bin/env python3
"""The redaction in capture_definition_shape.py, tested against a definition
built entirely out of things that must not escape a private tenant.

This is a privacy claim printed in a public log, so it is asserted rather than
reviewed: a redactor that quietly passes one value through publishes a real
customer's hostname forever, and the reviewer who would have caught it is
reading a diff, not the output.
"""

import importlib.util
import json
import pathlib
import sys

spec = importlib.util.spec_from_file_location(
    "cds", pathlib.Path(__file__).with_name("capture_definition_shape.py"))
cds = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cds)

# Every leaf here is something a real tenant would rather not publish.
SECRETS = [
    "contoso-prod-warehouse",
    "https://contoso.blob.core.windows.net/raw",
    "SELECT * FROM dbo.Payroll",
    "9f8e7d6c-5b4a-3210-fedc-ba9876543210",
    "sales@contoso.com",
    "/Files/finance/2026/q3-actuals.csv",
]
DEFINITION = {
    "properties": {
        "activities": [
            {
                "name": SECRETS[0],
                "type": "Copy",
                "typeProperties": {
                    "source": {"type": "LakehouseTableSource", "table": SECRETS[0]},
                    "sink": {"type": "LakehouseTableSink", "path": SECRETS[5]},
                    "url": SECRETS[1],
                    "query": SECRETS[2],
                    "enableStaging": False,
                    "parallelCopies": 4,
                    "timeout": None,
                    "expr": "@pipeline().parameters.day",
                },
                "dependsOn": [],
            },
            {
                "name": "notify",
                "type": "Teams",
                "typeProperties": {"recipient": SECRETS[4], "workspaceId": SECRETS[3]},
            },
        ],
        "parameters": {"day": {"type": "string", "defaultValue": SECRETS[5]}},
    }
}


def fail(msg):
    print(f"FAIL: {msg}")
    sys.exit(1)


def main():
    rendered = json.dumps(cds.shape(DEFINITION), indent=2)

    # 1. Nothing from the tenant survives. This is the whole claim.
    for secret in SECRETS:
        if secret in rendered:
            fail(f"a tenant value survived redaction: {secret!r}")

    # 2. The discriminators DO survive — a redactor that ate them would be
    #    safe and useless, and the capture exists for exactly these.
    for kept in ("Copy", "Teams", "LakehouseTableSource", "LakehouseTableSink"):
        if f'"{kept}"' not in rendered:
            fail(f"the discriminator {kept!r} was redacted away — nothing is captured")

    # 3. Key paths survive, because the SHAPE is the other half of the oracle.
    for key in ("typeProperties", "enableStaging", "parallelCopies", "recipient", "dependsOn"):
        if f'"{key}"' not in rendered:
            fail(f"the property name {key!r} was lost — the shape is the point")

    # 4. Types are distinguishable: a caller has to know whether a property
    #    takes a bool, a number or an expression.
    for marker in ("<bool>", "<number>", "<string>", "<null>", "<expression>"):
        if marker not in rendered:
            fail(f"{marker} never appears — the type of a redacted value is still information")

    # 5. `parameters.day.type` is "string" — a discriminator key holding a
    #    harmless value. Kept, and its sibling defaultValue (a real path) is not.
    if '"defaultValue": "<string>"' not in rendered:
        fail("a parameter's defaultValue was not redacted")

    # 6. The discriminator sweep finds every type in the tree, including the
    #    nested source/sink ones a shallow walk would miss.
    found = cds.discriminators(DEFINITION, set())
    for kept in ("Copy", "Teams", "LakehouseTableSource", "LakehouseTableSink", "string"):
        if kept not in found:
            fail(f"discriminators() missed {kept!r}")
    for secret in SECRETS:
        if secret in found:
            fail(f"discriminators() collected a tenant value: {secret!r}")

    print(f"capture redaction: PASS ({len(SECRETS)} tenant values withheld, "
          f"{len(found)} discriminators kept)")


if __name__ == "__main__":
    main()

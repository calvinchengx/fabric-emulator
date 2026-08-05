"""Land the vendor export in OneLake — uploaded by `fab`, not by us.

`fab cp <local> <onelake path>` is the CLI's OneLake data-plane client, and it
is the reason this step exists as its own file: the other examples PUT the bytes
themselves with `requests`, which proves the emulator's DFS surface answers a
hand-written request. This proves it answers Microsoft's own uploader.

The fixture is the same seeded generator the medallion examples use, so the row
counts asserted downstream are the generator's own arithmetic rather than a
number read off a previous run.
"""
import pathlib

import fabctl as fab
import source_system as src

HERE = pathlib.Path(__file__).resolve().parent
LANDING = HERE / "build" / "landing"
LANDING.mkdir(parents=True, exist_ok=True)

# The vendor refuses a wrong key — the same gate the medallion example checks,
# kept here because an export step that cannot fail authentication is not
# modelling a vendor API at all.
try:
    src.export("wrong-key")
    raise AssertionError("source system accepted a wrong API key")
except PermissionError:
    pass

export = src.export(src.API_KEY)
csv = export["customers.csv"]
(LANDING / "customers.csv").write_bytes(csv)
fab.log(f"generated customers.csv ({len(csv):,} bytes, "
        f"{src.EXPECTED_BRONZE_CUSTOMERS:,} rows)")

# The folder FIRST. `fab cp` resolves its destination before copying, and a
# OneLake path that does not exist yet is resolved as a LOCAL path instead —
# so copying straight to Files/landing/customers.csv fails with
# "Source and destination must be of the same type", which reads like a bad
# argument rather than a missing directory. `Files` itself already exists,
# which is why the one-level-down case works and this one does not.
fab.run("mkdir", f"{fab.LAKEHOUSE}/Files/landing")

# /work is this directory, bind-mounted into the fab container (docker-compose.yml).
fab.run("cp", "/work/build/landing/customers.csv",
        f"{fab.LAKEHOUSE}/Files/landing/customers.csv", "-f")

listing = fab.run("ls", f"{fab.LAKEHOUSE}/Files/landing")
assert "customers.csv" in listing, f"fab uploaded nothing it can see back:\n{listing}"
fab.log("fab cp -> OneLake Files/landing/customers.csv, and fab ls sees it")

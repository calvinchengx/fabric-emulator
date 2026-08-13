# ARM capacities consume

Opt-in chain: Microsoft's `azure-mgmt-fabric` creates a `Microsoft.Fabric/capacities`
resource on a **sibling arm-emulator**, and fabric-emulator lists that capacity's
Fabric REST GUID on `GET /v1/capacities`.

This is not a CI job yet. The GHCR arm-emulator image does not serve this
provider until it is released; the harness therefore builds a local checkout.

```bash
# sibling at ../arm-emulator, or ARM_EMULATOR_REPO=/path/to/arm-emulator
python3 e2e/arm-capacities/run.py
```

Standalone fabric-emulator (no `FABRIC_ARM_URL`) is unchanged: one seeded
capacity, enough for fabric-cicd.

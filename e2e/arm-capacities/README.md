# ARM capacities consume

Opt-in chain: Microsoft's `azure-mgmt-fabric` creates a `Microsoft.Fabric/capacities`
resource on arm-emulator, and fabric-emulator lists that capacity's Fabric REST
GUID on `GET /v1/capacities`.

A sibling `../arm-emulator` checkout is used when present (family development).
Otherwise the harness `go install`s the pinned release (`ARM_VERSION`, default
`v0.4.1`). CI has no sibling, so the pin is the witness.

```bash
python3 e2e/arm-capacities/run.py
```

Standalone fabric-emulator (no `FABRIC_ARM_URL`) is unchanged: one seeded
capacity, enough for fabric-cicd.

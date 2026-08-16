# Data Factory pipeline activities, driven by `az`

Microsoft's Azure CLI creating, defining and running real pipelines against
this checkout, then reading `queryactivityruns` and **OneLake** to check what
each activity actually did.

```sh
python3 e2e/pipeline-activities/run.py
```

Needs Docker. Same unmodified `mcr.microsoft.com/azure-cli` image and the same
alias trick as [`e2e/az-rest`](../az-rest/README.md), whose login this suite
**imports** rather than copies — cert harvesting, the ARM stub `az login`
insists on, and private-cloud registration are identical here, and two copies
would drift.

## Why a separate job from `az-rest`

Same client, and these could have been four more checks in that driver. They
are not, because `check_witnesses.py` already reports `ci:az-rest` carrying more
claims than any other witness, under its own *"check none is over-credited"*
heading. Adding the activity rows would mean one flaky container takes two dozen
parity rows red together, and one silent regression hides all of them. Separate
job, separate witness id, separate blast radius.

## What it witnesses

| claim | how |
|---|---|
| Delete Data activity | seed two files, delete one; `filesDeleted` is 1, the target is **gone from OneLake**, and the sibling is **still there** |
| GetMetadata activity (OneLake path) | `exists` / `itemType` / `itemName`, and `size` compared against the bytes actually written |
| Lookup activity | a real CSV in OneLake; `count` is 2, `firstRow.name` is `alice`, **and** that value arrives in a downstream `SetVariable` through `@activity('Lk').output.firstRow.name` |
| Validation activity | present data → `exists` / `itemName` / real `size`; **absent** data → the job **Fails** |

## The rule every check here follows

**Judge the data, not the status.** A Delete that deleted nothing, a Validation
that blessed a file which was never written, and a Lookup that found no rows all
report `Completed`. Reading back a status would witness the job contract and
nothing about the work, which is the false-green shape this repo keeps finding —
so every check above asserts an effect: bytes present or absent in OneLake, a
size matching what was written, a looked-up value arriving somewhere downstream.

Two of them go further and assert the **negative** case, because that is where
the activity earns its place:

- Delete checks the *sibling survived*. Without it, an activity that wiped the
  whole tree would pass.
- Validation checks that absent data **fails the job**. Without it, an activity
  hardcoded to succeed would pass — and a Validation that passes on missing data
  hands the pipeline a guard's blessing to read something that is not there.

## Known trap, recorded because it cost a run

`onelake_read` strips the response, so a payload ending in a newline reads back
shorter than it was written. The first version of the Delete check seeded
`"x\n1\n"`, compared the sibling literally, and failed — looking exactly like a
Delete that had taken the wrong path. Payloads here carry no leading or trailing
whitespace for that reason.

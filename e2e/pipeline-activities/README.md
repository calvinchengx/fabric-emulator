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
| ExecutePipeline | a parent invokes a child **by name**; the child's `Copy` moves bytes, and the destination is read from OneLake — a parent reporting `Completed` having invoked nothing would pass any check that read only the parent |
| ForEach (sequential / parallel) | `@createArray('a','b','c')` with `isSequential` both ways and `batchCount=3`; the body must appear **3 times**, not once |
| Control flow (If / Switch / Until / expressions / dependsOn) | `IfCondition` runs the true branch **and not the false one**; `Switch` runs the matching case and neither the other case nor the default; `Until` runs its body exactly 3 times before the condition holds |
| Per-activity retry policy | an activity pointed at a missing notebook with `policy.retry: 2` — the job **Fails**, and there is **one** activity record carrying `retryAttempt: 2`, not three records |
| Web + WebHook | `WebActivity` GETs a real endpoint and `pong: true` appears in its output; `WebHook` posts, **parks** while nothing has called back, then the callback releases it and `approved: true` reaches the output |

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

## Three traps, all of them mine

Recorded because each cost a run, and because in every case the emulator was
right and the witness was wrong. That ratio is the argument for writing these:
each one forced a reading of the contract instead of an assumption about it.

**`onelake_read` strips its response.** The Delete check seeded `"x\n1\n"` and
compared the sibling literally, so it read back shorter and failed — looking
exactly like a Delete that had taken the wrong path.

**`az rest` on a plain-HTTP endpoint.** Fetching the receiver's own state with
`az rest` attaches an Azure token to something that neither wants nor
understands one. Reading test scaffolding is not part of the claim; `urllib`
does it.

**The WebHook callback contract, wrong twice in one call.** `callBackUri` is a
**path**, not an absolute URL — deliberately, because the emulator cannot know
which base a caller reached it on and "advertising a wrong absolute would be
worse than advertising none". And the callback route takes **no bearer**: an
external receiver has no Fabric token, so possession of the exact URI is the
authentication, exactly as ADF's own `callBackUri` works. `az rest` violated
both. The fix is a plain POST with the emulator's CA trusted, which is what a
real receiver does.

## The TLS finding, which is not a trap

The Web activity originally pointed at the emulator's own `/health` and failed
with `x509: certificate signed by unknown authority`. That is **correct
behaviour**: the emulator serves a self-signed leaf, the Web activity verifies
TLS, and there is no insecure-skip knob for it — deliberately. The wrong fix
would have been to add one so a test could pass. Instead the suite grew a real
HTTP target, which is also closer to what a Web activity does in life.

## Earlier note, kept

`onelake_read` strips the response, so a payload ending in a newline reads back
shorter than it was written. The first version of the Delete check seeded
`"x\n1\n"`, compared the sibling literally, and failed — looking exactly like a
Delete that had taken the wrong path. Payloads here carry no leading or trailing
whitespace for that reason.

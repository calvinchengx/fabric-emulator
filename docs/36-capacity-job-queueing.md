# 36 — Capacity job queueing: the gap, and why a broker is the wrong answer

**Status: not implemented, and deliberately not built yet.** The emulator has no
job concurrency model at all — no admission control, no queue, no throttling,
and no `429`/`430` anywhere in `internal/`. Real Fabric has all of it, and this
document scopes what closing that would mean.

It exists because the question arrived in the shape of an answer — *"do we need
a built-in queue, NATS for example?"* — after a scheduled notebook run collided
with an event-triggered one. It is worth separating the three things that got
tangled there, because two of them are not this gap.

## What actually happened, and what it was not

A scheduled run of a notebook started one second into an event-triggered run of
the same notebook. Both wrote the same Delta tables. One lost:

```
SparkRuntimeException: Failed to commit transaction: 0
```

**That is correct.** Real Fabric does not mutually exclude a scheduled run and a
triggered run of one notebook; they land on a Spark pool together and Delta's
optimistic concurrency decides. An emulator that serialised them would report
success where production reports a conflict — it would make consumer code look
safer than it is, which is the worst thing this project can do.

So the collision was not the bug. Two other things were, and neither is a queue:

- The consumer's own pipeline raced itself, scheduling a notebook while its
  previous step's run was still in flight. Fixed there, by waiting.
- The consumer's assertion read only that a job *existed* with
  `invokeType=Scheduled`, never its status — so a failed run reported "the
  platform runs unattended" and the pipeline went green. Also fixed there, and
  it is [10-testing.md](10-testing.md)'s recurring failure once more.

Queueing would have prevented none of it. Both jobs would still have been
admitted, and they would still have collided.

## The real gap

Fabric bounds work by **capacity**, not by item:

| Behaviour | Fabric | Emulator |
|---|---|---|
| Background jobs queue when a capacity is saturated | ✅ | ❌ nothing |
| Interactive requests are throttled / rejected | ✅ `429`, `430` | ❌ nothing |
| A job reports that it is queued rather than running | ✅ | ❌ no such state |
| Bursting and smoothing over a window | ✅ | ❌ |

A consumer writing code that must survive a busy capacity — retry on `430`,
tolerate a job sitting queued for minutes, back off — **cannot exercise any of
that here.** Every job is admitted instantly and forever. That is a real
fidelity gap and it is the interesting half of the question.

## Why not NATS, or any broker

The emulator is one Go binary over SQLite: distroless, no volume required,
`docker compose up` and it answers. A broker would mean the emulator could no
longer run without a message bus — a new container and a new dependency for
every consumer, to model behaviour that lives inside Fabric's own control plane
and is not observable as a queue from outside.

Nothing about the contract is message-shaped. What a client can see is:

- a job instance in a `Queued`-like state instead of running,
- a `429`/`430` with a `Retry-After`,
- and the ordering in which queued work is admitted.

All three are **state**, and the emulator already has a store for state and a
controllable clock for making time pass without waiting. A queue table with a
capacity, drained on the same clock ticks the scheduler already uses, gives the
whole observable contract with no new process.

(Consumers may well run a broker — `contoso-data-platform` runs Redpanda for the
ERP change stream. That is a *source system* being modelled, at a different
layer entirely. It is not evidence that the emulator should contain one.)

## Scope, if it is built

1. **Capacity limits as store state.** A per-capacity concurrent-job ceiling,
   seeded with a default and overridable per capacity, so a test can set it to 1
   and get deterministic queueing.
2. **A queued job state**, reported on the job instance the way Fabric reports
   it, so a client polling a job sees "not started yet" rather than "running".
3. **Admission on the existing clock.** The scheduler already evaluates on clock
   moves and on list; draining the queue there keeps the one lever that makes
   this testable without sleeping.
4. **`429`/`430` on the interactive path**, with `Retry-After` — the header
   consumer retry logic actually reads.
5. **An e2e that saturates a capacity of 1** and proves a second job waits, then
   runs, and that a client which ignores `Retry-After` fails.

Size: **M**. No engine work and no research risk — it is control-plane state and
one admission check. The value is entirely in item 5: without a test that a job
really waits, a `Queued` field is decoration.

## What must NOT be done

**Do not serialise same-item jobs to avoid write conflicts.** That is the change
this document exists to argue against. The conflict is the behaviour a consumer
needs to meet, and hiding it here means meeting it for the first time in
production.

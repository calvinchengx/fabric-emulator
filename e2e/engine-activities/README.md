# The pipeline activities that execute code

Custom (ADF `Batch`), `HDInsightSpark` and `SparkJobDefinition`, run against the
**shipped Spark agent** with Sail behind it.

```sh
python3 e2e/engine-activities/run.py
```

## Why these three are not in `e2e/pipeline-activities`

They need an engine. That suite's value is being a fast `az`-CLI stack that a
flaky Sail cannot take red, and it already carries ten claims. Separate suite,
separate blast radius — the same reasoning that split it from `e2e/az-rest`.

## What a fake agent could not tell you

Each of these was witnessed only by a Go test driving a **fake** agent that
records the statement it was handed. That proves *dispatch*: the emulator sent
something. It cannot show that the command ran, what it printed, or that a
failure fails the activity.

| activity | positive | the negative half, which carries the row |
|---|---|---|
| Custom | `exitCode: 0` and a marker in `stdout` | `exit 3` must **fail the activity**. Without it the row is satisfied by an activity that runs the command and ignores its verdict — the shape the parity doc names as why Custom was once off-by-default |
| HDInsightSpark | a valid entry file succeeds | an entry file that **raises** must fail. A submission that never executed the code cannot fail on its contents |
| SparkJobDefinition | a valid `main.py` part succeeds | a second item differing **only** in its `main.py` must fail. That is what proves the part is executed rather than named |

## The assertion that had to change, and why the replacement is better

The first version asserted a marker printed by the entry file. It could never
work: unlike Custom, this activity's output carries `rootPath`, `entryFilePath`,
`arguments` and `executedBy` — and **not the program's stdout**. The two report
differently, which the first draft assumed away.

Running the same activity twice and changing only the FILE is stronger than a
stdout marker would have been: it asserts a property of the *execution* rather
than of the *reporting*, so it survives any change to what the output carries.

# pbix-desktop — does Power BI Desktop itself read what we generate?

**A spike that passed: 5 of 5** (run 30821205384), 6m41s–8m05s per attempt,
agreeing with `executeQueries` bit-for-bit (`rel = 0.000e+00`). It answers one
question: *does Microsoft's own Power BI Desktop open a
`.pbix` built from our TMSL, and does it evaluate the same DAX to the same
numbers as `executeQueries`?*

[docs/33-pbix-tooling.md](../../docs/33-pbix-tooling.md) is the plan; this is
Phase 0b of it.

## Why it exists

Phase 0 established that a `.pbix` built from our model round-trips: pbix-mcp
writes it, pbixray reads it back, and its DAX matches ours to below IEEE double
epsilon. That is two independent **Python** implementations agreeing. No
Microsoft code has read the file.

Desktop hosts a real Analysis Services instance (`msmdsrv.exe`) on a loopback
port — which is how Tabular Editor and DAX Studio attach, and the only way to
make Desktop answer a question without a human clicking. Connecting to it turns
the claim from *"two libraries agree"* into *"Desktop loaded it and answered."*

## What it does

| Stage | Reported as | Meaning of failure |
|---|---|---|
| `download` | `STAGE download :: OK (n MB)` | The installer URL moved |
| `install` | `STAGE install :: OK` | Silent install refused |
| `locate` | `STAGE locate :: OK <path>` | Installed somewhere unexpected |
| `launch` | `STAGE launch :: OK pid=n` | The GUI would not start |
| `port` | `STAGE port :: OK <file>` | **Started but never hosted Analysis Services** — the display/Server risk landing |
| `connect` | `STAGE connect :: OK` | Listening but refusing ADOMD |
| `query` | `STAGE query :: OK` | Connected but the DAX failed |

Stages are separate on purpose. A single pass/fail would collapse "Desktop
cannot run here at all" and "Desktop ran and disagreed with us" into one red
tick, and those are opposite findings.

## What can be tested WITHOUT Windows

`verify.py` holds the port-file parsing, the row parsing and the numeric
comparison, and `test_verify.py` exercises them anywhere in milliseconds:

```bash
uv run --frozen --group test pytest e2e/pbix-desktop/test_verify.py -q
```

That split is deliberate. Everything that can only run in CI tends to be
written once and debugged by watching CI fail, so the logic that does not need
Desktop is kept where it can be run and mutated locally. Three mutations were
used to check the suite is not decorative — scraping every line instead of
`ROW` lines, treating an empty result set as agreement, and swapping the
relative tolerance for an absolute one. The third initially passed, which is
how `test_the_tolerance_is_relative_and_this_is_the_case_that_proves_it` came
to exist: one ulp on a 25-million figure is `3.7e-09` absolute, which an
absolute epsilon rejects and a relative one accepts.

## The two documented risks, both measured false

- `windows-latest` is Windows **Server** 2025 and Microsoft recommends a
  **client** Windows. It runs anyway: the stated reason — IE Enhanced Security
  blocking sign-in to the Power BI service — does not apply to opening a local
  file.
- Desktop documents a **1440x900 minimum display** and no **system account**. A
  hosted runner's virtual display under `runneradmin` satisfies both.

Neither was answerable by reading. Both took eight minutes to settle.

## What it still CANNOT establish

The **licence** position on automated use — Microsoft documents
`-quiet ACCEPT_EULA=1`, which evidences automated *install*, not a reading of
the EULA. And **durability**: five runs on one afternoon against one Desktop
build is a pass rate, not a trend, which is why the schedule is weekly.

## Flake is a result, not a nuisance

A GUI application driven headlessly that passes four times in five is a coin,
not an oracle. The decision to adopt this rests on a measured pass rate over
repeated runs and on which stage the failures land in — not on whether it ever
went green once.

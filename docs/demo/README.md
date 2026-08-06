# Demo GIFs

Two, answering different questions. **Both are generated** — neither is a
cropped screen recording, and the script beside each is its source of truth.

| | Shows | Recorded by |
|---|---|---|
| `demo.gif` | a real Entra token → workspace → lakehouse → a file written to and read back from OneLake, against two local binaries | [VHS](https://github.com/charmbracelet/vhs), `demo.tape` |
| `flow.gif` | the **advanced medallion** drawing itself in the portal's Data flow view — three sources → bronze → silver → a Warehouse star → a semantic model, 35 nodes | Playwright, `flow.py` + `flow_scene.js` |

## Regenerate `demo.gif`

```sh
brew install vhs                 # pulls ttyd + ffmpeg
brew install calvinchengx/tap/entra-emulator   # the issuer the demo needs
vhs docs/demo/demo.tape          # from the repo root → rewrites demo.gif
```

The tape builds `fabric-emulator`, starts it against a local entra-emulator on
odd ports (`:18099` / `:19099`, so a dev stack on `:8443` / `:9443` does not
collide), runs the demo, then stops both. No containers — two Go binaries, which
is what keeps a ~40s recording feasible.

Two things that bite when editing it:

- **VHS has no escape sequence.** `Type "... \" ..."` does not parse; commands
  needing double quotes are delimited with backticks instead.
- **`-data-dir` must exist.** The emulator opens its SQLite file inside it but
  will not create it, so a bare `rm -rf` leaves it failing with
  `unable to open database file (14)` — and the tape then hangs on a health
  endpoint that never comes up rather than failing.

## Regenerate `flow.gif`

```sh
brew install ffmpeg
pnpm install
pnpm --filter fabric-emulator-portal exec playwright install chromium
uv run --frozen --group demo python docs/demo/flow.py
```

Also needs **Microsoft ODBC Driver 18** — gold is built by dbt-fabric over real
TDS, on the host.

`flow.py` builds the emulator from the tree, brings up the full stack
(engines + the governance profile), starts the recorder, and **only then** runs
[`examples/medallion-advanced-pyspark`](../../examples/medallion-advanced-pyspark/)
unmodified — so the graph is empty when filming begins and fills in on camera.
23 steps, about five minutes.

**It films the example rather than a purpose-built seed.** The hero image should
show what the README claims, driven by code a reader can run; and `pipeline.py`
asserts its own results, so a recording that completes is also a passing test.
Nothing is staged for the camera.

Every published port is remapped (`DEMO_FABRIC_PORT` and friends), so this runs
beside a dev stack — or beside a sibling project holding 9443, which is why. The
example is pointed at them through the environment variables it already reads,
so no endpoint is written down twice.

### Four deliberate choices

- **Played at 16×.** Five minutes is not a README image. Speeding the playback
  keeps every frame rather than cutting a window out of the middle;
  `DEMO_SPEED=1` gives real time.
- **Recorded at 1280×800, shipped at 960.** The graph reaches 35 nodes across
  seven columns; at 960 the frame would hold four of them, so the hero would be
  the top-left corner of a medallion rather than its shape.
- **64 colours.** The dark palette is a few greys and one green — at 128 this
  take is 4.2 MiB and over the ceiling, at 64 it is 3.3 MiB and looks the same.
- **It ends on the star.** The chain runs left to right, so the recorder scrolls
  to the right-hand end and holds there: the payoff the run exists to reach,
  rather than the bronze tables it started with.

**A blank take is refused.** `flow_scene.js` exits non-zero below 2 graph nodes
or 1 log row. The dangerous failure is not a crash — it is a valid GIF of an
empty view shipped as the hero image.

The previous `flow.gif` could not be regenerated here at all: it was a
hand-cropped excerpt of a run in the sibling **contoso-data-platform** repo, and
had drifted far enough to predate the terminal pane.

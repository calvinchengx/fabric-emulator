# Demo GIFs

Two, answering different questions. **Both are generated** — neither is a
cropped screen recording, and the script beside each is its source of truth.

| | Shows | Recorded by |
|---|---|---|
| `demo.gif` | a real Entra token → workspace → lakehouse → a file written to and read back from OneLake, against two local binaries | [VHS](https://github.com/charmbracelet/vhs), `demo.tape` |
| `flow.gif` | a medallion drawing itself in the portal's **Data flow** view: `bronze_orders → silver_orders → gold_orders`, with the event log filling in beside it | Playwright, `flow.py` + `flow_scene.js` |

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

Ports are overridable when a dev stack holds them:
`DEMO_FABRIC_PORT=9643 DEMO_ENTRA_PORT=8643 DEMO_KV_PORT=8644`.

`flow.py` builds the emulator from the tree, starts two containers (it and
entra-emulator, because real tokens are validated), starts the recorder, and
**only then seeds** — so the graph is empty when filming begins and fills in on
camera. It writes `bronze_orders` with delta-rs, runs two Copy activities
through the emulator's own executor, then converts the take with ffmpeg.

**No Spark, ODBC driver or catalog needed.** The graph is drawn from recorded
lineage, and lineage only comes from movements the emulator can *know* — a Copy
activity is one, because its own executor moved the bytes.

The previous `flow.gif` could not be regenerated here at all: it was a
hand-cropped excerpt of a run in the sibling **contoso-data-platform** repo
(three vendor sources, an OpenMetadata catalog), and had drifted far enough to
predate the terminal pane. For that richer story, record it there.

Three deliberate choices:

- **Plays at 2×; the take is real time.** A Delta write and two pipeline runs
  take ~40s and nobody watches a 40s README image. Speeding playback keeps every
  frame instead of cutting a window out of the middle. `DEMO_SPEED=1` for real
  time.
- **Recorded at the size it ships in** (900×620, via `DEMO_WIDTH`/`DEMO_HEIGHT`),
  not cropped after. The old asset was a 760×700 crop of a 1600×900 take, so its
  framing depended on the layout that week.
- **A blank take is refused.** `flow_scene.js` exits non-zero below 2 graph nodes
  or 1 log row. The dangerous failure is not a crash — it is a valid GIF of an
  empty view shipped as the hero image.

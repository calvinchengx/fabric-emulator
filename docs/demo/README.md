# Demo GIFs

Two, because they answer different questions.

| | Shows | Recorded by |
|---|---|---|
| `demo.gif` | a real Entra token → a workspace → a lakehouse → a file written to and read back from OneLake, all against two local binaries | [VHS](https://github.com/charmbracelet/vhs), `demo.tape` |
| `flow.gif` | a medallion drawing itself in the portal's **Data flow** view: `bronze_orders → silver_orders → gold_orders`, with the event log filling in beside it | Playwright, `flow.py` + `flow_scene.js` |

**Both are generated.** Neither is a screen recording somebody cropped, and
neither can go stale without someone choosing to leave it stale — the script
beside it is the source of truth.

## Regenerate `demo.gif`

Deterministic via VHS — the `.tape` is the source of truth:

```sh
brew install vhs                 # pulls ttyd + ffmpeg
brew install calvinchengx/tap/entra-emulator   # the issuer the demo needs
vhs docs/demo/demo.tape          # run from the repo root → rewrites demo.gif
```

The tape builds `fabric-emulator`, starts it against a local entra-emulator on
odd ports (`:18099` / `:19099`, so a dev stack on `:8443` / `:9443` does not
collide), runs the demo, then stops both. No containers are involved — it is
two Go binaries, which is what keeps a ~40s recording feasible.

### Two things that will bite you when editing the tape

**VHS has no escape sequence.** `Type "... \" ..."` does not parse. Commands
needing double quotes are delimited with backticks instead — inside those, both
`'` and `"` pass through literally.

**`-data-dir` must exist.** The emulator opens its SQLite file inside that
directory but will not create it, so a bare `rm -rf` leaves it failing to start
with `unable to open database file (14)` — and because the tape then waits on a
health endpoint that never comes up, the recording hangs rather than fails.

## Regenerate `flow.gif`

```sh
brew install ffmpeg
pnpm install
pnpm --filter fabric-emulator-portal exec playwright install chromium
uv run --frozen --group demo python docs/demo/flow.py
```

Ports are overridable for a machine where a dev stack already holds them:

```sh
DEMO_FABRIC_PORT=9643 DEMO_ENTRA_PORT=8643 DEMO_KV_PORT=8644 \
    uv run --frozen --group demo python docs/demo/flow.py
```

`flow.py` builds the emulator from the tree, starts **two** containers (the
emulator and entra-emulator, because real tokens are validated), starts the
recorder, and only then seeds — so the graph is empty when filming begins and
fills in on camera. It writes `bronze_orders` with delta-rs, runs two Copy
activities through the emulator's own executor, then stops the recorder and
converts the take with ffmpeg.

### Why it needs no Spark, no ODBC driver and no catalog

The graph is drawn from recorded **lineage**, and lineage comes only from
movements the emulator can KNOW. A Copy activity is one: its own executor moved
the bytes. So the medallion on camera is produced by two containers and one
script, which is what makes this reproducible from this repository alone.

The previous `flow.gif` was not. It was a hand-cropped excerpt of a run in the
sibling **contoso-data-platform** repository — three vendor sources and an
OpenMetadata catalog — so regenerating the README's hero image meant driving a
different project, and the asset had drifted far enough to predate the terminal
pane. If you want that richer story back, record it there; this one is the
emulator's own.

### Three things to know before editing

**The GIF plays at 2×, and the take is real time.** A Delta write and two
pipeline runs take about forty seconds, and nobody watches a forty-second README
image. Speeding the playback keeps every frame of what happened rather than
cutting a window out of the middle. `DEMO_SPEED=1` gives real time.

**It is recorded at the size it ships in** — 900×620, set by `DEMO_WIDTH` /
`DEMO_HEIGHT` — rather than cropped afterwards. The old asset was a 760×700 crop
of a 1600×900 recording, so its framing depended on where the portal's layout
happened to put things that week.

**A blank take is refused.** `flow_scene.js` exits non-zero unless it saw at
least two graph nodes and one log row, because the failure that matters here is
not a crash: it is a perfectly valid GIF of an empty view, shipped as the
README's hero image, saying the product does nothing.

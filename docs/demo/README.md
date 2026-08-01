# Demo GIF

`demo.gif` is the README hero — a real Entra token → a workspace → a lakehouse →
a file written to and read back from OneLake, all against two local binaries.

## Regenerate

Deterministic via [VHS](https://github.com/charmbracelet/vhs) — the `.tape` is
the source of truth:

```sh
brew install vhs                 # pulls ttyd + ffmpeg
brew install calvinchengx/tap/entra-emulator   # the issuer the demo needs
vhs docs/demo/demo.tape          # run from the repo root → rewrites demo.gif
```

The tape builds `fabric-emulator`, starts it against a local entra-emulator on
odd ports (`:18099` / `:19099`, so a dev stack on `:8443` / `:9443` does not
collide), runs the demo, then stops both. No containers are involved — it is
two Go binaries, which is what keeps a ~40s recording feasible.

## Two things that will bite you when editing the tape

**VHS has no escape sequence.** `Type "... \" ..."` does not parse. Commands
needing double quotes are delimited with backticks instead — inside those, both
`'` and `"` pass through literally.

**`-data-dir` must exist.** The emulator opens its SQLite file inside that
directory but will not create it, so a bare `rm -rf` leaves it failing to start
with `unable to open database file (14)` — and because the tape then waits on a
health endpoint that never comes up, the recording hangs rather than fails.

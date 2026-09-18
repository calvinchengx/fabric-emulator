# Microsoft Fabric REST API specifications (Swagger 2.0)

Microsoft's own machine-readable definition of the Fabric REST surface: 55
`swagger.json` files over 62 item and platform areas, plus the `definitions.json`
each refers to and the shared schemas under `common/`.

- **Source:** https://github.com/microsoft/fabric-rest-api-specs
- **Commit:** `8baaa4c9efbf8c16de78bc3c55f475cb2bc53490` (2026-09-15)
- **Licence:** MIT (`LICENSE.txt`, kept alongside).
- **Pruned:** the upstream `examples/` trees and the repo's own governance
  Markdown. Neither is read by the checker, and a vendored tree should carry
  what it is used for and nothing else.
- **Used by:** `scripts/check_openapi_conformance.py`, which validates responses
  the emulator actually returned against the schema for the route that returned
  them. It runs in the `fabric-cli` CI job, immediately after that suite — and
  NOT in `make check`, because its input is a recording that only exists once a
  suite has run. A gate that needs a live stack does not belong in the offline
  one, and claiming it did would be the kind of unbacked sentence this
  repository writes checkers to catch.

## Why this is here rather than consulted

`third_party/powerbi-rest-swagger/` was vendored as the "golden reference" for
`executeQueries` and has only ever been READ — cited in Go comments, never
compared against a response. That is the same gap this repository keeps finding
elsewhere: a claim with nothing underneath it. This tree is vendored to be
EXECUTED against, not to be consulted.

It is also a different KIND of oracle from the rest of the witness set. Every
`ci:` witness is a client: it drives a surface and its own model rejects a wrong
shape, which is strong and narrow. A surface no packaged client speaks has only
this repository's tests behind it — the implementation and the expectation
written by the same hand. A published schema is neither of those.

## What conformance does and does not mean

SHAPE, NEVER SEMANTICS. A pass means the answer was shaped the way Microsoft
documents; it says nothing about whether it was true. The failure this
repository exists to hunt — a job reported `Succeeded` that ran nothing — has a
perfectly conformant shape. That still needs a client and an engine.

## Refresh

Re-clone the repository at a newer commit, prune `examples/` and the governance
Markdown, and update the commit and date above. Deliberately manual and
deliberately not in `make check`: an upstream spec change must be a reviewable
re-pin, not a surprise red on an unrelated pull request.

#!/usr/bin/env python3
"""Generate the portal's event contract from internal/store/bus.go.

WHY GENERATE RATHER THAN SERVE. The SSE stream names every frame
(`event: <kind>`), and EventSource has no wildcard listener — so a kind absent
from the client's subscription list is INVISIBLE. No error, no dropped count,
no event. The portal carried that list as a literal in three places, and nothing
checked them against the Go constants.

An endpoint the client fetches at runtime would also work, and would be the
right answer if the client could be older than the server. It cannot:
portal/embed.go embeds portal/dist into the binary, so the portal and the
emulator are the same artifact and always ship together. Generating therefore
makes drift IMPOSSIBLE rather than detected, with no endpoint, no round trip and
no fallback path to get wrong.

WHY TYPESCRIPT. The list alone only guarantees the client SUBSCRIBES to every
kind; it says nothing about whether the client can render one. Emitting
`EventKind` as a union turns a `switch` that forgot a kind into a build failure,
and emitting the `Event` struct as an interface does the same for a renamed
field — `ev.rowsAdded` becoming `ev.rows` is a silent `undefined` in JavaScript
and a compile error here.

The labels and the field comments come from the Go declarations, so the one
place a kind or a field is described is the one place it is declared.

Usage:
    gen_event_kinds.py            rewrite portal/src/eventKinds.ts
    gen_event_kinds.py --check    exit non-zero if it is out of date
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BUS = ROOT / "internal" / "store" / "bus.go"
OUT = ROOT / "portal" / "src" / "eventKinds.ts"

# `KindFile     = "file"     // a OneLake path was written, renamed, or deleted`
CONST = re.compile(r'^\s+Kind(\w+)\s+=\s+"([a-z]+)"\s*//\s*(.+?)\s*$', re.M)
# `var ViewKinds = []string{ … }` — the slice bodies, so the split between a
# platform event and the subscriber signal is Go's decision, not this script's.
SLICE = r"var\s+%s\s*=\s*\[\]string\{(.*?)\}"
# `type Event struct { … }` up to the closing brace in column 0.
STRUCT = r"^type %s struct \{\n(.*?)^\}"
# `\tVersion      *int64  `json:"version,omitempty"` // trailing note`
FIELD = re.compile(r'^\t(\w+)\s+(\S+)\s+`json:"([^",]+)(,omitempty)?"`\s*(?://\s*(.*))?$')

# Go's numeric widths all arrive as JSON numbers; a pointer is simply a field
# that may be absent, which `omitempty` has already said.
SCALARS = {
    "string": "string",
    "bool": "boolean",
    "int": "number",
    "int8": "number",
    "int16": "number",
    "int32": "number",
    "int64": "number",
    "float32": "number",
    "float64": "number",
}
# Structs this generator also emits, so a field may refer to one by name. Any
# other named type is a hole in the contract and stops the generator rather
# than becoming an `any` nobody notices.
STRUCTS = ("Attribution",)


def slice_members(src: str, name: str) -> list[str]:
    m = re.search(SLICE % name, src, re.S)
    if not m:
        sys.exit(f"gen_event_kinds: no `var {name} = []string{{…}}` in {BUS.name}")
    return re.findall(r"Kind(\w+)", m.group(1))


def ts_type(go_type: str, json_name: str, struct: str) -> str:
    """The TypeScript for one Go field type."""
    # `kind` is the discriminant: typing it `string` would leave every switch
    # over it non-exhaustive, which is the entire point of generating this.
    if struct == "Event" and json_name == "kind":
        return "EventKind"
    bare = go_type.lstrip("*")
    if bare in SCALARS:
        return SCALARS[bare]
    if bare in STRUCTS:
        return bare
    sys.exit(f"gen_event_kinds: {struct}.{json_name} is a {go_type}, which this "
             f"generator has no TypeScript for — add it to SCALARS/STRUCTS "
             f"deliberately rather than letting the field go untyped")


def interface(src: str, go: str, ts: str, doc: list[str]) -> list[str]:
    """Emit the Go struct `go` as the TS interface `ts`, comments and all.

    The Go comments are carried through verbatim: they are already `//` lines,
    and they explain things a reader of the wire format needs (why `version` is
    a pointer, why the job status vocabulary is not the API's enum). Copying
    them keeps the explanation beside the field rather than only in the Go.
    """
    m = re.search(STRUCT % go, src, re.S | re.M)
    if not m:
        sys.exit(f"gen_event_kinds: no `type {go} struct` in {BUS.name}")
    out = [*doc, f"export interface {ts} {{"]
    for line in m.group(1).split("\n"):
        if not line.strip():
            out.append("")
            continue
        if line.lstrip().startswith("//"):
            out.append("  " + line.strip())
            continue
        f = FIELD.match(line)
        if not f:
            sys.exit(f"gen_event_kinds: cannot read this line of {go}:\n  {line}\n"
                     f"every field needs a json tag for the wire shape to be known")
        _, go_type, json_name, omit, note = f.groups()
        opt = "?" if omit else ""
        tail = f" // {note}" if note else ""
        out.append(f"  {json_name}{opt}: {ts_type(go_type, json_name, go)};{tail}")
    while out and not out[-1]:
        out.pop()
    return [*out, "}", ""]


def render() -> str:
    src = BUS.read_text(encoding="utf-8")
    consts = {name: (value, doc) for name, value, doc in CONST.findall(src)}
    if not consts:
        sys.exit(f"gen_event_kinds: no Kind… constants found in {BUS.name} — "
                 f"this generator is reading the wrong thing")

    all_names = slice_members(src, "AllKinds")
    view_names = slice_members(src, "ViewKinds")
    for n in all_names + view_names:
        if n not in consts:
            sys.exit(f"gen_event_kinds: AllKinds/ViewKinds names Kind{n}, which is "
                     f"not declared in {BUS.name}")

    lines = [
        "// GENERATED by scripts/gen_event_kinds.py from internal/store/bus.go.",
        "// Do not edit: `make check` fails when this file and the Go declarations",
        "// disagree. Change them there and regenerate.",
        "//",
        "// The SSE stream names every frame (`event: <kind>`) and EventSource has no",
        "// wildcard listener, so a kind missing from EVENT_KINDS is not merely",
        "// unfiltered — it never arrives at all. The types go further: a `switch`",
        "// over EventKind that forgets a kind, or a field renamed in Go, is a compile",
        "// error rather than a blank on screen.",
        "",
        "/** Every kind the bus can carry. Subscribe to all of them. */",
        "export const EVENT_KINDS = [",
    ]
    lines += [f"  {consts[n][0]!r}," for n in all_names]
    lines += [
        "] as const;",
        "",
        "/** The kinds a user may filter: things that happened to the PLATFORM.",
        " *",
        " * `dropped` is deliberately absent — it reports that THIS browser fell",
        " * behind, so hiding it would hide that the log is incomplete.",
        " */",
        "export const VIEW_KINDS = [",
    ]
    lines += [f"  {consts[n][0]!r}," for n in view_names]
    lines += [
        "] as const;",
        "",
        "/** Every kind, as a type: `switch (ev.kind)` is exhaustive against this. */",
        "export type EventKind = (typeof EVENT_KINDS)[number];",
        "",
        "/** The filterable kinds, as a type. */",
        "export type ViewKind = (typeof VIEW_KINDS)[number];",
        "",
        "/** What each kind means, for tooltips and for anyone reading this file. */",
        "export const KIND_DOC: Record<EventKind, string> = {",
    ]
    lines += [f"  {consts[n][0]!r}: {consts[n][1]!r}," for n in all_names]
    lines += [
        "};",
        "",
        "/** Narrows an arriving `kind` to one this build knows.",
        " *",
        " * The portal ships inside its own emulator so it cannot be out of step with",
        " * its server, but `/_emulator/events` is a stream anything may read and",
        " * anything may point at a different build — so a parsed frame is treated as",
        " * untrusted rather than assumed to match these types.",
        " */",
        "export function isEventKind(kind: string): kind is EventKind {",
        "  return (EVENT_KINDS as readonly string[]).includes(kind);",
        "}",
        "",
        "/** Is this a kind the log renders? See VIEW_KINDS. */",
        "export function isViewKind(kind: string): kind is ViewKind {",
        "  return (VIEW_KINDS as readonly string[]).includes(kind);",
        "}",
        "",
    ]
    lines += interface(src, "Attribution", "Attribution", [
        "/** Which unit of work moved some bytes. Never inferred: an engine reports",
        " * it, or the emulator's own executor knows it because it did the write.",
        " */",
    ])
    # `EmulatorEvent`, not `Event`: the Go struct is store.Event, but `Event` is a
    # DOM global, and an interface of that name inside a component shadows the
    # type every `onclick`/`onsubmit` handler is checked against.
    lines += interface(src, "Event", "EmulatorEvent", [
        "/** One event, in the flat envelope the SSE endpoint serialises.",
        " *",
        " * `store.Event` in Go; renamed here because `Event` is a DOM global and an",
        " * interface of that name would shadow the type DOM event handlers are",
        " * checked against.",
        " *",
        " * Fields are per-kind and absent when they do not apply, so this is every",
        " * kind's shape in one interface rather than a discriminated union: the",
        " * server emits one struct and this mirrors it exactly.",
        " */",
    ])
    lines += [
        "/** An event as it ARRIVES, before its kind has been checked. */",
        "export type RawEmulatorEvent = Omit<EmulatorEvent, 'kind'> & { kind: string };",
        "",
    ]
    return "\n".join(lines)


def main() -> int:
    want = render()
    if "--check" in sys.argv:
        have = OUT.read_text(encoding="utf-8") if OUT.exists() else ""
        if have != want:
            print(f"{OUT.relative_to(ROOT)} is out of date — run "
                  f"scripts/gen_event_kinds.py", file=sys.stderr)
            return 1
        print(f"event kinds: {OUT.relative_to(ROOT)} matches internal/store/bus.go")
        return 0
    OUT.write_text(want, encoding="utf-8")
    print(f"wrote {OUT.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

"""The event-contract generator must read Go correctly, or lie in TypeScript.

`scripts/gen_event_kinds.py` turns `internal/store/bus.go` into the portal's
`eventKinds.ts`. Everything downstream trusts it: the portal subscribes to the
kinds it lists, `EventKind` is what makes a forgotten `switch` arm a compile
error, and `EmulatorEvent` is what makes a renamed field one.

A generator that misreads its input does not fail — it emits something plausible.
A field whose type it does not recognise becomes `any`; a `,omitempty` it misses
becomes a required field the server never sends. Both compile. So each case here
feeds it Go and asserts what it produced, and the malformed cases assert it STOPS
rather than guessing.

The happy path runs against the REAL bus.go, because a fixture would keep passing
after the struct changed shape.
"""
import importlib.util
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

spec = importlib.util.spec_from_file_location(
    "gen_event_kinds", REPO / "scripts" / "gen_event_kinds.py")
assert spec and spec.loader
gen = importlib.util.module_from_spec(spec)
sys.modules["gen_event_kinds"] = gen
spec.loader.exec_module(gen)


# A miniature bus.go: enough shape for every branch, small enough to read.
BUS = '''package store

const (
	KindFile    = "file"    // a OneLake path was written
	KindDropped = "dropped" // a subscriber fell behind
)

var AllKinds = []string{
	KindFile, KindDropped,
}

var ViewKinds = []string{
	KindFile,
}

type Event struct {
	Seq  int64  `json:"seq"`
	At   int64  `json:"at"` // emulator time
	Kind string `json:"kind"`

	// KindFile
	Path string `json:"path,omitempty"`
	// Version is a pointer so version 0 is still reported.
	Version   *int64       `json:"version,omitempty"`
	Ratio     float64      `json:"ratio,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Attribution *Attribution `json:"attribution,omitempty"`
}

type Attribution struct {
	JobID     string `json:"jobId,omitempty"`
	CellIndex *int   `json:"cellIndex,omitempty"`
}
'''


def render(bus_source: str, tmp_path: Path, monkeypatch) -> str:
    """Run the generator over `bus_source` instead of the real bus.go."""
    fake = tmp_path / "bus.go"
    fake.write_text(bus_source, encoding="utf-8")
    monkeypatch.setattr(gen, "BUS", fake)
    return gen.render()


# --- the happy path, against the real file ---------------------------------

def test_the_real_bus_generates_the_committed_file():
    """The generator and the committed contract agree.

    This is the same claim `make check` makes, asserted here so a change to the
    generator that forgets to regenerate is caught by the unit suite too — and
    so the functions below are exercised against the real input, not only a
    fixture that cannot drift.
    """
    assert gen.render() == gen.OUT.read_text(encoding="utf-8")


def test_every_declared_kind_reaches_the_client_list():
    src = (REPO / "internal" / "store" / "bus.go").read_text(encoding="utf-8")
    declared = {value for _, value, _ in gen.CONST.findall(src)}
    out = gen.render()
    for kind in declared:
        assert f"  {kind!r}," in out, f"{kind} is declared in Go but not in EVENT_KINDS"


# --- reading the constants -------------------------------------------------

def test_the_kind_union_and_the_runtime_list_come_from_the_same_slice(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    assert "export const EVENT_KINDS = [\n  'file',\n  'dropped',\n] as const;" in out
    # The union is DERIVED from the array rather than emitted twice: two
    # literals could disagree, and the one nothing reads would be wrong.
    assert "export type EventKind = (typeof EVENT_KINDS)[number];" in out


def test_dropped_is_carried_but_not_offered_as_a_filter(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    view = out.split("export const VIEW_KINDS = [")[1].split("]")[0]
    assert "'file'," in view
    assert "'dropped'" not in view, "hiding the signal that says the log is incomplete"


def test_each_label_comes_from_its_go_comment(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    assert "'file': 'a OneLake path was written'," in out
    assert "'dropped': 'a subscriber fell behind'," in out


# --- reading the struct ----------------------------------------------------

def test_omitempty_becomes_optional_and_a_required_field_stays_required(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    assert "  seq: number;" in out
    assert "  path?: string;" in out


def test_kind_is_the_union_not_a_string(tmp_path, monkeypatch):
    """`kind: string` would leave every switch over it non-exhaustive."""
    out = render(BUS, tmp_path, monkeypatch)
    assert "  kind: EventKind;" in out


def test_a_pointer_is_just_an_optional_number(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    assert "  version?: number;" in out
    assert "  cellIndex?: number;" in out


def test_go_widths_all_arrive_as_numbers_and_bool_as_boolean(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    assert "  ratio?: number;" in out
    assert "  truncated?: boolean;" in out


def test_a_nested_struct_is_emitted_and_referenced_by_name(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    assert "export interface Attribution {" in out
    assert "  attribution?: Attribution;" in out
    # Declared before it is used: TS hoists interfaces, but a reader should not
    # have to know that.
    assert out.index("export interface Attribution") < out.index("attribution?: Attribution")


def test_the_event_struct_is_renamed_because_event_is_a_dom_global(tmp_path, monkeypatch):
    out = render(BUS, tmp_path, monkeypatch)
    assert "export interface EmulatorEvent {" in out
    assert "export interface Event {" not in out, (
        "an interface named Event shadows the DOM type every handler is checked against")


def test_field_comments_are_carried_across(tmp_path, monkeypatch):
    """The explanation must sit beside the field, not only in the Go."""
    out = render(BUS, tmp_path, monkeypatch)
    assert "// Version is a pointer so version 0 is still reported." in out
    assert "  at: number; // emulator time" in out


# --- refusing to guess -----------------------------------------------------

def test_an_unmapped_go_type_stops_the_generator(tmp_path, monkeypatch):
    """The alternative is a silent `any`, which is how a contract rots."""
    bad = BUS.replace('Ratio     float64      `json:"ratio,omitempty"`',
                      'Ratio     complex128   `json:"ratio,omitempty"`')
    with pytest.raises(SystemExit) as e:
        render(bad, tmp_path, monkeypatch)
    assert "complex128" in str(e.value)


def test_a_field_without_a_json_tag_stops_the_generator(tmp_path, monkeypatch):
    bad = BUS.replace('Path string `json:"path,omitempty"`', "Path string")
    with pytest.raises(SystemExit) as e:
        render(bad, tmp_path, monkeypatch)
    assert "json tag" in str(e.value)


def test_a_missing_slice_stops_the_generator(tmp_path, monkeypatch):
    bad = BUS.replace("var ViewKinds = []string{\n\tKindFile,\n}", "")
    with pytest.raises(SystemExit) as e:
        render(bad, tmp_path, monkeypatch)
    assert "ViewKinds" in str(e.value)


def test_a_slice_naming_an_undeclared_kind_stops_the_generator(tmp_path, monkeypatch):
    bad = BUS.replace("\tKindFile, KindDropped,", "\tKindFile, KindDropped, KindGhost,")
    with pytest.raises(SystemExit) as e:
        render(bad, tmp_path, monkeypatch)
    assert "KindGhost" in str(e.value)


def test_a_bus_with_no_kinds_stops_the_generator(tmp_path, monkeypatch):
    """Reading the wrong file must be loud, not an empty contract."""
    with pytest.raises(SystemExit) as e:
        render("package store\n", tmp_path, monkeypatch)
    assert "reading the wrong thing" in str(e.value)


def test_a_missing_struct_stops_the_generator(tmp_path, monkeypatch):
    bad = BUS[:BUS.index("type Attribution struct {")]
    with pytest.raises(SystemExit) as e:
        render(bad, tmp_path, monkeypatch)
    assert "Attribution" in str(e.value)


# --- the --check gate ------------------------------------------------------

def test_check_mode_fails_when_the_committed_file_is_stale(tmp_path, monkeypatch, capsys):
    # ROOT too: the messages name the file relative to the repo, and a path
    # outside it has no relative form.
    monkeypatch.setattr(gen, "ROOT", tmp_path)
    monkeypatch.setattr(gen, "OUT", tmp_path / "eventKinds.ts")
    (tmp_path / "eventKinds.ts").write_text("// stale\n", encoding="utf-8")
    monkeypatch.setattr(sys, "argv", ["gen_event_kinds.py", "--check"])
    assert gen.main() == 1
    assert "out of date" in capsys.readouterr().err


def test_check_mode_passes_on_what_it_just_wrote(tmp_path, monkeypatch):
    out = tmp_path / "eventKinds.ts"
    monkeypatch.setattr(gen, "ROOT", tmp_path)
    monkeypatch.setattr(gen, "OUT", out)
    monkeypatch.setattr(sys, "argv", ["gen_event_kinds.py"])
    assert gen.main() == 0
    monkeypatch.setattr(sys, "argv", ["gen_event_kinds.py", "--check"])
    assert gen.main() == 0

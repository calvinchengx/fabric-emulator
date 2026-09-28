r"""The backward-compatibility gate, held to the standard it holds the tree to.

check_backward_compat answers the question its four HTTP siblings cannot: does
the surface an external user BOUND TO still exist. It has four extractors and a
three-way ratchet, and EVERY ONE OF THEM FAILS AS A HEALTHY TREE:

  * An extractor that resolves NOTHING -- `fs` renamed, the subcommand switch
    moved, a package relocated -- makes the baseline comparison match nothing
    against nothing for that surface. The gate then passes forever having
    stopped measuring, which reads exactly like a clean tree. THIS IS THE
    DANGEROUS ONE.
  * A removal that --update can launder is not a gate, it is a changelog
    agreeing with itself, so --update must PRESERVE `removed` and must not be
    reachable as the fix for a vanished id.
  * A `removed` entry that outlives the removal goes on excusing a surface
    nobody is holding.
  * And the near-misses cut the other way: a flag merely REORDERED, or a route
    whose parameter name changed but whose shape did not, must be silent. A
    gate that cries at a rename teaches people to run --update without
    reading the diff, which is the only review this ledger gets.

So every test below drives the checker over a SYNTHETIC tree under tmp_path.
Never over the real one: this repository's own surface rotates, and a test
asserting against it would just be a second copy of docs/compat-surface.json.
"""
import json
import pathlib
import re
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_backward_compat as bc  # noqa: E402
import check_route_coverage as cov  # noqa: E402
import check_surface_ledger as led  # noqa: E402

# main.go as this repository writes it: a subcommand switch before the flagset,
# then flags in all three of the shapes the real file uses (a config field, a
# local string, a bool).
MAIN_GO = '''
package main

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Println("fabric-emulator", cfg.Build())
			return nil
		case "healthcheck":
			return healthcheck(cfg.Addr)
		}
	}
	fs := flag.NewFlagSet("fabric-emulator", flag.ContinueOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	fs.StringVar(&cfg.KQLURL, "kql-url", cfg.KQLURL, "real Kusto engine")
	fs.BoolVar(&cfg.TSQLStrict, "tsql-strict", cfg.TSQLStrict, "refuse T-SQL")
	fs.IntVar(&cfg.ListPageSize, "list-page-size", cfg.ListPageSize, "page size")
	return nil
}
'''

# config.go: the os reads and the typed helpers, plus a FABRIC_* name in a
# COMMENT that must not be mistaken for a knob.
CONFIG_GO = '''
package config

// FABRIC_TARGET is named here only to explain the toggle; it is not read.
func FromEnvPartial() *Config {
	return &Config{
		Addr:        envOr("FABRIC_ADDR", ":9443"),
		DataDir:     envDefault("FABRIC_DATA_DIR", DefaultDataDir),
		KQLURL:      os.Getenv("FABRIC_KQL_URL"),
		TSQLStrict:  boolEnv("FABRIC_TSQL_STRICT"),
		PageSize:    intEnv("FABRIC_LIST_PAGE_SIZE"),
		Reservation: durationEnv("FABRIC_NAME_RESERVATION"),
	}
}
'''

# Routes, registered the two ways the tree does it: literals and a family
# assembled from a map literal's keys. `{wid}` is deliberately a DIFFERENT
# parameter name from the one the rename near-miss uses.
ROUTES_GO = '''
package api

var typedCollections = map[string]string{
	"notebooks":  "Notebook",
	"warehouses": "Warehouse",
}

func (a *API) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{wid}/items", a.listItems)
	mux.HandleFunc("POST /v1/workspaces/{wid}/items", a.createItem)
	for collection, itemType := range typedCollections {
		mux.HandleFunc("GET /v1/workspaces/{wid}/"+collection+"/{iid}", a.typedGet(itemType))
	}
}
'''

# A configuration table naming one of the synthetic knobs and not the others,
# so the docs cross-check has something to find and something to skip.
CONFIG_DOC = """# 04 — Configuration

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `FABRIC_ADDR` | `-addr` | `:9443` | Listen address. |
"""


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """A whole miniature repository, with every input the gate reads."""
    main = tmp_path / "cmd" / "fabric-emulator"
    main.mkdir(parents=True)
    (main / "main.go").write_text(MAIN_GO, encoding="utf-8")

    config = tmp_path / "internal" / "config"
    config.mkdir(parents=True)
    (config / "config.go").write_text(CONFIG_GO, encoding="utf-8")

    api = tmp_path / "internal" / "api"
    api.mkdir(parents=True)
    (api / "routes.go").write_text(ROUTES_GO, encoding="utf-8")

    (tmp_path / "04-configuration.md").write_text(CONFIG_DOC, encoding="utf-8")

    monkeypatch.setattr(bc, "MAIN_GO", main / "main.go")
    monkeypatch.setattr(bc, "ENV_SOURCES", (tmp_path / "internal", tmp_path / "cmd"))
    monkeypatch.setattr(bc, "CONFIG_DOC", tmp_path / "04-configuration.md")
    monkeypatch.setattr(bc, "BASELINE", tmp_path / "compat-surface.json")
    # The route surface comes from check_route_coverage, so its sources move too.
    monkeypatch.setattr(cov, "SOURCES", (tmp_path / "internal",))
    return tmp_path


def baseline(tree):
    return json.loads((tree / "compat-surface.json").read_text(encoding="utf-8"))


def rewrite(tree, mutate):
    data = baseline(tree)
    mutate(data)
    (tree / "compat-surface.json").write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")


# --- the extractors find things ------------------------------------------------

def test_every_extractor_resolves_the_synthetic_surface(tree):
    """Each half must FIND something, or nothing below it means anything."""
    assert bc.subcommands() == ["healthcheck", "version"]
    assert bc.cli_flags() == ["addr", "kql-url", "list-page-size", "tsql-strict"]
    assert bc.env_vars() == [
        "FABRIC_ADDR", "FABRIC_DATA_DIR", "FABRIC_KQL_URL",
        "FABRIC_LIST_PAGE_SIZE", "FABRIC_NAME_RESERVATION", "FABRIC_TSQL_STRICT",
    ]
    # BOTH SPELLINGS of each typed collection, because the emulator registers
    # both: ASP.NET matches path segments case-insensitively and Go's ServeMux
    # does not, so `.../Notebooks` is a URL a client can really have written
    # down and can really lose on its own.
    assert bc.routes() == [
        "GET /v1/workspaces/{}/Notebooks/{}",
        "GET /v1/workspaces/{}/Warehouses/{}",
        "GET /v1/workspaces/{}/items",
        "GET /v1/workspaces/{}/notebooks/{}",
        "GET /v1/workspaces/{}/warehouses/{}",
        "POST /v1/workspaces/{}/items",
    ]


def test_a_commented_variable_is_not_a_knob(tree):
    """FABRIC_TARGET appears in config.go's prose and is read nowhere."""
    assert "FABRIC_TARGET" not in bc.env_vars()
    assert bc.unrecognised_env_reads() == []


def test_a_read_form_the_parser_does_not_know_is_named_not_dropped(tree):
    """The env reader list is five function names; renaming one must fail."""
    (tree / "internal" / "config" / "config.go").write_text(
        'package config\nvar x = lookupEnvSomehow("FABRIC_MYSTERY")\n', encoding="utf-8")
    assert bc.unrecognised_env_reads() == [
        ((tree / "internal" / "config" / "config.go").as_posix(), "FABRIC_MYSTERY")]
    assert bc.main_for_test(strict=True) == 1


# --- the zero-extraction guard ------------------------------------------------

@pytest.mark.parametrize("surface,break_it", [
    # `fs` renamed: every flag disappears and the surface reads as empty.
    ("cliFlags", lambda t: (t / "cmd" / "fabric-emulator" / "main.go").write_text(
        MAIN_GO.replace("fs.", "flags."), encoding="utf-8")),
    # The subcommand switch moved or was rewritten.
    ("subcommands", lambda t: (t / "cmd" / "fabric-emulator" / "main.go").write_text(
        MAIN_GO.replace("switch args[0] {", "switch command(args) {"), encoding="utf-8")),
    # A package relocated out from under the env scan.
    ("envVars", lambda t: (t / "internal" / "config" / "config.go").unlink()),
    # …and the same for the route registrations.
    ("routes", lambda t: (t / "internal" / "api" / "routes.go").unlink()),
])
def test_a_zero_extraction_fails_rather_than_passing_vacuously(tree, surface, break_it,
                                                               capsys):
    assert bc.main_for_test(update=True) == 0
    break_it(tree)
    # NOT --strict. An extractor that has stopped measuring is a broken gate,
    # not drift, so it must fail even on the reporting run -- otherwise the
    # bare invocation would print a clean tree.
    assert bc.main_for_test() == 1
    assert surface in capsys.readouterr().err


# --- the ratchet, in all three directions -------------------------------------

def test_a_removed_flag_is_a_breaking_change(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(
        "\n".join(line for line in MAIN_GO.splitlines() if '"tsql-strict"' not in line), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "BREAKING CHANGE" in out
    assert "tsql-strict" in out


def test_a_removed_route_is_a_breaking_change(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    (tree / "internal" / "api" / "routes.go").write_text(
        ROUTES_GO.replace(
            '\tmux.HandleFunc("POST /v1/workspaces/{wid}/items", a.createItem)\n', ""), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "BREAKING CHANGE" in out
    assert "POST /v1/workspaces/{}/items" in out


def test_a_removed_env_var_is_a_breaking_change(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    (tree / "internal" / "config" / "config.go").write_text(
        CONFIG_GO.replace('durationEnv("FABRIC_NAME_RESERVATION")', "0"), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 1
    assert "FABRIC_NAME_RESERVATION" in capsys.readouterr().out


def test_a_removed_subcommand_is_a_breaking_change(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(
        MAIN_GO.replace('case "healthcheck":', 'case "probe":'), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "BREAKING CHANGE" in out and "healthcheck" in out


def test_a_declared_removal_passes(tree):
    """The only way past a removal: write down the release and the reason."""
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(
        "\n".join(line for line in MAIN_GO.splitlines() if '"tsql-strict"' not in line), encoding="utf-8")
    rewrite(tree, lambda d: d["removed"].append({
        "surface": "cliFlags", "id": "tsql-strict", "removedIn": "v0.99.0",
        "reason": "The strict T-SQL mode moved into the warehouse relay.",
    }))
    assert bc.main_for_test(strict=True) == 0


def test_a_removal_with_no_reason_is_refused(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(
        "\n".join(line for line in MAIN_GO.splitlines() if '"tsql-strict"' not in line), encoding="utf-8")
    rewrite(tree, lambda d: d["removed"].append({
        "surface": "cliFlags", "id": "tsql-strict", "removedIn": "v0.99.0",
        "reason": "   ",
    }))
    assert bc.main_for_test(strict=True) == 1
    assert "reason is empty" in capsys.readouterr().out


def test_a_removal_naming_an_unknown_surface_is_refused(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    rewrite(tree, lambda d: d["removed"].append({
        "surface": "composeServices", "id": "ttyd", "removedIn": "v0.99.0",
        "reason": "Out of scope -- and that is exactly why it may not be filed here.",
    }))
    assert bc.main_for_test(strict=True) == 1
    assert "is not one of" in capsys.readouterr().out


def test_a_resurrected_removal_fails(tree, capsys):
    """A stale exemption goes on excusing a surface nobody is holding."""
    assert bc.main_for_test(update=True) == 0
    rewrite(tree, lambda d: d["removed"].append({
        "surface": "cliFlags", "id": "tsql-strict", "removedIn": "v0.99.0",
        "reason": "Recorded as gone while the flag is right there in main.go.",
    }))
    assert bc.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "recorded as" in out and "back in the tree" in out and "tsql-strict" in out


def test_an_undeclared_addition_is_a_stale_ledger(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(MAIN_GO.replace(
        '\tfs.StringVar(&cfg.Addr, "addr"',
        '\tfs.StringVar(&cfg.Brand, "brand-new", cfg.Brand, "new")\n'
        '\tfs.StringVar(&cfg.Addr, "addr"'), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "STALE LEDGER" in out and "brand-new" in out


def test_strict_is_what_decides_the_exit_code(tree):
    """Bare runs REPORT drift. Only --strict fails on it -- the house CLI."""
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(
        "\n".join(line for line in MAIN_GO.splitlines() if '"tsql-strict"' not in line), encoding="utf-8")
    assert bc.main_for_test() == 0
    assert bc.main_for_test(strict=True) == 1


def test_no_baseline_at_all_fails_rather_than_creating_one(tree, capsys):
    assert bc.main_for_test(strict=True) == 1
    assert "Create one with --update" in capsys.readouterr().err


# --- the near-misses this must NOT report -------------------------------------

def test_reordering_the_flags_is_not_drift(tree):
    """A flag's position in main.go is not a surface. Only its name is."""
    assert bc.main_for_test(update=True) == 0
    lines = MAIN_GO.splitlines()
    flags = [i for i, line in enumerate(lines) if line.startswith("\tfs.")]
    lines[flags[0]], lines[flags[-1]] = lines[flags[-1]], lines[flags[0]]
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text("\n".join(lines), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 0


def test_renaming_a_route_parameter_is_not_drift(tree):
    """`{wid}` to `{workspaceId}` changes no URL any client ever writes."""
    assert bc.main_for_test(update=True) == 0
    (tree / "internal" / "api" / "routes.go").write_text(
        ROUTES_GO.replace("{wid}", "{workspaceId}").replace("{iid}", "{itemId}"), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 0


def test_renaming_a_flag_is_both_halves_at_once(tree, capsys):
    """The other side of the same coin: a rename IS a break, and is reported
    as both a removal and an addition, because for a bound client it is both."""
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(
        MAIN_GO.replace('"tsql-strict"', '"tsql-pedantic"'), encoding="utf-8")
    assert bc.main_for_test(strict=True) == 1
    out = capsys.readouterr().out
    assert "BREAKING CHANGE" in out and "tsql-strict" in out
    assert "STALE LEDGER" in out and "tsql-pedantic" in out


def test_the_shape_rule_agrees_with_the_surface_ledger(tree):
    """Two gates, one reading of a route template.

    check_surface_ledger erases parameter names so `{wid}` and `{workspaceId}`
    are the same operation; this gate erases them so a rename is not a false
    break. A rule that drifted between them would not be a finding, it would
    be an inconsistency wearing one -- the same assertion
    test_check_undocumented_routes makes about its own copy.
    """
    for template in ("/v1/workspaces/{wid}/items/{iid}",
                     "/v1/livyapi/versions/{v}/sessions/{livypath...}",
                     "/v1/workspaces/{wid}/items/"):
        assert bc._shape(template) == led._shape(template)


# --- --update ------------------------------------------------------------------

def test_update_round_trips_to_a_clean_run(tree):
    assert bc.main_for_test(update=True) == 0
    assert bc.main_for_test(strict=True) == 0
    first = (tree / "compat-surface.json").read_text(encoding="utf-8")
    assert bc.main_for_test(update=True) == 0
    assert (tree / "compat-surface.json").read_text(encoding="utf-8") == first, \
        "regenerating must be a no-op diff, or every change carries noise"
    assert first.endswith("\n")


def test_update_cannot_launder_a_removal(tree, capsys):
    """The claim the docstring makes, asserted rather than asserted-about.

    Without this refusal the fix for "you deleted a flag" is to regenerate: the
    id leaves the list, --strict goes green, and the gate is a changelog
    agreeing with itself. FOUND BY DRIVING IT -- a real flag and a real route
    were deleted from this repository's tree, --strict named both, and --update
    then regenerated both away without comment.
    """
    assert bc.main_for_test(update=True) == 0
    (tree / "cmd" / "fabric-emulator" / "main.go").write_text(
        "\n".join(line for line in MAIN_GO.splitlines() if '"tsql-strict"' not in line), encoding="utf-8")
    assert bc.main_for_test(update=True) == 1
    assert "cannot launder a removal" in capsys.readouterr().err
    assert "tsql-strict" in baseline(tree)["cliFlags"], "the baseline must be untouched"
    # …and once the removal is a written-down decision, --update proceeds.
    rewrite(tree, lambda d: d["removed"].append({
        "surface": "cliFlags", "id": "tsql-strict", "removedIn": "v0.99.0",
        "reason": "Folded into the warehouse relay.",
    }))
    assert bc.main_for_test(update=True) == 0
    assert "tsql-strict" not in baseline(tree)["cliFlags"]
    assert bc.main_for_test(strict=True) == 0


def test_update_cannot_bless_a_resurrection(tree, capsys):
    """Regenerating must not look like the fix for a stale exemption either."""
    assert bc.main_for_test(update=True) == 0
    rewrite(tree, lambda d: d["removed"].append({
        "surface": "cliFlags", "id": "addr", "removedIn": "v0.99.0",
        "reason": "Recorded as gone while -addr is right there in main.go.",
    }))
    assert bc.main_for_test(update=True) == 1
    assert "recorded as removed and back" in capsys.readouterr().err


def test_update_preserves_the_hand_written_sections(tree):
    """--update must not be able to launder a removal by erasing its record."""
    assert bc.main_for_test(update=True) == 0
    rewrite(tree, lambda d: (
        d["removed"].append({
            "surface": "cliFlags", "id": "old-flag", "removedIn": "v0.9.0",
            "reason": "Retired with the subsystem it configured.",
        }),
        d["docsUndocumented"].update({"FABRIC_DATA_DIR": "recorded on purpose"}),
    ))
    assert bc.main_for_test(update=True) == 0
    data = baseline(tree)
    assert [e["id"] for e in data["removed"]] == ["old-flag"]
    assert "FABRIC_DATA_DIR" in data["docsUndocumented"]


def test_the_counts_block_matches_the_lists(tree):
    """The counts are what a reviewer skims; they must not be able to lie."""
    assert bc.main_for_test(update=True) == 0
    data = baseline(tree)
    for surface in bc.SURFACES:
        assert data["counts"][surface] == len(data[surface])


# --- the docs cross-check ------------------------------------------------------

def test_the_docs_cross_check_names_an_undocumented_knob(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    assert bc.main_for_test(strict=True) == 0  # REPORT ONLY: it must not fail
    out = capsys.readouterr().out
    assert "REPORT ONLY" in out
    assert "-tsql-strict" in out and "FABRIC_KQL_URL" in out
    assert "-addr" not in out and "`FABRIC_ADDR`" not in out


def test_an_accepted_omission_is_silent(tree, capsys):
    assert bc.main_for_test(update=True) == 0
    rewrite(tree, lambda d: d["docsUndocumented"].update({
        name: "not a user-facing knob" for name in
        ("-kql-url", "-list-page-size", "-tsql-strict", "FABRIC_DATA_DIR",
         "FABRIC_KQL_URL", "FABRIC_LIST_PAGE_SIZE", "FABRIC_NAME_RESERVATION",
         "FABRIC_TSQL_STRICT")}))
    assert bc.main_for_test(strict=True) == 0
    assert "REPORT ONLY" not in capsys.readouterr().out


def test_the_real_repository_documents_or_records_every_knob(tree):
    """The one assertion made against THIS tree, and it is the promise the
    docstring makes: the cross-check ships EMPTY rather than muted, so it can
    be promoted into --strict without first fixing a backlog.

    The `tree` fixture is taken deliberately: it redirects only the paths this
    assertion does not use, so the real MAIN_GO/ENV_SOURCES/CONFIG_DOC below
    are named explicitly and the test cannot accidentally read the synthetic
    ones.
    """
    real = json.loads((bc.ROOT / "docs" / "compat-surface.json").read_text(encoding="utf-8"))
    surface = {
        "cliFlags": sorted(set(re.findall(
            bc._FLAG.pattern,
            (bc.ROOT / "cmd" / "fabric-emulator" / "main.go").read_text(encoding="utf-8")))),
        "envVars": real["envVars"],
    }
    assert surface["cliFlags"], "the real main.go must still parse"
    doc = bc.ROOT / "docs" / "04-configuration.md"
    table = "\n".join(line for line in doc.read_text(encoding="utf-8").splitlines()
                      if line.startswith("|"))
    accepted = real["docsUndocumented"]
    missing = [f"-{f}" for f in surface["cliFlags"]
               if f"`-{f}`" not in table and f"-{f}" not in accepted]
    missing += [e for e in surface["envVars"]
                if f"`{e}`" not in table and e not in accepted]
    assert not missing, (
        "docs/04-configuration.md does not mention these and the baseline does "
        f"not record why: {missing}")


def test_every_recorded_omission_carries_a_reason():
    """An accepted omission with an empty reason is the omission it excuses."""
    real = json.loads((bc.ROOT / "docs" / "compat-surface.json").read_text(encoding="utf-8"))
    for name, why in real["docsUndocumented"].items():
        assert why.strip(), f"{name} is recorded as undocumented with no reason"
    assert not bc.removal_problems(real)

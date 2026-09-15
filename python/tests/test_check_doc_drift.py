r"""A drift guard that can itself drift silently is worth nothing.

check_doc_drift exists because prose is the one artifact here with nothing
underneath it: a renamed directory breaks a Go import and CI goes red, while the
same rename in a sentence breaks nothing at all. That argument applies with full
force to the checker. It is ~200 lines of regex against hand-rolled Markdown,
and every one of its precision concessions is a place it could quietly stop
finding anything:

  * the MAKE extractor is anchored to command-shaped spans, because the naive
    `make \w+` form returns 13 hits on this tree and all 13 are English prose.
    Anchoring is what makes the class usable -- and an anchor tightened one
    notch too far catches nothing and still passes;
  * a dot that is not a known extension means "not a path", so the Go symbol
    `internal/tsql.DataFlows` is left alone. Widen that and real files stop
    being checked;
  * `docs/24` is this repo's shorthand for a NUMBERED DOCUMENT, not a path;
  * release notes are historical and skipped, as are EXEMPT forward references.

So the tests that matter most are the ones asserting each class FAILS on a dead
reference. A checker that always passes is indistinguishable from a checker that
is working, which is the exact failure docs/10 catalogues at length -- and the
reason the two currently-clean classes (make, env) are tested against synthetic
drift rather than trusted because the tree is green.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_doc_drift as c  # noqa: E402

MAKEFILE = """\
.PHONY: help check lint
help: ## Show the targets
check: lint ## Repo invariants
\t@echo checking
lint:
\t@echo linting
VAR := not-a-target
"""


@pytest.fixture
def tree(tmp_path, monkeypatch):
    """Point the checker at a repo-shaped tree we control.

    `tracked_files` is stubbed rather than a real git init: the checker falls
    back to a filesystem walk outside a checkout, and pinning the list keeps
    these tests from depending on which fallback fires.
    """
    def build(docs, files=(), makefile=MAKEFILE, exempt=None):
        for rel in files:
            path = tmp_path / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("placeholder\n")
        for rel, body in docs.items():
            path = tmp_path / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body)
        (tmp_path / "Makefile").write_text(makefile)
        monkeypatch.setattr(c, "ROOT", tmp_path)
        monkeypatch.setattr(c, "MAKEFILE", tmp_path / "Makefile")
        monkeypatch.setattr(c, "EXEMPT", exempt or {})
        tracked = list(files) + list(docs)
        monkeypatch.setattr(c, "tracked_files", lambda: tracked)
        return tmp_path
    return build


def kinds(found):
    return sorted(f[0] for f in found)


# --- class 1: dead repo paths -------------------------------------------------

def test_a_dead_path_fails(tree):
    # THE PROPERTY A CHECK THAT ALWAYS PASSES GETS WRONG.
    tree({"docs/a.md": "see `internal/store/gone.go` for it\n"},
         files=["internal/store/real.go"])
    found = c.findings()
    assert kinds(found) == ["path"]
    assert found[0][3] == "internal/store/gone.go"


def test_a_live_path_passes(tree):
    tree({"docs/a.md": "see `internal/store/real.go` for it\n"},
         files=["internal/store/real.go"])
    assert c.findings() == []


def test_a_go_symbol_is_not_treated_as_a_path(tree):
    # `internal/tsql.DataFlows` is prose ABOUT code, not a pointer to a file.
    # Flagging it would be the false positive that gets the check muted.
    tree({"docs/a.md": "recorded by `internal/tsql.DataFlows` on the way past\n"},
         files=["internal/tsql/flows.go"])
    assert c.findings() == []


def test_the_repos_numbered_document_shorthand_is_not_a_path(tree):
    # "`docs/24`'s L is unchanged" -- used constantly in this tree, and nine of
    # these were the first run's entire false-positive set.
    tree({"docs/a.md": "as `docs/24` records, and `docs/10` twice\n"},
         files=["docs/24-thing.md"])
    assert c.findings() == []


def test_a_directory_counts_as_existing(tree):
    tree({"docs/a.md": "the suite lives in `e2e/delta-rs`\n"},
         files=["e2e/delta-rs/run.py"])
    assert c.findings() == []


def test_a_path_under_an_untracked_top_level_dir_is_ignored(tree):
    # `_site/` and `node_modules/` are build output; a reference into one is
    # not this checker's business and cannot be "fixed".
    tree({"docs/a.md": "published to `_site/index.html`\n"},
         files=["docs/a.md"])
    assert c.findings() == []


def test_a_trailing_line_citation_is_stripped(tree):
    tree({"docs/a.md": "see `internal/store/real.go:42` there\n"},
         files=["internal/store/real.go"])
    assert c.findings() == []


def test_a_path_inside_a_fenced_block_is_not_read(tree):
    # A fenced block is a transcript, and may legitimately describe another
    # machine's filesystem.
    tree({"docs/a.md": "```\ncat /opt/other/thing.go\n`internal/gone.go`\n```\n"},
         files=["docs/a.md"])
    assert c.findings() == []


def test_a_wrong_case_path_fails_on_every_platform(tree):
    # Existence is decided against git, not the filesystem, because a default
    # macOS APFS volume answers `.exists()` case-INSENSITIVELY while the Linux
    # CI runner does not. Under the filesystem answer this test passes on a
    # laptop and fails in CI -- the checker disagreeing with itself by platform.
    tree({"docs/a.md": "see `internal/store/Real.go` for it\n"},
         files=["internal/store/real.go"])
    found = c.findings()
    assert kinds(found) == ["path"]
    assert found[0][3] == "internal/store/Real.go"


def test_an_intermediate_directory_counts_as_existing(tree):
    # The ancestor set is what keeps the git-backed lookup from regressing
    # `test_a_directory_counts_as_existing`: git tracks FILES, so a cited
    # directory is only known through the ancestors derived from them.
    tree({"docs/a.md": "the suite lives in `e2e/delta-rs`\n"},
         files=["e2e/delta-rs/harness/run.py"])
    assert c.findings() == []


# --- class 2: dead make targets -----------------------------------------------

def test_a_dead_make_target_fails(tree):
    tree({"docs/a.md": "run `make nonexistent` first\n"}, files=["docs/a.md"])
    found = c.findings()
    assert kinds(found) == ["make"]
    assert found[0][3] == "make nonexistent"


def test_a_live_make_target_passes(tree):
    tree({"docs/a.md": "run `make check` first\n"}, files=["docs/a.md"])
    assert c.findings() == []


def test_prose_that_merely_contains_the_word_make_is_ignored(tree):
    # The measurement that justifies anchoring: the naive bare-word regex
    # returns 13 hits on the real tree and every one is English like these.
    tree({"docs/a.md":
          "This is what would make the check useful, and make it worth\n"
          "having; make every claim carry a witness, so make a habit of it.\n"},
         files=["docs/a.md"])
    assert c.findings() == []


def test_a_fenced_make_command_is_caught(tree):
    # The other half of anchoring: a command in a fenced block IS a command.
    tree({"docs/a.md": "```bash\nmake nonexistent\n```\n"}, files=["docs/a.md"])
    assert kinds(c.findings()) == ["make"]


def test_a_dollar_prompt_before_make_is_handled(tree):
    tree({"docs/a.md": "```\n$ make nonexistent\n```\n"}, files=["docs/a.md"])
    assert kinds(c.findings()) == ["make"]


def test_a_variable_override_is_not_a_target(tree):
    # `make up PROFILE=` -- the first word after `make` is the target here, but
    # a bare `make PROFILE=x` names no target at all.
    tree({"docs/a.md": "run `make PROFILE=lean` to override\n"}, files=["docs/a.md"])
    assert c.findings() == []


def test_a_flag_is_not_a_target(tree):
    # The sibling of the override case above, and the one that got this wrong:
    # `-` inside the name character class also matched it at the FRONT, so
    # `make -j4` was reported as "invokes a target the Makefile does not
    # define". No doc in the tree writes a make flag today, which is exactly why
    # a green tree proved nothing -- this checker gates `make check` and CI, so
    # the first doc to write `make -j4 test` would have hard-failed the build on
    # a legitimate edit. Both documented forms, because `-C` takes an argument
    # and so reaches the guard with a different shape from `-j4`.
    tree({"docs/a.md": "run `make -j4 test` or `make -C portal build`\n"},
         files=["docs/a.md"])
    assert c.findings() == []


def test_a_hyphen_inside_a_target_name_is_still_a_target(tree):
    # The other direction, which is what makes the fix above a fix rather than a
    # mute: hyphens are legal INSIDE a target name, so narrowing the class must
    # not cost `up-jvm`/`docs-build`. A checker that stopped reading hyphenated
    # targets would pass this tree by seeing nothing at all.
    tree({"docs/a.md": "run `make docs-build` then `make no-such-target`\n"},
         files=["docs/a.md"],
         makefile=MAKEFILE + "docs-build: ## Build the site\n\t@echo building\n")
    assert "docs-build" in c.make_targets()
    # Exactly one finding: the hyphenated target that IS defined is not
    # reported, and the hyphenated one that is not still is.
    found = c.findings()
    assert kinds(found) == ["make"]
    assert found[0][3] == "make no-such-target"


def test_phony_is_not_counted_as_a_target(tree):
    # `.PHONY` is a directive whose VALUE is the target list; counting it would
    # make `make .PHONY` look defined and, worse, admit its whole value list.
    tree({"docs/a.md": "x\n"}, files=["docs/a.md"])
    assert "PHONY" not in {t.upper() for t in c.make_targets()}


def test_targets_are_parsed_but_variable_assignment_is_not(tree):
    tree({"docs/a.md": "x\n"}, files=["docs/a.md"])
    assert c.make_targets() == {"help", "check", "lint"}


def test_a_rule_naming_several_targets_defines_all_of_them(tree):
    # `foo bar:` is one rule and two targets. Capturing only the first would
    # report `make bar` as undefined -- a false "target not defined" on a target
    # the Makefile plainly declares.
    tree({"docs/a.md": "run `make alpha` then `make beta`\n"},
         files=["docs/a.md"],
         makefile=MAKEFILE + "alpha beta:\n\t@echo both\n")
    assert {"alpha", "beta"} <= c.make_targets()
    assert c.findings() == []


def test_a_tilde_fence_is_not_closed_by_a_backtick_fence(tree):
    # A ``` quoted INSIDE a ~~~ block must not close it. Toggling on either
    # marker inverts the state for the rest of the file, so the prose after the
    # block would be read as commands -- here that would surface as `make it`
    # being reported as an undefined target.
    # The nesting must be at the START of a line to reach the toggle at all --
    # a first version of this test quoted ``` mid-line, where the anchored
    # `_FENCE` never matches, and so passed with the bug still in place.
    tree({"docs/a.md":
          "~~~\n```bash\n~~~\n"
          "make the documentation say what the code does\n"},
         files=["docs/a.md"])
    # Under the toggle-on-either bug the ~~~ block never closes in the right
    # place, the state inverts, and the PROSE line below it is read as a shell
    # command -- reporting a target named `the`.
    assert c.findings() == []


# --- class 3: env vars nothing reads ------------------------------------------

def test_an_env_var_no_code_reads_fails(tree):
    tree({"docs/a.md": "set `FABRIC_NOTHING_READS_THIS` to enable it\n"},
         files=["internal/server/main.go"])
    found = c.findings()
    assert kinds(found) == ["env"]
    assert found[0][3] == "FABRIC_NOTHING_READS_THIS"


def test_an_env_var_code_reads_passes(tree, tmp_path):
    tree({"docs/a.md": "set `FABRIC_FORCE_LRO` to enable it\n"},
         files=["internal/server/main.go"])
    (tmp_path / "internal/server/main.go").write_text('os.Getenv("FABRIC_FORCE_LRO")\n')
    assert c.findings() == []


def test_code_under_docs_counts_as_code(tree, tmp_path):
    # docs/demo/flow.py and flow-override.yml are real code living beside
    # prose. Excluding `docs/` by DIRECTORY instead of by extension wrongly
    # reports DEMO_FABRIC_PORT as read by nothing.
    tree({"docs/a.md": "the recording sets `DEMO_FABRIC_PORT`\n"},
         files=["docs/demo/flow.py"])
    (tmp_path / "docs/demo/flow.py").write_text('env["DEMO_FABRIC_PORT"] = "9843"\n')
    assert c.findings() == []


def test_a_variable_outside_the_projects_prefixes_is_ignored(tree):
    tree({"docs/a.md": "your `PATH` and `HOME` are yours\n"}, files=["docs/a.md"])
    assert c.findings() == []


# --- scope, exemptions, reporting ---------------------------------------------

def test_release_notes_are_skipped(tree):
    # A v0.16 note naming a since-renamed file is CORRECT about the tree at
    # that tag; editing it would falsify a historical record.
    tree({"docs/release-notes/v0.16.0.md": "shipped `internal/store/gone.go`\n"},
         files=["docs/a.md"])
    assert c.findings() == []


def test_an_exempt_entry_is_skipped(tree):
    tree({"docs/a.md": "planned: `scripts/check_contracts.py`\n"},
         files=["docs/a.md"],
         exempt={("docs/a.md", "scripts/check_contracts.py"): "planned, not built"})
    assert c.findings() == []


def test_an_exemption_is_scoped_to_its_own_document(tree):
    # Keyed by (file, token), so exempting a forward reference in one doc does
    # not silence the same dead path somewhere it is a genuine error.
    tree({"docs/a.md": "planned: `scripts/check_contracts.py`\n",
          "docs/b.md": "run `scripts/check_contracts.py`\n"},
         files=["scripts/check_real.py"],
         exempt={("docs/a.md", "scripts/check_contracts.py"): "planned, not built"})
    found = c.findings()
    assert [f[1] for f in found] == ["docs/b.md"]


def test_strict_exits_non_zero_and_names_the_file_and_token(tree, capsys, monkeypatch):
    tree({"docs/a.md": "see `internal/store/gone.go`\n"},
         files=["internal/store/real.go"])
    monkeypatch.setattr(sys, "argv", ["check_doc_drift.py", "--strict"])
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "docs/a.md:1" in out
    assert "internal/store/gone.go" in out


def test_without_strict_it_reports_but_exits_zero(tree, capsys, monkeypatch):
    # The sibling convention: report everywhere, fail only where asked.
    tree({"docs/a.md": "see `internal/store/gone.go`\n"},
         files=["internal/store/real.go"])
    monkeypatch.setattr(sys, "argv", ["check_doc_drift.py"])
    assert c.main() == 0
    assert "internal/store/gone.go" in capsys.readouterr().out


def test_a_clean_tree_reports_the_counts(tree, capsys, monkeypatch):
    tree({"docs/a.md": "run `make check`\n"}, files=["docs/a.md"])
    monkeypatch.setattr(sys, "argv", ["check_doc_drift.py", "--strict"])
    assert c.main() == 0
    assert "no drift" in capsys.readouterr().out


def test_all_three_classes_can_fire_at_once(tree):
    # Each class is independent; a doc may be wrong in more than one way, and
    # reporting only the first would hide the rest behind one fix.
    tree({"docs/a.md": "`internal/gone.go`, `make nope`, `FABRIC_UNREAD_NAME`\n"},
         files=["internal/real.go"])
    assert kinds(c.findings()) == ["env", "make", "path"]


# --- the git fallback ---------------------------------------------------------

def test_tracked_files_reads_git(tmp_path, monkeypatch):
    monkeypatch.setattr(c, "ROOT", tmp_path)
    monkeypatch.setattr(c.subprocess, "run",
                        lambda *a, **k: _Completed("docs/a.md\0internal/b.go\0"))
    assert c.tracked_files() == ["docs/a.md", "internal/b.go"]


def test_tracked_files_falls_back_to_a_walk_outside_a_checkout(tmp_path, monkeypatch):
    # A tarball or a vendored copy has no .git, and `git ls-files` fails. The
    # fallback is what keeps the check from silently seeing an EMPTY repo --
    # which would make every top-level dir untracked, every path candidate
    # ineligible, and the whole class pass while verifying nothing.
    (tmp_path / "internal").mkdir()
    (tmp_path / "internal" / "b.go").write_text("x")
    monkeypatch.setattr(c, "ROOT", tmp_path)

    def explode(*args, **kwargs):
        raise FileNotFoundError("no git on PATH")

    monkeypatch.setattr(c.subprocess, "run", explode)
    assert c.tracked_files() == ["internal/b.go"]


def test_an_empty_git_answer_also_falls_back(tmp_path, monkeypatch):
    # `git ls-files` succeeding with NO output means we are not in a checkout
    # of this repo, not that the repo is empty.
    (tmp_path / "internal").mkdir()
    (tmp_path / "internal" / "b.go").write_text("x")
    monkeypatch.setattr(c, "ROOT", tmp_path)
    monkeypatch.setattr(c.subprocess, "run", lambda *a, **k: _Completed(""))
    assert c.tracked_files() == ["internal/b.go"]


class _Completed:
    def __init__(self, stdout):
        self.stdout = stdout


# --- the real tree ------------------------------------------------------------

def test_every_exemption_carries_a_reason():
    # An exemption without a stated reason is just a silencer.
    for key, reason in c.EXEMPT.items():
        assert isinstance(reason, str) and len(reason) > 20, key


def test_every_exemption_is_still_live():
    # An exemption for a reference that no longer exists -- or that now
    # resolves -- is stale bookkeeping, and this list is only auditable while
    # every entry is doing work.
    for (rel, token), _reason in c.EXEMPT.items():
        path = c.ROOT / rel
        assert path.exists(), f"{rel} is exempted but no longer exists"
        assert f"`{token}`" in path.read_text(encoding="utf-8"), \
            f"{rel} no longer mentions {token}; drop the exemption"
        assert not (c.ROOT / token).exists(), \
            f"{token} exists now; drop the exemption rather than keeping it"


def test_the_real_repo_passes_its_own_check(capsys):
    # What makes a future rename fail here rather than in review.
    assert c.findings() == [], capsys.readouterr().out

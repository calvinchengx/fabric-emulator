"""Tests for the Homebrew cask deprecated-stanza checker.

WHY THE CHECKER CANNOT BE FIXED DOWNSTREAM, which is what makes it a gate rather
than a lint: the cask is GENERATED. GoReleaser's template hardcodes
`postflight do` around whatever `homebrew_casks[].hooks` contains, so a
deprecation corrected by hand in the tap is undone by the next release. The only
place a regression can be introduced is the source, and the only place it can be
caught before a tag is this script.

It runs in two modes that fail at different times and catch different things, and
BOTH are driven here. The default mode reads `.goreleaser.yaml` on every push --
where a regression is introduced. `--rendered DIR` reads what GoReleaser actually
wrote at release time -- the check that survives GoReleaser changing its template
underneath us. A test covering only the first would leave the release-time half
unexercised, which is the half that runs least often and is therefore likeliest
to have quietly stopped working.

Every case drives synthetic YAML and synthetic casks under tmp_path. Pointing at
this repository's real `.goreleaser.yaml` would assert that today's config is
clean -- which `make check` already does on every run -- and could not tell a
working checker from one whose regex stopped matching.

THE `_steps` NEAR-MISSES ARE THE WHOLE POINT. The fix for a deprecated
`postflight do` is `postflight_steps do`, so a pattern that matched the prefix
would flag the very form it is asking for. That makes the checker impossible to
satisfy, which is worse than not having it: an unsatisfiable gate gets deleted or
commented out, and then nothing is watching.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_cask_stanzas as c  # noqa: E402

# Every stanza Homebrew deprecated, and the `_steps` form that replaces each.
DEPRECATED = ("preflight", "postflight", "uninstall_preflight", "uninstall_postflight")


def goreleaser(tmp_path, body):
    """A tmp_path standing in for the repo root, holding one .goreleaser.yaml."""
    (tmp_path / ".goreleaser.yaml").write_text(body, encoding="utf-8")
    return tmp_path


# --- the pattern, in both directions ------------------------------------------


@pytest.mark.parametrize("stanza", DEPRECATED)
def test_every_deprecated_stanza_is_matched(stanza):
    """All four, not a representative one. The list is transcribed from
    Homebrew's deprecation and a pattern built by joining it can lose a member to
    an alternation ordering mistake -- `preflight|uninstall_preflight` matches the
    first alternative inside the second, so which one `m.group(1)` reports depends
    on the order, and a wrong name in the message sends the reader to the wrong
    stanza."""
    assert c.DEPRECATED_RE.search(f"  {stanza} do\n    system 'x'\n  end\n")


@pytest.mark.parametrize("stanza", DEPRECATED)
def test_the_steps_form_it_asks_for_is_not_matched(stanza):
    """The near-miss that decides whether this checker is usable at all: the
    prescribed fix must not itself be a finding. A prefix match would flag
    `postflight_steps do` -- the thing the error message tells you to write --
    and an unsatisfiable gate gets switched off."""
    assert not c.DEPRECATED_RE.search(f"  {stanza}_steps do\n    system 'x'\n  end\n")


def test_the_longest_name_is_reported_not_its_substring():
    """`uninstall_postflight` contains `postflight`. The reported name has to be
    the stanza that is actually there, because the message tells the reader which
    `_steps` form to write."""
    m = c.DEPRECATED_RE.search("  uninstall_postflight do\n  end\n")
    assert m and m.group(1) == "uninstall_postflight"


def test_a_bare_name_without_do_is_not_a_block():
    """`postflight` in a comment or inside a string is not an opened block.
    Flagging prose is how a checker earns a reputation for crying wolf -- the
    lesson check_doc_drift.py's 84 false positives already paid for here."""
    assert not c.DEPRECATED_RE.search("  # postflight was removed in 2024\n")
    assert not c.DEPRECATED_RE.search('  name "postflight"\n')


def test_the_word_must_stand_alone():
    """`\\b` after `do`, and a whole-word start: `mypostflight do` is somebody
    else's identifier and `postflight download` is not a block opener."""
    assert not c.DEPRECATED_RE.search("  mypostflight do\n")
    assert not c.DEPRECATED_RE.search("  postflight download\n")


# --- the SOURCE mode: .goreleaser.yaml ----------------------------------------


def test_hooks_are_refused_whatever_they_contain(tmp_path, monkeypatch):
    """`hooks` is refused on sight, and that is deliberate rather than lazy: the
    template wraps ANY hook in a deprecated block, so there is no hook content
    that renders acceptably. Checking what is inside it would be checking the
    wrong thing."""
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, '''
homebrew_casks:
  - name: fabric-emulator
    hooks:
      post:
        install: |
          system_command "/bin/true"
'''))
    problems = c.check_source()
    assert len(problems) == 1
    assert "hooks" in problems[0] and "custom_block" in problems[0]
    assert "fabric-emulator" in problems[0], "the cask must be named in the finding"


def test_a_deprecated_block_inside_custom_block_is_refused(tmp_path, monkeypatch):
    """`custom_block` is the sanctioned escape from `hooks` because the template
    emits it verbatim -- which is exactly why it is the one place a deprecated
    stanza can be written by hand and reach the tap."""
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, '''
homebrew_casks:
  - name: fabric-emulator
    custom_block: |
      postflight do
        system_command "/bin/true"
      end
'''))
    problems = c.check_source()
    assert len(problems) == 1
    assert "postflight_steps" in problems[0], \
        "the finding must name the replacement, or the reader has to go look it up"


def test_the_steps_form_in_custom_block_passes(tmp_path, monkeypatch):
    """The corrected config, which must be silent. This is the assertion that
    says the checker can be SATISFIED -- a gate nothing can pass is not a gate."""
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, '''
homebrew_casks:
  - name: fabric-emulator
    custom_block: |
      postflight_steps do
        system_command "/bin/true"
      end
'''))
    assert c.check_source() == []


def test_a_cask_with_no_hooks_and_no_custom_block_passes(tmp_path, monkeypatch):
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, '''
homebrew_casks:
  - name: fabric-emulator
    binary: fabric-emulator
'''))
    assert c.check_source() == []


def test_a_config_with_no_casks_at_all_passes(tmp_path, monkeypatch):
    """`homebrew_casks` absent, and `null` -- both real YAML states. The `or []`
    in the script exists for the second, where `cfg.get` returns None rather than
    a missing key and iterating it would be a TypeError."""
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, "builds:\n  - id: x\n"))
    assert c.check_source() == []
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, "homebrew_casks:\n"))
    assert c.check_source() == []


def test_an_unnamed_cask_still_produces_a_readable_finding(tmp_path, monkeypatch):
    """`name` is optional in the config. A finding that interpolated None would
    read as a crash rather than as a problem with the cask."""
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, '''
homebrew_casks:
  - hooks:
      post:
        install: "true"
'''))
    problems = c.check_source()
    assert len(problems) == 1 and "<unnamed>" in problems[0]


def test_the_yml_spelling_is_found_too(tmp_path, monkeypatch):
    """GoReleaser accepts both spellings. A checker that only looked for
    `.goreleaser.yaml` in a repo using `.yml` would exit "no deprecated cask
    stanzas" having read nothing -- the vacuous pass this whole directory is
    about."""
    (tmp_path / ".goreleaser.yml").write_text(
        'homebrew_casks:\n  - name: x\n    custom_block: |\n      preflight do\n      end\n',
        encoding="utf-8")
    monkeypatch.setattr(c, "ROOT", tmp_path)
    assert len(c.check_source()) == 1


def test_a_missing_config_exits_rather_than_passing(tmp_path, monkeypatch):
    """No config at all must be an error, not a clean run. "I found nothing to
    check" and "I checked and found nothing" are the two readings this repository
    keeps having to tell apart."""
    monkeypatch.setattr(c, "ROOT", tmp_path)
    with pytest.raises(SystemExit):
        c.check_source()


# --- the RENDERED mode: dist/homebrew ----------------------------------------


def test_a_rendered_cask_with_a_deprecated_stanza_is_refused(tmp_path):
    """The release-time half, over the cask GoReleaser actually wrote. This is
    the check that survives the template changing underneath us, so the source
    being clean says nothing about it."""
    d = tmp_path / "dist" / "homebrew"
    d.mkdir(parents=True)
    (d / "fabric-emulator.rb").write_text(
        'cask "fabric-emulator" do\n'
        '  version "1.0.0"\n'
        '  postflight do\n'
        '    system_command "/bin/true"\n'
        '  end\n'
        'end\n', encoding="utf-8")
    problems = c.check_rendered(d)
    assert len(problems) == 1 and "postflight" in problems[0]


def test_a_rendered_cask_using_the_steps_form_passes(tmp_path):
    d = tmp_path / "dist" / "homebrew"
    d.mkdir(parents=True)
    (d / "fabric-emulator.rb").write_text(
        'cask "fabric-emulator" do\n'
        '  postflight_steps do\n'
        '    system_command "/bin/true"\n'
        '  end\n'
        'end\n', encoding="utf-8")
    assert c.check_rendered(d) == []


def test_a_rendered_cask_is_found_in_a_nested_directory(tmp_path):
    """`rglob`, not `glob`: GoReleaser nests its output per cask, so a
    non-recursive search would find nothing and report success."""
    d = tmp_path / "dist" / "homebrew"
    (d / "Casks" / "f").mkdir(parents=True)
    (d / "Casks" / "f" / "fabric-emulator.rb").write_text(
        "  preflight do\n  end\n", encoding="utf-8")
    assert len(c.check_rendered(d)) == 1


def test_no_rendered_cask_at_all_is_itself_the_finding(tmp_path):
    """The script's own comment says it: "a silent zero here would be the whole
    point of the check evaporating: no files scanned reads exactly like no
    problems found." At release time an empty dist means GoReleaser did not write
    the cask, and passing on that ships the deprecation unexamined."""
    d = tmp_path / "dist" / "homebrew"
    d.mkdir(parents=True)
    problems = c.check_rendered(d)
    assert len(problems) == 1 and "nothing was checked" in problems[0]


def test_every_finding_in_every_rendered_cask_is_reported(tmp_path):
    """Two casks, two stanzas each. Stopping at the first would leave the rest to
    be discovered one release at a time."""
    d = tmp_path / "dist" / "homebrew"
    d.mkdir(parents=True)
    for name in ("a.rb", "b.rb"):
        (d / name).write_text(
            "  preflight do\n  end\n  uninstall_postflight do\n  end\n",
            encoding="utf-8")
    assert len(c.check_rendered(d)) == 4


# --- the exit-code contract ---------------------------------------------------


def test_main_exits_non_zero_and_names_the_problem(tmp_path, monkeypatch, capsys):
    """`main` is what the witnesses job and release.yml run, so the exit code is
    the contract. A checker that finds the problem and returns 0 is not a gate."""
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, '''
homebrew_casks:
  - name: fabric-emulator
    custom_block: |
      preflight do
      end
'''))
    monkeypatch.setattr(sys, "argv", ["check_cask_stanzas.py"])
    assert c.main() == 1
    assert "Deprecated Homebrew cask stanzas" in capsys.readouterr().out


def test_main_exits_zero_on_a_clean_config(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(c, "ROOT", goreleaser(tmp_path, "homebrew_casks:\n  - name: x\n"))
    monkeypatch.setattr(sys, "argv", ["check_cask_stanzas.py"])
    assert c.main() == 0
    assert "no deprecated cask stanzas" in capsys.readouterr().out


def test_main_rendered_mode_reads_the_directory_it_is_given(tmp_path, monkeypatch, capsys):
    """`--rendered DIR` must reach check_rendered rather than falling through to
    the source. The two modes answer different questions, and a flag that was
    parsed but not acted on would run the wrong one at release time."""
    d = tmp_path / "dist" / "homebrew"
    d.mkdir(parents=True)
    (d / "x.rb").write_text("  postflight do\n  end\n", encoding="utf-8")
    monkeypatch.setattr(sys, "argv", ["check_cask_stanzas.py", "--rendered", str(d)])
    # ROOT is left pointing at a directory with NO config, so a fall-through to
    # check_source would SystemExit instead of reporting the rendered finding.
    monkeypatch.setattr(c, "ROOT", tmp_path)
    assert c.main() == 1
    assert "deprecated `postflight`" in capsys.readouterr().out

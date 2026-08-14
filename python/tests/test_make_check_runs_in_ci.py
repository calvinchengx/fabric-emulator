"""Every invariant `make check` runs, CI runs too.

WHY THIS EXISTS. `make check` is described in the Makefile as "the checks that
used to exist only in CI". The drift went the other way and nobody noticed:
three of its eleven scripts were never invoked by `ci.yml`, so they ran only on
the machine of whoever happened to type `make check`.

- `check_example_portability.py` — the whole segregation boundary between code
  that runs on Fabric and code that runs here.
- `check_capture_redaction.py` — a PRIVACY assertion, whose own docstring warns
  that a redactor passing one value through "publishes a real customer's
  hostname forever".
- `check_entra_install.py` — a retry asserted in both directions.

All three passed the entire time, which is why the gap survived: **a check that
passes is indistinguishable from a check that is running.** The only way to tell
them apart is to ask which one CI invokes, and nothing asked.

Fixing the three instances would leave the shape intact — the next script added
to `make check` repeats it silently. So the list is asserted instead.
"""
import pathlib
import re

REPO = pathlib.Path(__file__).resolve().parents[2]
MAKEFILE = REPO / "Makefile"
CI = REPO / ".github" / "workflows" / "ci.yml"

# Scripts deliberately local-only belong here WITH A REASON, so an exemption is
# a decision someone wrote down rather than an omission nobody saw. Empty today.
LOCAL_ONLY: dict[str, str] = {}


def _check_target_scripts() -> set[str]:
    """The scripts the Makefile's `check` target runs."""
    text = MAKEFILE.read_text(encoding="utf-8")
    body, seen = [], False
    for line in text.split("\n"):
        if line.startswith("check:"):
            seen = True
            continue
        if seen:
            # A recipe line is TAB-indented; the first non-tab, non-blank line
            # after it ends the target.
            if line.startswith("\t"):
                body.append(line)
            elif line.strip():
                break
    return set(re.findall(r"scripts/([a-z_0-9]+)\.py", "\n".join(body)))


def test_the_makefile_target_is_parsed_at_all():
    """A parser that silently matches nothing turns this whole file into a
    vacuous pass — the exact defect it exists to prevent, one level up."""
    scripts = _check_target_scripts()
    assert len(scripts) >= 8, f"parsed {len(scripts)} scripts from `make check`; the parser is broken"
    assert "check_witnesses" in scripts


def test_every_make_check_script_is_also_invoked_by_ci():
    ci = CI.read_text(encoding="utf-8")
    missing = sorted(
        s for s in _check_target_scripts()
        if s not in LOCAL_ONLY and f"scripts/{s}.py" not in ci
    )
    assert not missing, (
        "`make check` runs these and ci.yml does not, so they are enforced only "
        f"on a developer's laptop: {missing}\n"
        "Add them to the repo-invariants job, or record an exemption with its "
        "reason in LOCAL_ONLY."
    )


def test_exemptions_carry_a_reason():
    """An exemption with an empty reason is an omission wearing a decision's
    clothes."""
    for name, why in LOCAL_ONLY.items():
        assert why.strip(), f"{name} is exempt with no reason given"
        assert f"scripts/{name}.py" in MAKEFILE.read_text(encoding="utf-8"), (
            f"{name} is exempt but `make check` no longer runs it — a stale "
            "exemption hides the next real omission"
        )

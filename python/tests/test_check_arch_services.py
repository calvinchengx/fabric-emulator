"""The architecture-roster check had no tests of its own.

It exists because a diagram's CAST OF SERVICES is a list, and lists drift: the
architecture doc described a two-system model for as long as `keyvault-emulator`
had been a default service. A checker guarding against silent drift is worth
nothing if it can itself drift silently — and this one shipped with its logic
executed by no test at all.

Two properties matter and neither is obvious from reading it:

  * the compose parse is hand-rolled (no yaml dependency), so it has to be
    exercised against the shapes a real compose file takes;
  * it must FAIL when a service is undescribed, which is the whole point and
    the thing a check that always passes gets wrong.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_arch_services as c  # noqa: E402

COMPOSE = """\
services:
  entra-emulator:
    image: ghcr.io/x/entra:1
    ports:
      - "8443:8443"
  fabric-emulator:
    image: ghcr.io/x/fabric:1
  openmetadata:
    profiles: [governance]
    image: om:1
  kustainer:
    image: k:1
    profiles:
      - rti

volumes:
  data:
"""


# --- the parse ----------------------------------------------------------------

def test_only_unprofiled_services_are_core():
    # Profiled services are opt-in and documented elsewhere; demanding they
    # appear in the diagram would fail on correct prose.
    assert c.core_services(COMPOSE) == ["entra-emulator", "fabric-emulator"]


def test_a_profiles_key_in_either_yaml_style_is_seen():
    # `profiles: [governance]` (flow) and a `- rti` block sequence are both
    # valid compose; missing one style would let a service through as core.
    assert "openmetadata" not in c.core_services(COMPOSE)
    assert "kustainer" not in c.core_services(COMPOSE)


def test_keys_outside_the_services_block_are_not_services():
    # `volumes:` has 2-space children too. Treating them as services would
    # demand the architecture doc describe a volume.
    assert "data" not in c.core_services(COMPOSE)


def test_a_file_with_no_services_block_yields_nothing():
    assert c.core_services("volumes:\n  data:\n") == []


# --- the verdict --------------------------------------------------------------

@pytest.fixture
def stack(tmp_path, monkeypatch):
    """Point the checker at a compose file and an architecture doc we control."""
    def build(compose=COMPOSE, arch="", mermaid=None):
        cpath, apath = tmp_path / "docker-compose.yml", tmp_path / "arch.md"
        cpath.write_text(compose)
        body = arch
        if mermaid is not None:
            body += "\n```mermaid\n" + mermaid + "\n```\n"
        apath.write_text(body)
        monkeypatch.setattr(c, "COMPOSE", cpath)
        monkeypatch.setattr(c, "ARCH", apath)
    return build


def test_passes_when_every_core_service_is_in_prose_and_diagram(stack, capsys):
    stack(arch="entra-emulator and fabric-emulator talk.",
          mermaid="A[entra-emulator] --> B[fabric-emulator]")
    assert c.main() == 0
    assert "2 core services, all described" in capsys.readouterr().out


def test_fails_when_a_service_is_in_neither_prose_nor_diagram(stack, capsys):
    # `arch` is the WHOLE file, and the mermaid block is part of it — so a
    # service present in the diagram is necessarily present in the document.
    # "missing from the document" is therefore only reachable together with
    # "and the mermaid diagram", which is what an entirely undescribed service
    # looks like.
    stack(arch="entra-emulator only.", mermaid="A[entra-emulator]")
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "fabric-emulator" in out
    assert "the document and the mermaid diagram" in out


def test_a_service_in_the_prose_but_not_the_diagram_is_reported_for_the_diagram(stack, capsys):
    # THE ORIGINAL BUG's exact shape: described in words, absent from the
    # picture — and the picture is what a reader believes.
    stack(arch="entra-emulator and fabric-emulator talk.",
          mermaid="A[entra-emulator]")
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "fabric-emulator" in out
    assert "missing from the mermaid diagram" in out
    assert "the document and" not in out, "it IS in the document"


def test_a_longer_name_in_the_doc_still_matches(stack, capsys):
    # `keyvault-emulator` in compose, `azure-keyvault-emulator` in the doc: the
    # check is a substring so the doc may be more explicit than compose.
    stack(compose="services:\n  keyvault-emulator:\n    image: kv:1\n",
          arch="the azure-keyvault-emulator validates tokens",
          mermaid="A[azure-keyvault-emulator]")
    assert c.main() == 0


def test_a_document_with_no_mermaid_block_fails(stack, capsys):
    stack(arch="prose only, no diagram")
    assert c.main() == 1
    assert "no mermaid block" in capsys.readouterr().out


def test_a_compose_that_parses_to_nothing_fails(stack, capsys):
    # A parse finding nothing must fail rather than vacuously pass — otherwise
    # a compose reformat silently disarms the check.
    stack(compose="volumes:\n  data:\n", arch="x", mermaid="A[x]")
    assert c.main() == 1
    assert "parsed no core services" in capsys.readouterr().out


def test_the_real_repo_passes_its_own_check(capsys):
    # The checker is only useful if the repo satisfies it; this is also what
    # makes a future service addition fail here rather than in review.
    assert c.main() == 0, capsys.readouterr().out

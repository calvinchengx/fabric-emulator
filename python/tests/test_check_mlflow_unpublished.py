r"""A reachability mitigation is only as good as the check that asserts it.

check_mlflow_unpublished exists because GHSA-h7x2-h6g9-p789 has no fixed
release and no available downgrade: what keeps the e2e MLflow server harmless
is that nothing outside its compose network can reach it. That is a property
somebody can delete in one line while debugging, which is why there is a check
-- and it had no tests, which is why there are now these.

The failure mode that matters is NOT the check reporting a false positive. It
is the check quietly matching nothing: the regex is two narrow patterns against
raw YAML, and a compose file that spelled `ports` any other way would sail
past while the report still said "publishes no host port". So every test below
plants a real `ports:` and asserts the check FAILS -- a green from this checker
has to be a green it earned.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_mlflow_unpublished as c  # noqa: E402

CLEAN = """\
services:
  mlflow:
    image: ghcr.io/mlflow/mlflow:3.15.2
    command: ["mlflow", "server", "--disable-security-middleware"]
    expose:
      - "5000"
"""


def write(tmp_path, text):
    """Point the checker at a compose file under tmp_path."""
    compose = tmp_path / "docker-compose.yml"
    compose.write_text(text)
    return compose


def test_a_stack_that_publishes_nothing_passes(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(c, "COMPOSE", write(tmp_path, CLEAN))
    assert c.main() == 0
    assert "publishes no host port" in capsys.readouterr().out


def test_the_block_form_is_caught(tmp_path, monkeypatch, capsys):
    """The spelling a debugging session actually adds."""
    monkeypatch.setattr(c, "COMPOSE", write(tmp_path, CLEAN + """\
    ports:
      - "5000:5000"
"""))
    assert c.main() == 1
    err = capsys.readouterr().err
    assert "publishes a port" in err
    assert "GHSA-h7x2-h6g9-p789" in err, "the report must name the advisory it protects"


def test_the_inline_list_form_is_caught(tmp_path, monkeypatch):
    """`ports: ["5000:5000"]` publishes exactly as much as the block form."""
    monkeypatch.setattr(c, "COMPOSE", write(tmp_path, CLEAN + '    ports: ["5000:5000"]\n'))
    assert c.main() == 1


def test_the_offending_line_is_named(tmp_path, monkeypatch, capsys):
    """A line number, because the point is to get it reverted, not just failed."""
    monkeypatch.setattr(c, "COMPOSE", write(tmp_path, CLEAN + "    ports:\n      - 5000\n"))
    assert c.main() == 1
    err = capsys.readouterr().err
    assert "7: ports:" in err, err


def test_a_missing_compose_file_fails_rather_than_passing(tmp_path, monkeypatch, capsys):
    """Fail closed. A deleted file must not read as 'nothing publishes a port'.

    This is the difference between a check that ran and a check that could not,
    and it is the one a green would otherwise hide.
    """
    monkeypatch.setattr(c, "COMPOSE", tmp_path / "gone.yml")
    assert c.main() == 1
    assert "does not exist" in capsys.readouterr().err


@pytest.mark.parametrize("line", [
    "    # ports:\n",           # a commented-out remnant is not a published port
    "    expose:\n      - 5000\n",  # expose does not publish to the host
])
def test_what_is_deliberately_not_caught(tmp_path, monkeypatch, line):
    """Precision: neither of these publishes anything, and reporting them is
    how a checker teaches people to skim past it."""
    monkeypatch.setattr(c, "COMPOSE", write(tmp_path, CLEAN + line))
    assert c.main() == 0


def test_the_real_compose_file_is_clean():
    """The tree itself, so the guard is not only tested against fixtures."""
    assert c.COMPOSE.is_file(), f"{c.COMPOSE} moved; the checker points at nothing"
    assert c.main() == 0

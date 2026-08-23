"""What `/statements` refuses, and what it merely notes.

#349: the route accepted `env` and `spark_conf` and applied neither, returning
`{"status":"ok"}` with the field on the floor. Nothing sent them by the time it
was filed — the harm was the SILENCE, which had already sent a reader to file a
bug against databricks-emulator rather than against this agent.

Two rules, and the asymmetry between them is the design:

  * a field we KNOW is inert is named in the log with the reason it is
    dropped, so a caller learns about it instead of guessing;
  * a field we simply do not recognise is logged too, more briefly.

NOTHING IS REFUSED, and the first cut of this got that wrong. It returned an
error envelope for `env` and `spark_conf` on the issue's premise that nothing
sends them -- true of databricks-emulator's main, false of every release of it.
`e2e/databricks-chain` pins 0.2.4, which sends both, and failed within minutes.
The refusal can only be added once no caller in the fleet still sends them.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))
import statement_fields as sf  # noqa: E402, I001


def test_a_plain_statement_is_accepted():
    assert sf.check({"code": "1 + 1", "kind": "python", "session": "s1"}) is None


def test_env_is_named_in_the_log_with_where_to_put_it_instead(capsys):
    assert sf.check({"code": "x", "env": {"LAKEHOUSE_ID": "contoso"}}) is None
    err = capsys.readouterr().err
    assert "env" in err and "does NOT apply" in err
    # Actionable, or it just moves the confusion somewhere else.
    assert "in the code it runs" in err


def test_spark_conf_says_why_it_cannot_be_statement_scoped(capsys):
    assert sf.check({"code": "x", "spark_conf": {"spark.sql.shuffle": "1"}}) is None
    assert "outlives the statement" in capsys.readouterr().err


def test_neither_inert_field_fails_the_statement(capsys):
    """THE REGRESSION THAT REACHED CI. Every released databricks-emulator sends
    both (v0.2.4 through v0.2.9); the fix that stops sending them merged after
    the last release. Returning an error envelope here failed
    `e2e/databricks-chain` outright."""
    for field in sf.IGNORED_STATEMENT_FIELDS:
        assert sf.check({"code": "x", field: {"a": "b"}}) is None, (
            f"{field} was refused; every released databricks-emulator sends it")
        assert field in capsys.readouterr().err, f"{field} was dropped in silence"


def test_a_field_the_route_reads_is_not_reported_as_unknown(capsys):
    """The known set is what makes the log worth reading. If it drifted behind
    the handler, every ordinary statement would print a line and the one that
    matters would be lost in it."""
    sf.check({
        "code": "x", "kind": "python", "session": "s1", "jobId": "j", "cellIndex": 0,
        "principal": "p", "workspace": "w", "item": "i", "lakehouse": "l",
        "schema": "s", "schemas": [], "tables": [],
        "workspaceId": "w", "lakehouseId": "l", "notebookId": "n",
        "currentWorkspaceId": "w", "defaultLakehouseId": "l",
        "currentNotebookId": "n", "currentJobId": "j", "isForPipeline": False,
    })
    assert capsys.readouterr().err == ""


def test_an_unknown_field_is_logged_and_the_statement_still_runs(capsys):
    """NOT refused. A newer emulator sending a field an older agent has not
    heard of must degrade, not fail: these are separate images with independent
    versions, so a strict schema would turn a rolling upgrade into an outage."""
    assert sf.check({"code": "x", "somethingNew": 1}) is None
    assert "somethingNew" in capsys.readouterr().err


def test_several_unknown_fields_are_named_together(capsys):
    sf.check({"code": "x", "bbb": 1, "aaa": 2})
    err = capsys.readouterr().err
    assert "aaa, bbb" in err, err


def test_the_known_set_matches_what_the_handler_reads():
    """Derived from the source, so the list and the handler cannot drift.

    A name the handler reads but this set omits makes every request carrying
    it print a spurious line, and the one line that matters is then lost in
    the noise.
    """
    import re

    agent = (Path(__file__).resolve().parents[1] / "spark_agent" / "agent.py").read_text()

    def reads(text):
        return set(re.findall(r'req\.get\("([a-zA-Z]+)"', text))

    # The statements branch itself...
    branch = agent[agent.index('if self.path == "/statements":'):
                   agent.index('elif self.path == "/register":')]
    # ...and the helpers it hands the same dict to.
    helpers = ""
    for name in ("def remember_context(", "def _apply_onelake_security("):
        i = agent.index(name)
        # To the next TOP-LEVEL definition, which may be a class: slicing to
        # the next "\ndef " raised "substring not found" here, because
        # _apply_onelake_security is followed by `class Handler`.
        nxt = re.search(r"^(?:def |class )", agent[i + 1:], re.M)
        helpers += agent[i:i + 1 + nxt.start()] if nxt else agent[i:]

    missing = (reads(branch) | reads(helpers)) - sf.KNOWN_STATEMENT_FIELDS
    assert not missing, (
        f"the handler reads {sorted(missing)} but KNOWN_STATEMENT_FIELDS omits "
        "them, so every request carrying one logs a spurious line")

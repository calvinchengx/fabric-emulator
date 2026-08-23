"""What `/statements` refuses, and what it merely notes.

#349: the route accepted `env` and `spark_conf` and applied neither, returning
`{"status":"ok"}` with the field on the floor. Nothing sent them by the time it
was filed — the harm was the SILENCE, which had already sent a reader to file a
bug against databricks-emulator rather than against this agent.

Two rules, and the asymmetry between them is the design:

  * a field we KNOW is inert is refused by name, so a caller learns at the
    first request instead of from missing behaviour later;
  * a field we simply do not recognise is logged and ignored, because this is
    an internal protocol between two images that version independently and a
    newer emulator must be able to talk to an older agent.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))
import statement_fields as sf  # noqa: E402, I001


def test_a_plain_statement_is_accepted():
    assert sf.check({"code": "1 + 1", "kind": "python", "session": "s1"}) is None


def test_env_is_refused_by_name_and_says_where_to_put_it():
    got = sf.check({"code": "x", "env": {"LAKEHOUSE_ID": "contoso"}})
    assert got is not None and got["status"] == "error"
    assert got["ename"] == "UnsupportedField"
    assert "env" in got["evalue"]
    # The refusal has to be actionable, or it just moves the confusion.
    assert "in the code it runs" in got["evalue"]


def test_spark_conf_is_refused_because_it_cannot_be_statement_scoped():
    got = sf.check({"code": "x", "spark_conf": {"spark.sql.shuffle.partitions": "1"}})
    assert got is not None and got["ename"] == "UnsupportedField"
    assert "outlives the statement" in got["evalue"]


def test_a_refusal_names_the_field_the_caller_actually_sent():
    for field in sf.REFUSED_STATEMENT_FIELDS:
        got = sf.check({"code": "x", field: {}})
        assert field in got["evalue"], f"{field} refusal does not name it"


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

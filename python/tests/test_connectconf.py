"""The agent normalises a byte-size conf only when the engine spells it unreadably.

Both directions have shipped as bugs, one per repo, because the agent image is
shared and the two emulators pin different Sail builds:

- Presetting unconditionally capped local relations at 64MiB on an engine
  offering 3GiB (#277 removed it for that reason, correctly).
- Presetting never made every `createDataFrame` raise
  `ValueError: invalid literal for int() with base 10: '3GB'` on the engine
  databricks-emulator pins — including `delta_ops`' own one-row result frame,
  so an intercepted MERGE could not even return its answer.

So the tests below pin the decision, not a constant.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))

import connectconf  # noqa: E402


class FakeConf:
    def __init__(self, served, settable=True):
        self.served = served
        self.settable = settable
        self.sets = []

    def get(self, key):
        if isinstance(self.served, Exception):
            raise self.served
        return self.served

    def set(self, key, value):
        if not self.settable:
            raise RuntimeError("engine went away")
        self.sets.append((key, value))
        self.served = value


class FakeSpark:
    def __init__(self, served, settable=True):
        self.conf = FakeConf(served, settable)


@pytest.mark.parametrize("value,want", [
    ("3GB", 3 * 1024 ** 3),
    ("3gb", 3 * 1024 ** 3),
    ("3g", 3 * 1024 ** 3),
    ("512m", 512 * 1024 ** 2),
    ("512 MB", 512 * 1024 ** 2),
    ("1024k", 1024 * 1024),
    ("2t", 2 * 1024 ** 4),
    ("1.5g", int(1.5 * 1024 ** 3)),
    # Already a plain byte count: nothing to rewrite, and rewriting would be a
    # change to a value the client can read perfectly well.
    ("3221225472", None),
    ("0", None),
    ("", None),
    ("nonsense", None),
    (None, None),
    (12345, None),
])
def test_byte_size_parses_the_jvm_spelling_and_only_that(value, want):
    assert connectconf.byte_size(value) == want


def test_a_byte_count_the_client_can_read_is_left_exactly_alone():
    # This is Sail 0.7.0, which fabric-emulator pins. Rewriting here is what
    # capped user code at 64MiB for a bug the engine does not have.
    spark = FakeSpark("3221225472")
    assert connectconf.apply(spark) == "ok"
    assert spark.conf.sets == []


def test_a_jvm_byte_string_is_rewritten_to_the_same_size_not_a_constant():
    # This is the older Sail databricks-emulator pins. The rewrite has to
    # preserve the limit: presetting 64MiB here would be a 48x cut.
    spark = FakeSpark("3GB")
    assert connectconf.apply(spark) == "rewritten"
    assert spark.conf.sets == [(connectconf.CONF, str(3 * 1024 ** 3))]
    assert int(spark.conf.get(connectconf.CONF)) == 3 * 1024 ** 3


def test_an_unrecognised_value_is_reported_and_left_alone(capsys):
    # Refuse rather than guess: writing an invented number over a value we do
    # not understand would be the silent wrong thing.
    spark = FakeSpark("banana")
    assert connectconf.apply(spark) == "unreadable"
    assert spark.conf.sets == []
    assert "cannot parse" in capsys.readouterr().err


def test_an_unreachable_engine_is_not_fatal(capsys):
    spark = FakeSpark(RuntimeError("connection refused"))
    assert connectconf.apply(spark) == "unavailable"
    assert "could not read" in capsys.readouterr().err


def test_an_engine_that_refuses_the_write_is_not_fatal(capsys):
    spark = FakeSpark("3GB", settable=False)
    assert connectconf.apply(spark) == "unavailable"
    assert "could not rewrite" in capsys.readouterr().err


def test_the_rewrite_is_what_pyspark_would_have_choked_on():
    # The client's failure is literally int(served). Asserting against that
    # rather than against our own parser keeps the test honest about the bug.
    served = "3GB"
    with pytest.raises(ValueError):
        int(served)
    spark = FakeSpark(served)
    connectconf.apply(spark)
    int(spark.conf.get(connectconf.CONF))  # no raise: that is the whole fix

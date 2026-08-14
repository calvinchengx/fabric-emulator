"""The `sc` facade: it does the measured subset, and refuses the rest loudly.

THE ORACLE QUESTION, because this repo has been bitten three times by tests
that share their expectations with the implementation. The expected values here
are NOT written twice: they live in `spark_agent/rdd_contract.py`, and
`e2e/spark-jvm/job.py` evaluates the SAME snippets against a real Apache Spark
3.5 JVM in CI. So these tests prove the facade matches the contract, and the
JVM job proves the contract matches Spark. Either alone would be a claim about
my own head.
"""
import logging
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))

import rdd_contract  # noqa: E402
import rddfacade  # noqa: E402


class _FakeSession:
    """Only what the facade touches: createDataFrame, and the guarded names."""

    def __init__(self):
        self.created: tuple | None = None

    def createDataFrame(self, rows, schema=None):  # noqa: N802 — Spark's spelling
        self.created = (list(rows), schema)
        return _FakeDF(list(rows))

    def __getattr__(self, name):
        # Mirrors pyspark's Connect guard, so the attach() test proves the real
        # thing rather than a permissive stand-in.
        if name in ("_jsc", "_jconf", "_jvm", "_jsparkSession", "sparkContext", "newSession"):
            raise AttributeError(f"JVM_ATTRIBUTE_NOT_SUPPORTED: {name}")
        raise AttributeError(name)


class _FakeDF:
    def __init__(self, rows):
        self._rows = rows

    def count(self):
        return len(self._rows)


@pytest.fixture
def sc():
    return rddfacade.SparkContextFacade(_FakeSession())


# --- the contract, evaluated exactly as the JVM oracle evaluates it -----------

@pytest.mark.parametrize("label,snippet,expected",
                         rdd_contract.CASES, ids=[c[0] for c in rdd_contract.CASES])
def test_the_measured_idioms_give_sparks_answer(sc, label, snippet, expected):
    assert eval(snippet, {"sc": sc}) == expected  # noqa: S307 — the contract IS source text


@pytest.mark.parametrize("label,snippet",
                         rdd_contract.VOID_CASES, ids=[c[0] for c in rdd_contract.VOID_CASES])
def test_the_void_idioms_return_none_like_spark(sc, label, snippet):
    assert eval(snippet, {"sc": sc}) is None  # noqa: S307


def test_the_contract_is_not_empty():
    """A parametrized suite over an empty list passes while testing nothing."""
    assert len(rdd_contract.CASES) >= 5
    assert rdd_contract.VOID_CASES
    assert rdd_contract.REFUSED_CASES, "no refusal is pinned, so over-permissiveness is unchecked"


# --- setLogLevel really does something ---------------------------------------

def test_set_log_level_actually_moves_the_level(sc):
    """Measured twice in `microsoft/fabric-samples`. A no-op accepting the call
    would be the accepted-but-inert pattern the engine-matrix probes exist to
    catch."""
    sc.setLogLevel("ERROR")
    assert logging.getLogger().level == logging.ERROR
    sc.setLogLevel("WARN")
    assert logging.getLogger().level == logging.WARNING


def test_an_unknown_log_level_is_refused_rather_than_ignored(sc):
    with pytest.raises(ValueError, match="unknown log level"):
        sc.setLogLevel("VERBOSE")


# --- the ceiling, which is the honest part -----------------------------------

def test_parallelize_refuses_above_the_local_ceiling(sc):
    """`map` runs Python over a list. That is right for a four-element smoke
    idiom and a different execution model at scale, so it stops rather than
    quietly pretending to distribute."""
    with pytest.raises(NotImplementedError) as e:
        sc.parallelize(range(rddfacade.LOCAL_LIMIT + 1))
    msg = str(e.value)
    assert "eager and LOCAL" in msg
    assert "docker-compose.spark-jvm.yml" in msg, "the refusal names no way out"


def test_parallelize_accepts_exactly_the_ceiling(sc):
    assert sc.parallelize(range(rddfacade.LOCAL_LIMIT)).count() == rddfacade.LOCAL_LIMIT


def test_the_ceiling_is_far_above_the_measured_idiom():
    """Four elements is what Fabric's docs use; the ceiling must not be so tight
    that a slightly larger honest use trips it."""
    assert rddfacade.LOCAL_LIMIT >= 1000


# --- everything unmeasured refuses, and says where to go ---------------------

@pytest.mark.parametrize("attr", [
    "textFile", "wholeTextFiles", "accumulator", "broadcast", "emptyRDD",
    "getConf", "addPyFile", "union", "runJob", "statusTracker",
])
def test_unmeasured_sparkcontext_attributes_refuse_with_a_pointer(sc, attr):
    with pytest.raises(NotImplementedError) as e:
        getattr(sc, attr)
    msg = str(e.value)
    assert attr in msg
    assert "docker-compose.spark-jvm.yml" in msg
    assert "protocol limit" in msg, "the message blames Sail for a Connect limit"


@pytest.mark.parametrize("attr", [
    "mapPartitions", "reduceByKey", "groupByKey", "saveAsTextFile",
    "repartition", "cache", "persist", "zipWithIndex",
])
def test_unmeasured_rdd_methods_refuse_with_a_pointer(sc, attr):
    rdd = sc.parallelize([1, 2, 3])
    with pytest.raises(NotImplementedError) as e:
        getattr(rdd, attr)
    assert attr in str(e.value)
    assert "docker-compose.spark-jvm.yml" in str(e.value)


def test_the_refusal_explains_it_is_a_protocol_limit_not_a_sail_gap(sc):
    """Blaming Sail would send someone to Sail's issue tracker for something
    Apache's own Connect server does identically."""
    with pytest.raises(NotImplementedError) as e:
        sc.textFile("/x")
    assert "any engine" in str(e.value)


# --- toDF, the shape Microsoft's smoke idiom needs ---------------------------

def test_todf_refuses_scalars_exactly_as_pyspark_does():
    """This test asserted the OPPOSITE and was wrong, which the JVM oracle
    caught the first time it ran.

    The facade wrapped bare scalars into one-tuples and returned a DataFrame.
    Real PySpark raises CANNOT_INFER_SCHEMA_FOR_TYPE. The idiom that inspired
    it — `sc.parallelize(Seq(1,2,3,4)).toDF()` in Microsoft's diagnostic-emitter
    docs — is SCALA, where implicits supply the encoder, and transcribing it
    into a Python contract made the emulator accept what a tenant rejects.
    """
    sc = rddfacade.SparkContextFacade(_FakeSession())
    with pytest.raises(TypeError) as e:
        sc.parallelize([1, 2, 3]).toDF()
    msg = str(e.value)
    assert "CANNOT_INFER_SCHEMA_FOR_TYPE" in msg, "does not name Spark's own error"
    assert "(1,)" in msg, "the refusal shows no working form"


@pytest.mark.parametrize("label,snippet",
                         rdd_contract.REFUSED_CASES, ids=[c[0] for c in rdd_contract.REFUSED_CASES])
def test_the_refused_contract_cases_are_refused(label, snippet):
    """Same list the JVM oracle asserts real Spark refuses."""
    sc = rddfacade.SparkContextFacade(_FakeSession())
    with pytest.raises((TypeError, NotImplementedError)):
        eval(snippet, {"sc": sc})  # noqa: S307


def test_todf_accepts_the_row_shape_pyspark_accepts():
    session = _FakeSession()
    sc = rddfacade.SparkContextFacade(session)
    sc.parallelize([(1,), (2,)]).toDF()
    assert session.created is not None
    rows, schema = session.created
    assert rows == [(1,), (2,)]
    assert schema is None, "no schema should be invented when none was given"


def test_todf_passes_an_explicit_schema_through():
    session = _FakeSession()
    sc = rddfacade.SparkContextFacade(session)
    sc.parallelize([(1, "a")]).toDF(["id", "name"])
    assert session.created == ([(1, "a")], ["id", "name"])


def test_todf_without_a_session_refuses_rather_than_crashing():
    sc = rddfacade.SparkContextFacade(None)
    with pytest.raises(NotImplementedError, match="No Spark session"):
        sc.parallelize([1]).toDF()


# --- the _jvm facade ---------------------------------------------------------

def test_the_measured_log4j_idiom_works_end_to_end():
    """`spark._jvm.org.apache.log4j.LogManager.getLogger(...)` — every verified
    `_jvm` hit in MicrosoftDocs/fabric-docs is exactly this."""
    jvm = rddfacade.JvmFacade()
    logger = jvm.org.apache.log4j.LogManager.getLogger("PySparkLogger")
    logger.info("Application started.")
    logger.warn("w")
    logger.error("e")
    logger.setLevel("INFO")


def test_the_log4j_logger_writes_through_python_logging(caplog):
    """Backed by real logging, not swallowed: a logger that accepts calls and
    emits nothing is the accepted-but-inert pattern again."""
    jvm = rddfacade.JvmFacade()
    with caplog.at_level(logging.INFO, logger="Probe"):
        jvm.org.apache.log4j.LogManager.getLogger("Probe").info("hello from log4j")
    assert "hello from log4j" in caplog.text


@pytest.mark.parametrize("path", ["java", "scala", "py4j"])
def test_other_jvm_namespaces_refuse(path):
    """`java.lang.Class.forName` would have to RETURN something, and any answer
    would be invented. Refusing is the only honest option without a JVM."""
    jvm = rddfacade.JvmFacade()
    with pytest.raises(NotImplementedError) as e:
        getattr(jvm, path)
    assert "org.apache.log4j" in str(e.value)


def test_other_apache_packages_refuse():
    jvm = rddfacade.JvmFacade()
    with pytest.raises(NotImplementedError):
        _ = jvm.org.apache.spark


# --- attach(): the pyspark guard bypass --------------------------------------

def test_attach_binds_past_pysparks_getattr_guard():
    """pyspark's Connect session raises JVM_ATTRIBUTE_NOT_SUPPORTED from
    `__getattr__`, which Python consults only when normal lookup FAILS — so an
    instance assignment is never intercepted. This is the whole mechanism, and
    if a pyspark version ever turned these into properties it would break here
    rather than silently in production."""
    session = _FakeSession()
    with pytest.raises(AttributeError, match="JVM_ATTRIBUTE_NOT_SUPPORTED"):
        _ = session.sparkContext

    sc = rddfacade.attach(session)

    assert session.sparkContext is sc
    assert isinstance(session._jvm, rddfacade.JvmFacade)
    assert session.sparkContext.parallelize([1, 2, 3]).sum() == 6


def test_attach_returns_the_same_object_it_binds():
    """The agent binds the return value as the bare `sc` global; if it differed
    from `spark.sparkContext`, the two spellings would drift."""
    session = _FakeSession()
    sc = rddfacade.attach(session)
    assert sc is session.sparkContext


def test_the_repr_says_it_is_a_facade(sc):
    """Someone printing `sc` in a notebook should not conclude they have a real
    SparkContext."""
    assert "facade" in repr(sc)
    assert "facade" in repr(rddfacade.JvmFacade())
    assert "not distributed" in repr(sc.parallelize([1]))

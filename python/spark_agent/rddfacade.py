"""A local `sc` for the Sail engine, sized by measured usage.

WHY THIS EXISTS. Real Fabric notebooks see `sc` and `spark._jvm`; Spark Connect
exposes neither, on ANY engine — Apache's own Connect server has the same hole,
so this is a protocol limit rather than something Sail will grow. Until now the
agent bound `sc` to a stub that refused everything, which is honest but means a
notebook doing the one thing Microsoft's own docs demonstrate fails here and
works in production: a fidelity inversion.

WHAT IT IS NOT. This is not an RDD engine. `docs/50-rdd-usage-capture.md`
measured four corpora — our shipped notebooks, contoso-data-platform,
`microsoft/fabric-samples` and `MicrosoftDocs/fabric-docs` — and found the
entire `sc`/`_jvm` surface in use is **logging plus one four-element smoke
idiom**. Nobody computes with RDDs. So this implements exactly that, EAGERLY
AND LOCALLY, and refuses everything else with a pointer to the JVM overlay.

THE SIZE CEILING IS THE HONEST PART. `map` here runs a Python callable over a
Python list. For the measured idiom that is exactly right. At scale it is a
different execution model wearing the same name — no partitioning, no
distribution, and memory bounded by the driver. A facade that quietly accepted
a million rows would be the fabricated-success pattern this repo keeps removing:
it would work, slowly and differently, and nothing would say so. Above the
ceiling it refuses and names the engine that can do it for real.

DIRECTION OF FAILURE. Real Fabric has the whole API; this has a subset. So code
either behaves identically or raises a pointer — it never invents a result
Fabric would not produce. That is the safe direction, and it is why a subset is
worth shipping at all.
"""
import logging

DOC = "docs/20-lakesail-engine.md"
OVERLAY = "docker compose -f docker-compose.yml -f docker-compose.override.yml -f docker-compose.spark-jvm.yml up"

# Elements accepted by `parallelize`. Generous next to the measured idiom (four
# elements) and far below anything that could pass for distributed work, so the
# refusal lands on intent rather than on an arbitrary edge.
LOCAL_LIMIT = 10_000


def _refuse(what, detail=""):
    return NotImplementedError(
        f"{what}: not available on the emulator's default Sail (Spark Connect) "
        f"engine.{detail} Spark Connect exposes no SparkContext or JVM bridge on any "
        f"engine, so this is a protocol limit rather than a Sail gap. Use the "
        f"DataFrame/SQL API, or run the JVM overlay for the real thing:\n"
        f"  {OVERLAY}\nSee {DOC}."
    )


class LocalRDD:
    """An eager, local stand-in carrying only the measured chain."""

    def __init__(self, data, session=None):
        self._data = list(data)
        self._session = session

    # -- the measured surface -------------------------------------------------
    def map(self, f):
        return LocalRDD([f(x) for x in self._data], self._session)

    def filter(self, f):
        return LocalRDD([x for x in self._data if f(x)], self._session)

    def collect(self):
        return list(self._data)

    def count(self):
        return len(self._data)

    def sum(self):
        return sum(self._data)

    def toDF(self, schema=None):
        """The `.toDF()` in Microsoft's smoke idiom.

        Scalars are wrapped in one-tuples because `createDataFrame` needs a row
        shape; a bare list of ints is what the idiom actually passes.
        """
        if self._session is None:
            raise _refuse("rdd.toDF()", " No Spark session is attached.")
        # Scalars are REFUSED, exactly as PySpark refuses them. An earlier
        # version wrapped them into one-tuples and succeeded, which the JVM
        # oracle caught: real Spark raises CANNOT_INFER_SCHEMA_FOR_TYPE. The
        # `sc.parallelize(Seq(1,2,3,4)).toDF()` idiom in Microsoft's docs is
        # SCALA, where implicits supply the encoder; accepting it here made the
        # emulator more permissive than the tenant, which is the direction that
        # ships broken code.
        bad = next((x for x in self._data if not isinstance(x, (tuple, list, dict))), None)
        if bad is not None:
            raise TypeError(
                f"[CANNOT_INFER_SCHEMA_FOR_TYPE] Can not infer schema for type: "
                f"`{type(bad).__name__}`. Real PySpark refuses this too — "
                f"`toDF()` needs a row shape. Use "
                f"`sc.parallelize([(1,), (2,)]).toDF()`, or pass a schema."
            )
        if schema is not None:
            return self._session.createDataFrame(list(self._data), schema)
        return self._session.createDataFrame(list(self._data))

    # -- everything else ------------------------------------------------------
    def __getattr__(self, name):
        raise _refuse(
            f"rdd.{name}",
            " Only the measured subset (map/filter/collect/count/sum/toDF) is "
            "emulated; partition-level APIs, accumulators and broadcasts are not.",
        )

    def __iter__(self):
        return iter(self._data)

    def __len__(self):
        return len(self._data)

    def __repr__(self):
        return f"<LocalRDD n={len(self._data)} (emulator facade — not distributed)>"


class SparkContextFacade:
    """`sc`, implementing only what Fabric code was measured to call."""

    def __init__(self, session=None, logger=None):
        self._session = session
        self._log = logger or logging.getLogger("spark_agent.sc")

    def setLogLevel(self, level):
        """Measured in `microsoft/fabric-samples` (twice). Really applied: the
        agent's logger level moves, so the call has an effect rather than being
        silently swallowed."""
        mapped = {
            "ALL": logging.DEBUG, "TRACE": logging.DEBUG, "DEBUG": logging.DEBUG,
            "INFO": logging.INFO, "WARN": logging.WARNING, "WARNING": logging.WARNING,
            "ERROR": logging.ERROR, "FATAL": logging.CRITICAL, "OFF": logging.CRITICAL + 1,
        }.get(str(level).upper())
        if mapped is None:
            raise ValueError(f"unknown log level: {level!r}")
        self._log.setLevel(mapped)
        logging.getLogger().setLevel(mapped)
        return None

    def parallelize(self, seq, numSlices=None):  # noqa: N803 — Spark's spelling
        data = list(seq)
        if len(data) > LOCAL_LIMIT:
            raise _refuse(
                f"sc.parallelize() with {len(data)} elements",
                f" This facade is eager and LOCAL — it runs Python over a list, "
                f"which is right for the small smoke idiom Fabric's own docs use "
                f"and wrong for real work, so it stops at {LOCAL_LIMIT} rather "
                f"than pretending to distribute.",
            )
        return LocalRDD(data, self._session)

    def __getattr__(self, name):
        raise _refuse(f"sc.{name}")

    def __repr__(self):
        return "<sc: emulator facade (setLogLevel/parallelize) — not a SparkContext>"


class _Log4jLogger:
    """What `LogManager.getLogger(name)` hands back in the measured idiom."""

    def __init__(self, name):
        self._log = logging.getLogger(name)

    def info(self, msg, *a):
        self._log.info(msg, *a)

    def warn(self, msg, *a):
        self._log.warning(msg, *a)

    def warning(self, msg, *a):
        self._log.warning(msg, *a)

    def error(self, msg, *a):
        self._log.error(msg, *a)

    def debug(self, msg, *a):
        self._log.debug(msg, *a)

    def setLevel(self, _level):
        return None


class _LogManager:
    @staticmethod
    def getLogger(name="PySparkLogger"):
        return _Log4jLogger(name)


class _Log4jPackage:
    LogManager = _LogManager


class JvmFacade:
    """`spark._jvm`, answering ONLY `org.apache.log4j`.

    That is the whole measured `_jvm` surface: every verified hit in
    `MicrosoftDocs/fabric-docs` is `spark._jvm.org.apache.log4j` followed by
    `LogManager.getLogger(...)`. Anything else raises, because answering a
    Java namespace we cannot back with a JVM is how a facade becomes a lie —
    `java.lang.Class.forName` would have to return something, and any answer
    would be invented.
    """

    class _Org:
        class _Apache:
            log4j = _Log4jPackage

            def __getattr__(self, name):
                raise _refuse(f"spark._jvm.org.apache.{name}")

        apache = _Apache()

        def __getattr__(self, name):
            raise _refuse(f"spark._jvm.org.{name}")

    org = _Org()

    def __getattr__(self, name):
        raise _refuse(
            f"spark._jvm.{name}",
            " Only `org.apache.log4j` is emulated, because it is the only "
            "`_jvm` use measured in Fabric's own documentation.",
        )

    def __repr__(self):
        return "<spark._jvm: emulator facade (org.apache.log4j only)>"


def attach(session):
    """Bind `sc` and `_jvm` onto a Connect session, and return the `sc`.

    Instance assignment is what makes this possible without patching pyspark:
    its Connect `SparkSession` guards these names in `__getattr__`, which Python
    only consults when normal attribute lookup FAILS. Setting them on the
    instance means the guard is never reached. This rests on Python's attribute
    protocol rather than on pyspark internals, so a client bump cannot quietly
    re-arm the guard — `test_attach_binds_past_pysparks_getattr_guard` asserts
    the guard fires BEFORE attach and not after, and it runs against whatever
    client is pinned. The test is the claim; naming a version here would only
    record which bump was current when someone last edited the comment.

    A JVM session already has real ones; this must never overwrite them, which
    is why the caller checks first and this function is only reached for Connect.
    """
    sc = SparkContextFacade(session)
    session.sparkContext = sc
    session._jvm = JvmFacade()
    return sc

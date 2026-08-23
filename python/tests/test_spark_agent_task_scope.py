"""Two tasks in one interpreter must not see each other's argv or environment.

The agent isolates per-session user globals but not MODULE state. A
`spark_python_task`'s parameters arrive as `sys.argv = [...]` and its
environment as `os.environ.update({...})` (databricks-emulator
`internal/server/jobs.go`, `pythonPreamble`), and `sys`/`os` are one object
each per interpreter. Real Databricks runs each task in its own process, so
both are private; here a wave of independent tasks is dispatched CONCURRENTLY
into one agent, and the second assignment wins for both. Both tasks then report
SUCCESS, because nothing failed — the wrong parameters were simply processed.

THE INTERLEAVING IS AN INPUT, NOT A SAMPLE. Every test here releases its
threads from a `Barrier` placed between the write and the read, which is the
order that breaks: both tasks have assigned before either looks. Stress-looping
for it would pass on a broken tree most of the time, and a fix graded that way
is graded by luck. With the barrier the unfixed agent fails every run.

`install()` is process-global and these tests share an interpreter with the
rest of the suite. That is safe by construction: with no scope bound both
attributes fall through to the real process values, which the
`unbound_*` tests below assert directly rather than assume.
"""
import io
import os
import subprocess
import sys
import threading
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import task_scope  # noqa: E402

task_scope.install()


def run_concurrently(bodies):
    """Run each body in its own thread, released together, and return results.

    Every body takes a `sync` callable to be invoked between writing its state
    and reading it back; that is the barrier the defect needs.
    """
    barrier = threading.Barrier(len(bodies))
    results = {}
    errors = []

    def wrap(name, body):
        def go():
            try:
                results[name] = body(barrier.wait)
            except BaseException as exc:  # noqa: BLE001 - re-raised in the parent
                errors.append(exc)
                barrier.abort()
        return go

    threads = [threading.Thread(target=wrap(n, b)) for n, b in bodies.items()]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    if errors:
        raise errors[0]
    return results


# --- argv --------------------------------------------------------------------

def test_concurrent_tasks_each_read_their_own_argv():
    """The reported defect: two parameterised tasks, dispatched in one wave."""
    def task(params):
        def body(sync):
            with task_scope.scoped(task_scope.TaskScope()):
                sys.argv = ["/task.py", *params]
                sync()  # the other task has now assigned too
                return list(sys.argv)
        return body

    got = run_concurrently({
        "a": task(["AAA"]),
        "b": task(["BBB"]),
    })
    assert got["a"] == ["/task.py", "AAA"]
    assert got["b"] == ["/task.py", "BBB"]


def test_user_code_reimporting_sys_still_sees_its_own_argv():
    """`import sys` in the task body is what defeats rebinding a local name.

    It re-fetches the one module object out of `sys.modules`, walking past
    anything the preamble bound in the session's globals — so the isolation has
    to live on that shared object, which is what makes this the load-bearing case.
    """
    def task(marker):
        def body(sync):
            with task_scope.scoped(task_scope.TaskScope()):
                exec_globals = {}
                exec(f"import sys\nsys.argv = ['/t.py', {marker!r}]", exec_globals)
                sync()
                seen = {}
                exec("import sys\nout['v'] = sys.argv[-1]",
                     {"out": seen})
                return seen["v"]
        return body

    got = run_concurrently({"a": task("AAA"), "b": task("BBB")})
    assert got["a"] == "AAA"
    assert got["b"] == "BBB"


def test_argv_persists_across_statements_in_one_session():
    """A session stands in for the task process, so argv outlives one statement."""
    scope = task_scope.TaskScope()
    with task_scope.scoped(scope):
        sys.argv = ["/task.py", "once"]
    with task_scope.scoped(scope):  # the session's next statement
        assert sys.argv == ["/task.py", "once"]


def test_unbound_argv_is_the_process_argv():
    assert sys.argv == sys.__dict__["argv"]


def test_a_task_cannot_mutate_the_process_argv():
    """Including by appending, which never assigns and so never looks like a write."""
    before = list(sys.__dict__["argv"])
    with task_scope.scoped(task_scope.TaskScope()):
        sys.argv.append("/injected")
    assert sys.__dict__["argv"] == before


# --- environment -------------------------------------------------------------

def test_concurrent_tasks_each_read_their_own_env():
    """`SparkEnvVars` carries RESOLVED SECRETS, so this leak crosses a trust
    boundary rather than merely confusing a parameter."""
    def task(secret):
        def body(sync):
            with task_scope.scoped(task_scope.TaskScope()):
                os.environ.update({"TASK_SECRET": secret})
                sync()
                return os.environ["TASK_SECRET"]
        return body

    got = run_concurrently({"a": task("secret-a"), "b": task("secret-b")})
    assert got["a"] == "secret-a"
    assert got["b"] == "secret-b"


@pytest.mark.parametrize("read", [
    pytest.param(lambda k: os.environ[k], id="os.environ"),
    pytest.param(lambda k: os.getenv(k), id="os.getenv"),
    pytest.param(lambda k: os.environ.copy()[k], id="copy"),
    pytest.param(lambda k: os.path.expandvars("$" + k), id="expandvars"),
    pytest.param(lambda k: __import__("os").environ[k], id="reimported-os"),
])
def test_every_route_to_the_environment_agrees(read):
    """One split-brain route is a leak: whichever one a task happens to use wins.

    `os.getenv` and `os.path.expandvars` both resolve through the `os.environ`
    MAPPING, so isolation has to be on that object. Shimming the `os` module
    instead would leave each of these reading the unscoped environment.
    """
    with task_scope.scoped(task_scope.TaskScope()):
        os.environ["ROUTE_CHECK"] = "scoped"
        assert read("ROUTE_CHECK") == "scoped"
    assert os.environ.get("ROUTE_CHECK") is None


def test_a_capture_taken_before_the_task_started_is_still_live():
    """`from os import environ` at import time is everywhere in the stdlib.

    It binds the OBJECT, so the fix has to preserve that object's identity.
    """
    from os import environ as captured
    with task_scope.scoped(task_scope.TaskScope()):
        os.environ["CAPTURED_CHECK"] = "scoped"
        assert captured["CAPTURED_CHECK"] == "scoped"
    assert captured.get("CAPTURED_CHECK") is None  # and it left with the task


def test_a_task_write_does_not_reach_the_process_or_a_subprocess():
    with task_scope.scoped(task_scope.TaskScope()):
        os.environ["LEAK_CHECK"] = "from-task"
        # Documented boundary (task_scope.py): a subprocess does NOT inherit
        # the task's env, and passing `env=os.environ` is the exact workaround.
        inherited = subprocess.run(
            ["sh", "-c", "echo ${LEAK_CHECK:-<unset>}"],
            capture_output=True, text=True).stdout.strip()
        explicit = subprocess.run(
            ["sh", "-c", "echo ${LEAK_CHECK:-<unset>}"], env=dict(os.environ),
            capture_output=True, text=True).stdout.strip()
    assert inherited == "<unset>"
    assert explicit == "from-task"
    assert "LEAK_CHECK" not in os.environ


def test_a_task_reads_process_variables_it_did_not_set():
    """The overlay falls through; it does not replace the environment."""
    os.environ["PROCESS_LEVEL"] = "from-process"
    try:
        with task_scope.scoped(task_scope.TaskScope()):
            assert os.environ["PROCESS_LEVEL"] == "from-process"
            assert "PROCESS_LEVEL" in os.environ
    finally:
        del os.environ["PROCESS_LEVEL"]


def test_a_task_deleting_a_process_variable_hides_it_only_from_itself():
    """Otherwise the fix reintroduces its own bug in the opposite direction."""
    os.environ["SHARED_VAR"] = "shared"
    try:
        def task(should_delete):
            def body(sync):
                with task_scope.scoped(task_scope.TaskScope()):
                    if should_delete:
                        del os.environ["SHARED_VAR"]
                    sync()
                    return os.environ.get("SHARED_VAR", "<gone>")
            return body

        got = run_concurrently({"deleter": task(True), "bystander": task(False)})
        assert got["deleter"] == "<gone>"
        assert got["bystander"] == "shared"
        assert os.environ["SHARED_VAR"] == "shared"
    finally:
        os.environ.pop("SHARED_VAR", None)


def test_a_scoped_write_rejects_a_non_string_like_the_real_mapping():
    """A scoped write must not be quietly more permissive than an unscoped one."""
    with task_scope.scoped(task_scope.TaskScope()), pytest.raises(TypeError):
        # Deliberately the wrong type: the assertion IS that this is rejected.
        os.environ["BAD"] = 1  # ty: ignore[invalid-assignment]


def test_env_persists_across_statements_in_one_session():
    scope = task_scope.TaskScope()
    with task_scope.scoped(scope):
        os.environ["SESSION_VAR"] = "set-once"
    with task_scope.scoped(scope):
        assert os.environ["SESSION_VAR"] == "set-once"
    assert "SESSION_VAR" not in os.environ  # the session's, not the agent's


# --- the case the agent hits ------------------------------------------------

def test_cell_identity_survives_an_overlapping_statement():
    """`storage.cell_context` save/restores FABRIC_JOB_ID around a statement.

    Its docstring's goal is that one cell's identity must not be attributed to
    the NEXT statement. Save/restore delivers that sequentially; the agent is a
    ThreadingHTTPServer and runs statements CONCURRENTLY, where the restore put
    the other statement's value back underneath a still-running one. Binding a
    scope outside cell_context makes its writes per-session, so this holds.

    TWO barriers, and the second one is the whole test. With only the first the
    threads serialise: one reads and RESTORES before the other reads, so the
    second sees its own value handed back and the assertion passes against the
    UNFIXED agent -- measured, not supposed. One barrier synchronises the start
    of the read phase, not the reads. The second holds both past their reads,
    so neither can restore under the other.
    """
    def task(job):
        def body(sync):
            with task_scope.scoped(task_scope.TaskScope()):
                prev = os.environ.get("FABRIC_JOB_ID")
                os.environ["FABRIC_JOB_ID"] = job
                try:
                    sync()  # both statements have exported their identity
                    seen = os.environ["FABRIC_JOB_ID"]
                    sync()  # ... and NEITHER has restored yet
                    return seen
                finally:
                    if prev is None:
                        os.environ.pop("FABRIC_JOB_ID", None)
                    else:
                        os.environ["FABRIC_JOB_ID"] = prev
        return body

    got = run_concurrently({"a": task("job-a"), "b": task("job-b")})
    assert got["a"] == "job-a"
    assert got["b"] == "job-b"
    assert "FABRIC_JOB_ID" not in os.environ


# --- stdout -------------------------------------------------------------------
#
# #346: `sys.stdout` is the same class of shared module state as `argv`, and it
# was captured with `contextlib.redirect_stdout` — the save/restore discipline
# this module's docstring rules out. Measured with three concurrent tasks: one
# response carried ANOTHER task's output and two carried nothing.
#
# Same barrier as the argv tests, and for the same reason: the interleaving is
# an input, not a sample. Both statements are inside their capture before
# either prints, which is the order that breaks. A stress loop would pass on a
# broken tree most of the time.

def test_concurrent_statements_each_capture_their_own_output():
    def task(word):
        def body(sync):
            buffer = io.StringIO()
            with task_scope.capturing(buffer):
                sync()  # the other statement is now inside its capture too
                print(word)
                sync()  # ...and has printed, before either reads
            return buffer.getvalue()
        return body

    got = run_concurrently({"a": task("alpha"), "b": task("beta")})
    assert got["a"] == "alpha\n", f"a's response carried {got['a']!r}"
    assert got["b"] == "beta\n", f"b's response carried {got['b']!r}"


def test_a_statement_that_reimports_sys_still_captures_its_own_output():
    """`import sys; sys.stdout.write(...)` walks past any rebound local name,
    the same way the argv test's re-import does."""
    def task(word):
        def body(sync):
            buffer = io.StringIO()
            with task_scope.capturing(buffer):
                sync()
                import sys as reimported

                reimported.stdout.write(word)
                sync()
            return buffer.getvalue()
        return body

    got = run_concurrently({"a": task("alpha"), "b": task("beta")})
    assert got == {"a": "alpha", "b": "beta"}


def test_output_outside_a_capture_goes_to_the_real_stdout(capsys):
    """The agent's own logging must not vanish into whichever statement happens
    to be running, and must not raise when none is."""
    print("agent: a line of its own")
    assert "agent: a line of its own" in capsys.readouterr().out


def test_a_capture_ends_when_the_statement_does():
    buffer = io.StringIO()
    with task_scope.capturing(buffer):
        print("inside")
    assert buffer.getvalue() == "inside\n"
    # And the next print does not land in a buffer nobody is reading.
    assert task_scope._capture.get() is None


def test_print_is_captured_not_only_sys_stdout_write():
    """THE CASE A PROPERTY WOULD HAVE MISSED, which is why the proxy exists.

    `print` reaches stdout through `PySys_GetObject` — a read of the sys module
    DICT — so a property on the module's type is never consulted. Measured
    while building this: with the property approach, `sys.stdout.write` was
    captured and `print` went straight to the real stream. Since `print` is how
    a statement almost always produces output, that version would have captured
    almost nothing while passing a `sys.stdout.write` test.
    """
    buffer = io.StringIO()
    with task_scope.capturing(buffer):
        print("printed")
        sys.stdout.write("written\n")
    assert buffer.getvalue() == "printed\nwritten\n"


def test_a_harness_that_swaps_stdout_does_not_disable_capturing():
    """pytest assigns `sys.stdout` after import, replacing the dict entry. The
    proxy is re-applied per capture so that cannot silently switch capturing
    off — which is how this would rot in the one environment that tests it."""
    replacement = io.StringIO()
    saved = sys.__dict__["stdout"]
    try:
        sys.__dict__["stdout"] = replacement  # the harness swaps it
        buffer = io.StringIO()
        with task_scope.capturing(buffer):
            print("still captured")
        assert buffer.getvalue() == "still captured\n"
        # ...and output outside a capture reaches what the harness installed.
        print("to the harness")
        assert "to the harness" in replacement.getvalue()
    finally:
        sys.__dict__["stdout"] = saved

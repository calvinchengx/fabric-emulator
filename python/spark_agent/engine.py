"""An engine per user, which is the boundary Fabric actually draws.

WHY NOT ONE SHARED ENGINE. Our single long-lived Sail serving every caller is
this emulator's compression, not the product's shape: "in standard mode, each
notebook or pipeline activity starts its own Spark session", and
high-concurrency mode shares one only within a **single-user boundary** -- a
property the docs list under *Security*, with "if any requirement differs,
Fabric starts a separate Spark session". Fabric never shares an engine across
users.

That compression is what leaves the last OneLake security gap open. Sail builds
its Azure store with `MicrosoftAzureBuilder::from_env()`, so its credential is
per process and fixed at start-up -- correct for a process belonging to ONE
user, wrong for one serving everybody. Measured (docs/54): with a shared
engine, a narrowed caller's `spark.read.load("abfss://...")` returns every row,
because the read executes with the engine's identity rather than the caller's.
Give the caller their own engine, holding their own token, and OneLake refuses
it -- the same mechanism Fabric relies on, not a lock we added.

PER USER, NOT PER SESSION. The user context is per Livy session, but the
credential boundary is the principal, so engines are keyed by principal and
shared across that principal's sessions. That is high-concurrency mode's rule,
and it is also what keeps the cost sane: measured at ~66 MiB steady per engine
(`e2e/sail-per-user-footprint`), so the bill scales with identities rather than
notebooks.

EVERY EXTERNAL EFFECT IS INJECTED -- spawning, port selection, readiness,
sleeping -- because a registry that reference-counts processes is exactly the
code that must be unit tested, and none of it needs a real engine to be wrong.
"""
import os
import socket
import subprocess
import time


def free_port(host="127.0.0.1"):
    """A port nothing is listening on, by asking the kernel for one.

    Racy by nature: between closing this socket and the engine binding it,
    another process could take it. Accepted rather than papered over -- the
    alternative is a fixed range that collides deterministically instead of
    rarely, and `start()` surfaces a bind failure either way.
    """
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind((host, 0))
        return s.getsockname()[1]


def accepts(host, port, timeout=1.0):
    try:
        with socket.create_connection((host, port), timeout):
            return True
    except OSError:
        return False


class Engine:
    """One Sail process, holding one principal's credential.

    The token is passed in the ENVIRONMENT because that is the only channel
    Sail has: `MicrosoftAzureBuilder::from_env()` reads it once at start-up and
    there is no per-session or per-read alternative (measured,
    `e2e/sail-per-read-credential`). It is the reason this class exists rather
    than a function that reconfigures a shared server.
    """

    def __init__(self, principal, token, env=None, popen=None, port=None,
                 probe=None, sleep=None, host="127.0.0.1"):
        self.principal = principal
        self.token = token
        self.host = host
        self._env = os.environ if env is None else env
        self._popen = popen or subprocess.Popen
        self._port = port or free_port
        self._probe = probe or accepts
        self._sleep = sleep or time.sleep
        self.port = None
        self.proc = None
        self.sessions = set()

    @property
    def remote(self):
        return f"sc://{self.host}:{self.port}" if self.port else None

    def child_env(self):
        """What the engine process gets: the storage settings, and ONE token.

        Inherited wholesale minus the minting material. An engine that could
        mint would not be scoped to this principal at all -- it would hold a
        service credential and be exactly the shared engine we are replacing,
        with extra steps.
        """
        env = dict(self._env)
        for name in ("ENTRA_TOKEN_URL", "ENTRA_CLIENT_ID", "ENTRA_CLIENT_SECRET"):
            env.pop(name, None)
        env["AZURE_STORAGE_TOKEN"] = self.token
        return env

    def start(self, deadline=30.0):
        self.port = self._port()
        self.proc = self._popen(
            ["sail", "spark", "server", "--ip", self.host, "--port", str(self.port)],
            env=self.child_env())
        waited = 0.0
        while waited < deadline:
            if self._probe(self.host, self.port):
                return self
            if self.proc.poll() is not None:
                raise RuntimeError(
                    f"the engine for {self.principal!r} exited with "
                    f"{self.proc.returncode} before accepting connections")
            self._sleep(0.2)
            waited += 0.2
        self.stop()
        raise RuntimeError(
            f"the engine for {self.principal!r} did not accept connections "
            f"within {deadline:.0f}s")

    def stop(self):
        if self.proc is None:
            return
        try:
            self.proc.terminate()
            self.proc.wait(timeout=10)
        except Exception:  # noqa: BLE001 - an engine that will not go is killed
            try:
                self.proc.kill()
            except Exception:  # noqa: BLE001
                pass
        self.proc, self.port = None, None


class Registry:
    """Engines by principal, reference-counted by the sessions using them.

    REFERENCE COUNTED, because the two lifetimes differ: a principal may hold
    several Livy sessions and the engine must outlive any one of them, but it
    must not outlive the last. Stopping on the first `/close` would take an
    engine away from a live session; never stopping would leak one per identity
    for the life of the agent.
    """

    def __init__(self, build=None):
        self._build = build or (lambda principal, token: Engine(principal, token).start())
        self._engines = {}

    def acquire(self, principal, session, token):
        engine = self._engines.get(principal)
        if engine is None:
            engine = self._build(principal, token)
            self._engines[principal] = engine
        engine.sessions.add(session)
        return engine

    def release(self, session):
        """Drop one session; stop the engine when it was the last.

        Takes the SESSION alone: the caller releasing it is a `/close` handler
        that knows the session id and should not have to remember which
        principal it belonged to — and if those two ever disagreed, the engine
        would be stopped for the wrong user.
        """
        for principal, engine in list(self._engines.items()):
            if session not in engine.sessions:
                continue
            engine.sessions.discard(session)
            if not engine.sessions:
                engine.stop()
                del self._engines[principal]
            return True
        return False

    def stop_all(self):
        for engine in list(self._engines.values()):
            engine.stop()
        self._engines.clear()

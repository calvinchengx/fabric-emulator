#!/usr/bin/env python3
"""The HTTP endpoint the Web and WebHook activities call.

WHY A SERVICE AND NOT THE EMULATOR ITSELF. The first version pointed the Web
activity at the emulator's own `/health`. It failed, correctly:

    x509: certificate signed by unknown authority

The emulator serves a self-signed leaf and the Web activity verifies TLS, as it
should — there is no insecure-skip knob for it and there should not be. Rather
than weaken the activity to make a test pass, the test got a real target over
plain HTTP, which is also closer to what a Web activity does in life: call
something that is not Fabric.

TWO ROUTES, because the claim covers two activities:

  GET  /ping.json   a fixed JSON body, so the Web activity's output can be
                    compared against something it could not have invented from
                    the URL alone.

  POST /hook        the WebHook receiver. Fabric's contract is that the initial
                    call carries a `callBackUri`, the activity then PARKS, and
                    the pipeline only proceeds when something POSTs to that uri.
                    This records the uri and answers 200 WITHOUT calling it, so
                    the parked state is real and observable; the driver fetches
                    it from /captured and does the callback itself. A receiver
                    that called back immediately would make "parked" untestable.

  GET  /captured    the callBackUri the last POST carried, or {} if none yet.

  POST /api/<name>  an Azure Functions host. The emulator sends the key as
                    `x-functions-key` and builds the URL as
                    functionAppUrl + "/api/" + functionName, so a WRONG key must
                    401 — otherwise the suite could not tell an activity that
                    sends the secret from one that forgets it. Records each call
                    as "METHOD /api/<name>" so the driver can assert the URL was
                    built correctly and called exactly once.
"""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_captured: dict[str, str] = {}
_fn_calls: list[str] = []
FUNCTION_KEY = "s3cret"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        return

    def _send(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/ping.json":
            # `pong` is the assertion: nothing about the URL implies it, so an
            # activity that reported success without calling cannot produce it.
            self._send(200, {"pong": True, "who": "pipeline-activities-target"})
        elif path == "/captured":
            self._send(200, dict(_captured))
        elif path == "/fn-calls":
            self._send(200, {"calls": list(_fn_calls)})
        else:
            self._send(404, {"error": f"no route {path}"})

    def do_POST(self):
        path = self.path.split("?", 1)[0]
        if path.startswith("/api/"):
            # A wrong (or absent) key is 401, exactly as a Functions host does.
            # Without this the witness could not distinguish an activity that
            # sends `x-functions-key` from one that silently omits it.
            if self.headers.get("x-functions-key") != FUNCTION_KEY:
                self._send(401, {"error": "invalid key"})
                return
            _fn_calls.append(f"POST {path}")
            self._send(200, {"rows": 3})
            return
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n).decode() if n else ""
        try:
            body = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            body = {}
        uri = body.get("callBackUri") or body.get("callbackUri") or ""
        if uri:
            _captured["callBackUri"] = uri
        _captured["lastBody"] = raw
        self._send(200, {"received": True})


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

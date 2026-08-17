"""Encode one HTTP reply body, never raising.

Split out of agent.py the same way catalog.py and sqlrun.py were, and for the
same reason: agent.py builds and DIALS its SparkSession at import, so anything
left in it cannot be unit-tested. This module imports nothing but json.
"""
import json


def encode_response(code, obj):
    """Return (status, body bytes) for one reply.

    An unencodable value used to raise inside the handler, so the reply was
    never written and the caller saw a bare `RemoteDisconnected` -- a typed
    result set was indistinguishable from a network fault, and the dropped
    socket pointed at OOM and timeouts instead of at the encoder.

    This is a backstop, not the fix: values are normalised where the rows are
    built (sqlrun._jsonable). Blanket-stringifying here would hide the next gap
    the same way this one hid, so the reply names the failure instead.
    """
    try:
        return code, json.dumps(obj).encode()
    except TypeError as exc:
        return 500, json.dumps({
            "status": "error",
            "ename": "ResponseNotSerializable",
            "evalue": str(exc),
            "traceback": [],
        }).encode()

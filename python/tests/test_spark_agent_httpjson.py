"""A reply the agent cannot encode must still be a reply.

THE BUG THIS PINS. `_send` called `json.dumps` directly, so an unencodable
value raised inside the handler and the reply was never written. The caller saw
`RemoteDisconnected: Remote end closed connection without response` and could
not tell a typed result set from a network fault — reported from the field as
first-suspected OOM, then a timeout, before the encoder was suspected at all.

The values themselves are normalised in sqlrun._jsonable; this is the backstop
for whatever that does not cover, and it answers by NAME rather than
stringifying, so the next gap is visible instead of silent.

encode_response lives in httpjson.py because agent.py builds and DIALS its
SparkSession at import — a bogus SPARK_REMOTE hangs the import — so nothing
left in agent.py can be unit-tested. Same split as catalog.py and sqlrun.py.
"""
import datetime
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import httpjson  # noqa: E402


def test_an_encodable_reply_passes_through_with_its_status():
    code, body = httpjson.encode_response(200, {"state": "idle"})
    assert code == 200
    assert json.loads(body) == {"state": "idle"}


def test_a_non_200_status_is_preserved():
    code, body = httpjson.encode_response(404, {"error": "nope"})
    assert code == 404
    assert json.loads(body) == {"error": "nope"}


def test_an_unencodable_reply_becomes_a_500_that_names_the_failure():
    code, body = httpjson.encode_response(200, {"d": datetime.date(2026, 8, 18)})
    assert code == 500
    out = json.loads(body)
    assert out["status"] == "error"
    assert out["ename"] == "ResponseNotSerializable"
    # The reason must survive: a bare 500 would be the dropped socket again,
    # just with a status line.
    assert "date" in out["evalue"]
    assert out["traceback"] == []


def test_the_fallback_is_itself_always_encodable():
    class Opaque:
        def __repr__(self):
            raise RuntimeError("even repr fails")

    # Whatever went wrong, encode_response must return bytes rather than raise:
    # it is the last thing between a handler and a dropped connection.
    code, body = httpjson.encode_response(200, {"x": Opaque()})
    assert code == 500
    assert json.loads(body)["ename"] == "ResponseNotSerializable"


def test_body_is_bytes_so_content_length_is_measurable():
    _, body = httpjson.encode_response(200, {"a": 1})
    assert isinstance(body, bytes)
    assert len(body) > 0

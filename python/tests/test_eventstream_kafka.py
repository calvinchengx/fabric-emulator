"""Eventstream kafka wrap rewrites Fabric options to OSS Kafka, never to rate."""
import base64
import email.message
import io
import json
import os
import sys
import types
from datetime import UTC, datetime, timedelta
from pathlib import Path
from urllib.error import HTTPError

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import eventstream_kafka as ek  # noqa: E402


def test_partial_options_fail_loud():
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.rewrite_load("kafka", {"eventstream.itemid": "abc"})
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.rewrite_load("kafka", {"eventstream.datasourceid": "def"})


def test_non_kafka_and_native_kafka_are_untouched():
    assert ek.rewrite_load("rate", {"eventstream.itemid": "abc",
                                    "eventstream.datasourceid": "def"}) is None
    assert ek.rewrite_load("kafka", {"kafka.bootstrap.servers": "k:9092",
                                     "subscribe": "t"}) is None
    assert ek.should_rewrite("delta", {"eventstream.itemid": "abc"}) is False


def test_kafka_options_are_oss_kafka_not_rate():
    opts = ek.kafka_options("kafka:9092", "item.ds", {
        "eventstream.itemid": "item",
        "failOnDataLoss": "false",
    })
    assert opts["kafka.bootstrap.servers"] == "kafka:9092"
    assert opts["subscribe"] == "item.ds"
    assert opts["startingOffsets"] == "earliest"
    assert opts["failOnDataLoss"] == "false"
    assert "eventstream.itemid" not in opts
    assert "rowsPerSecond" not in opts
    assert set(opts) >= {"kafka.bootstrap.servers", "subscribe", "startingOffsets"}


def test_resolve_unknown_id_fails_loud(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")

    def boom(req, timeout=60, context=None):
        body = json.dumps({
            "errorCode": "EventstreamSourceNotFound",
            "message": "The Eventstream datasource id is not available.",
        }).encode()
        raise HTTPError(req.full_url, 404, "Not Found",
                        hdrs=email.message.Message(), fp=io.BytesIO(body))

    monkeypatch.setattr(ek.urllib.request, "urlopen", boom)
    with pytest.raises(ek.EventstreamError, match=r"not available|404"):
        ek.resolve_source("item", "ds", env={"FABRIC_API_URL": "http://fabric"})


def test_consume_unknown_id_fails_loud(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")

    def boom(req, timeout=60, context=None):
        body = json.dumps({
            "errorCode": "ItemNotFound",
            "message": "The Eventstream item is not available.",
        }).encode()
        raise HTTPError(req.full_url, 404, "Not Found",
                        hdrs=email.message.Message(), fp=io.BytesIO(body))

    monkeypatch.setattr(ek.urllib.request, "urlopen", boom)
    with pytest.raises(ek.EventstreamError, match=r"not available|404"):
        ek.consume_events("item", "ds", env={"FABRIC_API_URL": "http://fabric"})


def test_consume_events_hits_the_events_path(monkeypatch):
    seen = {}

    class FakeResp:
        def read(self):
            return json.dumps({
                "topic": "item.ds",
                "records": [{
                    "key": "a2k=",
                    "value": "eyJuIjoxfQ==",
                    "topic": "item.ds",
                    "partition": 0,
                    "offset": 0,
                    "timestamp": "1970-01-01T00:00:00Z",
                    "timestampType": 0,
                }],
            }).encode()

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def fake_urlopen(req, timeout=60, context=None):
        seen["url"] = req.full_url
        return FakeResp()

    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")
    monkeypatch.setattr(ek.urllib.request, "urlopen", fake_urlopen)
    recs = ek.consume_events("aaaa", "bbbb", env={"FABRIC_API_URL": "http://fabric"})
    assert "/v1/eventstreams/aaaa/sources/bbbb/events" in seen["url"]
    assert "max=100" in seen["url"]
    assert recs[0]["topic"] == "item.ds"


def test_records_to_rows_are_kafka_not_rate():
    rows = ek.records_to_rows([{
        "key": base64.b64encode(b"k0").decode(),
        "value": base64.b64encode(b'{"n":0}').decode(),
        "topic": "item.ds",
        "partition": 0,
        "offset": 3,
        "timestamp": "1970-01-01T00:00:00Z",
        "timestampType": 0,
    }])
    assert len(rows) == 1
    key, value, topic, partition, offset, _ts, ts_type = rows[0]
    assert key == b"k0"
    assert value == b'{"n":0}'
    assert topic == "item.ds"
    assert partition == 0
    assert offset == 3
    assert ts_type == 0
    assert "rate" not in topic


def test_local_foreach_only_for_eventstream_df():
    class Writer:
        _emu_foreach_batch = lambda df, i: None  # noqa: E731

    marked = type("DF", (), {"_emu_eventstream": True})()
    plain = type("DF", (), {})()
    assert ek.should_run_local_foreach(marked, Writer()) is True
    assert ek.should_run_local_foreach(plain, Writer()) is False
    assert ek.should_run_local_foreach(marked, type("W", (), {})()) is False


def test_oneshot_query_is_already_done():
    q = ek.OneShotStreamingQuery(name="clicks")
    assert q.awaitTermination(60) is True
    assert q.isActive is False
    q.stop()
    assert q.isActive is False


def test_rewrite_load_sets_bootstrap_and_subscribe(monkeypatch):
    monkeypatch.setattr(ek, "resolve_source", lambda item, ds, env=None: {
        "bootstrapServers": "kafka:9092",
        "topic": f"{item}.{ds}",
    })
    opts = ek.rewrite_load("kafka", {
        "eventstream.itemid": "aaaa",
        "eventstream.datasourceid": "bbbb",
    })
    assert opts["kafka.bootstrap.servers"] == "kafka:9092"
    assert opts["subscribe"] == "aaaa.bbbb"
    assert "rate" not in str(opts).lower()


def test_fabric_token_uses_fabric_audience(monkeypatch):
    seen = {}

    class FakeResp:
        def read(self):
            return json.dumps({"access_token": "tok", "expires_in": 3600}).encode()

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def fake_urlopen(req, timeout=60, context=None):
        seen["url"] = req.full_url
        seen["body"] = req.data.decode()
        return FakeResp()

    monkeypatch.setattr(ek.urllib.request, "urlopen", fake_urlopen)
    token = ek._fabric_token({
        "ENTRA_TOKEN_URL": "http://entra/token",
        "ENTRA_CLIENT_ID": "c",
        "ENTRA_CLIENT_SECRET": "s",
    })
    assert token == "tok"
    assert "api.fabric.microsoft.com" in seen["body"]


def test_fabric_token_requires_entra_url():
    with pytest.raises(ek.EventstreamError, match="ENTRA_TOKEN_URL"):
        ek._fabric_token({})


def test_resolve_source_requires_both_ids():
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.resolve_source("", "ds")


def test_resolve_source_returns_bootstrap_and_topic(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_request", lambda *a, **k: {
        "bootstrapServers": "kafka:9092",
        "topic": "item.ds",
    })
    src = ek.resolve_source("item", "ds")
    assert src == {"bootstrapServers": "kafka:9092", "topic": "item.ds"}


def test_resolve_source_requires_bootstrap_and_topic(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_request", lambda *a, **k: {"topic": "t"})
    with pytest.raises(ek.EventstreamError, match="no bootstrap"):
        ek.resolve_source("item", "ds")


def test_consume_events_requires_both_ids():
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.consume_events("item", "")


def test_consume_events_requires_records_field(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_request", lambda *a, **k: {"topic": "t"})
    with pytest.raises(ek.EventstreamError, match="no records field"):
        ek.consume_events("item", "ds")


def test_http_error_body_need_not_be_json(monkeypatch):
    monkeypatch.setattr(ek, "_fabric_token", lambda env=None: "tok")

    def boom(req, timeout=60, context=None):
        raise HTTPError(req.full_url, 502, "Bad Gateway",
                        hdrs=email.message.Message(), fp=io.BytesIO(b"not-json"))

    monkeypatch.setattr(ek.urllib.request, "urlopen", boom)
    with pytest.raises(ek.EventstreamError, match=r"not-json|502"):
        ek.resolve_source("item", "ds", env={"FABRIC_API_URL": "http://fabric"})


def test_kafka_options_do_not_overwrite_core_keys():
    opts = ek.kafka_options("kafka:9092", "item.ds", {"subscribe": "other"})
    assert opts["subscribe"] == "item.ds"


def test_eventstream_ids_accept_camel_case():
    assert ek.eventstream_ids({
        "eventstream.itemId": "A",
        "eventstream.datasourceId": "B",
    }) == ("A", "B")


def test_as_bytes_and_timestamp_shapes():
    rows = ek.records_to_rows([
        None,
        {"key": None, "value": b"raw", "topic": "t", "timestamp": None},
        {"key": bytearray(b"k"), "value": "", "topic": "t",
         "timestamp": datetime(1970, 1, 1, tzinfo=UTC)},
        {"key": "abc", "value": 1, "topic": "t", "timestamp": "nope"},
        {"key": b"x", "value": b"y", "topic": "t",
         "timestamp": datetime(1970, 1, 1)},
    ])
    assert rows[0][0] is None
    assert rows[1][1] == b"raw"
    assert rows[2][0] == b"k"
    assert rows[2][1] == b""
    assert rows[2][5].tzinfo is UTC
    assert rows[3][0] == b"abc"
    assert rows[3][1] == bytes(1)
    assert rows[4][5] == datetime(1970, 1, 1)


def test_records_to_rows_empty():
    assert ek.records_to_rows(None) == []
    assert ek.records_to_rows([]) == []


def test_as_timestamp_accepts_kafka_epoch_millis():
    ts = ek._as_timestamp(1_700_000_000_000)
    assert ts.year == 2023
    assert ek._as_timestamp(None) is None
    assert ek._as_timestamp(datetime(2020, 1, 1)) == datetime(2020, 1, 1)


def test_native_kafka_is_not_an_eventstream_rewrite():
    assert ek.should_consume_native("kafka", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    }) is True
    assert ek.should_consume_native("kafka", {
        "eventstream.itemid": "a", "eventstream.datasourceid": "b",
    }) is False
    assert ek.should_consume_native("rate", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    }) is False
    assert ek.should_rewrite("kafka", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    }) is False


def test_consume_plain_kafka_fails_loud_on_incomplete_or_conflicting_options():
    with pytest.raises(ek.KafkaSourceError, match="bootstrap"):
        ek.consume_plain_kafka({"subscribe": "t"})
    with pytest.raises(ek.KafkaSourceError, match="subscribe"):
        ek.consume_plain_kafka({"kafka.bootstrap.servers": "k:9092"})
    with pytest.raises(ek.KafkaSourceError, match="more than one"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "subscribe": "t",
            "subscribePattern": "clicks-.*",
        })
    with pytest.raises(ek.KafkaSourceError, match=r"PLAIN or GSSAPI"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "subscribe": "t",
            "kafka.security.protocol": "SASL_SSL",
            "kafka.sasl.mechanism": "SCRAM-SHA-256",
        })
    with pytest.raises(ek.KafkaSourceError, match="truststore not found"):
        ek.consume_plain_kafka({
            "kafka.bootstrap.servers": "k:9092",
            "subscribe": "t",
            "kafka.security.protocol": "SSL",
            "kafka.ssl.truststore.location": "missing.jks",
        })
    with pytest.raises(ek.KafkaSourceError, match="earliest"):
        ek.kafka_source_spec({
            "kafka.bootstrap.servers": "k:9092",
            "subscribe": "t",
            "endingOffsets": "earliest",
        })


def test_kafka_source_spec_subscribe_pattern_assign_and_json_offsets():
    spec = ek.kafka_source_spec({
        "kafka.bootstrap.servers": "k:9092",
        "subscribePattern": "clicks-.*",
        "startingOffsets": '{"clicks-a":{"0":12}}',
        "endingOffsets": "latest",
        "includeHeaders": "true",
    })
    assert spec["pattern"] == "clicks-.*"
    assert spec["topics"] == []
    assert spec["starting"] == {"clicks-a": {0: 12}}
    assert spec["ending"] == "latest"
    assert spec["include_headers"] is True

    assigned = ek.kafka_source_spec({
        "kafka.bootstrap.servers": "k:9092",
        "assign": '{"t":[0,1]}',
        "startingOffsets": "earliest",
    })
    assert assigned["assign"] == {"t": [0, 1]}
    assert assigned["topics"] == []


def test_sasl_plain_from_jaas_and_username():
    jaas = (
        'org.apache.kafka.common.security.plain.PlainLoginModule required '
        'username="alice" password="s3cret";'
    )
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SASL_PLAINTEXT",
        "kafka.sasl.jaas.config": jaas,
    })
    assert kw["sasl_plain_username"] == "alice"
    assert kw["sasl_plain_password"] == "s3cret"
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SASL_SSL",
        "kafka.sasl.username": "bob",
        "kafka.sasl.password": "pw",
        "kafka.ssl.truststore.location": "/tmp/ca.pem",
    })
    assert kw["ssl_cafile"] == "/tmp/ca.pem"
    assert kw["sasl_plain_username"] == "bob"


def _self_signed_cert():
    from cryptography import x509
    from cryptography.hazmat.primitives import hashes
    from cryptography.hazmat.primitives.asymmetric import rsa
    from cryptography.x509.oid import NameOID

    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "emu-kafka-ca")])
    now = datetime.now(UTC)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(1)
        .not_valid_before(now)
        .not_valid_after(now + timedelta(days=1))
        .sign(key, hashes.SHA256())
    )
    return key, cert


def test_gssapi_from_jaas_sets_keytab_and_service(monkeypatch):
    monkeypatch.delenv("KRB5_CLIENT_KTNAME", raising=False)
    jaas = (
        'com.sun.security.auth.module.Krb5LoginModule required '
        'useKeyTab=true storeKey=true '
        'keyTab="/var/keytabs/kafka.keytab" '
        'principal="alice@EXAMPLE.COM";'
    )
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SASL_PLAINTEXT",
        "kafka.sasl.mechanism": "GSSAPI",
        "kafka.sasl.jaas.config": jaas,
        "kafka.sasl.kerberos.service.name": "kafka",
    })
    assert kw["sasl_mechanism"] == "GSSAPI"
    assert kw["sasl_kerberos_service_name"] == "kafka"
    assert kw["sasl_kerberos_name"] == "alice@EXAMPLE.COM"
    assert os.environ["KRB5_CLIENT_KTNAME"] == "/var/keytabs/kafka.keytab"
    inferred = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SASL_PLAINTEXT",
        "kafka.sasl.jaas.config": jaas,
    })
    assert inferred["sasl_mechanism"] == "GSSAPI"
    ticket = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SASL_PLAINTEXT",
        "kafka.sasl.mechanism": "GSSAPI",
    })
    assert ticket["sasl_mechanism"] == "GSSAPI"
    assert ticket["sasl_kerberos_service_name"] == "kafka"


def test_jks_and_pkcs12_truststores_become_pem(tmp_path):
    import jks
    from cryptography.hazmat.primitives.serialization import (
        BestAvailableEncryption,
        Encoding,
        pkcs12,
    )

    _key, cert = _self_signed_cert()
    der = cert.public_bytes(Encoding.DER)
    jks_path = tmp_path / "trust.jks"
    entry = jks.TrustedCertEntry.new("ca", der)
    jks_path.write_bytes(jks.KeyStore.new("jks", [entry]).saves("changeit"))
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(jks_path),
        "kafka.ssl.truststore.password": "changeit",
    })
    ca = Path(kw["ssl_cafile"]).read_text()
    assert "BEGIN CERTIFICATE" in ca

    p12_path = tmp_path / "trust.p12"
    p12_path.write_bytes(pkcs12.serialize_key_and_certificates(
        b"ca", None, cert, None, BestAvailableEncryption(b"secret"),
    ))
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(p12_path),
        "kafka.ssl.truststore.password": "secret",
    })
    assert "BEGIN CERTIFICATE" in Path(kw["ssl_cafile"]).read_text()

    pfx_path = tmp_path / "trust.pfx"
    pfx_path.write_bytes(p12_path.read_bytes())
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(pfx_path),
        "kafka.ssl.truststore.password": "secret",
        "kafka.ssl.truststore.type": "PKCS12",
    })
    assert Path(kw["ssl_cafile"]).read_text().count("BEGIN CERTIFICATE") >= 1


def test_jks_keystore_becomes_client_pem(tmp_path):
    import jks
    from cryptography.hazmat.primitives.serialization import Encoding, NoEncryption, PrivateFormat

    key, cert = _self_signed_cert()
    key_der = key.private_bytes(Encoding.DER, PrivateFormat.PKCS8, NoEncryption())
    cert_der = cert.public_bytes(Encoding.DER)
    dumped = jks.PrivateKeyEntry.new("client", [cert_der], key_der)
    store = tmp_path / "client.jks"
    store.write_bytes(jks.KeyStore.new("jks", [dumped]).saves("changeit"))
    ca = tmp_path / "ca.pem"
    ca.write_text("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(ca),
        "kafka.ssl.keystore.location": str(store),
        "kafka.ssl.keystore.password": "changeit",
    })
    assert "BEGIN CERTIFICATE" in Path(kw["ssl_certfile"]).read_text()
    assert "BEGIN" in Path(kw["ssl_keyfile"]).read_text() and "PRIVATE KEY" in Path(kw["ssl_keyfile"]).read_text()
    # Same bytes as a truststore: CA material comes from the private-key chain.
    ca_from_key = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(store),
        "kafka.ssl.truststore.password": "changeit",
        "kafka.ssl.truststore.type": "JKS",
    })
    assert "BEGIN CERTIFICATE" in Path(ca_from_key["ssl_cafile"]).read_text()


def test_pkcs12_keystore_becomes_client_pem(tmp_path):
    from cryptography.hazmat.primitives.serialization import (
        BestAvailableEncryption,
        pkcs12,
    )

    key, cert = _self_signed_cert()
    extra_key, extra = _self_signed_cert()
    del extra_key
    store = tmp_path / "client.p12"
    store.write_bytes(pkcs12.serialize_key_and_certificates(
        b"client", key, cert, [extra], BestAvailableEncryption(b"secret"),
    ))
    ca = tmp_path / "ca.pem"
    ca.write_text("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(ca),
        "kafka.ssl.keystore.location": str(store),
        "kafka.ssl.keystore.password": "secret",
    })
    assert Path(kw["ssl_certfile"]).read_text().count("BEGIN CERTIFICATE") >= 2
    assert "PRIVATE KEY" in Path(kw["ssl_keyfile"]).read_text()
    as_ca = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(store),
        "kafka.ssl.truststore.password": "secret",
    })
    assert Path(as_ca["ssl_cafile"]).read_text().count("BEGIN CERTIFICATE") >= 2

    bag = tmp_path / "trust-extra.p12"
    bag.write_bytes(pkcs12.serialize_key_and_certificates(
        b"ca", None, cert, [extra], BestAvailableEncryption(b"secret"),
    ))
    ca_kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": str(bag),
        "kafka.ssl.truststore.password": "secret",
    })
    assert Path(ca_kw["ssl_cafile"]).read_text().count("BEGIN CERTIFICATE") >= 2


def test_java_store_types_and_missing_keystore(tmp_path):
    ca = tmp_path / "ca.pem"
    ca.write_text("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
    assert ek._is_java_store("x.pem", "JKS") is True
    assert ek._is_java_store("x.pem", "PEM") is False
    assert ek._is_java_store("x.jks", "") is True
    pem, key = ek._materialize_client(str(ca), "")
    assert pem == str(ca) and key == ""
    with pytest.raises(ek.KafkaSourceError, match="keystore not found"):
        ek.kafka_client_kwargs({
            "kafka.security.protocol": "SSL",
            "kafka.ssl.truststore.location": str(ca),
            "kafka.ssl.keystore.location": str(tmp_path / "missing.jks"),
        })


def test_jks_needs_pyjks(monkeypatch):
    monkeypatch.setitem(sys.modules, "jks", None)
    with pytest.raises(ek.KafkaSourceError, match="pyjks"):
        ek._load_jks("x.jks", "pw")


def test_pkcs12_needs_cryptography(monkeypatch):
    monkeypatch.setitem(sys.modules, "cryptography.hazmat.primitives.serialization", None)
    with pytest.raises(ek.KafkaSourceError, match="cryptography"):
        ek._load_p12("x.p12", "pw")


def test_jks_and_pkcs12_fail_loud_on_bad_or_empty_stores(tmp_path, monkeypatch):
    junk = tmp_path / "bad.jks"
    junk.write_bytes(b"not-a-keystore")
    with pytest.raises(ek.KafkaSourceError, match="could not open JKS"):
        ek._load_jks(str(junk), "pw")
    p12 = tmp_path / "bad.p12"
    p12.write_bytes(b"not-pkcs12")
    with pytest.raises(ek.KafkaSourceError, match="could not open PKCS12"):
        ek._load_p12(str(p12), "pw")

    monkeypatch.setattr(
        ek, "_load_jks",
        lambda *a, **k: types.SimpleNamespace(certs={}, private_keys={}),
    )
    with pytest.raises(ek.KafkaSourceError, match="contains no certificates"):
        ek._jks_ca_pem("empty.jks", "")
    with pytest.raises(ek.KafkaSourceError, match="no private key"):
        ek._jks_client_pems("empty.jks", "")
    pk = types.SimpleNamespace(cert_chain=[], pkey=b"x")
    monkeypatch.setattr(
        ek, "_load_jks",
        lambda *a, **k: types.SimpleNamespace(private_keys={"k": pk}),
    )
    with pytest.raises(ek.KafkaSourceError, match="no certificate chain"):
        ek._jks_client_pems("nochains.jks", "")

    chain = types.SimpleNamespace(cert_chain=[b"raw", (b"type", b"tup")])
    ders = ek._jks_cert_ders(types.SimpleNamespace(
        certs=None, private_keys={"k": chain},
    ))
    assert ders == [b"raw", b"tup"]

    monkeypatch.setattr(ek, "_load_p12", lambda *a, **k: (None, None, None))
    with pytest.raises(ek.KafkaSourceError, match="contains no certificates"):
        ek._p12_ca_pem("empty.p12", "")
    with pytest.raises(ek.KafkaSourceError, match="private key and cert"):
        ek._p12_client_pems("empty.p12", "")


def test_consume_plain_kafka_passes_bytes_through_poll():
    seen = {}

    def poll(spec):
        seen["spec"] = spec
        return [{"key": b"k", "value": b"hello", "topic": spec["topics"][0]}]

    recs = ek.consume_plain_kafka({
        "kafka.bootstrap.servers": "kafka:9092",
        "subscribe": "plain, other",
        "startingOffsets": "earliest",
        "maxOffsetsPerTrigger": "7",
        "kafkaConsumer.pollTimeoutMs": "1234",
    }, poll=poll)
    assert seen["spec"]["bootstrap"] == "kafka:9092"
    assert seen["spec"]["topics"] == ["plain", "other"]
    assert seen["spec"]["starting"] == "earliest"
    assert seen["spec"]["timeout_ms"] == 1234
    assert seen["spec"]["max_records"] == 7
    assert recs[0]["value"] == b"hello"
    rows = ek.records_to_rows(recs)
    assert rows[0][0] == b"k"
    assert rows[0][1] == b"hello"
    assert rows[0][2] == "plain"


def test_records_to_rows_include_headers():
    rows = ek.records_to_rows([{
        "key": b"k", "value": b"v", "topic": "t",
        "headers": [("h", b"1")],
    }], include_headers=True)
    assert rows[0][-1] == [("h", b"1")]


def test_produce_plain_kafka_sends_key_value_topic():
    sent = []

    def send(bootstrap, topic, records, client):
        sent.append((bootstrap, topic, records, client))
        return len(records)

    n = ek.produce_plain_kafka(
        [{"key": b"k", "value": b"v", "topic": "t"}],
        {"kafka.bootstrap.servers": "k:9092", "topic": "fallback"},
        send=send,
    )
    assert n == 1
    assert sent[0][0] == "k:9092"
    assert sent[0][1] == "fallback"
    assert sent[0][2][0]["value"] == b"v"
    with pytest.raises(ek.KafkaSourceError, match="bootstrap"):
        ek.produce_plain_kafka([{}], {"topic": "t"})


def test_kafka_poll_maps_consumer_bytes(monkeypatch):
    class Msg:
        key = b"k"
        value = b"hello-engine-matrix"
        topic = "t"
        partition = 0
        offset = 3
        timestamp = 1_700_000_000_000
        timestamp_type = 0

    assigned = {}

    class Consumer:
        def __init__(self, **kw):
            pass

        def partitions_for_topic(self, topic):
            return {0}

        def assign(self, tps):
            assigned["tps"] = tps

        def seek_to_beginning(self, *tps):
            assigned["seek"] = "beginning"

        def seek_to_end(self, *tps):
            assigned["seek"] = "end"

        def __iter__(self):
            return iter([Msg()])

        def close(self):
            assigned["closed"] = True

    class TP:
        def __init__(self, topic, p):
            self.topic, self.partition = topic, p

    kafka_mod = types.ModuleType("kafka")
    kafka_mod.KafkaConsumer = Consumer
    kafka_mod.TopicPartition = TP
    monkeypatch.setitem(sys.modules, "kafka", kafka_mod)
    spec = {
        "bootstrap": "k:9092",
        "topics": ["t"],
        "pattern": "",
        "assign": None,
        "starting": "earliest",
        "ending": None,
        "timeout_ms": 500,
        "max_records": 10,
        "include_headers": False,
        "client": {},
    }
    rows = ek._kafka_poll(spec)
    assert rows[0]["value"] == b"hello-engine-matrix"
    assert assigned["seek"] == "beginning" and assigned["closed"] is True
    spec["starting"] = "latest"
    spec["ending"] = "latest"
    rows = ek._kafka_poll(spec)
    assert rows == []
    assert assigned["seek"] == "end"


def _poll_spec(**overrides):
    spec = {
        "bootstrap": "k:9092",
        "topics": ["t"],
        "pattern": "",
        "assign": None,
        "starting": "earliest",
        "ending": None,
        "timeout_ms": 50,
        "max_records": 10,
        "include_headers": False,
        "client": {},
    }
    spec.update(overrides)
    return spec


def _install_fake_kafka(monkeypatch, *, messages=None, topics=None,
                        partitions=None, begin=None, end=None):
    """kafka-python stand-in for poll + produce unit tests."""
    state = {
        "init": {},
        "assigned": None,
        "seeks": [],
        "closed": False,
        "sent": [],
        "flushed": False,
        "producer_init": {},
    }

    class Msg:
        def __init__(self, **kw):
            self.key = kw.get("key")
            self.value = kw.get("value", b"v")
            self.topic = kw.get("topic", "t")
            self.partition = kw.get("partition", 0)
            self.offset = kw.get("offset", 0)
            self.timestamp = kw.get("timestamp", 0)
            self.timestamp_type = kw.get("timestamp_type", 0)
            self.headers = kw.get("headers")

    class TP:
        def __init__(self, topic, p):
            self.topic, self.partition = topic, p

        def __hash__(self):
            return hash((self.topic, self.partition))

        def __eq__(self, other):
            return self.topic == other.topic and self.partition == other.partition

    class Consumer:
        def __init__(self, **kw):
            state["init"] = kw

        def topics(self):
            return list(topics or [])

        def partitions_for_topic(self, topic):
            if partitions is None:
                return {0}
            return partitions.get(topic)

        def assign(self, tps):
            state["assigned"] = tps

        def seek_to_beginning(self, *tps):
            state["seeks"].append(("beginning", tps))

        def seek_to_end(self, *tps):
            state["seeks"].append(("end", tps))

        def beginning_offsets(self, tps):
            table = begin or {}
            return {tp: table.get((tp.topic, tp.partition), 0) for tp in tps}

        def end_offsets(self, tps):
            table = end or {}
            return {tp: table.get((tp.topic, tp.partition), 10) for tp in tps}

        def seek(self, tp, off):
            state["seeks"].append(("seek", tp.topic, tp.partition, off))

        def __iter__(self):
            out = []
            for m in messages or []:
                out.append(m if isinstance(m, Msg) else Msg(**m))
            return iter(out)

        def close(self):
            state["closed"] = True

    class Producer:
        def __init__(self, **kw):
            state["producer_init"] = kw

        def send(self, topic, value=None, key=None, **kw):
            state["sent"].append({"topic": topic, "value": value, "key": key, **kw})

        def flush(self):
            state["flushed"] = True

        def close(self):
            state["closed"] = True

    kafka_mod = types.ModuleType("kafka")
    kafka_mod.KafkaConsumer = Consumer
    kafka_mod.KafkaProducer = Producer
    kafka_mod.TopicPartition = TP
    monkeypatch.setitem(sys.modules, "kafka", kafka_mod)
    return state, Msg, TP


def test_parse_offset_option_json_sentinels_and_errors():
    assert ek._parse_offset_option("", "startingOffsets") is None
    assert ek._parse_offset_option("earliest", "startingOffsets") == "earliest"
    assert ek._parse_offset_option("latest", "endingOffsets") == "latest"
    parsed = ek._parse_offset_option('{"t":{"0":-2,"1":-1,"2":9}}', "startingOffsets")
    assert parsed == {"t": {0: -2, 1: -1, 2: 9}}
    with pytest.raises(ek.KafkaSourceError, match="not earliest"):
        ek._parse_offset_option("{", "startingOffsets")
    with pytest.raises(ek.KafkaSourceError, match="must be an object"):
        ek._parse_offset_option("[1]", "startingOffsets")
    with pytest.raises(ek.KafkaSourceError, match="partition"):
        ek._parse_offset_option('{"t":[0]}', "startingOffsets")


def test_kafka_source_spec_rejects_bad_assign_and_offset_json():
    with pytest.raises(ek.KafkaSourceError, match="not JSON"):
        ek.kafka_source_spec({
            "kafka.bootstrap.servers": "k:9092", "assign": "{",
        })
    with pytest.raises(ek.KafkaSourceError, match="topic"):
        ek.kafka_source_spec({
            "kafka.bootstrap.servers": "k:9092", "assign": "[0]",
        })
    with pytest.raises(ek.KafkaSourceError, match="more than one"):
        ek.kafka_source_spec({
            "kafka.bootstrap.servers": "k:9092",
            "subscribePattern": "t-.*",
            "assign": '{"t":[0]}',
        })
    spec = ek.kafka_source_spec({
        "kafka.bootstrap.servers": "k:9092",
        "subscribe": "t",
    })
    assert spec["timeout_ms"] == 8000
    assert spec["max_records"] == 10000
    assert spec["starting"] == "earliest"
    assert spec["ending"] is None
    assert spec["client"] == {}


def test_kafka_client_kwargs_plaintext_ssl_and_fail_loud():
    assert ek.kafka_client_kwargs({}) == {}
    assert ek.kafka_client_kwargs({"kafka.security.protocol": "PLAINTEXT"}) == {}
    with pytest.raises(ek.KafkaSourceError, match="not in this wrap"):
        ek.kafka_client_kwargs({"kafka.security.protocol": "SASL_KERBEROS"})
    with pytest.raises(ek.KafkaSourceError, match=r"jaas.config or"):
        ek.kafka_client_kwargs({"kafka.security.protocol": "SASL_PLAINTEXT"})
    with pytest.raises(ek.KafkaSourceError, match=r"PEM CA|truststore"):
        ek.kafka_client_kwargs({"kafka.security.protocol": "SSL"})
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SSL",
        "kafka.ssl.truststore.location": "/tmp/ca.pem",
        "kafka.ssl.keystore.location": "/tmp/client.pem",
        "kafka.ssl.key.location": "/tmp/client.key",
    })
    assert kw == {
        "security_protocol": "SSL",
        "ssl_cafile": "/tmp/ca.pem",
        "ssl_certfile": "/tmp/client.pem",
        "ssl_keyfile": "/tmp/client.key",
    }
    jaas = (
        'org.apache.kafka.common.security.plain.PlainLoginModule required '
        'username="from-jaas" password="p";'
    )
    kw = ek.kafka_client_kwargs({
        "kafka.security.protocol": "SASL_PLAINTEXT",
        "kafka.sasl.username": "ignored",
        "kafka.sasl.password": "ignored",
        "kafka.sasl.jaas.config": jaas,
    })
    assert kw["sasl_plain_username"] == "from-jaas"


def test_parse_jaas_plain_requires_both_fields():
    with pytest.raises(ek.KafkaSourceError, match="username="):
        ek.parse_jaas_plain('username="only"')
    with pytest.raises(ek.KafkaSourceError, match="username="):
        ek.parse_jaas_plain("")


def test_should_produce_and_collect_sink_rows():
    assert ek.should_produce("kafka", {"kafka.bootstrap.servers": "k:9092"}) is True
    assert ek.should_produce("kafka", {}) is False
    assert ek.should_produce("delta", {"kafka.bootstrap.servers": "k:9092"}) is False
    with pytest.raises(ek.KafkaSourceError, match="no DataFrame"):
        ek.collect_kafka_sink_rows(None)

    class Row:
        def asDict(self, recursive=True):
            return {"value": b"from-row"}

    df = types.SimpleNamespace(collect=lambda: [Row(), {"value": b"from-dict"}])
    assert ek.collect_kafka_sink_rows(df) == [
        {"value": b"from-row"}, {"value": b"from-dict"},
    ]
    assert ek._row_dict([("value", b"x")]) == {"value": b"x"}

    proto = types.SimpleNamespace(format="kafka", options={"topic": "from-proto"})
    writer = types.SimpleNamespace(
        _write_proto=proto, _options={"a": "1"}, _format="parquet",
    )
    fmt, held = ek._writer_fmt_opts(writer, options={"b": "2"})
    assert fmt == "kafka"
    assert held["topic"] == "from-proto"
    assert held["a"] == "1" and held["b"] == "2"


def test_kafka_helpers_resolve_seek_headers_and_end():
    class C:
        def topics(self):
            return ["clicks-a", "other"]

        def partitions_for_topic(self, topic):
            return None if topic == "missing" else {0, 1}

    class TP:
        def __init__(self, topic, p):
            self.topic, self.partition = topic, p

    assert ek._resolve_topics(C(), {"assign": {"t": [0]}}) == ["t"]
    assert ek._resolve_topics(C(), {"topics": ["a", "b"]}) == ["a", "b"]
    assert ek._resolve_topics(C(), {
        "pattern": "clicks-.*", "timeout_ms": 50, "bootstrap": "k",
    }) == ["clicks-a"]
    with pytest.raises(ek.KafkaSourceError, match="not a regex"):
        ek._resolve_topics(C(), {
            "pattern": "[", "timeout_ms": 1, "bootstrap": "k",
        })
    with pytest.raises(ek.KafkaSourceError, match="matched no topics"):
        ek._resolve_topics(C(), {
            "pattern": "zzz", "timeout_ms": 1, "bootstrap": "k:9092",
        })
    tps = ek._partitions_for(C(), TP, {"assign": {"t": [0, 2]}}, [])
    assert [(tp.topic, tp.partition) for tp in tps] == [("t", 0), ("t", 2)]
    tps = ek._partitions_for(C(), TP, {"timeout_ms": 50, "bootstrap": "k"}, ["ok"])
    assert [(tp.topic, tp.partition) for tp in tps] == [("ok", 0), ("ok", 1)]
    with pytest.raises(ek.KafkaSourceError, match="metadata not found"):
        ek._partitions_for(
            C(), TP, {"timeout_ms": 1, "bootstrap": "k:9092"}, ["missing"],
        )

    tp = TP("t", 0)
    assert ek._offset_for("latest", tp, 9) == 9
    assert ek._offset_for({"t": {0: 4}}, tp, -2) == 4
    assert ek._offset_for({"t": {"0": 5}}, tp, -2) == 5
    assert ek._offset_for({"t": {1: 1}}, tp, -2) == -2

    class Seeker:
        def __init__(self):
            self.ops = []

        def seek_to_end(self, *tps):
            self.ops.append(("end", tps))

        def seek_to_beginning(self, *tps):
            self.ops.append(("begin", tps))

        def beginning_offsets(self, tps):
            return {tps[0]: 0}

        def end_offsets(self, tps):
            return {tps[0]: 99}

        def seek(self, tp, off):
            self.ops.append(("seek", off))

    s = Seeker()
    ek._seek_start(s, [tp], "latest")
    ek._seek_start(s, [tp], "earliest")
    ek._seek_start(s, [tp], None)
    ek._seek_start(s, [tp], {"t": {0: -2}})
    ek._seek_start(s, [tp], {"t": {0: -1}})
    ek._seek_start(s, [tp], {"t": {0: 7}})
    assert s.ops[0][0] == "end"
    assert s.ops[1][0] == "begin" and s.ops[2][0] == "begin"
    assert ("seek", 0) in s.ops and ("seek", 99) in s.ops and ("seek", 7) in s.ops

    assert ek._past_end(None, tp, 0) is False
    assert ek._past_end("latest", tp, 0) is False
    assert ek._past_end({"t": {0: -1}}, tp, 9) is False
    assert ek._past_end({"t": {0: 5}}, tp, 4) is False
    assert ek._past_end({"t": {0: 5}}, tp, 5) is True

    assert ek._msg_headers(types.SimpleNamespace(headers=None)) == []
    assert ek._msg_headers(types.SimpleNamespace(headers=[
        ("h", b"1"), {"key": "k", "value": b"2"}, "skip",
    ])) == [("h", b"1"), ("k", b"2")]


def test_kafka_poll_assign_pattern_json_offsets_headers(monkeypatch):
    state, _, _ = _install_fake_kafka(
        monkeypatch,
        messages=[{
            "topic": "clicks-a", "value": b"a", "offset": 4,
            "headers": [("h", b"1")],
        }, {
            "topic": "clicks-a", "value": b"b", "offset": 5,
        }],
        topics=["clicks-a"],
        begin={("clicks-a", 0): 0},
        end={("clicks-a", 0): 9},
    )
    rows = ek._kafka_poll(_poll_spec(
        topics=[],
        pattern="clicks-.*",
        starting={"clicks-a": {0: 4}},
        ending={"clicks-a": {0: 5}},
        include_headers=True,
        client={"sasl_mechanism": "PLAIN"},
    ))
    assert state["init"]["sasl_mechanism"] == "PLAIN"
    assert state["closed"] is True
    assert [r["value"] for r in rows] == [b"a"]
    assert rows[0]["headers"] == [("h", b"1")]
    assert ("seek", "clicks-a", 0, 4) in state["seeks"]

    state, _, _ = _install_fake_kafka(
        monkeypatch, messages=[{"value": b"x", "topic": "t", "partition": 1}],
    )
    rows = ek._kafka_poll(_poll_spec(topics=[], assign={"t": [1]}))
    assert len(state["assigned"]) == 1
    assert state["assigned"][0].partition == 1
    assert rows[0]["value"] == b"x"


def test_kafka_poll_caps_records_empty_assign_and_import_error(monkeypatch):
    state, _, _ = _install_fake_kafka(
        monkeypatch,
        messages=[{"value": b"1"}, {"value": b"2"}, {"value": b"3"}],
    )
    rows = ek._kafka_poll(_poll_spec(max_records=2))
    assert [r["value"] for r in rows] == [b"1", b"2"]

    state, _, _ = _install_fake_kafka(monkeypatch, messages=[{"value": b"x"}])
    assert ek._kafka_poll(_poll_spec(topics=[], assign={"t": []})) == []
    assert state["closed"] is True

    monkeypatch.setitem(sys.modules, "kafka", None)
    with pytest.raises(ek.KafkaSourceError, match="kafka-python"):
        ek._kafka_poll(_poll_spec())


def test_kafka_send_produces_headers_and_requires_topic(monkeypatch):
    state, _, _ = _install_fake_kafka(monkeypatch)
    n = ek._kafka_send("k:9092, k2:9092", "fallback", [
        {"key": b"k", "value": b"v", "headers": [("h", b"1")]},
        {"value": b"w", "topic": "other", "headers": [{"key": "x", "value": b"y"}]},
    ], {"security_protocol": "SSL"})
    assert n == 2
    assert state["producer_init"]["bootstrap_servers"] == ["k:9092", "k2:9092"]
    assert state["producer_init"]["security_protocol"] == "SSL"
    assert state["sent"][0]["topic"] == "fallback"
    assert state["sent"][0]["headers"] == [("h", b"1")]
    assert state["sent"][1]["topic"] == "other"
    assert state["flushed"] and state["closed"]
    with pytest.raises(ek.KafkaSourceError, match="topic column"):
        ek._kafka_send("k:9092", "", [{"value": b"v"}], {})
    monkeypatch.setitem(sys.modules, "kafka", None)
    with pytest.raises(ek.KafkaSourceError, match="kafka-python"):
        ek._kafka_send("k:9092", "t", [{"value": b"v"}], {})


def test_records_to_rows_header_dicts():
    rows = ek.records_to_rows([{
        "value": b"v", "topic": "t",
        "headers": [{"key": "h", "value": "YQ=="}, ("z", b"2")],
    }], include_headers=True)
    assert rows[0][-1] == [("h", b"a"), ("z", b"2")]


def test_connect_kafka_df_eventstream_and_headers(monkeypatch, capsys):
    monkeypatch.setattr(
        ek, "consume_events",
        lambda *a, **k: [{"value": b"es", "topic": "item.ds"}],
    )
    monkeypatch.setattr(
        ek, "consume_plain_kafka",
        lambda opts: [{"value": b"n", "topic": "t", "headers": [("h", b"1")]}],
    )
    monkeypatch.setattr(ek, "kafka_schema", lambda include_headers=False: include_headers)
    created = []

    class Spark:
        def createDataFrame(self, rows, schema):
            created.append((rows, schema))
            return types.SimpleNamespace(rows=rows, schema=schema)

    spark = Spark()
    df = ek.connect_kafka_df(spark, "kafka", {
        "eventstream.itemid": "a", "eventstream.datasourceid": "b",
    })
    assert df._emu_eventstream is True
    assert created[0][1] is False
    assert "emulator consume" in capsys.readouterr().err
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        ek.connect_kafka_df(spark, "kafka", {"eventstream.itemid": "only"})
    df = ek.connect_kafka_df(spark, "kafka", {
        "kafka.bootstrap.servers": "k:9092",
        "subscribe": "t",
        "includeHeaders": "true",
    })
    assert created[-1][1] is True
    assert created[-1][0][0][-1] == [("h", b"1")]


def test_should_consume_native_assign_and_pattern():
    assert ek.should_consume_native("kafka", {
        "kafka.bootstrap.servers": "k:9092", "assign": '{"t":[0]}',
    }) is True
    assert ek.should_consume_native("kafka", {
        "subscribePattern": "t-.*",
    }) is True


def test_connect_kafka_df_native_uses_the_spark_session(monkeypatch, capsys):
    """Bytes go through spark.createDataFrame — that is the Sail LocalRelation."""
    monkeypatch.setattr(
        ek, "consume_plain_kafka",
        lambda opts: [{"key": b"k", "value": b"payload", "topic": "t"}],
    )
    monkeypatch.setattr(ek, "kafka_schema", lambda include_headers=False: "kafka-schema")
    created = []

    class Spark:
        def createDataFrame(self, rows, schema):
            created.append((rows, schema))
            return types.SimpleNamespace(rows=rows, schema=schema)

    spark = Spark()
    df = ek.connect_kafka_df(spark, "kafka", {
        "kafka.bootstrap.servers": "k:9092", "subscribe": "t",
    })
    assert created and created[0][1] == "kafka-schema"
    assert created[0][0][0][1] == b"payload"
    assert df._emu_eventstream is True
    assert "driver consume" in capsys.readouterr().err
    assert ek.connect_kafka_df(spark, "rate", {}) is None


def test_oneshot_query_explain_and_process(capsys):
    q = ek.OneShotStreamingQuery()
    q.processAllAvailable()
    q.explain()
    assert ek.FOREACH_ANNOUNCE in capsys.readouterr().out


def test_kafka_schema_is_kafka_not_rate(monkeypatch):
    types_mod = types.ModuleType("pyspark.sql.types")

    class Field:
        def __init__(self, name, typ, nullable):
            self.name, self.dataType, self.nullable = name, typ, nullable

    class Struct:
        def __init__(self, fields):
            self.fields = fields

    types_mod.BinaryType = lambda: "binary"
    types_mod.IntegerType = lambda: "int"
    types_mod.LongType = lambda: "long"
    types_mod.StringType = lambda: "string"
    types_mod.TimestampType = lambda: "timestamp"
    types_mod.StructField = Field
    types_mod.StructType = Struct
    types_mod.ArrayType = lambda inner: ("array", inner)
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.types", types_mod)
    schema = ek.kafka_schema()
    names = [f.name for f in schema.fields]
    assert names == ["key", "value", "topic", "partition", "offset",
                     "timestamp", "timestampType"]
    assert "rate" not in names
    headed = ek.kafka_schema(include_headers=True)
    assert [f.name for f in headed.fields][-1] == "headers"


def test_materialize_marks_the_dataframe(monkeypatch):
    monkeypatch.setattr(ek, "kafka_schema", lambda include_headers=False: "schema")

    class Spark:
        def createDataFrame(self, rows, schema):
            return types.SimpleNamespace(rows=rows, schema=schema)

    df = ek.materialize_kafka_df(Spark(), [{"value": b"x", "topic": "t"}])
    assert df._emu_eventstream is True
    assert df.schema == "schema"


@pytest.fixture
def reset_install():
    ek._classic_installed = False
    ek._connect_installed = False
    yield
    ek._classic_installed = False
    ek._connect_installed = False


def test_classic_install_skips_when_remote(monkeypatch, reset_install):
    monkeypatch.setenv("SPARK_REMOTE", "sc://sail:50051")
    assert ek._install_classic() is False


def test_classic_install_rewrites_eventstream_options(monkeypatch, reset_install):
    monkeypatch.delenv("SPARK_REMOTE", raising=False)
    applied = []

    class DataStreamReader:
        def format(self, source):
            return self

        def option(self, key, value):
            applied.append((key, value))
            return self

        def options(self, **kwargs):
            return self

        def load(self, path=None, format=None, schema=None, **kwargs):
            return "df"

    streaming = types.ModuleType("pyspark.sql.streaming")
    streaming.DataStreamReader = DataStreamReader
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.streaming", streaming)
    monkeypatch.setattr(ek, "rewrite_load", lambda fmt, opts: {
        "kafka.bootstrap.servers": "kafka:9092",
        "subscribe": "t",
        "startingOffsets": "earliest",
    } if ek.should_rewrite(fmt, opts) else None)

    spark = types.SimpleNamespace(read=None)
    assert ek.install(spark) is True
    assert ek._install_classic() is True

    r = DataStreamReader()
    r.format("kafka")
    r.option("eventstream.itemid", "aaaa")
    r.option("eventstream.datasourceid", "bbbb")
    r.option("failOnDataLoss", "false")
    r.options(foo="bar")
    assert r.load() == "df"
    keys = [k for k, _ in applied]
    assert "kafka.bootstrap.servers" in keys
    assert "subscribe" in keys
    assert "failOnDataLoss" in keys
    assert "foo" in keys
    assert "eventstream.itemid" not in keys

    native = DataStreamReader()
    native.format("kafka")
    native.option("subscribe", "plain")
    assert native.load() == "df"


def test_connect_install_runs_foreach_locally(monkeypatch, reset_install, capsys):
    monkeypatch.setenv("SPARK_REMOTE", "sc://sail:50051")
    engine = {"load": 0, "foreach": 0, "start": 0}

    class DataFrameReader:
        def __init__(self):
            self._format = None
            self._options = {}

        def format(self, source):
            self._format = source
            return self

        def load(self, path=None, format=None, schema=None, **kwargs):
            engine["batch"] = engine.get("batch", 0) + 1
            return "engine-batch"

    class DataStreamReader:
        def __init__(self):
            self._format = None
            self._options = {}
            self._client = object()

        def format(self, source):
            self._format = source
            return self

        def load(self, path=None, format=None, schema=None, **kwargs):
            engine["load"] += 1
            return "engine-df"

    class DataStreamWriter:
        def foreachBatch(self, func):
            engine["foreach"] += 1
            return self

        def start(self, path=None, format=None, outputMode=None, partitionBy=None,
                  queryName=None, **options):
            engine["start"] += 1
            return "engine-query"

    class DataFrameWriter:
        def __init__(self):
            self._format = None
            self._options = {}
            self._df = None

        def save(self, path=None, format=None, mode=None, partitionBy=None, **options):
            engine["save"] = engine.get("save", 0) + 1
            return "saved"

    class ConnectDF:
        def __init__(self, marked=False):
            self._emu_eventstream = marked

        @property
        def writeStream(self):
            return DataStreamWriter()

    streaming = types.ModuleType("pyspark.sql.connect.streaming.readwriter")
    streaming.DataStreamReader = DataStreamReader
    streaming.DataStreamWriter = DataStreamWriter
    rw = types.ModuleType("pyspark.sql.connect.readwriter")
    rw.DataFrameReader = DataFrameReader
    rw.DataFrameWriter = DataFrameWriter
    dfmod = types.ModuleType("pyspark.sql.connect.dataframe")
    dfmod.DataFrame = ConnectDF
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect", types.ModuleType("pyspark.sql.connect"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.readwriter", rw)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.dataframe", dfmod)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming",
                        types.ModuleType("pyspark.sql.connect.streaming"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming.readwriter", streaming)

    marked = types.SimpleNamespace(_emu_eventstream=True)
    monkeypatch.setattr(ek, "consume_events", lambda *a, **k: [{"topic": "t"}])
    monkeypatch.setattr(
        ek, "materialize_kafka_df",
        lambda spark, recs, include_headers=False: marked,
    )

    spark = types.SimpleNamespace(read=DataFrameReader())
    assert ek.install(spark) is True
    assert ek._install_connect(spark) is True

    r = DataStreamReader()
    r.format("kafka")
    r._options = {"eventstream.itemid": "a", "eventstream.datasourceid": "b"}
    df = r.load()
    assert df is marked
    assert engine["load"] == 0
    err = capsys.readouterr().err
    assert "LocalRelation" in err

    r_kw = DataStreamReader()
    assert r_kw.load(format="kafka", **{
        "eventstream.itemid": "a", "eventstream.datasourceid": "b",
    }) is marked

    r2 = DataStreamReader()
    r2._format = "kafka"
    r2._options = {"eventstream.itemid": "only"}
    with pytest.raises(ek.EventstreamError, match="both eventstream"):
        r2.load()

    r3 = DataStreamReader()
    r3._format = "rate"
    assert r3.load() == "engine-df"
    assert engine["load"] == 1

    seen = []
    w = DataStreamWriter()
    w._emu_stream_df = marked
    assert w.foreachBatch(lambda batch, i: seen.append((batch, i))) is w
    assert engine["foreach"] == 0
    q = w.start(queryName="clicks")
    assert seen == [(marked, 0)]
    assert q.isActive is False
    assert q.name == "clicks"
    assert engine["start"] == 0

    bound = ConnectDF(marked=True)
    ws = bound.writeStream
    assert ws._emu_stream_df is bound

    plain = DataStreamWriter()
    plain._emu_stream_df = ConnectDF()
    plain.foreachBatch(lambda *a: None)
    assert engine["foreach"] == 1
    assert plain.start() == "engine-query"
    assert engine["start"] == 1

    monkeypatch.setattr(
        ek, "consume_plain_kafka",
        lambda opts: [{"value": b"hello", "topic": opts["subscribe"]}],
    )
    n = DataStreamReader()
    n.format("kafka")
    n._options = {"kafka.bootstrap.servers": "kafka:9092", "subscribe": "plain"}
    assert n.load() is marked
    assert engine["load"] == 1
    assert "driver consume" in capsys.readouterr().err

    b = DataFrameReader()
    b.format("kafka")
    b._options = {"kafka.bootstrap.servers": "k:9092", "subscribe": "plain"}
    assert b.load() is marked
    assert engine.get("batch", 0) == 0
    plain_batch = DataFrameReader()
    plain_batch.format("parquet")
    assert plain_batch.load() == "engine-batch"
    assert engine["batch"] == 1

    sent = []
    monkeypatch.setattr(ek, "produce_plain_kafka", lambda recs, opts: sent.append((recs, opts)))
    monkeypatch.setattr(ek, "collect_kafka_sink_rows", lambda df: [{"value": b"out"}])
    sink = DataFrameWriter()
    sink._format = "kafka"
    sink._options = {"kafka.bootstrap.servers": "k:9092", "topic": "out"}
    sink._df = marked
    assert sink.save() is None
    assert sent and sent[0][0] == [{"value": b"out"}]
    assert engine.get("save", 0) == 0
    other_sink = DataFrameWriter()
    other_sink._format = "delta"
    assert other_sink.save() == "saved"
    assert engine["save"] == 1

    wkafka = DataStreamWriter()
    wkafka._emu_stream_df = marked
    wkafka._format = "kafka"
    wkafka._options = {"kafka.bootstrap.servers": "k:9092", "topic": "out"}
    qk = wkafka.start()
    assert qk.isActive is False
    assert engine["start"] == 1


def test_connect_install_skips_non_connect_reader(monkeypatch, reset_install):
    monkeypatch.setenv("SPARK_REMOTE", "sc://x")

    class DataFrameReader:
        pass

    streaming = types.ModuleType("pyspark.sql.connect.streaming.readwriter")
    streaming.DataStreamReader = type("DataStreamReader", (), {})
    streaming.DataStreamWriter = type("DataStreamWriter", (), {})
    rw = types.ModuleType("pyspark.sql.connect.readwriter")
    rw.DataFrameReader = DataFrameReader
    rw.DataFrameWriter = type("DataFrameWriter", (), {})
    dfmod = types.ModuleType("pyspark.sql.connect.dataframe")
    dfmod.DataFrame = type("DataFrame", (), {})
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect", types.ModuleType("pyspark.sql.connect"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.readwriter", rw)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.dataframe", dfmod)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming",
                        types.ModuleType("pyspark.sql.connect.streaming"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming.readwriter", streaming)
    spark = types.SimpleNamespace(read=object())
    assert ek._install_connect(spark) is False
    monkeypatch.setenv("SPARK_REMOTE", "sc://x")
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.streaming.readwriter", None)
    spark = types.SimpleNamespace(read=object())
    assert ek._install_connect(spark) is False
    assert ek.install(spark) is False


def test_install_swallows_import_errors(monkeypatch, reset_install):
    monkeypatch.delenv("SPARK_REMOTE", raising=False)

    def boom_classic():
        raise ImportError("no classic pyspark")

    def boom_connect(spark=None):
        raise ImportError("no connect pyspark")

    monkeypatch.setattr(ek, "_install_classic", boom_classic)
    monkeypatch.setattr(ek, "_install_connect", boom_connect)
    assert ek.install() is False

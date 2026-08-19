"""Fabric Eventstream options → Kafka records, on JVM Spark and on Sail.

Microsoft's notebook API is:

    spark.readStream.format("kafka").options(**{
        "eventstream.itemid": item_id,
        "eventstream.datasourceid": datasource_id,
    }).load()

    df_raw.writeStream.foreachBatch(show_df).outputMode("append").start()

The closed-source Fabric adapter uses the notebook Entra token to resolve
those IDs to a Kafka endpoint. User code has no bootstrap servers. The schema
is Kafka's (`key`, `value`, `topic`, `partition`, `offset`, `timestamp`) —
never `rate` (`timestamp`, `value`). Mapping Eventstream onto `rate` would
paint the engine matrix green with the wrong columns; this module does not.

Two engines, one notebook snippet:

- **JVM** (`SPARK_REMOTE` unset): rewrite `eventstream.*` to
  `kafka.bootstrap.servers` + `subscribe` on the real OSS Kafka source.
- **Sail** (Connect): the engine has no Kafka source and rejects
  `foreachBatch` at `start()`. The wrap consumes through the emulator
  (`GET …/sources/{ds}/events`), materialises a LocalRelation with the Kafka
  schema, and runs `foreachBatch` in this process. One micro-batch, announced,
  no checkpoint — the same class of wrap as CDF / JSON `multiLine`.

Native `format("kafka")` with `kafka.bootstrap.servers` plus
`subscribe` / `subscribePattern` / `assign` is honoured on Sail too: the
agent consumes the topic (kafka-python) and `createDataFrame`s the
Kafka-schema rows into Sail. JSON offsets, `includeHeaders`, SASL PLAIN,
GSSAPI (JAAS keytab → `KRB5_CLIENT_KTNAME`), PEM SSL, and JKS/P12
truststores (converted to PEM) are honoured; `write`/`writeStream.format("kafka")`
produces from collected rows. Subsequent `select`/`filter`/SQL run on the
engine. One micro-batch, announced, no checkpoint — the same class of wrap
as CDF / JSON `multiLine`. JVM keeps the jar (this module does not rewrite
native options on a classic session).

`stream_sinks.py` still does not intercept a kafka *sink* or `foreachBatch`
on an engine stream — kafka I/O on Sail is this module. Mapping Kafka
onto `rate` stays forbidden.
"""
from __future__ import annotations

import base64
import json
import os
import re
import ssl
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path


class EventstreamError(RuntimeError):
    """The wrap recognised Eventstream options but could not honour them."""


class KafkaSourceError(RuntimeError):
    """OSS format('kafka') on Sail was recognised but cannot be honoured."""


ITEM_KEYS = ("eventstream.itemid", "eventstream.itemId")
DATASOURCE_KEYS = ("eventstream.datasourceid", "eventstream.datasourceId")
ANNOUNCE = "[eventstream] kafka source via emulator consume (LocalRelation, one micro-batch)"
NATIVE_ANNOUNCE = (
    "[kafka] OSS format(kafka) via driver consume "
    "(LocalRelation → Sail, one micro-batch)"
)
SINK_ANNOUNCE = "[kafka] OSS format(kafka) sink via driver produce (one micro-batch)"
FOREACH_ANNOUNCE = "[eventstream] foreachBatch in the agent (Sail cannot pickle UDFs)"
NATIVE_OPTION_KEYS = (
    "kafka.bootstrap.servers",
    "subscribe",
    "subscribepattern",
    "assign",
)
_JAVA_STORE_EXT = (".jks", ".p12", ".pfx")
_ssl_temps: list[str] = []

_classic_installed = False
_connect_installed = False


def _opt(options, names):
    lower = {str(k).lower(): v for k, v in (options or {}).items()}
    for name in names:
        value = lower.get(name.lower())
        if value is not None and str(value).strip():
            return str(value).strip()
    return ""


def eventstream_ids(options) -> tuple[str, str]:
    return _opt(options, ITEM_KEYS), _opt(options, DATASOURCE_KEYS)


def should_rewrite(fmt, options) -> bool:
    """True when this is a kafka read carrying at least one Eventstream id."""
    if (fmt or "").strip().lower() != "kafka":
        return False
    item, ds = eventstream_ids(options)
    return bool(item or ds)


def kafka_options(bootstrap: str, topic: str, extra=None) -> dict:
    """OSS Kafka source options. startingOffsets=earliest so a notebook that
    starts after a Custom produce still sees the records (Fabric's first
    read of a stream behaves the same way for the sample)."""
    out = {
        "kafka.bootstrap.servers": bootstrap,
        "subscribe": topic,
        "startingOffsets": "earliest",
    }
    for key, value in (extra or {}).items():
        if str(key).lower().startswith("eventstream."):
            continue
        if key in out:
            continue
        out[key] = value
    return out


def _fabric_token(env=None):
    env = os.environ if env is None else env
    url = env.get("ENTRA_TOKEN_URL")
    if not url:
        raise EventstreamError(
            "eventstream kafka adapter needs ENTRA_TOKEN_URL to mint a Fabric token"
        )
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": env["ENTRA_CLIENT_ID"],
        "client_secret": env["ENTRA_CLIENT_SECRET"],
        "scope": env.get("ENTRA_FABRIC_SCOPE", "https://api.fabric.microsoft.com/.default"),
    }).encode()
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = urllib.request.Request(url, data=form)
    with urllib.request.urlopen(req, timeout=30, context=ctx) as response:
        return json.loads(response.read())["access_token"]


def _fabric_request(path, env=None, query=None):
    """Entra-gated GET. Unknown IDs and a missing broker fail loudly."""
    env = os.environ if env is None else env
    base = (env.get("FABRIC_API_URL") or "http://api.fabric.microsoft.com").rstrip("/")
    url = base + path
    if query:
        url += "?" + urllib.parse.urlencode(query)
    token = _fabric_token(env)
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + token})
    try:
        with urllib.request.urlopen(req, timeout=60, context=ctx) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as err:
        detail = err.read().decode("utf-8", "replace")
        try:
            parsed = json.loads(detail)
            detail = parsed.get("message") or parsed.get("errorCode") or detail
        except json.JSONDecodeError:
            pass
        raise EventstreamError(
            f"Eventstream {path} failed ({err.code}): {detail}"
        ) from err


def resolve_source(item_id, datasource_id, env=None):
    """Entra-gated lookup. Unknown IDs and a missing broker fail loudly."""
    item_id = (item_id or "").strip()
    datasource_id = (datasource_id or "").strip()
    if not item_id or not datasource_id:
        raise EventstreamError(
            "both eventstream.itemid and eventstream.datasourceid are required"
        )
    body = _fabric_request(
        f"/v1/eventstreams/{item_id}/sources/{datasource_id}", env=env,
    )
    bootstrap = body.get("bootstrapServers")
    topic = body.get("topic")
    if not bootstrap or not topic:
        raise EventstreamError(
            f"Eventstream resolve returned no bootstrap/topic: {body!r}"
        )
    return {"bootstrapServers": bootstrap, "topic": topic}


def consume_events(item_id, datasource_id, env=None, max_records=100, timeout_ms=8000):
    """Pull Kafka-shaped records through the control plane (Sail has no client)."""
    item_id = (item_id or "").strip()
    datasource_id = (datasource_id or "").strip()
    if not item_id or not datasource_id:
        raise EventstreamError(
            "both eventstream.itemid and eventstream.datasourceid are required"
        )
    body = _fabric_request(
        f"/v1/eventstreams/{item_id}/sources/{datasource_id}/events",
        env=env,
        query={"max": str(max_records), "timeoutMs": str(timeout_ms)},
    )
    recs = body.get("records")
    if recs is None:
        raise EventstreamError(
            f"Eventstream consume returned no records field: {body!r}"
        )
    return recs


def rewrite_load(fmt, options) -> dict | None:
    """Return Kafka options to apply, or None to leave the reader alone.

    Raises EventstreamError when the wrap recognised the API but cannot
    honour it (partial options, unknown IDs, no broker).
    """
    options = {str(k): v for k, v in (options or {}).items()}
    if not should_rewrite(fmt, options):
        return None
    item, ds = eventstream_ids(options)
    if not item or not ds:
        raise EventstreamError(
            "both eventstream.itemid and eventstream.datasourceid are required"
        )
    source = resolve_source(item, ds)
    return kafka_options(source["bootstrapServers"], source["topic"], options)


def native_subscribe(options) -> tuple[str, list[str]]:
    bootstrap = _opt(options, ("kafka.bootstrap.servers",))
    subscribe = _opt(options, ("subscribe",))
    topics = [t.strip() for t in subscribe.split(",") if t.strip()] if subscribe else []
    return bootstrap, topics


def should_consume_native(fmt, options) -> bool:
    """True when this is OSS format('kafka') carrying bootstrap/subscribe/assign."""
    if (fmt or "").strip().lower() != "kafka":
        return False
    if should_rewrite(fmt, options):
        return False
    lower = {str(k).lower() for k in (options or {})}
    return any(key in lower for key in NATIVE_OPTION_KEYS)


def consume_plain_kafka(options, poll=None):
    """Pull Kafka records for OSS bootstrap + subscribe/pattern/assign."""
    spec = kafka_source_spec(options)
    return (poll or _kafka_poll)(spec)


def _truthy(value) -> bool:
    return str(value or "").strip().lower() in ("true", "1", "yes")


def _parse_offset_option(raw, name):
    """Spark: 'earliest' / 'latest' / JSON {topic:{partition:offset}}.

    -1 is latest, -2 is earliest. Returns a str or {topic: {part: int}}.
    """
    text = (raw or "").strip()
    if not text:
        return None
    if text in ("earliest", "latest"):
        return text
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError as exc:
        raise KafkaSourceError(
            f"{name} {text!r} is not earliest, latest, or JSON"
        ) from exc
    if not isinstance(parsed, dict):
        raise KafkaSourceError(f"{name} JSON must be an object, got {text!r}")
    out = {}
    for topic, parts in parsed.items():
        if not isinstance(parts, dict):
            raise KafkaSourceError(f"{name} for {topic!r} must be {{partition: offset}}")
        out[str(topic)] = {int(p): int(off) for p, off in parts.items()}
    return out


def parse_jaas_plain(config) -> tuple[str, str]:
    """Pull username/password out of a Kafka PLAIN JAAS string."""
    text = config or ""
    user = re.search(r'username\s*=\s*"([^"]*)"', text)
    password = re.search(r'password\s*=\s*"([^"]*)"', text)
    if not user or not password:
        raise KafkaSourceError(
            "kafka.sasl.jaas.config must set username= and password= for PLAIN"
        )
    return user.group(1), password.group(1)


def _jaas_field(text, *names) -> str:
    for name in names:
        quoted = re.search(rf'{name}\s*=\s*"([^"]*)"', text, re.I)
        if quoted:
            return quoted.group(1)
        bare = re.search(rf'{name}\s*=\s*([^;\s]+)', text, re.I)
        if bare:
            return bare.group(1).strip()
    return ""


def parse_jaas_gssapi(config) -> dict:
    """Pull principal / keyTab out of a Krb5LoginModule JAAS string."""
    text = config or ""
    return {
        "principal": _jaas_field(text, "principal"),
        "keyTab": _jaas_field(text, "keyTab", "keytab"),
        "useKeyTab": _jaas_field(text, "useKeyTab").lower() in ("true", "yes", "1"),
        "useTicketCache": _jaas_field(text, "useTicketCache").lower()
        in ("true", "yes", "1"),
    }


def _sasl_mechanism(options) -> str:
    jaas = _opt(options, ("kafka.sasl.jaas.config",))
    mech = (_opt(options, ("kafka.sasl.mechanism",)) or "").upper()
    if mech:
        return mech
    if jaas and re.search(r"Krb5LoginModule", jaas, re.I):
        return "GSSAPI"
    return "PLAIN"


def _write_pem_temp(suffix, payload: bytes) -> str:
    fd, path = tempfile.mkstemp(prefix="emu-kafka-", suffix=suffix)
    with os.fdopen(fd, "wb") as fh:
        fh.write(payload)
    os.chmod(path, 0o600)
    _ssl_temps.append(path)
    return path


def _certs_to_pem(ders) -> bytes:
    chunks = []
    for der in ders:
        b64 = base64.encodebytes(der).decode("ascii")
        chunks.append(
            "-----BEGIN CERTIFICATE-----\n" + b64 + "-----END CERTIFICATE-----\n"
        )
    return "".join(chunks).encode()


def _is_java_store(path, store_type="") -> bool:
    typ = (store_type or "").strip().upper()
    if typ in ("JKS", "PKCS12"):
        return True
    if typ in ("PEM", "PEM_CERTIFICATE"):
        return False
    return Path(path).suffix.lower() in _JAVA_STORE_EXT


def _load_jks(path, password):
    try:
        import jks
    except ImportError as exc:
        raise KafkaSourceError(
            "JKS truststores need pyjks in the spark-agent image"
        ) from exc
    try:
        return jks.KeyStore.load(path, password if password is not None else "")
    except Exception as exc:
        raise KafkaSourceError(
            f"could not open JKS {path}: {exc} "
            "(set kafka.ssl.truststore.password / kafka.ssl.keystore.password)"
        ) from exc


def _jks_cert_ders(ks) -> list[bytes]:
    ders = []
    for entry in (getattr(ks, "certs", None) or {}).values():
        ders.append(entry.cert)
    for pk in (getattr(ks, "private_keys", None) or {}).values():
        for cert in pk.cert_chain or []:
            ders.append(cert[1] if isinstance(cert, (tuple, list)) else cert)
    return ders


def _jks_ca_pem(path, password) -> bytes:
    ders = _jks_cert_ders(_load_jks(path, password))
    if not ders:
        raise KafkaSourceError(f"JKS {path} contains no certificates")
    return _certs_to_pem(ders)


def _der_key_to_pem(key_der: bytes) -> bytes:
    from cryptography.hazmat.primitives.serialization import (
        Encoding,
        NoEncryption,
        PrivateFormat,
        load_der_private_key,
    )
    key = load_der_private_key(key_der, password=None)
    return key.private_bytes(Encoding.PEM, PrivateFormat.TraditionalOpenSSL, NoEncryption())


def _jks_client_pems(path, password) -> tuple[bytes, bytes]:
    ks = _load_jks(path, password)
    pkeys = getattr(ks, "private_keys", None) or {}
    if not pkeys:
        raise KafkaSourceError(f"JKS keystore {path} has no private key")
    pk = next(iter(pkeys.values()))
    cert_ders = [
        cert[1] if isinstance(cert, (tuple, list)) else cert
        for cert in (pk.cert_chain or [])
    ]
    if not cert_ders:
        raise KafkaSourceError(f"JKS keystore {path} has no certificate chain")
    return _certs_to_pem(cert_ders), _der_key_to_pem(pk.pkey)


def _load_p12(path, password):
    try:
        from cryptography.hazmat.primitives.serialization import pkcs12
    except ImportError as exc:
        raise KafkaSourceError(
            "PKCS12 truststores need cryptography in the spark-agent image"
        ) from exc
    data = Path(path).read_bytes()
    pwd = password.encode() if password else None
    try:
        return pkcs12.load_key_and_certificates(data, pwd)
    except Exception as exc:
        raise KafkaSourceError(
            f"could not open PKCS12 {path}: {exc} "
            "(set kafka.ssl.truststore.password / kafka.ssl.keystore.password)"
        ) from exc


def _p12_ca_pem(path, password) -> bytes:
    from cryptography.hazmat.primitives.serialization import Encoding
    _key, cert, extra = _load_p12(path, password)
    ders = []
    if cert is not None:
        ders.append(cert.public_bytes(Encoding.DER))
    for item in extra or []:
        ders.append(item.public_bytes(Encoding.DER))
    if not ders:
        raise KafkaSourceError(f"PKCS12 {path} contains no certificates")
    return _certs_to_pem(ders)


def _p12_client_pems(path, password) -> tuple[bytes, bytes]:
    from cryptography.hazmat.primitives.serialization import (
        Encoding,
        NoEncryption,
        PrivateFormat,
    )
    key, cert, extra = _load_p12(path, password)
    if key is None or cert is None:
        raise KafkaSourceError(f"PKCS12 keystore {path} needs a private key and cert")
    key_pem = key.private_bytes(
        Encoding.PEM, PrivateFormat.TraditionalOpenSSL, NoEncryption(),
    )
    certs = [cert, *(extra or [])]
    cert_pem = b"".join(c.public_bytes(Encoding.PEM) for c in certs)
    return cert_pem, key_pem


def _store_kind(path, store_type="") -> str:
    typ = (store_type or "").strip().upper()
    if typ in ("JKS", "PKCS12"):
        return typ
    ext = Path(path).suffix.lower()
    if ext == ".jks":
        return "JKS"
    if ext in (".p12", ".pfx"):
        return "PKCS12"
    return "PEM"


def _materialize_ca(path, password, store_type="") -> str:
    kind = _store_kind(path, store_type)
    if kind == "JKS":
        return _write_pem_temp(".ca.pem", _jks_ca_pem(path, password))
    if kind == "PKCS12":
        return _write_pem_temp(".ca.pem", _p12_ca_pem(path, password))
    return path


def _materialize_client(path, password, store_type="") -> tuple[str, str]:
    kind = _store_kind(path, store_type)
    if kind == "JKS":
        cert_pem, key_pem = _jks_client_pems(path, password)
        return (
            _write_pem_temp(".cert.pem", cert_pem),
            _write_pem_temp(".key.pem", key_pem),
        )
    if kind == "PKCS12":
        cert_pem, key_pem = _p12_client_pems(path, password)
        return (
            _write_pem_temp(".cert.pem", cert_pem),
            _write_pem_temp(".key.pem", key_pem),
        )
    return path, ""


def kafka_client_kwargs(options) -> dict:
    """kafka-python kwargs from Spark's kafka.* client options."""
    proto = (_opt(options, ("kafka.security.protocol",)) or "PLAINTEXT").upper()
    allowed = ("PLAINTEXT", "SSL", "SASL_PLAINTEXT", "SASL_SSL")
    if proto not in allowed:
        raise KafkaSourceError(
            f"kafka.security.protocol={proto!r} is not in this wrap ({', '.join(allowed)})"
        )
    kwargs = {}
    if proto != "PLAINTEXT":
        kwargs["security_protocol"] = proto
    if proto.startswith("SASL_"):
        mech = _sasl_mechanism(options)
        if mech not in ("PLAIN", "GSSAPI"):
            raise KafkaSourceError(
                f"kafka.sasl.mechanism={mech!r} is not in this wrap (PLAIN or GSSAPI)"
            )
        kwargs["sasl_mechanism"] = mech
        jaas = _opt(options, ("kafka.sasl.jaas.config",))
        if mech == "GSSAPI":
            kwargs["sasl_kerberos_service_name"] = (
                _opt(options, ("kafka.sasl.kerberos.service.name",)) or "kafka"
            )
            gss = parse_jaas_gssapi(jaas) if jaas else {}
            if gss.get("keyTab"):
                os.environ["KRB5_CLIENT_KTNAME"] = gss["keyTab"]
            if gss.get("principal"):
                kwargs["sasl_kerberos_name"] = gss["principal"]
        else:
            user = _opt(options, ("kafka.sasl.username",))
            password = _opt(options, ("kafka.sasl.password",))
            if jaas:
                user, password = parse_jaas_plain(jaas)
            if not user or not password:
                raise KafkaSourceError(
                    "SASL PLAIN needs kafka.sasl.jaas.config or "
                    "kafka.sasl.username + kafka.sasl.password"
                )
            kwargs["sasl_plain_username"] = user
            kwargs["sasl_plain_password"] = password
    if proto in ("SSL", "SASL_SSL"):
        cafile = _opt(options, ("kafka.ssl.cafile", "kafka.ssl.truststore.location"))
        if not cafile:
            raise KafkaSourceError(
                "SSL needs kafka.ssl.truststore.location "
                "(PEM CA, JKS, or PKCS12)"
            )
        if not Path(cafile).is_file() and _is_java_store(
            cafile, _opt(options, ("kafka.ssl.truststore.type",)),
        ):
            raise KafkaSourceError(f"SSL truststore not found: {cafile}")
        kwargs["ssl_cafile"] = _materialize_ca(
            cafile,
            _opt(options, ("kafka.ssl.truststore.password",)),
            _opt(options, ("kafka.ssl.truststore.type",)),
        )
        cert = _opt(options, ("kafka.ssl.keystore.location", "kafka.ssl.certfile"))
        key = _opt(options, ("kafka.ssl.key.location", "kafka.ssl.keyfile"))
        if cert and _is_java_store(cert, _opt(options, ("kafka.ssl.keystore.type",))):
            if not Path(cert).is_file():
                raise KafkaSourceError(f"SSL keystore not found: {cert}")
            cert_pem, key_pem = _materialize_client(
                cert,
                _opt(options, ("kafka.ssl.keystore.password", "kafka.ssl.key.password")),
                _opt(options, ("kafka.ssl.keystore.type",)),
            )
            kwargs["ssl_certfile"] = cert_pem
            kwargs["ssl_keyfile"] = key_pem
        else:
            if cert:
                kwargs["ssl_certfile"] = cert
            if key:
                kwargs["ssl_keyfile"] = key
    return kwargs


def kafka_source_spec(options) -> dict:
    """Normalise OSS Kafka source options into one consume request."""
    options = {str(k): v for k, v in (options or {}).items()}
    bootstrap, topics = native_subscribe(options)
    pattern = _opt(options, ("subscribePattern", "subscribepattern"))
    assign_raw = _opt(options, ("assign",))
    if sum(bool(x) for x in (topics, pattern, assign_raw)) > 1:
        raise KafkaSourceError(
            "Do not use more than one of: subscribe, subscribePattern, assign"
        )
    assigned = None
    if assign_raw:
        try:
            parsed = json.loads(assign_raw)
        except json.JSONDecodeError as exc:
            raise KafkaSourceError(f"assign is not JSON: {assign_raw!r}") from exc
        if not isinstance(parsed, dict):
            raise KafkaSourceError(
                f"assign JSON must be {{topic: [partitions]}}, got {assign_raw!r}"
            )
        assigned = {
            str(topic): [int(p) for p in (parts or [])]
            for topic, parts in parsed.items()
        }
    if not bootstrap:
        raise KafkaSourceError("option 'kafka.bootstrap.servers' is required")
    if not topics and not pattern and not assigned:
        raise KafkaSourceError(
            "one of subscribe, subscribePattern, assign is required"
        )
    starting = _parse_offset_option(
        _opt(options, ("startingOffsets", "startingoffsets")) or "earliest",
        "startingOffsets",
    )
    ending = _parse_offset_option(
        _opt(options, ("endingOffsets", "endingoffsets")),
        "endingOffsets",
    )
    if ending == "earliest":
        raise KafkaSourceError("endingOffsets cannot be earliest")
    return {
        "bootstrap": bootstrap,
        "topics": topics,
        "pattern": pattern,
        "assign": assigned,
        "starting": starting or "earliest",
        "ending": ending,
        "timeout_ms": int(
            _opt(options, ("kafkaConsumer.pollTimeoutMs",
                           "kafkaconsumer.polltimeoutms")) or 8000
        ),
        "max_records": int(
            _opt(options, ("maxOffsetsPerTrigger", "maxoffsetspertrigger")) or 10000
        ),
        "include_headers": _truthy(_opt(options, ("includeHeaders", "includeheaders"))),
        "client": kafka_client_kwargs(options),
    }


def _resolve_topics(consumer, spec):
    if spec.get("assign"):
        return list(spec["assign"])
    if spec.get("topics"):
        return list(spec["topics"])
    pattern = spec.get("pattern") or ""
    try:
        rx = re.compile(pattern)
    except re.error as exc:
        raise KafkaSourceError(f"subscribePattern is not a regex: {pattern!r}") from exc
    deadline = time.monotonic() + max(int(spec["timeout_ms"]), 1) / 1000.0
    matched = []
    while time.monotonic() < deadline:
        matched = [t for t in (consumer.topics() or []) if rx.search(t)]
        if matched:
            return matched
        time.sleep(0.2)
    raise KafkaSourceError(
        f"subscribePattern {pattern!r} matched no topics at {spec['bootstrap']}"
    )


def _partitions_for(consumer, TopicPartition, spec, topics):
    if spec.get("assign"):
        return [
            TopicPartition(topic, p)
            for topic, parts in spec["assign"].items()
            for p in parts
        ]
    tps = []
    deadline = time.monotonic() + max(int(spec["timeout_ms"]), 1) / 1000.0
    missing = list(topics)
    while missing and time.monotonic() < deadline:
        still = []
        for topic in missing:
            parts = consumer.partitions_for_topic(topic)
            if not parts:
                still.append(topic)
                continue
            tps.extend(TopicPartition(topic, p) for p in sorted(parts))
        missing = still
        if missing:
            time.sleep(0.2)
    if missing:
        raise KafkaSourceError(
            f"kafka topic metadata not found for {missing} at {spec['bootstrap']}"
        )
    return tps


def _offset_for(table, tp, sentinel):
    if not isinstance(table, dict):
        return sentinel
    parts = table.get(tp.topic) or {}
    if tp.partition in parts:
        return int(parts[tp.partition])
    if str(tp.partition) in parts:
        return int(parts[str(tp.partition)])
    return sentinel


def _seek_start(consumer, tps, starting):
    if starting == "latest":
        consumer.seek_to_end(*tps)
        return
    if starting == "earliest" or starting is None:
        consumer.seek_to_beginning(*tps)
        return
    begin = consumer.beginning_offsets(tps)
    end = consumer.end_offsets(tps)
    for tp in tps:
        off = _offset_for(starting, tp, -2)
        if off == -2:
            consumer.seek(tp, begin[tp])
        elif off == -1:
            consumer.seek(tp, end[tp])
        else:
            consumer.seek(tp, off)


def _past_end(ending, tp, offset) -> bool:
    if ending is None or ending == "latest":
        return False
    stop = _offset_for(ending, tp, None)
    if stop is None or stop < 0:
        return False
    return int(offset) >= int(stop)


def _msg_headers(msg) -> list[tuple]:
    raw = getattr(msg, "headers", None) or []
    out = []
    for item in raw:
        if isinstance(item, (tuple, list)) and len(item) >= 2:
            out.append((str(item[0]), item[1]))
        elif isinstance(item, dict):
            out.append((str(item.get("key") or ""), item.get("value")))
    return out


def _kafka_poll(spec):
    """kafka-python consumer → list of Kafka-shaped dicts (bytes intact)."""
    try:
        from kafka import KafkaConsumer, TopicPartition
    except ImportError as exc:
        raise KafkaSourceError(
            "OSS format('kafka') on Sail needs kafka-python in the spark-agent image"
        ) from exc
    servers = [s.strip() for s in spec["bootstrap"].split(",") if s.strip()]
    timeout_ms = int(spec["timeout_ms"])
    conf = {
        "bootstrap_servers": servers,
        "enable_auto_commit": False,
        "consumer_timeout_ms": timeout_ms,
        "request_timeout_ms": max(timeout_ms + 5000, 15000),
    }
    # `api_version_auto_timeout_ms` bounds the broker version probe, and
    # kafka-python 3 REMOVED it — passing it there is not ignored, it raises
    # `KafkaConfigurationError: Unrecognized configs`, so every eventstream read
    # fails before it connects. Asked of the installed client rather than
    # pinned to a version, because this agent image is consumed by more than one
    # emulator and they do not upgrade in step (same reasoning as connectconf).
    if "api_version_auto_timeout_ms" in getattr(KafkaConsumer, "DEFAULT_CONFIG", {}):
        conf["api_version_auto_timeout_ms"] = min(timeout_ms, 10000)
    consumer = KafkaConsumer(**conf, **(spec.get("client") or {}))
    try:
        topics = _resolve_topics(consumer, spec)
        tps = _partitions_for(consumer, TopicPartition, spec, topics)
        if not tps:
            return []
        consumer.assign(tps)
        starting = spec.get("starting") or "earliest"
        if starting == "latest" and spec.get("ending") in (None, "latest"):
            consumer.seek_to_end(*tps)
            return []
        _seek_start(consumer, tps, starting)
        rows = []
        dummy = TopicPartition
        for msg in consumer:
            tp = dummy(msg.topic, msg.partition)
            if _past_end(spec.get("ending"), tp, msg.offset):
                continue
            rec = {
                "key": msg.key,
                "value": msg.value,
                "topic": msg.topic,
                "partition": msg.partition,
                "offset": msg.offset,
                "timestamp": msg.timestamp,
                "timestampType": int(getattr(msg, "timestamp_type", 0) or 0),
            }
            if spec.get("include_headers"):
                rec["headers"] = _msg_headers(msg)
            rows.append(rec)
            if len(rows) >= int(spec["max_records"]):
                break
        return rows
    finally:
        consumer.close()


def connect_kafka_df(spark, fmt, options):
    """Materialise a Kafka-schema LocalRelation, or None to leave the reader."""
    options = {str(k): v for k, v in (options or {}).items()}
    if should_rewrite(fmt, options):
        item, ds = eventstream_ids(options)
        if not item or not ds:
            raise EventstreamError(
                "both eventstream.itemid and eventstream.datasourceid are required"
            )
        recs = consume_events(item, ds)
        print(ANNOUNCE, file=sys.stderr, flush=True)
        return materialize_kafka_df(spark, recs)
    if should_consume_native(fmt, options):
        recs = consume_plain_kafka(options)
        print(NATIVE_ANNOUNCE, file=sys.stderr, flush=True)
        headers = _truthy(_opt(options, ("includeHeaders", "includeheaders")))
        return materialize_kafka_df(spark, recs, include_headers=headers)
    return None


def _as_bytes(value):
    if value is None:
        return None
    if isinstance(value, (bytes, bytearray)):
        return bytes(value)
    if isinstance(value, str):
        if value == "":
            return b""
        try:
            return base64.b64decode(value)
        except (ValueError, TypeError):
            return value.encode("utf-8")
    return bytes(value)


def _as_timestamp(value):
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return value
    if isinstance(value, (int, float)):
        # kafka-python timestamps are milliseconds since epoch.
        millis = float(value)
        if millis > 1e12:
            millis = millis / 1000.0
        return datetime.fromtimestamp(millis, tz=timezone.utc).replace(tzinfo=None)
    text = str(value).replace("Z", "+00:00")
    try:
        ts = datetime.fromisoformat(text)
    except ValueError:
        return datetime.fromtimestamp(0, tz=timezone.utc).replace(tzinfo=None)
    if ts.tzinfo is not None:
        ts = ts.astimezone(timezone.utc).replace(tzinfo=None)
    return ts


def records_to_rows(records, include_headers=False) -> list[tuple]:
    """Tuples matching Spark's kafka source schema (never rate)."""
    rows = []
    for rec in records or []:
        rec = rec or {}
        row = (
            _as_bytes(rec.get("key")),
            _as_bytes(rec.get("value")),
            rec.get("topic") or "",
            int(rec.get("partition") or 0),
            int(rec.get("offset") or 0),
            _as_timestamp(rec.get("timestamp")),
            int(rec.get("timestampType") or 0),
        )
        if include_headers:
            headers = []
            for item in rec.get("headers") or []:
                if isinstance(item, (tuple, list)) and len(item) >= 2:
                    headers.append((str(item[0]), _as_bytes(item[1])))
                elif isinstance(item, dict):
                    headers.append((str(item.get("key") or ""), _as_bytes(item.get("value"))))
            row = (*row, headers)
        rows.append(row)
    return rows


def kafka_schema(include_headers=False):
    from pyspark.sql.types import (
        BinaryType,
        IntegerType,
        LongType,
        StringType,
        StructField,
        StructType,
        TimestampType,
    )
    fields = [
        StructField("key", BinaryType(), True),
        StructField("value", BinaryType(), True),
        StructField("topic", StringType(), False),
        StructField("partition", IntegerType(), False),
        StructField("offset", LongType(), False),
        StructField("timestamp", TimestampType(), True),
        StructField("timestampType", IntegerType(), False),
    ]
    if include_headers:
        from pyspark.sql.types import ArrayType
        fields.append(StructField(
            "headers",
            ArrayType(StructType([
                StructField("key", StringType(), False),
                StructField("value", BinaryType(), True),
            ])),
            True,
        ))
    return StructType(fields)


def materialize_kafka_df(spark, records, include_headers=False):
    """LocalRelation with Kafka columns. `.explain()` is not a streaming plan."""
    df = spark.createDataFrame(
        records_to_rows(records, include_headers=include_headers),
        kafka_schema(include_headers=include_headers),
    )
    df._emu_eventstream = True
    return df


def should_produce(fmt, options) -> bool:
    if (fmt or "").strip().lower() != "kafka":
        return False
    return bool(_opt(options, ("kafka.bootstrap.servers",)))


def produce_plain_kafka(records, options, send=None):
    """Produce Kafka records from a collected DataFrame. Never a rate sink."""
    options = {str(k): v for k, v in (options or {}).items()}
    bootstrap = _opt(options, ("kafka.bootstrap.servers",))
    if not bootstrap:
        raise KafkaSourceError("option 'kafka.bootstrap.servers' is required")
    topic = _opt(options, ("topic",))
    return (send or _kafka_send)(
        bootstrap, topic, records, kafka_client_kwargs(options),
    )


def _row_dict(row) -> dict:
    if hasattr(row, "asDict"):
        return row.asDict(recursive=True)
    if isinstance(row, dict):
        return dict(row)
    return dict(row)


def collect_kafka_sink_rows(df) -> list[dict]:
    if df is None:
        raise KafkaSourceError("kafka sink has no DataFrame to produce")
    return [_row_dict(r) for r in df.collect()]


def _kafka_send(bootstrap, default_topic, records, client):
    try:
        from kafka import KafkaProducer
    except ImportError as exc:
        raise KafkaSourceError(
            "OSS format('kafka') sink on Sail needs kafka-python in the spark-agent image"
        ) from exc
    producer = KafkaProducer(
        bootstrap_servers=[s.strip() for s in bootstrap.split(",") if s.strip()],
        acks="all",
        **(client or {}),
    )
    n = 0
    try:
        for rec in records or []:
            rec = rec or {}
            topic = rec.get("topic") or default_topic
            if not topic:
                raise KafkaSourceError(
                    "kafka sink needs option topic or a topic column"
                )
            headers = rec.get("headers") or []
            kw = {}
            if headers:
                packed = []
                for item in headers:
                    if isinstance(item, (tuple, list)) and len(item) >= 2:
                        packed.append((str(item[0]), _as_bytes(item[1])))
                    elif isinstance(item, dict):
                        packed.append((
                            str(item.get("key") or ""),
                            _as_bytes(item.get("value")),
                        ))
                if packed:
                    kw["headers"] = packed
            producer.send(
                str(topic),
                value=_as_bytes(rec.get("value")),
                key=_as_bytes(rec.get("key")),
                **kw,
            )
            n += 1
        producer.flush()
    finally:
        producer.close()
    return n


def _writer_fmt_opts(writer, format=None, options=None):
    proto = getattr(writer, "_write_proto", None)
    proto_opts = {}
    proto_fmt = None
    if proto is not None:
        proto_fmt = getattr(proto, "format", None)
        proto_opts = dict(getattr(proto, "options", None) or {})
    held = {
        **dict(getattr(writer, "_options", None) or {}),
        **proto_opts,
        **{str(k): v for k, v in (options or {}).items()},
    }
    fmt = format or proto_fmt or getattr(writer, "_format", None)
    return fmt, held


def should_run_local_foreach(stream_df, writer) -> bool:
    """True only for an Eventstream wrap + foreachBatch. Other streams stay on the engine."""
    if writer is None or getattr(writer, "_emu_foreach_batch", None) is None:
        return False
    return bool(getattr(stream_df, "_emu_eventstream", False))


class OneShotStreamingQuery:
    """Stand-in after a local foreachBatch. availableNow: already finished."""

    def __init__(self, name=None):
        self._active = False
        self.name = name or None
        self.id = "emu-eventstream"
        self.runId = "emu-eventstream-run"
        self.lastProgress = None
        self.recentProgress = []

    @property
    def isActive(self):
        return self._active

    def stop(self):
        self._active = False

    def awaitTermination(self, timeout=None):
        return True

    def processAllAvailable(self):
        return None

    def explain(self, extended=False):
        print(FOREACH_ANNOUNCE, flush=True)


def _install_classic():
    """Wrap classic DataStreamReader so Eventstream options never reach OSS Kafka."""
    global _classic_installed
    if os.environ.get("SPARK_REMOTE"):
        return False
    if _classic_installed:
        return True
    from pyspark.sql.streaming import DataStreamReader

    orig_format = DataStreamReader.format
    orig_option = DataStreamReader.option
    orig_load = DataStreamReader.load

    def format(self, source):  # noqa: A001 — Spark's name
        self._emu_format = source
        return orig_format(self, source)

    def option(self, key, value):
        held = getattr(self, "_emu_opts", None)
        if held is None:
            held = {}
            self._emu_opts = held
        held[str(key)] = value
        if str(key).lower().startswith("eventstream."):
            return self
        return orig_option(self, key, value)

    def options(self, **kwargs):
        for key, value in kwargs.items():
            option(self, key, value)
        return self

    def load(self, path=None, format=None, schema=None, **kwargs):  # noqa: A002
        held = dict(getattr(self, "_emu_opts", {}) or {})
        held.update({str(k): v for k, v in kwargs.items()})
        fmt = format or getattr(self, "_emu_format", None)
        rewritten = rewrite_load(fmt, held)
        if rewritten is not None:
            kwargs = {k: v for k, v in kwargs.items()
                      if not str(k).lower().startswith("eventstream.")}
            for key, value in rewritten.items():
                orig_option(self, key, value)
        return orig_load(self, path=path, format=format, schema=schema, **kwargs)

    DataStreamReader.format = format
    DataStreamReader.option = option
    DataStreamReader.options = options
    DataStreamReader.load = load
    _classic_installed = True
    return True


def _install_connect(spark):
    """Wrap Connect read / readStream / foreachBatch for Kafka on Sail."""
    global _connect_installed
    if _connect_installed:
        return True
    try:
        from pyspark.sql.connect.dataframe import DataFrame as ConnectDF
        from pyspark.sql.connect.readwriter import DataFrameReader, DataFrameWriter
        from pyspark.sql.connect.streaming.readwriter import (
            DataStreamReader,
            DataStreamWriter,
        )
    except ImportError:
        return False
    if spark is not None:
        reader = getattr(spark, "read", None)
        if reader is None or not isinstance(reader, DataFrameReader):
            return False

    orig_load = DataStreamReader.load
    orig_batch_load = DataFrameReader.load
    orig_save = DataFrameWriter.save
    orig_foreach = DataStreamWriter.foreachBatch
    orig_start = DataStreamWriter.start
    orig_ws = ConnectDF.writeStream

    def _held(reader, format, kwargs):  # noqa: A002
        if format is not None:
            reader.format(format)
        held = dict(getattr(reader, "_options", {}) or {})
        held.update({str(k): v for k, v in kwargs.items()})
        fmt = format or getattr(reader, "_format", None)
        return fmt, held

    def load(self, path=None, format=None, schema=None, **kwargs):  # noqa: A002
        fmt, held = _held(self, format, kwargs)
        df = connect_kafka_df(spark, fmt, held)
        if df is not None:
            return df
        return orig_load(self, path=path, format=format, schema=schema, **kwargs)

    def batch_load(self, path=None, format=None, schema=None, **kwargs):  # noqa: A002
        fmt, held = _held(self, format, kwargs)
        df = connect_kafka_df(spark, fmt, held)
        if df is not None:
            return df
        return orig_batch_load(self, path=path, format=format, schema=schema, **kwargs)

    def save(self, path=None, format=None, mode=None, partitionBy=None, **options):
        fmt, held = _writer_fmt_opts(self, format, options)
        if should_produce(fmt, held):
            df = getattr(self, "_df", None)
            print(SINK_ANNOUNCE, file=sys.stderr, flush=True)
            produce_plain_kafka(collect_kafka_sink_rows(df), held)
            return None
        return orig_save(
            self, path=path, format=format, mode=mode,
            partitionBy=partitionBy, **options,
        )

    def foreachBatch(self, func):  # noqa: N802 — Spark's name
        self._emu_foreach_batch = func
        stream_df = getattr(self, "_emu_stream_df", None)
        if getattr(stream_df, "_emu_eventstream", False):
            return self
        return orig_foreach(self, func)

    def writeStream(self):  # noqa: N802
        writer = orig_ws.fget(self) if isinstance(orig_ws, property) else orig_ws(self)
        writer._emu_stream_df = self
        return writer

    def start(self, path=None, format=None, outputMode=None, partitionBy=None,
              queryName=None, **options):
        stream_df = getattr(self, "_emu_stream_df", None)
        if should_run_local_foreach(stream_df, self):
            print(FOREACH_ANNOUNCE, file=sys.stderr, flush=True)
            self._emu_foreach_batch(stream_df, 0)
            return OneShotStreamingQuery(name=queryName)
        fmt, held = _writer_fmt_opts(self, format, options)
        if should_produce(fmt, held):
            print(SINK_ANNOUNCE, file=sys.stderr, flush=True)
            produce_plain_kafka(collect_kafka_sink_rows(stream_df), held)
            return OneShotStreamingQuery(name=queryName)
        return orig_start(
            self, path=path, format=format, outputMode=outputMode,
            partitionBy=partitionBy, queryName=queryName, **options,
        )

    DataStreamReader.load = load
    DataFrameReader.load = batch_load
    DataFrameWriter.save = save
    DataStreamWriter.foreachBatch = foreachBatch
    DataStreamWriter.start = start
    # Replacing a property with a property is the whole point of this function,
    # and ty compares the wrapped types rather than the shape. Suppressed at the
    # site rather than by ignoring invalid-assignment for the package: the other
    # five patches above pass the check, so a blanket ignore would hide a real
    # mismatch in any patch added later.
    ConnectDF.writeStream = property(writeStream)  # ty: ignore[invalid-assignment]
    DataStreamReader._emu_eventstream_patched = True
    _connect_installed = True
    return True


def install(spark=None):  # noqa: ARG001 — session used to detect Connect
    """Install the Eventstream wrap for whichever engine this session is."""
    try:
        classic = _install_classic()
    except ImportError:
        classic = False
    try:
        connect = _install_connect(spark)
    except ImportError:
        connect = False
    return classic or connect

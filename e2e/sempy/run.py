#!/usr/bin/env python3
"""e2e: Microsoft's `sempy` driven against a listener we control.

WHY THIS EXISTS SEPARATELY FROM `e2e/xmla`. That suite drives a hand-written
ADOMD.NET program and answers what it demands. This one drives the library
Fabric notebooks and `semantic-link-labs` actually use, and a real client asks
for things a probe never does: `getDatabaseName` appears here and NOWHERE in
`e2e/xmla`, because a probe connects with a database name it already knows.

It is a CLIENT-CONTRACT oracle. The emulator implements no XMLA surface; what is
pinned here is the exact sequence a real SemPy workload demands, so the work is
sized against measurement rather than a specification.

WHAT IT ASSERTS, all measured off the wire:

  1. sempy is redirectable. `StaticFabricContext(pbi_shared_host=...)` points
     BOTH transports at a host we name — the REST base and the `powerbi://`
     XMLA endpoint — with no Fabric runtime, capacity or notebook.
  2. The REST half round-trips: `list_workspaces` and `list_datasets` return
     DataFrames.
  3. `evaluate_dax` is XMLA and sends `Execute` with the DAX in a `<Statement>`.
     It never touches `executeQueries`.
  4. The metadata path is TWO mechanisms IN SEQUENCE: `Discover`
     `DISCOVER_XML_METADATA` (`ObjectExpansion=ExpandObject`), then TMSCHEMA.
     TMSCHEMA itself arrives TWO WAYS and they are easy to conflate:
       * TOM (`list_measures`, `list_tables`) sends an `Execute` whose Command
         is a `<Batch>` of ~35 `<Discover RequestType=TMSCHEMA_*>` elements,
         restricted by `<DatabaseName>`. No SQL anywhere.
       * sempy's own Python (`list_columns`, `list_partitions`,
         `list_relationships`, `list_hierarchies`) sends SQL
         `SELECT ... FROM $SYSTEM.TMSCHEMA_*` through `evaluate_dax`.
     The SOAP verb is `Execute` in both cases; the GRAMMAR differs.
  5. Nine transport gates, each named by the client's own error. They are
     asserted individually below so that a regression names the gate that broke
     rather than "sempy failed".

WHAT IT DOES NOT ESTABLISH: what the `TMSCHEMA_*` rowsets must CONTAIN. The
stub answers the Discover and stops at the first DMV, which is the
`internal/semanticmodel` half and another session's claim.

PLATFORM. This cannot run natively on macOS arm64: pythonnet finds no .NET
runtime and `Microsoft.Fabric.SemanticLink.XmlaTools` fails to load. The whole
driver runs in a `linux/amd64` container. A skip that reads as success is the
failure this repo keeps producing, so `SEMPY_REQUIRE=1` turns every skip into a
failure.
"""
import fcntl
import http.server
import json
import os
import re
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading

DIR = os.path.dirname(os.path.abspath(__file__))
PORT = int(os.environ.get("SEMPY_PORT", "18447"))   # 18446 is e2e/xmla's
HOST = "host.docker.internal"
WS = "00000000-0000-0000-0000-000000000001"
DATASET = "ds"
MARKER = "sempy-e2e-marker-4c19"
SESSION = "sempy-session-1"
NS_ENGINE = "http://schemas.microsoft.com/analysisservices/2003/engine"
# READ off Microsoft.AnalysisServices.Tabular.dll, not guessed: three guesses
# gave three different rejections.
NS_COMPAT = "http://schemas.microsoft.com/analysisservices/2010/engine/200"
FORWARDER = "sempy-e2e-443"
RUN_LOCK = os.path.join(tempfile.gettempdir(), "sempy-e2e.lock")
REQUIRED = os.environ.get("SEMPY_REQUIRE") == "1"

REQS = []
LOCK = threading.Lock()
_lock_fd = None


def log(msg):
    print(f"==> {msg}", flush=True)


def skip_or_fail(reason):
    if REQUIRED:
        raise SystemExit(f"FAILED: {reason}\n  SEMPY_REQUIRE=1, so this may not skip.")
    print(f"SKIPPED: {reason}")
    raise SystemExit(0)


def require_sole_run():
    """One run at a time: the work dir, the forwarder name and 443 are exclusive.

    Same defect `e2e/xmla` hit — a second invocation's setup dismantles a live
    run — and the same fix. A distinct lock file and container name from that
    suite, so the two never destroy each other.
    """
    global _lock_fd
    _lock_fd = open(RUN_LOCK, "w")
    try:
        fcntl.flock(_lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        raise SystemExit(
            "another e2e/sempy run holds the lock; it cannot share its 443 "
            f"forwarder or work dir.\n  Wait, or clear a crashed one: rm {RUN_LOCK}")


def require_free_port(port):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        if s.connect_ex(("127.0.0.1", int(port))) == 0:
            raise SystemExit(
                f"port {port} is in use, so this harness cannot capture.\n"
                f"  Free it or override: SEMPY_PORT=<free> python3 e2e/sempy/run.py")


def rowset(response_element, cols, values):
    """One `xml-analysis:rowset`, for Execute OR Discover.

    THE INLINE XSD IS LOAD-BEARING: the client reads the schema before it reads
    a row, so an envelope without it fails as "unrecognizable" rather than
    "empty". Built from [MS-SSAS] `xmla-rs:rowset`, the documented surface.
    """
    xsd = "".join(
        f'<xsd:element sql:field="{c}" name="{c}" type="xsd:string" minOccurs="0"/>'
        for c in cols)
    row = "".join(f"<{c}>{v}</{c}>" for c, v in zip(cols, values))
    return (
        '<?xml version="1.0" encoding="utf-8"?>'
        '<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">'
        '<soap:Body>'
        f'<{response_element} xmlns="urn:schemas-microsoft-com:xml-analysis">'
        '<return>'
        '<root xmlns="urn:schemas-microsoft-com:xml-analysis:rowset" '
        'xmlns:xsd="http://www.w3.org/2001/XMLSchema" '
        'xmlns:sql="urn:schemas-microsoft-com:xml-sql" '
        'xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">'
        '<xsd:schema targetNamespace="urn:schemas-microsoft-com:xml-analysis:rowset" '
        'xmlns:xsd="http://www.w3.org/2001/XMLSchema" '
        'xmlns:sql="urn:schemas-microsoft-com:xml-sql" elementFormDefault="qualified">'
        '<xsd:element name="root"><xsd:complexType><xsd:sequence>'
        '<xsd:element name="row" type="row" minOccurs="0" maxOccurs="unbounded"/>'
        '</xsd:sequence></xsd:complexType></xsd:element>'
        '<xsd:complexType name="row"><xsd:sequence>' + xsd +
        '</xsd:sequence></xsd:complexType></xsd:schema>'
        f'<row>{row}</row>'
        '</root></return>'
        f'</{response_element}>'
        '</soap:Body></soap:Envelope>').encode() + b"\x00"


def assl(database_scoped):
    """ASSL for DISCOVER_XML_METADATA, embedded as XML rather than escaped.

    Root depends on the RestrictionList: `<DatabaseID>` present means the client
    is reading a `Tabular.Database`, absent means a `Tabular.Server`. The wrong
    root fails as `Unexpected root 'Server' ... when trying to read
    '...Tabular.Database'`.

    Two contract details read off `Microsoft.AnalysisServices.Tabular.dll`:
      * `CompatibilityLevel` is XmlElement in NS_COMPAT, NOT the 2003 engine
        namespace and NOT `.../200/200`. Below 1200 the client refuses the
        database as non-tabular.
      * `Model` is `XmlIgnore` — it must NOT appear; the tabular model does not
        travel on this path.
    """
    db = (f"<Database xmlns='{NS_ENGINE}'><Name>{DATASET}</Name><ID>{DATASET}</ID>"
          f"<ddl200:CompatibilityLevel xmlns:ddl200='{NS_COMPAT}'>1567"
          "</ddl200:CompatibilityLevel>"
          "<LastUpdate>2026-08-10T00:00:00Z</LastUpdate></Database>")
    if database_scoped:
        return db
    return (f"<Server xmlns='{NS_ENGINE}'><Name>ws</Name><ID>ws</ID>"
            f"<Databases><Database><Name>{DATASET}</Name><ID>{DATASET}</ID>"
            "<LastUpdate>2026-08-10T00:00:00Z</LastUpdate></Database></Databases>"
            "</Server>")



# TOM asks for the whole TMSCHEMA catalogue in ONE Execute whose Command is a
# <Batch> of ~35 <Discover> elements. The response is a `multipleresults`
# envelope holding one <root> per request, IN ORDER.
# Columns are the SCALAR properties of each TOM type, read off
# Microsoft.AnalysisServices.Tabular.dll. TOM reads them from the rowset BY
# NAME and says which one is missing — `ArgumentException: Column 'Culture'
# does not belong to table Model` — so the type's own property list is the
# column contract. `ID` and the parent FK are added because the rowsets are
# relational where the objects are nested.
_MODEL = ["Name", "Description", "StorageLocation", "DefaultMode",
          "DefaultDataView", "Culture", "Collation", "ModifiedTime",
          "StructureModifiedTime", "DefaultPowerBIDataSourceVersion",
          "ForceUniqueNames", "DiscourageImplicitMeasures",
          "DiscourageReportMeasures", "DataSourceVariablesOverrideBehavior",
          "DataSourceDefaultMaxConnections", "SourceQueryCulture",
          "DiscourageCompositeModels", "DisableAutoExists",
          "MaxParallelismPerRefresh", "MaxParallelismPerQuery"]
_TABLE = ["Name", "DataCategory", "Description", "IsHidden", "ModifiedTime",
          "StructureModifiedTime", "ShowAsVariationsOnly", "IsPrivate",
          "AlternateSourcePrecedence", "ExcludeFromModelRefresh", "LineageTag",
          "SourceLineageTag", "SystemManaged",
          "ExcludeFromAutomaticAggregations"]
_COLUMN = ["ExplicitName", "SourceColumn", "DataCategory", "Description",
           "IsHidden", "State", "IsUnique", "IsKey", "IsNullable", "Alignment",
           "TableDetailPosition", "IsDefaultLabel", "IsDefaultImage",
           "SummarizeBy", "Type", "FormatString", "IsAvailableInMDX",
           "ModifiedTime", "StructureModifiedTime", "RefreshedTime",
           "KeepUniqueRows", "DisplayOrdinal", "ErrorMessage",
           "SourceProviderType", "DisplayFolder", "EncodingHint", "LineageTag",
           "SourceLineageTag", "ExplicitDataType", "IsDataTypeInferred"]
_MEASURE = ["Name", "Description", "DataType", "Expression", "FormatString",
            "IsHidden", "State", "ModifiedTime", "StructureModifiedTime",
            "IsSimpleMeasure", "ErrorMessage", "DisplayFolder", "DataCategory",
            "LineageTag", "SourceLineageTag"]
_PARTITION = ["Name", "Description", "State", "Mode", "DataView",
              "ModifiedTime", "RefreshedTime", "ErrorMessage",
              "RetainDataTillForceCalculate", "Type"]



# XSD TYPE PER COLUMN, from each TOM property's CLR type. Declaring everything
# `xsd:string` gets the NAMES accepted and then fails as
# `InvalidCastException: Unable to cast System.String to System.UInt64` — the
# rowset's inline schema is a type contract, not just a column list.
# IDs and foreign keys are UInt64; enums cross the wire as their integer value.
_CLR = {
    "ModifiedTime": "DateTime", "StructureModifiedTime": "DateTime",
    "RefreshedTime": "DateTime",
    "DefaultMode": "enum", "DefaultDataView": "enum", "State": "enum",
    "Alignment": "enum", "SummarizeBy": "enum", "Type": "enum",
    "EncodingHint": "enum", "DataType": "enum", "Mode": "enum",
    "DataView": "enum", "SourceType": "enum", "ObjectType": "enum",
    "DefaultPowerBIDataSourceVersion": "enum",
    "DataSourceVariablesOverrideBehavior": "enum",
    "ForceUniqueNames": "Boolean", "DiscourageImplicitMeasures": "Boolean",
    "DiscourageReportMeasures": "Boolean", "DiscourageCompositeModels": "Boolean",
    "IsHidden": "Boolean", "ShowAsVariationsOnly": "Boolean",
    "IsPrivate": "Boolean", "ExcludeFromModelRefresh": "Boolean",
    "SystemManaged": "Boolean", "ExcludeFromAutomaticAggregations": "Boolean",
    "IsUnique": "Boolean", "IsKey": "Boolean", "IsNullable": "Boolean",
    "IsDefaultLabel": "Boolean", "IsDefaultImage": "Boolean",
    "IsAvailableInMDX": "Boolean", "KeepUniqueRows": "Boolean",
    "IsDataTypeInferred": "Boolean", "IsSimpleMeasure": "Boolean",
    "RetainDataTillForceCalculate": "Boolean", "IsRemoved": "Boolean",
    "DataSourceDefaultMaxConnections": "Int32", "DisableAutoExists": "Int32",
    "MaxParallelismPerRefresh": "Int32", "MaxParallelismPerQuery": "Int32",
    "AlternateSourcePrecedence": "Int32", "TableDetailPosition": "Int32",
    "DisplayOrdinal": "Int32",
}
_XSD = {"DateTime": "xsd:dateTime", "Boolean": "xsd:boolean",
        "Int32": "xsd:int", "enum": "xsd:int", "UInt64": "xsd:unsignedLong",
        "String": "xsd:string"}
_DEFAULT = {"xsd:dateTime": "2026-08-10T00:00:00", "xsd:boolean": "false",
            "xsd:int": "0", "xsd:unsignedLong": "0", "xsd:string": ""}


def _xsd_type(col):
    if col == "Version":
        # `xsd:long`, NOT unsignedLong. `DdlUtil.GetVersionFromDataTable` does
        # `Utils.Verify(obj is long)` on row 0 — an ASSERTION, so an
        # unsignedLong parses fine, fails the type test, and surfaces as
        # `TomInternalException: An internal error has occured` with no field
        # named. The only error in this investigation that did not name itself,
        # and it was one xsd type.
        return "xsd:long"
    if col == "ID" or col.endswith("ID"):
        return "xsd:unsignedLong"
    return _XSD[_CLR.get(col, "String")]

def _row(cols, overrides):
    return [overrides.get(c, _DEFAULT[_xsd_type(c)]) for c in cols]


POPULATED = {
    "TMSCHEMA_MODEL": (["ID"] + _MODEL,
                       [_row(["ID"] + _MODEL,
                             {"ID": "1", "Name": "Model", "Culture": "en-US"})]),
    "TMSCHEMA_TABLES": (["ID", "ModelID"] + _TABLE,
                        [_row(["ID", "ModelID"] + _TABLE,
                              {"ID": "1", "ModelID": "1", "Name": "Sales"})]),
    "TMSCHEMA_COLUMNS": (["ID", "TableID"] + _COLUMN,
                         [_row(["ID", "TableID"] + _COLUMN,
                               {"ID": "1", "TableID": "1",
                                "ExplicitName": "Amount"})]),
    "TMSCHEMA_MEASURES": (["ID", "TableID"] + _MEASURE,
                          [_row(["ID", "TableID"] + _MEASURE,
                                {"ID": "1", "TableID": "1", "Name": "Total",
                                 "Expression": "SUM(Sales[Amount])"})]),
    "TMSCHEMA_PARTITIONS": (["ID", "TableID"] + _PARTITION,
                            [_row(["ID", "TableID"] + _PARTITION,
                                  {"ID": "1", "TableID": "1", "Name": "p1"})]),
}
# `AmoDataAdapter.AdjustTableNames` renames the DataSet's tables from
# `XmlaDataReader.RowsetNames`, and each entry is literally
# `xmlReader.GetAttribute("name")` on <root>. Omit the attribute and every name
# is null, nothing is renamed, and `DdlUtil.ObtainModelTable`'s
# `dataSet.Tables["Model"]` is null — reported as "failed to discover the state
# of the model", a message about the MODEL for a defect in ROOT NAMING.
ROOT_NAMES = {
    "TMSCHEMA_MODEL": "Model", "TMSCHEMA_TABLES": "Table",
    "TMSCHEMA_COLUMNS": "Column", "TMSCHEMA_MEASURES": "Measure",
    "TMSCHEMA_PARTITIONS": "Partition", "TMSCHEMA_RELATIONSHIPS": "Relationship",
    "TMSCHEMA_HIERARCHIES": "Hierarchy",
}


def _root(request_type):
    # EVERY rowset must yield a DataTable: `AdjustTableNames` bails out
    # entirely if `RowsetNames.Count != dataSet.Tables.Count`, so a
    # column-less root that Fill skips silently breaks the naming for ALL
    # 35 — and the failure surfaces as "Tables['Model'] is null".
    cols, rows = POPULATED.get(request_type, (["ID", "Name"], []))
    # EVERY TMSCHEMA rowset carries a `Version` column — the client refuses one
    # without it: `ResponseFormatException: The rowset is missing a Version
    # column`. It is the metadata version TOM tracks state with, which is why
    # its absence reads as a malformed ROWSET rather than a missing field.
    if "Version" not in cols:
        cols = list(cols) + ["Version"]
        rows = [list(r) + ["1"] for r in rows]
    name = ROOT_NAMES.get(request_type,
                          request_type.replace("TMSCHEMA_", "").title())
    xsd = "".join(
        f'<xsd:element sql:field="{c}" name="{c}" type="{_xsd_type(c)}" '
        'minOccurs="0"/>'
        for c in cols)
    body = "".join("<row>" + "".join(f"<{c}>{v}</{c}>" for c, v in zip(cols, r))
                   + "</row>" for r in rows)
    return (
        f'<root name="{name}" xmlns="urn:schemas-microsoft-com:xml-analysis:rowset" '
        'xmlns:xsd="http://www.w3.org/2001/XMLSchema" '
        'xmlns:sql="urn:schemas-microsoft-com:xml-sql">'
        '<xsd:schema targetNamespace="urn:schemas-microsoft-com:xml-analysis:rowset" '
        'xmlns:xsd="http://www.w3.org/2001/XMLSchema" '
        'xmlns:sql="urn:schemas-microsoft-com:xml-sql" elementFormDefault="qualified">'
        '<xsd:element name="root"><xsd:complexType><xsd:sequence>'
        '<xsd:element name="row" type="row" minOccurs="0" maxOccurs="unbounded"/>'
        '</xsd:sequence></xsd:complexType></xsd:element>'
        '<xsd:complexType name="row"><xsd:sequence>' + xsd +
        '</xsd:sequence></xsd:complexType></xsd:schema>' + body + '</root>')


def batch_response(request_types):
    """One <root> per <Discover>, in request order.

    The container namespace is NOT a suffix on the xml-analysis URN family;
    bi-shared-docs `results-element-xmla.md` gives a different scheme entirely.
    A wrong one is refused at the envelope before anything inside is read.
    """
    return (
        '<?xml version="1.0" encoding="utf-8"?>'
        '<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">'
        '<soap:Body>'
        '<ExecuteResponse xmlns="urn:schemas-microsoft-com:xml-analysis">'
        '<return><results xmlns="http://schemas.microsoft.com/analysisservices'
        '/2003/xmla-multipleresults">'
        + "".join(_root(rt) for rt in request_types) +
        '</results></return></ExecuteResponse>'
        '</soap:Body></soap:Envelope>').encode() + b"\x00"


class Capture(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _body(self):
        # XMLA arrives chunked with no Content-Length. Reading Content-Length
        # alone records the envelope as empty, which reads as "the client sent
        # nothing" — an absent measurement rendered as a negative observation.
        if "chunked" in (self.headers.get("Transfer-Encoding", "") or "").lower():
            out = []
            while True:
                line = self.rfile.readline().strip()
                if not line:
                    continue
                n = int(line.split(b";")[0], 16)
                if n == 0:
                    self.rfile.readline()
                    break
                out.append(self.rfile.read(n))
                self.rfile.readline()
            return b"".join(out)
        n = int(self.headers.get("Content-Length", 0) or 0)
        return self.rfile.read(n) if n else b""

    def _record(self, body):
        with LOCK:
            REQS.append({"method": self.command, "path": self.path,
                         "headers": {k.lower(): v for k, v in self.headers.items()},
                         "body": body.decode("utf-8", "replace")})

    def _send(self, code, body, ctype="application/json", extra=None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._record(self._body())
        p = self.path
        if "/powerbi/databases/v201606/workspaces" in p:
            # ADOMD's ROUTING call. A BARE ARRAY, not a {"value": [...]}
            # envelope — an envelope fails as "workspace is not found".
            # `capacitySku` is an ENUM VALUE ("Premium"); an SKU name like "P1"
            # fails as `Requested value 'P1' was not found`. `capacityUri` needs
            # >= 1 PATH SEGMENT; a bare origin gives IndexOutOfRangeException.
            return self._send(200, json.dumps([{
                "id": WS, "name": WS, "type": "Group", "capacitySku": "Premium",
                "capacityUri": f"https://{HOST}:{PORT}/{WS}"}]).encode(),
                "application/json; charset=utf-8")
        if "/datasets" in p:
            return self._send(200, json.dumps({"value": [
                {"id": "11111111-1111-1111-1111-111111111111",
                 "name": DATASET}]}).encode())
        if "/groups" in p:
            # ECHO the requested name: sempy filters by `name eq '<x>'` and then
            # matches the result against what it asked for.
            m = re.search(r"name%20eq%20'([^']+)'|name eq '([^']+)'", p)
            want = (m.group(1) or m.group(2)) if m else WS
            return self._send(200, json.dumps({"value": [
                {"id": WS, "name": want, "type": "Workspace"}]}).encode())
        return self._send(404, b'{"error":"capture"}')

    def do_POST(self):
        body = self._body()
        self._record(body)
        p, text = self.path, body.decode("utf-8", "replace")
        if "getDatabaseName" in p:
            # DISCOVERED BY DRIVING A REAL CLIENT. Absent from e2e/xmla entirely.
            return self._send(200, json.dumps({"databaseName": DATASET}).encode())
        if "clusterResolve" in p:
            return self._send(200, json.dumps({
                "clusterFQDN": HOST, "coreServerName": "ws", "tenantId": WS}).encode())
        if "generateastoken" in p:
            return self._send(200, b'{"Token":"emulator-mwc-token"}')
        if "executeQueries" in p:
            return self._send(200, json.dumps(
                {"results": [{"tables": [{"rows": [{"[Value]": 1}]}]}]}).encode())
        if "/webapi/xmla" in p or p.endswith("/xmla"):
            caps = {"x-ms-xmlacaps-negotiation-flags": "0,0,0,0,0"}
            # ORDER MATTERS: a batched Execute CONTAINS <Discover> children, so
            # a broad `"<Discover" in text` check shadows it entirely.
            rts = re.findall(r"<RequestType>(TMSCHEMA_\w+)</RequestType>", text)
            if rts and "<Batch" in text:
                pop = sum(1 for t in rts if t in POPULATED)
                print(f"    -> batch of {len(rts)}: {pop} populated, "
                      f"{len(rts) - pop} empty", flush=True)
                return self._send(200, batch_response(rts),
                                  "text/xml; charset=utf-8", caps)
            if "<Discover" in text:
                dbid = re.search(r"<DatabaseID>([^<]*)</DatabaseID>", text)
                return self._send(200, rowset("DiscoverResponse", ["METADATA"],
                                              [assl(bool(dbid))]),
                                  "text/xml; charset=utf-8", caps)
            # Session handshake and any Execute we do not model: a bare
            # ExecuteResponse plus the trailing 0x00 protocol byte (0 =
            # complete, 1 = LRO continuation, anything else = a transport
            # protocol error).
            env = (
                '<?xml version="1.0" encoding="utf-8"?>'
                '<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">'
                '<soap:Header><Session xmlns="urn:schemas-microsoft-com:xml-analysis" '
                f'SessionId="{SESSION}"/></soap:Header><soap:Body>'
                '<ExecuteResponse xmlns="urn:schemas-microsoft-com:xml-analysis">'
                '<return><root xmlns="urn:schemas-microsoft-com:xml-analysis:empty"/>'
                '</return></ExecuteResponse></soap:Body></soap:Envelope>'
            ).encode() + b"\x00"
            return self._send(200, env, "text/xml; charset=utf-8", caps)
        return self._send(404, b'{"error":"capture"}')

    def log_message(self, *a):
        pass


class TLSServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


# --------------------------------------------------------------------------
# Run
# --------------------------------------------------------------------------
require_sole_run()
require_free_port(PORT)
if not shutil.which("docker"):
    skip_or_fail("docker is not on PATH; sempy's XMLA path needs a linux/amd64 "
                 ".NET runtime and cannot run on this host directly")
if not shutil.which("openssl"):
    skip_or_fail("openssl is not on PATH, so no TLS cert can be issued")

WORK = tempfile.mkdtemp(prefix="sempy-e2e-")
subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048",
                "-keyout", os.path.join(WORK, "key.pem"),
                "-out", os.path.join(WORK, "cert.pem"),
                "-days", "2", "-nodes", "-subj", f"/CN={HOST}",
                "-addext", f"subjectAltName=DNS:{HOST},DNS:localhost,IP:127.0.0.1"],
               check=True, capture_output=True)

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.minimum_version = ssl.TLSVersion.TLSv1_2
ctx.load_cert_chain(os.path.join(WORK, "cert.pem"), os.path.join(WORK, "key.pem"))
srv = TLSServer(("0.0.0.0", PORT), Capture)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
threading.Thread(target=srv.serve_forever, daemon=True).start()
log(f"capture listener on :{PORT}")

# The client dials 443 whatever port the Data Source names — the endpoint
# address is DERIVED. Distinct container name from e2e/xmla's `xmla-e2e-443`:
# a shared name means one suite's cleanup kills the other's forwarder.
subprocess.run(["docker", "rm", "-f", FORWARDER], capture_output=True)
fwd = subprocess.run(
    ["docker", "run", "-d", "--rm", "--name", FORWARDER,
     "--add-host", f"{HOST}:host-gateway", "-p", "443:443", "alpine/socat",
     "TCP-LISTEN:443,fork,reuseaddr", f"TCP:{HOST}:{PORT}"],
    capture_output=True, text=True)
if fwd.returncode:
    srv.shutdown()
    skip_or_fail("could not publish 443; the client dials it after routing, so "
                 "nothing past the token would be captured")
log("443 forwarder up")

log("building the sempy image (cached after the first run)")
build = subprocess.run(
    ["docker", "build", "--platform", "linux/amd64", "-q", "-t", "sempy-e2e:local",
     os.path.join(DIR, "image")], capture_output=True, text=True)
if build.returncode:
    subprocess.run(["docker", "rm", "-f", FORWARDER], capture_output=True)
    srv.shutdown()
    raise SystemExit("FAILED: could not build the sempy image\n" + build.stderr[-800:])

log("running sempy (linux/amd64)")
proc = subprocess.run([
    "docker", "run", "--rm", "--platform", "linux/amd64",
    "--add-host", f"{HOST}:host-gateway",
    "-v", f"{os.path.join(DIR, 'driver.py')}:/driver.py:ro",
    "-v", f"{os.path.join(WORK, 'cert.pem')}:/usr/local/share/ca-certificates/sempy.crt:ro",
    "-e", f"SEMPY_HOST=https://{HOST}:{PORT}/", "-e", f"SEMPY_WORKSPACE={WS}",
    "-e", f"SEMPY_DATASET={DATASET}", "-e", f"SEMPY_MARKER={MARKER}",
    "sempy-e2e:local", "sh", "-c",
    "update-ca-certificates >/dev/null 2>&1; "
    "REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt "
    "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt /v/bin/python /driver.py",
], capture_output=True, text=True, timeout=1800)
srv.shutdown()
subprocess.run(["docker", "rm", "-f", FORWARDER], capture_output=True)

out = [ln[3:] for ln in proc.stdout.splitlines() if ln.startswith("###")]
if not out:
    raise SystemExit("FAILED: the driver produced no cases — that is a missing "
                     "measurement, not a result.\n" + proc.stderr[-1200:])
print("\n---- driver ----", flush=True)
for ln in out:
    print("  " + ln, flush=True)

results = dict(
    (m.group(1), m.group(2))
    for m in (re.match(r"RESULT (\S+) :: (\S+)", ln) for ln in out) if m)
bodies = "\n".join(r["body"] for r in REQS)
paths = [r["path"] for r in REQS]
xmla = [r for r in REQS if "xmla" in r["path"]]


def verbs(kind):
    return [r for r in xmla if f"<{kind}" in r["body"]]


CHECKS = [
    ("sempy is redirectable: REST calls arrive here",
     any("/v1.0/myorg/" in p for p in paths)),
    ("the auth control confirms OUR token, decoded",
     any(ln.startswith("AUTH mine=True") for ln in out)),
    ("list_workspaces returns a DataFrame over REST",
     results.get("list_workspaces") == "OK"),
    ("list_datasets returns a DataFrame (needs the ASSL Discover)",
     results.get("list_datasets") == "OK"),
    ("routing: the client asks for workspaces on the ADOMD path",
     any("/powerbi/databases/v201606/workspaces" in p for p in paths)),
    ("getDatabaseName is issued, with its documented body",
     any("getDatabaseName" in p for p in paths)
     and '"datasetName"' in bodies and '"workspaceType"' in bodies),
    ("the client reaches XMLA at all", len(xmla) > 0),
    ("evaluate_dax sends Execute with the DAX in a Statement",
     "<Statement>EVALUATE {1}</Statement>" in bodies),
    ("evaluate_dax does NOT use executeQueries",
     not any("executeQueries" in p for p in paths)),
    ("Discover asks for DISCOVER_XML_METADATA with ExpandObject",
     "DISCOVER_XML_METADATA" in bodies and "ExpandObject" in bodies),
    ("both Discover scopes appear (server-level and DatabaseID-scoped)",
     any("<DatabaseID>" not in r["body"] for r in verbs("Discover"))
     and any("<DatabaseID>" in r["body"] for r in verbs("Discover"))),
    # NAMED PRECISELY: the SOAP verb is Execute, but the payload is a BATCH of
    # Discover elements, not SQL. An earlier version of this check said "as an
    # Execute", which is true on the verb and misleading about the grammar —
    # and it misled another session's design before being caught.
    ("TMSCHEMA arrives as a Batch of Discover inside one Execute",
     "TMSCHEMA" in bodies
     and any("<Batch" in r["body"] and "TMSCHEMA" in r["body"]
             and "<Discover" in r["body"] for r in xmla)),
    ("TOM batches the whole TMSCHEMA catalogue in one round trip",
     max((len(re.findall(r"<RequestType>TMSCHEMA_", r["body"])) for r in xmla),
         default=0) >= 30),
    ("the TMSCHEMA batch is restricted by DatabaseName",
     any("<Batch" in r["body"] and "<DatabaseName>" in r["body"] for r in xmla)),
]

print("\n---- contract ----", flush=True)
failed = [name for name, ok in CHECKS if not ok]
for name, ok in CHECKS:
    print(f"  {'PASS' if ok else 'FAIL'}  {name}", flush=True)

print(f"\n{len(REQS)} request(s) captured, {len(xmla)} of them XMLA", flush=True)
if failed:
    raise SystemExit("FAILED: " + str(len(failed)) + " contract check(s) regressed:\n  "
                     + "\n  ".join(failed))
print("\nPASSED: the SemPy client contract is unchanged.")

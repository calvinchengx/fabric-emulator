"""A stand-in for a Salesforce org's OAuth and Bulk API 2.0 surfaces.

Faithful in the places that decide whether the connector works against a real
org rather than against a convenient fiction:

1. **Bulk API 2.0 is a LIFECYCLE, not a request.** A query job is created, spends
   time `InProgress`, and only then serves results. A connector that fetched
   results immediately would work against a canned mock and hang or 404 against
   a real org.

2. **Result paging ends on the literal string `"null"`** in `Sforce-Locator` —
   not an absent header, not an empty one. Get it wrong one way and you loop
   forever; the other way and you fetch a page named `null`.

3. **Ingest is four calls**: create, `PUT` the CSV (not POST), `PATCH` to
   `UploadComplete`, then poll. A job left `Open` accepts the upload and never
   processes it — success with nothing written.

4. **`#N/A` means NULL; an empty field means "leave unchanged".** The org records
   what it received so the driver can assert which one arrived.

Everything is in memory. No credentials, no network beyond the compose project.
"""

import csv
import io
import json
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

CLIENT_ID = "fabric-emulator-e2e"
CLIENT_SECRET = "local-only"
TOKEN = "00D000000000000!sf-bulk-e2e-token"
API = "v59.0"
PAGE = 2

# 5 accounts against a page size of 2 gives 3 pages — so a connector that read
# only the first, or stopped one short, is visible in the row count.
ACCOUNTS = [
    {"Id": f"001000000000{i:03d}", "Name": name, "Industry": industry}
    for i, (name, industry) in enumerate([
        ("Acme Corporation", "Manufacturing"),
        ("Globex", "Technology"),
        ("Initech", "Technology"),
        ("Umbrella Health", "Healthcare"),
        ("Soylent Foods", "Agriculture"),
    ])
]

QUERY_JOBS = {}   # id -> {"polls": n, "operation": op, "query": soql}
INGEST_JOBS = {}  # id -> {"state": s, "rows": [...], "object": o, "operation": op}


def _pages():
    """The query result pages, keyed by the locator that fetches them."""
    out, locators = {}, []
    chunks = [ACCOUNTS[i:i + PAGE] for i in range(0, len(ACCOUNTS), PAGE)]
    for i in range(len(chunks)):
        locators.append("" if i == 0 else f"LOC{i}")
    for i, chunk in enumerate(chunks):
        buf = io.StringIO()
        w = csv.DictWriter(buf, fieldnames=["Id", "Name", "Industry"], lineterminator="\n")
        w.writeheader()
        w.writerows(chunk)
        nxt = locators[i + 1] if i + 1 < len(locators) else "null"
        out[locators[i]] = (buf.getvalue(), nxt)
    return out


PAGES = _pages()


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, body, ctype="application/json", headers=None):
        raw = body.encode() if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(raw)

    def _body(self):
        return self.rfile.read(int(self.headers.get("Content-Length") or 0))

    def _authed(self):
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            self._send(401, json.dumps([{"errorCode": "INVALID_SESSION_ID",
                                         "message": "Session expired or invalid"}]))
            return False
        return True

    # -- OAuth ------------------------------------------------------------
    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path

        if path == "/services/oauth2/token":
            form = urllib.parse.parse_qs(self._body().decode())
            if (form.get("grant_type") != ["client_credentials"]
                    or form.get("client_id") != [CLIENT_ID]
                    or form.get("client_secret") != [CLIENT_SECRET]):
                self._send(400, json.dumps({"error": "invalid_client"}))
                return
            self._send(200, json.dumps({
                "access_token": TOKEN, "instance_url": "", "token_type": "Bearer"}))
            return

        if not self._authed():
            return

        if path == f"/services/data/{API}/jobs/query":
            spec = json.loads(self._body() or b"{}")
            job_id = f"750Q{len(QUERY_JOBS):06d}"
            QUERY_JOBS[job_id] = {"polls": 0, "operation": spec.get("operation"),
                                  "query": spec.get("query")}
            self._send(200, json.dumps({"id": job_id, "state": "UploadComplete",
                                        "operation": spec.get("operation")}))
            return

        if path == f"/services/data/{API}/jobs/ingest":
            spec = json.loads(self._body() or b"{}")
            job_id = f"750I{len(INGEST_JOBS):06d}"
            INGEST_JOBS[job_id] = {"state": "Open", "rows": [], "object": spec.get("object"),
                                   "operation": spec.get("operation"),
                                   "externalIdFieldName": spec.get("externalIdFieldName")}
            self._send(200, json.dumps({
                "id": job_id, "state": "Open",
                # Relative, which is what a real org usually returns.
                "contentUrl": f"services/data/{API}/jobs/ingest/{job_id}/batches"}))
            return

        self._send(404, json.dumps({"error": "no such endpoint", "path": path}))

    # -- upload -----------------------------------------------------------
    def do_PUT(self):
        if not self._authed():
            return
        path = urllib.parse.urlparse(self.path).path
        parts = path.strip("/").split("/")
        if not path.endswith("/batches") or len(parts) < 2:
            self._send(404, json.dumps({"error": "no such endpoint"}))
            return
        job_id = parts[-2]
        job = INGEST_JOBS.get(job_id)
        if job is None:
            self._send(404, json.dumps({"error": "unknown job"}))
            return
        # Bulk 2.0 requires text/csv; rejecting anything else is what proves the
        # connector sets it rather than defaulting to JSON.
        if self.headers.get("Content-Type") != "text/csv":
            self._send(415, json.dumps({"error": "expected text/csv"}))
            return
        rows = list(csv.DictReader(io.StringIO(self._body().decode())))
        job["rows"].extend(rows)
        self._send(201, "")

    # -- close ------------------------------------------------------------
    def do_PATCH(self):
        if not self._authed():
            return
        parts = urllib.parse.urlparse(self.path).path.strip("/").split("/")
        job = INGEST_JOBS.get(parts[-1])
        if job is None:
            self._send(404, json.dumps({"error": "unknown job"}))
            return
        state = json.loads(self._body() or b"{}").get("state")
        if state == "UploadComplete":
            # Only NOW does the org consider the rows submitted. A connector that
            # never PATCHes leaves the job Open and nothing is ever processed.
            job["state"] = "JobComplete"
        self._send(200, json.dumps({"id": parts[-1], "state": job["state"]}))

    # -- status and results -----------------------------------------------
    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        path = url.path

        # The driver reads this back to assert what the org actually received.
        if path == "/_debug/state":
            self._send(200, json.dumps({
                "queryJobs": QUERY_JOBS,
                "ingestJobs": {k: v for k, v in INGEST_JOBS.items()},
            }))
            return

        if not self._authed():
            return
        parts = path.strip("/").split("/")

        if path.endswith("/results"):
            job_id = parts[-2]
            if job_id not in QUERY_JOBS:
                self._send(404, json.dumps({"error": "unknown job"}))
                return
            loc = urllib.parse.parse_qs(url.query).get("locator", [""])[0]
            if loc not in PAGES:
                self._send(400, json.dumps({"error": f"unknown locator {loc!r}"}))
                return
            body, nxt = PAGES[loc]
            self._send(200, body, ctype="text/csv",
                       headers={"Sforce-Locator": nxt,
                                "Sforce-NumberOfRecords": str(body.count("\n") - 1)})
            return

        if "/jobs/query/" in path:
            job = QUERY_JOBS.get(parts[-1])
            if job is None:
                self._send(404, json.dumps({"error": "unknown job"}))
                return
            job["polls"] += 1
            # Spend time InProgress, as a real job does.
            state = "JobComplete" if job["polls"] > 2 else "InProgress"
            self._send(200, json.dumps({"id": parts[-1], "state": state}))
            return

        if "/jobs/ingest/" in path:
            job = INGEST_JOBS.get(parts[-1])
            if job is None:
                self._send(404, json.dumps({"error": "unknown job"}))
                return
            self._send(200, json.dumps({
                "id": parts[-1], "state": job["state"],
                "numberRecordsProcessed": len(job["rows"]), "numberRecordsFailed": 0}))
            return

        self._send(404, json.dumps({"error": "no such endpoint", "path": path}))

    def log_message(self, *_):
        pass


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

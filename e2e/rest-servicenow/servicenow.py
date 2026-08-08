"""A stand-in for ServiceNow's Table API.

ServiceNow is not an arbitrary choice of second REST target. It is the case
**Microsoft's own REST-connector documentation teaches pagination with** —
Example 1 of connector-rest's "Pagination rules examples" is literally

    baseUrl/api/now/table/incident?sysparm_limit=1000&sysparm_offset=0,
    baseUrl/api/now/table/incident?sysparm_limit=1000&sysparm_offset=1000, ...

with the rule `"QueryParameters.{offset}" : "RANGE:0:10000:1000"`. So this
suite proves the connector against the shape its own documentation uses to
explain itself, which `e2e/rest-helix` deliberately could not: Helix has no
Fabric connector at all, and its paging shape was *inherited* from this one.

**Faithful where it decides the outcome, and different from Helix on purpose:**

1. **Records are FLAT.** A real Table API answers
   `{"result":[{"number":"INC0010001","short_description":"…",…}]}` — scalars
   directly under each record. Helix nests under `values`, which is why that
   pipeline needs `translator.mappings`. Here auto-flattening must work with no
   mappings at all, so the two suites exercise opposite halves of row-shaping.

2. **`$.result` is the collection.** Not `$.entries`, not a bare array. This is
   inside the emulator's documented `collectionReference` JSONPath subset, and
   the response also carries no second array — so the no-mapping,
   single-array-inference path is what gets tested.

3. **Both documented paging routes are live.** `sysparm_offset`/`sysparm_limit`
   for the RANGE rule, and an RFC 5988 `Link` header (`rel="next"`, and `last`
   on the first page as the real API sends) so `SupportRFC5988` has something
   real to follow. A connector implementing only one route still passes one
   half and fails the other.

4. **Basic auth, the scheme the real connector uses.** Fabric's built-in
   ServiceNow connector is Basic-only — which is exactly why a user needing
   OAuth reaches for RestSource instead (see internal/api/restconnector.go).
   Anonymous and wrong credentials are both refused, so a pass cannot mean the
   endpoint let anyone in.
"""

import base64
import json
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

USER, PASSWORD = "fabric.emulator", "local-only"
BASIC = "Basic " + base64.b64encode(f"{USER}:{PASSWORD}".encode()).decode()

# 5 incidents against a page size of 2 gives 2 + 2 + 1: a connector that reads
# one page reports 2, one page too few reports 4, and only a complete read
# reports 5. The count is the assertion, so truncation cannot look plausible.
INCIDENTS = [
    {
        "number": f"INC001000{i + 1}",
        "short_description": desc,
        "state": state,
        "priority": str(priority),
        "sys_id": f"{i + 1:032x}",
    }
    for i, (desc, state, priority) in enumerate(
        [
            ("Laptop will not boot", "2", 2),
            ("VPN drops every 10 minutes", "2", 3),
            ("Mailbox quota exceeded", "6", 4),
            ("Badge reader offline in B2", "1", 1),
            ("Printer jam, 3rd floor", "7", 4),
        ]
    )
]
PAGE = 2


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, payload, link=None):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if link:
            self.send_header("Link", link)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        if url.path != "/api/now/table/incident":
            self.send_error(404, "only /api/now/table/incident is modelled")
            return

        # Negative control. Without this an unauthenticated pass would prove
        # nothing about whether the connector sent credentials at all.
        auth = self.headers.get("Authorization", "")
        if not auth:
            self.send_error(401, "no credentials")
            return
        if auth != BASIC:
            self.send_error(401, "bad credentials")
            return

        q = urllib.parse.parse_qs(url.query)
        try:
            limit = int(q.get("sysparm_limit", [str(PAGE)])[0])
            offset = int(q.get("sysparm_offset", ["0"])[0])
        except ValueError:
            self.send_error(400, "sysparm_limit and sysparm_offset must be integers")
            return

        page = INCIDENTS[offset : offset + limit]

        # RFC 5988. The real API sends first/next/last; `next` is absent on the
        # final page, which is what ends a SupportRFC5988 read.
        base = f"http://servicenow:8080/api/now/table/incident?sysparm_limit={limit}"
        links = [f'<{base}&sysparm_offset=0>;rel="first"']
        if offset + limit < len(INCIDENTS):
            links.append(f'<{base}&sysparm_offset={offset + limit}>;rel="next"')
        last = ((len(INCIDENTS) - 1) // limit) * limit
        links.append(f'<{base}&sysparm_offset={last}>;rel="last"')

        self._json(200, {"result": page}, link=",".join(links))

    def log_message(self, *_):
        pass


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

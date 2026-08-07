"""A stand-in for BMC Helix ITSM's AR System REST API.

Small on purpose, and faithful in the three places that decide whether the
emulator's REST connector really works against Helix rather than against a
convenient fiction:

1. **Login returns a RAW JWT**, not JSON. `POST /api/jwt/login` takes
   form-encoded credentials and answers with the token as the entire body — so
   a pipeline reads it as `@activity('Login').output.body`, not `.token`.

2. **The scheme is `AR-JWT`, not `Bearer`.** Requests carrying `Bearer <token>`
   are rejected exactly as the real server rejects them. This is what makes the
   emulator's `additionalHeaders` path load-bearing: Helix's scheme is not one
   of Fabric's built-in authentication types, so the token has to be threaded
   through as an expression.

3. **Records nest under `values`.** A real entry query answers
   `{"entries":[{"values":{…},"_links":{…}}]}`, which is why the pipeline needs
   `translator.mappings` — auto-flattening finds no scalar columns in that shape.

Paging is `limit`/`offset`, the same shape Microsoft's own REST-connector
documentation uses for its ServiceNow example.
"""

import json
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

TOKEN = "helix-ar-jwt-token-for-the-e2e"
FORM = "HPD:Help Desk"

# 5 incidents against a page size of 2 gives 2 + 2 + 1 + an empty page — enough
# that a connector reading only the first page, or one page too few, is visible
# in the row count rather than plausible.
INCIDENTS = [
    {"Incident Number": f"INC00000000{700 + i}",
     "Description": desc,
     "Status": status,
     "Priority": priority}
    for i, (desc, status, priority) in enumerate([
        ("Laptop will not boot", "Assigned", 2),
        ("VPN drops every 10 minutes", "In Progress", 3),
        ("Mailbox quota exceeded", "Resolved", 4),
        ("Badge reader offline in B2", "Assigned", 1),
        ("Printer jam, 3rd floor", "Closed", 4),
    ])
]


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if not self.path.startswith("/api/jwt/login"):
            self.send_error(404)
            return
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
        form = urllib.parse.parse_qs(raw)
        if form.get("username") != ["fabric-emulator"] or form.get("password") != ["local-only"]:
            self.send_error(401, "bad credentials")
            return
        # The raw token IS the body. Real ARS does this; a pipeline that expected
        # JSON here would work against a mock and fail against Helix.
        body = TOKEN.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        want = "/api/arsys/v1/entry/" + urllib.parse.quote(FORM)
        if urllib.parse.unquote(url.path) != urllib.parse.unquote(want):
            self.send_error(404)
            return

        auth = self.headers.get("Authorization", "")
        if not auth.startswith("AR-JWT "):
            # Deliberately strict: `Bearer <token>` is rejected here as it is by
            # the real server, so the e2e proves the scheme, not just the token.
            self.send_error(401, "expected the AR-JWT scheme")
            return
        if auth.removeprefix("AR-JWT ").strip() != TOKEN:
            self.send_error(401, "unknown token")
            return

        q = urllib.parse.parse_qs(url.query)
        try:
            limit = int(q.get("limit", ["2"])[0])
            offset = int(q.get("offset", ["0"])[0])
        except ValueError:
            self.send_error(400, "limit and offset must be integers")
            return

        page = INCIDENTS[offset:offset + limit]
        self._json(200, {"entries": [{"values": v, "_links": {"self": []}} for v in page]})

    def log_message(self, *_):
        pass


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

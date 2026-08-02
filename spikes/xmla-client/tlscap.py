import http.server, ssl
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _log(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(n) if n else b""
        print(f"\n=== {self.command} {self.path} ===", flush=True)
        for k, v in self.headers.items(): print(f"  {k}: {v}", flush=True)
        if body: print("--- BODY ---\n" + body.decode("utf-8","replace")[:3000], flush=True)
    def do_POST(self):
        self._log()
        b = b'<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><soap:Fault><faultcode>spike</faultcode><faultstring>capture only</faultstring></soap:Fault></soap:Body></soap:Envelope>'
        self.send_response(500); self.send_header("Content-Type","text/xml"); self.send_header("Content-Length",str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        self._log(); self.send_response(404); self.send_header("Content-Length","0"); self.end_headers()
    def log_message(self,*a): pass
srv = http.server.HTTPServer(("0.0.0.0",18080), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); ctx.load_cert_chain("cert.pem","key.pem")
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
print("TLS listener on 18080", flush=True); srv.serve_forever()

from http.server import BaseHTTPRequestHandler, HTTPServer

DATA = {
    "/adls/container/folder/data.txt": b"from-adls-gen2",
    "/s3/bucket/prefix/data.txt": b"from-amazon-s3",
}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = DATA.get(self.path)
        if body is None:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()

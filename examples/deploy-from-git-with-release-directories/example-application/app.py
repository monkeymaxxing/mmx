"""The greeter: answers every request with a greeting and its release."""
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import six  # From requirements.txt, installed into the release's virtual environment.

RELEASE = (Path(__file__).parent / "RELEASE").read_text().strip()


class Greeter(BaseHTTPRequestHandler):
    def do_GET(self):
        body = six.ensure_binary(f"Hello from release {RELEASE}\n")
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


HTTPServer(("127.0.0.1", 8080), Greeter).serve_forever()

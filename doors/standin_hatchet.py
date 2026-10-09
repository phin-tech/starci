"""A stand-in for Hatchet's webhook address, for testing doors.

It does what Hatchet does with a delivery: checks the forge's signature over
the body, and answers 200 or 403. It writes down what it was sent, so a test
can see that a door relayed a delivery unchanged.
"""

import hashlib
import hmac
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET, LOG = sys.argv[2].encode(), sys.argv[3]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path == "/broken":
            self.send_response(500)
            self.end_headers()
            return
        given = (self.headers.get("X-Forgejo-Signature") or self.headers.get("X-Hub-Signature-256") or "").removeprefix("sha256=")
        ok = hmac.compare_digest(given, hmac.new(SECRET, body, hashlib.sha256).hexdigest())
        with open(LOG, "a") as f:
            f.write(json.dumps({
                "ok": ok,
                "body": body.decode(),
                "event": self.headers.get("X-Forgejo-Event") or self.headers.get("X-GitHub-Event"),
                "content_type": self.headers.get("Content-Type"),
                "leaked": sorted(h.lower() for h in self.headers if h.lower() in ("cookie", "authorization", "x-forwarded-for")),
            }) + "\n")
        self.send_response(200 if ok else 403)
        self.end_headers()


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()

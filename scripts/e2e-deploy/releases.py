#!/usr/bin/env python3
"""Fake GitHub releases for the deploy e2e.

Serves the directory given as the first argument on 127.0.0.1:8000, and
answers /releases/latest like GitHub: a redirect to /releases/tag/<tag>,
where <tag> is the content of the LATEST file.
"""
import functools
import http.server
import os
import sys

root = sys.argv[1]


class Handler(http.server.SimpleHTTPRequestHandler):
    def _latest(self):
        if self.path != "/releases/latest":
            return False
        with open(os.path.join(root, "LATEST")) as f:
            tag = f.read().strip()
        self.send_response(302)
        self.send_header("Location", f"http://127.0.0.1:8000/releases/tag/{tag}")
        self.end_headers()
        return True

    def do_HEAD(self):
        if not self._latest():
            super().do_HEAD()

    def do_GET(self):
        if not self._latest():
            super().do_GET()


handler = functools.partial(Handler, directory=root)
http.server.ThreadingHTTPServer(("127.0.0.1", 8000), handler).serve_forever()

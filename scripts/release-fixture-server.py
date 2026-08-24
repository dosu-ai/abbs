#!/usr/bin/env python3
"""Serve installer fixtures and emulate GitHub's /releases/latest redirect."""

from __future__ import annotations

import http.server
import pathlib
import socketserver
import sys


class FixtureServer(http.server.ThreadingHTTPServer):
    """Threaded HTTP server that skips the stdlib's reverse-DNS lookup.

    http.server.HTTPServer.server_bind calls socket.getfqdn() during
    construction. On some hosts (notably GitHub's macOS runners) that reverse
    lookup blocks for several seconds, which delays writing the address file
    past the caller's readiness timeout and looks like the server never
    started. We only need the bound port, so record it without resolving a
    hostname.
    """

    def server_bind(self) -> None:
        socketserver.TCPServer.server_bind(self)
        host, port = self.server_address[:2]
        self.server_name = host if isinstance(host, str) else host.decode()
        self.server_port = port


def main() -> None:
    if len(sys.argv) != 4:
        raise SystemExit("usage: release-fixture-server.py ROOT TAG ADDRESS_FILE")

    root = pathlib.Path(sys.argv[1]).resolve()
    tag = sys.argv[2]
    address_file = pathlib.Path(sys.argv[3])

    class Handler(http.server.SimpleHTTPRequestHandler):
        def __init__(self, *args, **kwargs):
            super().__init__(*args, directory=str(root), **kwargs)

        def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            if self.path.rstrip("/") == "/releases/latest":
                self.send_response(302)
                self.send_header("Location", f"/releases/tag/{tag}")
                self.end_headers()
                return
            if self.path.rstrip("/") == f"/releases/tag/{tag}":
                self.send_response(200)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.end_headers()
                self.wfile.write(f"abbs {tag}\n".encode())
                return
            super().do_GET()

        def log_message(self, _format: str, *_args: object) -> None:
            pass

    server = FixtureServer(("127.0.0.1", 0), Handler)
    address_file.write_text(f"http://127.0.0.1:{server.server_port}", encoding="utf-8")
    server.serve_forever()


if __name__ == "__main__":
    main()

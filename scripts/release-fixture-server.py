#!/usr/bin/env python3
"""Serve installer fixtures and emulate GitHub's /releases/latest redirect."""

from __future__ import annotations

import http.server
import pathlib
import sys


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

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    address_file.write_text(f"http://127.0.0.1:{server.server_port}", encoding="utf-8")
    server.serve_forever()


if __name__ == "__main__":
    main()

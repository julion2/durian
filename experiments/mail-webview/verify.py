#!/usr/bin/env python3
"""Real WebKit + reachable tracking canary; uses only the synthetic fixture."""
import http.server
import pathlib
import subprocess
import sys
import threading
import urllib.request


hits = []


class Canary(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        hits.append(self.path)
        self.send_response(200)
        self.send_header("Content-Type", "text/css")
        self.end_headers()
        self.wfile.write(b"body { color: red !important }")

    def log_message(self, *_):
        pass


binary = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "target/debug/durian-mail-webview").resolve()
with http.server.ThreadingHTTPServer(("127.0.0.1", 9876), Canary) as server:
    threading.Thread(target=server.serve_forever, daemon=True).start()
    with urllib.request.urlopen("http://127.0.0.1:9876/control") as control:
        assert control.status == 200
    assert hits == ["/control"], "Canary must be reachable before testing"
    hits.clear()
    try:
        for flags in ([], ["--dark"], ["--narrow"], ["--dark", "--narrow"]):
            result = subprocess.run([str(binary), "--self-test", *flags], capture_output=True, text=True, timeout=30)
            print(result.stdout.strip())
            if result.returncode:
                print(result.stderr, file=sys.stderr)
                raise SystemExit(result.returncode)
            assert not hits, f"Mail leaked network requests: {hits}"
        print("PASS: 4 WebKit states; 0 tracking/CSS requests (reachable canary)")
    finally:
        server.shutdown()

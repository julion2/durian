#!/usr/bin/env python3
"""Loopback-only fault injection for native tests; never logs headers or content."""
import http.client
import http.server
import json
from pathlib import Path
import socket
import sys
import threading
import time
from urllib.parse import parse_qs, urlsplit

upstream_port, control_path, log_path, ready_path = sys.argv[1:]
lock = threading.Lock()


class Proxy(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def hold_response(self):
        Path(control_path + ".held").touch()
        deadline = time.monotonic() + 18
        while not Path(control_path + ".release").exists():
            if time.monotonic() >= deadline:
                raise TimeoutError("test response barrier expired")
            time.sleep(0.02)

    def forward(self):
        path = self.path.split("?", 1)[0]
        with lock:
            control = Path(control_path)
            fault = control.read_text().strip().split() if control.exists() else []
            mode = fault[2] if len(fault) == 3 and fault[:2] == [self.command, path] else ""
            if mode:
                control.write_text("")
        status = None
        scope = None
        connection = http.client.HTTPConnection("127.0.0.1", int(upstream_port), timeout=60)
        try:
            if mode == "before":
                return
            if mode == "reject":
                status = 503
                self.send_error(status, "Injected failure before dispatch")
                return
            size = int(self.headers.get("Content-Length", "0"))
            if size > 2 * 1024 * 1024:
                self.send_error(413)
                return
            body = self.rfile.read(size)
            if path == "/api/v1/calendars/sync/event":
                # Synthetic fixture identities only. Never dispatch to a provider.
                payload = json.loads(body)
                scope = {key: payload.get(key) for key in ("account", "calendar", "uid", "operation")}
                status = 503
                self.send_error(status, "Provider sync disabled in native test")
                return
            headers = {k: v for k, v in self.headers.items() if k.lower() not in {"host", "connection"}}
            headers["Connection"] = "close"
            connection.request(self.command, self.path, body, headers)
            response = connection.getresponse()
            status = response.status
            if mode == "after":
                response.read()  # the real write completed; discard its acknowledgment
                return
            streaming = path == "/api/v1/events"
            payload = None if streaming else response.read()
            if mode == "hold-reject":
                status = 503
                payload = b'{"ok":false,"error":"Injected startup search failure"}'
            if mode == "hold-headers":
                self.hold_response()
            self.send_response(status)
            for key, value in response.getheaders():
                if key.lower() not in {"connection", "transfer-encoding", "content-length"}:
                    self.send_header(key, value)
            self.send_header("Connection", "close")
            if payload is not None:
                self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if mode in {"hold", "hold-reject"}:
                # Freeze the response body, not the Go server, while native input
                # can issue newer requests. Tests release this explicit barrier.
                self.wfile.flush()
                self.hold_response()
            if payload is not None:
                self.wfile.write(payload)
            else:
                while data := response.read1(65536):
                    self.wfile.write(data)
                    self.wfile.flush()
        except (OSError, http.client.HTTPException) as error:
            print(f"proxy {self.command} {path}: {type(error).__name__}", file=sys.stderr)
        finally:
            connection.close()
            with lock, Path(log_path).open("a") as log:
                query = parse_qs(urlsplit(self.path).query).get("query", [None])[0]
                log.write(json.dumps({"method": self.command, "path": path, "query": query, "status": status, "fault": mode, "scope": scope}) + "\n")
            self.close_connection = True
            if mode in {"before", "after"}:
                try:
                    self.connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

    do_GET = do_POST = do_PUT = do_DELETE = forward


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Proxy)
Path(ready_path).write_text(str(server.server_port))
server.serve_forever()

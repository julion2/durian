#!/usr/bin/env python3
"""Exercise the real offscreen browser and IPC, not a browser screenshot mock."""
import base64
import http.server
import json
import os
import pathlib
import queue
import struct
import subprocess
import sys
import threading
import time
import urllib.request
import zlib


def png():
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 8, 8, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress((b"\0" + bytes([23, 137, 211]) * 8) * 8)) + chunk(b"IEND", b""))


hits = []


class Canary(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        hits.append(self.path)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"blocked")

    def log_message(self, *_):
        pass


def check(binary, dark, width, canary):
    env = dict(os.environ, LD_LIBRARY_PATH=str(binary.parent))
    process = subprocess.Popen([str(binary)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, env=env)
    events = queue.Queue()

    def read():
        try:
            while True:
                length = process.stdout.read(4)
                if not length:
                    return
                event = json.loads(process.stdout.read(struct.unpack("<I", length)[0]))
                if event["kind"] == "frame":
                    count = event["width"] * event["height"] * 4
                    assert 0 < count <= 64 * 1024 * 1024
                    event["pixels"] = process.stdout.read(count)
                    assert len(event["pixels"]) == count
                events.put(event)
        except Exception as error:
            events.put({"kind": "reader_error", "error": str(error)})

    threading.Thread(target=read, daemon=True).start()

    def send(kind, **values):
        process.stdin.write((json.dumps(dict(kind=kind, **values)) + "\n").encode())
        process.stdin.flush()

    def until(predicate):
        deadline = time.monotonic() + 15
        observed = []
        while time.monotonic() < deadline:
            try:
                event = events.get(timeout=0.2)
            except queue.Empty:
                assert process.poll() is None, process.stderr.read().decode(errors="replace")
                continue
            assert event["kind"] not in ("error", "reader_error"), event
            if predicate(event):
                return event
            observed.append({k: v for k, v in event.items() if k != "pixels"})
        raise AssertionError(f"Timed out waiting for browser state; last events: {observed[-5:]}")

    def pixel(frame, x, y):
        offset = (y * frame["width"] + x) * 4
        b, g, r, a = frame["pixels"][offset:offset + 4]
        return r, g, b, a

    html = f'''<style>
        @import url("{canary}/css");
        body {{ margin:0!important; font:16px/20px sans-serif }}
        .grid {{ display:grid;grid-template-columns:25% 75%;height:40px }}
        .one {{ background:rgb(210,60,30) }} .two {{ background:rgb(20,150,80) }}
        .remote {{background-image:url("{canary}/background");height:20px}}
        </style><body>
        <div class="grid"><div class="one"></div><div class="two"></div></div>
        <p style="margin:0;height:40px">Grüße, 日本語 — selectable mail</p>
        <img src="cid:verified" style="display:block;width:64px;height:64px">
        <div class="remote"></div><div style="height:1500px;background:linear-gradient(to bottom,transparent 536px,rgb(30,80,160) 536px,rgb(30,80,160) 556px,transparent 556px)">Long message</div>
        <a href="https://example.com/review?part=2&amp;mode=full">Review notes</a>
        <img src="{canary}/pixel"><script>fetch('{canary}/script')</script>
        </body>'''
    try:
        send("open", html=html, dark=dark, images={"verified": base64.b64encode(png()).decode()})
        height = until(lambda e: e["kind"] == "height")
        assert 1680 <= height["height"] <= 1720, height
        send("viewport", width=width, height=240, scale=2.0, y=0.0)
        frame = until(lambda e: e["kind"] == "frame" and e["width"] == width * 2
                      and e["height"] == 480 and pixel(e, 20, 190)[:3] == (23, 137, 211))
        assert pixel(frame, 20, 190) == (23, 137, 211, 255), "CID colors must not be inverted"
        background = pixel(frame, width * 2 - 10, 320)[:3]
        assert background == ((42, 42, 44) if dark else (255, 255, 255)), background
        if not dark:
            assert pixel(frame, width // 2 - 2, 10)[:3] == (210, 60, 30), "25% grid column"
            assert pixel(frame, width // 2 + 2, 10)[:3] == (20, 150, 80), "75% grid column"
        send("mouse", x=1, y=50, button=True, down=True, count=1)
        send("mouse", x=150, y=50, button=True, down=None, count=1)
        send("mouse", x=150, y=50, button=False, down=False, count=1)
        dragged = until(lambda e: e["kind"] == "selection" and "Grüße" in e["text"])
        assert "Review notes" not in dragged["text"], "Mouse coordinates must select only the first line"
        send("select_all")
        selection = until(lambda e: e["kind"] == "selection" and "日本語" in e["text"] and "Review notes" in e["text"])
        assert "Grüße" in selection["text"] and "Review notes" in selection["text"]
        send("viewport", width=width, height=240, scale=2.0, y=700.0)
        scrolled = until(lambda e: e["kind"] == "frame" and e["y"] == 700.0)
        assert pixel(scrolled, 10, 10)[:3] == (30, 80, 160), "CSS y=700 must paint the marker, not device y=1400"
        send("key", code=9, shift=False)
        send("key", code=13, shift=False)
        link = until(lambda e: e["kind"] == "link")
        assert link["url"] == "https://example.com/review?part=2&mode=full", link
        assert not hits, hits
        send("close")
        process.wait(timeout=5)
        assert process.returncode == 0, process.stderr.read().decode(errors="replace")
        print(f"PASS: {'dark' if dark else 'light'} {width}px @2x: CSS grid, CID, colors, Unicode selection, scroll, keyboard link, clean shutdown")
    finally:
        if process.poll() is None:
            process.stdin.close()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


binary = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "target/debug/durian-mail-cef").resolve()
with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Canary) as server:
    url = f"http://127.0.0.1:{server.server_port}"
    threading.Thread(target=server.serve_forever, daemon=True).start()
    with urllib.request.urlopen(url + "/control") as control:
        assert control.status == 200
    assert hits == ["/control"]
    hits.clear()
    try:
        for dark in [False, True]:
            for width in [640, 320]:
                check(binary, dark, width, url)
        print("PASS: 4 real CEF states; 0 tracking/CSS requests (reachable canary)")
    finally:
        server.shutdown()

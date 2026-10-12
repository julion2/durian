"""Real packaged WebKit + public Rust sanitizer; never raw mail or public services.

Private mount namespace supplies only this test's CA without changing system trust
or turning off TLS verification. Both allowed and denied requests hit local traps.
"""
import base64
import contextlib
import http.server
import io
import json
import os
from pathlib import Path
import select
import shutil
import signal
import ssl
import subprocess
import sys
import tempfile
import threading
import time

from PIL import Image

HELPER, PREPARE = (str(Path(p).resolve()) for p in sys.argv[1:3])


def prepare(html, **kw):
    result = subprocess.run([PREPARE], input=json.dumps(dict(html=html, **kw)), text=True, capture_output=True, check=True)
    return json.loads(result.stdout)


def color(path, xy, expected):
    with Image.open(path) as image:
        actual = image.convert("RGB").getpixel(xy)
        assert all(abs(a - b) < 12 for a, b in zip(actual, expected)), (xy, actual, expected)


def line(process, timeout=20):
    assert select.select([process.stdout], [], [], timeout)[0], "helper deadline"
    data = process.stdout.readline()
    assert data, f"helper EOF {process.poll()}"
    return json.loads(data)


def send(process, root, id, html, output=None):
    request = dict(id=id, html=html, width=664, out=str(root / (output or id + ".jpg")))
    process.stdin.write((json.dumps(request) + "\n").encode())
    process.stdin.flush()


@contextlib.contextmanager
def helper(root, ca, executable=None, extra=None):
    runtime = root / "runtime"
    runtime.mkdir(mode=0o700, exist_ok=True)
    env = dict(os.environ, HOME=str(root), TMPDIR=str(root), XDG_RUNTIME_DIR=str(runtime),
               XDG_CACHE_HOME=str(root / "cache"), XDG_CONFIG_HOME=str(root / "config"), **(extra or {}))
    # No WebKit debug/trust bypass variables. The unchanged library verifies TLS.
    command = ["bwrap", "--unshare-user", "--ro-bind", "/", "/", "--proc", "/proc", "--dev-bind", "/dev", "/dev",
               "--bind", str(root), str(root), "--ro-bind", str(ca), "/etc/ssl/certs/ca-certificates.crt", "--", executable or HELPER]
    with open(root / "helper.log", "ab") as log:
        process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log,
                                   env=env, start_new_session=True, bufsize=0)
        try:
            yield process
        finally:
            # Same owned process-group boundary as the Rust client, including on timeout.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=5)
            process.stdin.close()
            process.stdout.close()
            time.sleep(.15)
            ps = subprocess.check_output(["ps", "-eo", "pgid=,stat="], text=True)
            assert not any(int(p) == process.pid and not s.startswith("Z") for p, s in (r.split() for r in ps.splitlines()))


with tempfile.TemporaryDirectory(prefix="durian-render-") as tmp:
    root = Path(tmp)
    # Test a relocated package; a library symlink outside this directory breaks
    # both distribution and the web-process sandbox's read-only exposure.
    package = root / "package"
    package.mkdir()
    assert not (Path(HELPER).parent / "mailrender-extensions" / "libdurian-policy.so").is_symlink()
    shutil.copy(HELPER, package / "mailrender")
    shutil.copytree(Path(HELPER).parent / "mailrender-extensions", package / "mailrender-extensions")
    HELPER = str(package / "mailrender")
    out = Path(os.environ.get("DURIAN_UI_ARTIFACT_DIR", os.environ.get("TEST_UNDECLARED_OUTPUTS_DIR", tmp)))
    out.mkdir(parents=True, exist_ok=True)
    ca, key = root / "ca.pem", root / "key.pem"
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost", "-keyout", str(key), "-out", str(ca)],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    buf = io.BytesIO()
    Image.new("RGB", (90, 70), (35, 183, 107)).save(buf, "PNG")
    png = buf.getvalue()
    hits = []
    release_stall = threading.Event()

    class Trap(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            hits.append((self.server.scheme, self.path))
            if self.path == "/stall":
                release_stall.wait(25)
                return
            elif self.path == "/redirect":
                self.send_response(302)
                self.send_header("Location", f"http://localhost:{plain.server_port}/redirect-leak")
                self.end_headers()
            else:
                self.send_response(200)
                self.send_header("Content-Type", "image/png")
                self.send_header("Content-Length", str(len(png)))
                self.end_headers()
                self.wfile.write(png)

        def log_message(self, *args):
            pass

    plain = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Trap)
    tls = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Trap)
    plain.scheme, tls.scheme = "http", "https"
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(ca, key)
    tls.socket = ctx.wrap_socket(tls.socket, server_side=True)
    for server in (plain, tls):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    https = f"https://localhost:{tls.server_port}"
    http = f"http://localhost:{plain.server_port}"
    try:
        with helper(root, ca) as process:
            assert line(process)["ready"] is True
            document = prepare('''<body style="padding:0"><table cellspacing="0" cellpadding="0" style="width:600px;height:180px">
              <tr><td style="width:200px;background:#47abcd">Invoice</td><td style="background:#e49c42">42.00</td></tr></table>
              <img src="cid:logo"><p><a href="https://example.invalid/invoice">Open invoice</a></p></body>''',
                               images={"logo": "data:image/png;base64," + base64.b64encode(png).decode()})
            # Same output path, concurrent jobs: atomic publication must not lose a reply.
            for i in range(3):
                send(process, root, f"invoice-{i}", document, "invoice.jpg")
            answers = [line(process) for _ in range(3)]
            assert {a["id"] for a in answers} == {f"invoice-{i}" for i in range(3)}
            for answer in answers:
                assert answer["ok"] and answer["width"] == 664 and not answer["clipped"]
                assert answer["links"][0]["href"] == "https://example.invalid/invoice"
                assert answer["links"][0]["w"] > 30 and answer["links"][0]["y"] > 180
            image = root / "invoice.0.jpg"
            color(image, (120, 20), (71, 171, 205))
            color(image, (480, 20), (228, 156, 66))
            color(image, (30, 210), (35, 183, 107))
            assert image.stat().st_mode & 0o777 == 0o600
            shutil.copy(image, out / "table-cid.jpg")
            print("PASS real table/CSS/CID pixels, link geometry, three concurrent atomic outputs", flush=True)

            send(process, root, "long", prepare('<body style="padding:0"><div style="height:19000px;background:#47abcd">Long</div></body>'))
            long = line(process)
            assert long["height"] == 16000 and long["clipped"] and len(long["strips"]) == 4
            for i, strip in enumerate(long["strips"]):
                assert strip["y"] == i * 4000 and strip["height"] == 4000
                assert Image.open(strip["path"]).size == (664, 4000)
                color(strip["path"], (120, 3900), (71, 171, 205))
            print("PASS 16000px clipping and four nonblank strips", flush=True)

            # Authenticated positive image control and traps distinguish network denial
            # from a broken renderer/network stack. Scripts/imports never reach the trap.
            markup = f'''<body style="padding:0"><script>fetch('{https}/script')</script>
              <meta http-equiv="refresh" content="0;url={https}/navigation"><iframe src="{https}/frame"></iframe>
              <style>@import url('{https}/import'); @font-face{{font-family:trap;src:url('{https}/font')}}body{{font-family:trap}}</style>
              <img src="{https}/allowed" width="90" height="70"><img src="{http}/http-image">
              <div style="width:100px;height:100px;background:url('{https}/background')">Control</div>
              <img src="file:///etc/passwd"><img src="{https}/redirect"> </body>'''
            send(process, root, "blocked", prepare(markup))
            blocked = line(process)
            assert blocked["ok"] and blocked["height"] < 400  # shrink again after the long document
            assert hits == [], hits
            send(process, root, "enabled", prepare(markup, remote_images=True))
            enabled = line(process)
            assert enabled["ok"]
            color(root / "enabled.0.jpg", (30, 30), (35, 183, 107))
            assert sorted(hits) == [("https", "/allowed"), ("https", "/background"), ("https", "/redirect")], hits
            # HTTPS image URLs may redirect to HTTPS images; an HTTP downgrade is denied.
            assert not any(scheme == "http" for scheme, _ in hits)
            print("PASS default no network; enabled HTTPS image pixels; scripts/imports/frame/navigation/HTTP downgrade denied", flush=True)

            # Wrong hostname still fails certificate verification despite test CA trust.
            before = list(hits)
            send(process, root, "bad-cert", prepare(f'<img src="https://127.0.0.1:{tls.server_port}/bad-cert">', remote_images=True))
            assert line(process)["ok"] and hits == before
            start = time.monotonic()
            send(process, root, "stalled", prepare(f'<img src="{https}/stall">', remote_images=True))
            failure = line(process)
            assert failure["ready"] is False and failure["error"] == "render timed out", failure
            assert 14 <= time.monotonic() - start < 20
            assert ("https", "/stall") in hits
            print("PASS TLS hostname rejection and real stalled-image 15s deadline", flush=True)

        # Existing extension must be present, and sandbox disabling is a refusal.
        isolated = root / "missing" / "mailrender"
        isolated.parent.mkdir()
        shutil.copy(HELPER, isolated)
        with helper(root, ca, str(isolated)) as process:
            assert line(process)["ready"] is False
            assert process.wait(timeout=5) != 0
        with helper(root, ca, extra={"WEBKIT_DISABLE_SANDBOX_THIS_IS_DANGEROUS": "1"}) as process:
            assert line(process)["ready"] is False
        print("PASS missing-policy and unsafe-sandbox refusal; owned process groups terminated", flush=True)
    finally:
        release_stall.set()
        for server in (plain, tls):
            server.shutdown()
            server.server_close()
        if out != root:
            shutil.copy(root / "helper.log", out / "helper.log")

#!/usr/bin/python3
"""Read-only supplemental API timings/errors; NOT a GUI startup measurement."""
import argparse
import json
from pathlib import Path
import selectors
import socket
import subprocess
import time
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

from run import environment


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cli", type=Path, required=True)
    parser.add_argument("--corpus", type=Path, required=True)
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    env = environment(args.work.resolve(), args.corpus.resolve())
    stats = json.loads((args.corpus / "stats.json").read_text())
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    start = time.perf_counter()
    child = subprocess.Popen([str(args.cli.resolve()), "serve", "--port", str(port), "--exit-when-orphaned"],
                             env=env, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    events = []
    try:
        selector = selectors.DefaultSelector()
        selector.register(child.stdout, selectors.EVENT_READ)
        if not selector.select(timeout=10):
            raise RuntimeError("diagnostic owned CLI READY timed out")
        # Raw READY and token stay in memory; never print or serialize them.
        line = child.stdout.readline().strip()
        if not line.startswith("READY "):
            raise RuntimeError("diagnostic CLI did not report READY")
        fields = dict(part.split("=", 1) for part in line.split()[1:])
        if fields.get("api") != "1" or fields.get("addr") != f"127.0.0.1:{port}" or not fields.get("token"):
            raise RuntimeError("invalid diagnostic READY protocol")
        ready_ms = (time.perf_counter() - start) * 1000
        for endpoint, query in [("/profiles", None), ("/profiles", None),
                                ("/search", "tag:inbox"), ("/search/count", "BenchBody"),
                                ("/search", "BenchBody"),
                                ("/search/count", stats["SelectedBodyMarker"])]:
            params = {} if query is None else {"query": query}
            if endpoint == "/search":
                params.update(limit=7 if query == "BenchBody" else 200, enrich=0)
            request = Request("http://" + fields["addr"] + "/api/v1" + endpoint + "?" + urlencode(params),
                              headers={"Authorization": "Bearer " + fields["token"]})
            at = time.perf_counter()
            try:
                response = urlopen(request, timeout=20)
            except HTTPError as failure:
                response = failure
            with response:
                raw = response.read().decode()
                try:
                    body = json.loads(raw)
                except ValueError:
                    body = {"error": raw.strip().replace(fields["token"], "[REDACTED]")}
            events.append({"endpoint": endpoint, "query": query, "status": response.status,
                           "duration_ms": round((time.perf_counter() - at) * 1000, 3),
                           "error": body.get("error"), "count": body.get("count"),
                           "limit": params.get("limit"),
                           "rows": len(body["results"]) if body.get("results") is not None else None,
                           "threads": [r.get("thread", r.get("thread_id")) for r in body.get("results") or []]})
    finally:
        child.terminate()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait()
    result = {"ready_ms": round(ready_ms, 3), "events": events, "exit": child.returncode,
              "definition": "supplemental warm-filesystem owned-CLI API diagnostic, not GUI startup"}
    args.output.write_text(json.dumps(result, indent=2))
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()

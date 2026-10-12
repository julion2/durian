#!/usr/bin/python3
"""Observe local benchmark HTTP requests/responses without logging auth headers.

Run with CAP_NET_RAW/root on the isolated benchmark display. Raw packets remain
only in memory. Exports endpoint/query/status/error/timing, NEVER headers or tokens.
This diagnostic observer is separate from the startup timing matrix.
"""
import argparse
from collections import deque
import json
from pathlib import Path
import signal
import socket
import struct
import time
from urllib.parse import parse_qs, urlsplit


class Flow:
    def __init__(self):
        self.next = None
        self.pending = {}
        self.buffer = b""
        self.ignored = False

    def add(self, sequence, payload):
        if self.ignored:
            return
        if self.next is None:
            self.next = sequence
        if sequence < self.next:
            payload = payload[self.next - sequence:]
            sequence = self.next
        if not payload:
            return
        self.pending[sequence] = payload
        while self.next in self.pending:
            part = self.pending.pop(self.next)
            self.buffer += part
            self.next += len(part)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--seconds", type=float, default=30)
    args = parser.parse_args()
    stop = False

    def finish(*unused):
        nonlocal stop
        stop = True
    signal.signal(signal.SIGTERM, finish)
    signal.signal(signal.SIGINT, finish)
    sock = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0800))
    sock.bind(("lo", 0))
    # SO_RCVBUFFORCE (Linux, CAP_NET_ADMIN) applies to this socket only. The
    # ordinary option is capped at a small system limit and drops GSO frames.
    sock.setsockopt(socket.SOL_SOCKET, 33, 16 * 1024 * 1024)
    sock.settimeout(0.2)
    flows, requests, events = {}, {}, []
    started = time.perf_counter()
    print("Local sanitized HTTP observer ready", flush=True)
    while not stop and time.perf_counter() - started < args.seconds:
        try:
            # GSO loopback frames can exceed 64KiB including the Ethernet header.
            packet, address = sock.recvfrom(262144)
        except socket.timeout:
            continue
        # Loopback reports outgoing and incoming copies; keep only incoming.
        if address[2] == socket.PACKET_OUTGOING or len(packet) < 54:
            continue
        total = struct.unpack("!H", packet[16:18])[0]
        ip = packet[14:14 + total]
        if ip[0] >> 4 != 4 or ip[9] != 6 or ip[12:20] != b"\x7f\x00\x00\x01" * 2:
            continue
        tcp = ip[(ip[0] & 15) * 4:]
        if len(tcp) < 20:
            continue
        source, destination, sequence = struct.unpack("!HHI", tcp[:8])
        payload = tcp[(tcp[12] >> 4) * 4:]
        if not payload:
            continue
        key = (source, destination)
        flow = flows.setdefault(key, Flow())
        flow.add(sequence, payload)
        while not flow.ignored:
            end = flow.buffer.find(b"\r\n\r\n")
            if end < 0:
                break
            header = flow.buffer[:end].decode("latin1")
            lines = header.split("\r\n")
            # Only local Durian HTTP. Discard all other traffic immediately.
            if not lines[0].startswith(("GET /api/v1/", "HTTP/1.")):
                flow.ignored = True
                flow.buffer = b""
                break
            lengths = [int(line.split(":", 1)[1].strip()) for line in lines[1:] if line.lower().startswith("content-length:")]
            chunked = any(line.lower() == "transfer-encoding: chunked" for line in lines[1:])
            if any("text/event-stream" in line.lower() for line in lines[1:]):
                # SSE connections aren't reused. Do not retain their bodies.
                flow.ignored = True
                flow.buffer = b""
                break
            consumed = end + 4
            if chunked:
                parts, complete = [], False
                while True:
                    line_end = flow.buffer.find(b"\r\n", consumed)
                    if line_end < 0:
                        break
                    size = int(flow.buffer[consumed:line_end].split(b";", 1)[0], 16)
                    if len(flow.buffer) < line_end + 2 + size + 2:
                        break
                    parts.append(flow.buffer[line_end + 2:line_end + 2 + size])
                    consumed = line_end + 2 + size + 2
                    if size == 0:
                        complete = True
                        break
                if not complete:
                    break
                body = b"".join(parts)
            else:
                size = lengths[0] if lengths else 0
                if len(flow.buffer) < consumed + size:
                    break
                body = flow.buffer[consumed:consumed + size]
                consumed += size
            flow.buffer = flow.buffer[consumed:]
            now = (time.perf_counter() - started) * 1000
            if lines[0].startswith("GET "):
                target = urlsplit(lines[0].split()[1])
                params = parse_qs(target.query)
                event = {"request_ms": round(now, 3), "endpoint": target.path,
                         "query": params.get("query", [None])[0], "status": None}
                events.append(event)
                requests.setdefault(key, deque()).append(event)
            else:
                waiting = requests.get((destination, source))
                if not waiting:
                    continue
                event = waiting.popleft()
                event["status"] = int(lines[0].split()[1])
                event["response_ms"] = round(now, 3)
                event["duration_ms"] = round(now - event["request_ms"], 3)
                if event["status"] >= 400:
                    try:
                        event["error"] = json.loads(body).get("error")
                    except (ValueError, AttributeError):
                        event["error"] = body.decode("utf8", errors="replace").strip()
                elif event["endpoint"].endswith("/search/count"):
                    event["count"] = json.loads(body).get("count")
    received, dropped = struct.unpack("II", sock.getsockopt(263, 6, 8))
    sock.close()
    counts = [e for e in events if e["endpoint"].endswith("/search/count")]
    args.output.write_text(json.dumps({"events": events,
                                      "failed_counts": [e for e in counts if e["status"] is not None and e["status"] >= 400],
                                      "unanswered_counts": [e for e in counts if e["status"] is None],
                                      "packet_stats": {"received": received, "dropped": dropped}}, indent=2))
    print(f"Observed {len(events)} sanitized local API requests", flush=True)


if __name__ == "__main__":
    main()

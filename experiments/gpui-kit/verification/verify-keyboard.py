#!/usr/bin/env python3
"""Synthetic-only, own-PID Mac keyboard acceptance. Run only after approval.
Compile keyboard.swift into ./keyboard after approval. No source edits, wheel,
global key posting, desktop capture, or changes to accessibility permissions.
AX must expose Search and its focused editor; otherwise report UNVERIFIED.
"""
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time
import uuid

HERE = pathlib.Path(__file__).resolve().parent
HELPER = HERE / "keyboard"
COMMAND = 1 << 20
OPTION = 1 << 19
CONTROL = 1 << 18
SHIFT = 1 << 17
QUERY = "zzdurianprobe"
binary = pathlib.Path(sys.argv[1]).resolve()
private = pathlib.Path(tempfile.mkdtemp(prefix="pasteboard-", dir=HERE))
private.chmod(0o700)
backup = private / "pasteboard-private.json"

def helper(*args):
    return subprocess.check_output([str(HELPER), *map(str, args)], text=True, timeout=8).strip()

def clipboard():
    return subprocess.check_output(["pbpaste"], text=True, timeout=5)

def assert_mail(sentinel):
    value = clipboard()
    assert "The revised dashboard mockups are ready for review." in value
    assert "Could we take 30 minutes on Tuesday" in value
    assert sentinel not in value and "DURIAN_TEST_SENTINEL_" not in value
    print("PASS HTML clipboard fixture assertion (contents suppressed)", flush=True)

process = None
helper("layout")  # Fail before any clipboard mutation if the layout is unsupported.
helper("save", backup)
try:
    env = os.environ.copy()
    for variable, suffix in [("HOME", "demo-home"), ("XDG_CONFIG_HOME", "demo-home/config"),
                             ("XDG_DATA_HOME", "demo-home/data"), ("XDG_CACHE_HOME", "demo-home/cache"),
                             ("TMPDIR", "scratch")]:
        path = HERE / suffix
        path.mkdir(parents=True, exist_ok=True)
        env[variable] = str(path)
    with (HERE / "native-test.log").open("w") as log:
        process = subprocess.Popen([str(binary)], cwd=binary.parent, env=env, stdout=log, stderr=log)
        pid = process.pid
        def alive():
            assert process.poll() is None, "isolated app exited unexpectedly"
        def key(code, flags=0, wait=0.6):
            alive()
            helper("chord" if isinstance(code, str) else "key", pid, code, flags)
            time.sleep(wait)
        def copy():
            sentinel = "DURIAN_TEST_SENTINEL_" + uuid.uuid4().hex
            helper("sentinel", sentinel)
            key("a", COMMAND)
            key("c", COMMAND)
            return sentinel
        def search_state(expected, label):
            deadline = time.monotonic() + 5
            while True:
                alive()
                state = json.loads(helper("ax", pid))
                print(label, state, flush=True)
                if not state["available"]:
                    raise RuntimeError("UNVERIFIED: AX unavailable; cannot independently prove Search dialog/focus")
                if state["search"] == expected and (not expected or state["search_editor_focused"]):
                    print("PASS", label, flush=True)
                    return
                if time.monotonic() >= deadline:
                    raise RuntimeError("UNVERIFIED: " + label + " AX/dialog semantics not proven")
                time.sleep(0.25)
        time.sleep(8)
        alive()
        helper("activate", pid)
        time.sleep(1)
        # Establish a positive control without ever focusing WebKit first.
        print("ROOT before Search", json.loads(helper("ax", pid)), flush=True)
        helper("text", pid, "/")
        time.sleep(1.5)
        search_state(True, "Root Search positive control")
        helper("text", pid, QUERY)
        time.sleep(1)
        copy()
        assert clipboard() == QUERY, "Root Search input copy differs from query"
        print("PASS Root Search query/input/copy", flush=True)
        key(53, wait=1)
        search_state(False, "Root Search Escape")
        key("v", OPTION, 1)
        assert_mail(copy())
        # Deliberately no settling wait between slash and initial query.
        # Active-layout physical keys match GPUI's native event reconstruction.
        helper("burst", pid, QUERY)
        time.sleep(1)
        search_state(True, "WK burst Search")
        sentinel = copy()
        value = clipboard()
        assert value == QUERY and sentinel not in value, "Search input copy differs from complete rapid query"
        print("PASS complete rapid Search query clipboard (synthetic)", flush=True)
        key(53, wait=1)
        search_state(False, "WK Search Escape")
        key("v", OPTION, 1)
        assert_mail(copy())
        key("t", CONTROL | SHIFT, 2)
        key("v", OPTION, 1)
        assert_mail(copy())
        key("q", CONTROL, 0.1)
        assert process.wait(timeout=8) == 0
        print("PASS normal CtrlQ exit=0", flush=True)
finally:
    try:
        if process is not None and process.poll() is None:
            # Only the process this invocation created; never name-based cleanup.
            try:
                helper("terminate", process.pid)
            except (subprocess.SubprocessError, OSError):
                pass  # bounded PID-only SIGTERM fallback below
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.terminate()
                try:
                    process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=3)
            print("CLEANUP isolated PID exited", flush=True)
    finally:
        # Helper returns success only after full Item/Type/Data readback equality.
        # On restore failure the 0600 backup is retained; never delete evidence.
        helper("restore", backup)
        backup.unlink()
        private.rmdir()
        print("CLEANUP complete pasteboard restored", flush=True)

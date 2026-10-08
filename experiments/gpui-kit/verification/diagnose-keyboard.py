#!/usr/bin/env python3
"""Diagnostic collection, not acceptance. Existing demo binaries need no rebuild.

Only run on macOS after the agreed UI window. No live arguments, global events,
screenshots or permission changes. Missing AX/trace evidence stays UNVERIFIED.
"""
import json
import os
import pathlib
import signal
import subprocess
import sys
import tempfile
import time
import uuid

QUERY = "zzdurianprobe"
COMMAND, OPTION, CONTROL, SHIFT = 1 << 20, 1 << 19, 1 << 18, 1 << 17


def classify(copied, ax, events, tracing):
    """Report the last observed boundary, never infer a product bug from silence."""
    def has(stage, field=None):
        return any(e.get("stage") == stage and
                   (field is None or e.get("details", {}).get(field) is True) for e in events)

    ax_focus = ax.get("available") and ax.get("search") and ax.get("search_editor_focused")
    if copied:
        return "QUERY_VERIFIED" if ax_focus else "QUERY_VERIFIED_AX_UNVERIFIED"
    if has("search.input", "probe_matches"):
        return "QUERY_RECEIVED_COPY_UNVERIFIED"
    if has("search.open") or ax.get("search"):
        return "SEARCH_OBSERVED_INPUT_UNVERIFIED"
    if has("gpui.received", "slash"):
        return "GPUI_RECEIVED_SEARCH_UNOBSERVED"
    if has("native.key", "slash"):
        return "NATIVE_RECEIVED_GPUI_UNOBSERVED"
    return "INPUT_UNOBSERVED" if tracing else "UNVERIFIED_NO_TRACE"


def html_boundary(events):
    """Last explicit focus request only; an earlier ready/focus is not proof."""
    start = next((i for i in range(len(events) - 1, -1, -1)
                  if events[i].get("stage") == "html.request"), None)
    if start is None:
        return "HTML_REQUEST_UNOBSERVED"
    request = events[start].get("details", {})
    if request.get("content_exists") is False:
        return "HTML_CONTENT_ABSENT"
    details = {e["stage"]: e.get("details", {}) for e in events[start:]}
    content = details.get("html.content", {})
    if content.get("plain") or content.get("has_html") is False:
        return "HTML_CONTENT_NOT_HTML"
    if content.get("images_loading"):
        return "HTML_IMAGES_LOADING"
    if content.get("embedded") is False:
        return "HTML_EMBED_ABSENT"
    focus = details.get("html.focus", {})
    if not focus:
        return "HTML_FOCUS_UNOBSERVED"
    if focus.get("native") is False:
        return "HTML_NATIVE_ABSENT"
    if focus.get("ready") is False:
        return "HTML_DOCUMENT_NOT_READY"
    if focus.get("hidden") or focus.get("overlay_active") or focus.get("in_view") is False:
        return "HTML_NOT_VISIBLE"
    if focus.get("succeeded") is False:
        return "HTML_FOCUS_CALL_FAILED"
    required = {"native": True, "ready": True, "in_view": True, "hidden": False,
                "overlay_active": False, "window_present": True, "succeeded": True,
                "first_responder_in_webkit": True}
    if all(focus.get(key) is value for key, value in required.items()):
        return "HTML_RESPONDER_VERIFIED"
    return "HTML_RESPONDER_UNVERIFIED"


def traces(path, offset=0):
    result = []
    for line in path.read_bytes()[offset:].decode(errors="replace").splitlines():
        if line.startswith("DURIAN_INPUT "):
            try:
                result.append(json.loads(line.removeprefix("DURIAN_INPUT ")))
            except json.JSONDecodeError:
                pass  # A concurrent partial final line is not evidence.
    return result


def main(binary):
    here = pathlib.Path(__file__).resolve().parent
    helper_path = here / "keyboard"
    binary = pathlib.Path(binary).resolve(strict=True)
    for path in [here, binary]:
        if not str(path).startswith(("/tmp/gpui-", "/private/tmp/gpui-")):
            raise ValueError("Use the wrapper with isolated /tmp/gpui-* paths only")
    private = pathlib.Path(tempfile.mkdtemp(prefix="pasteboard-", dir=here))
    private.chmod(0o700)
    backup = private / "pasteboard-private.json"
    results = []

    def helper(*args):
        return subprocess.check_output([str(helper_path), *map(str, args)],
                                       text=True, timeout=12).strip()

    def clipboard():
        return subprocess.check_output(["pbpaste"], text=True, timeout=5)

    def interrupt(*_):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupt)
    (here / "layout.json").write_text(helper("layout") + "\n")
    helper("save", backup)
    try:
        # Each control starts with a fresh demo app, never an inherited focus state.
        for phase in ["root-keyboard", "root-ax-press", "webkit-burst"]:
            result = {"phase": phase, "status": "INCOMPLETE"}
            results.append(result)
            env = os.environ.copy()
            env["DURIAN_INPUT_TRACE"] = "1"
            for variable, suffix in [("HOME", "home"), ("XDG_CONFIG_HOME", "home/config"),
                                     ("XDG_DATA_HOME", "home/data"), ("XDG_CACHE_HOME", "home/cache"),
                                     ("TMPDIR", "scratch")]:
                path = here / phase / suffix
                path.mkdir(parents=True, exist_ok=True)
                env[variable] = str(path)
            log_path = here / (phase + ".log")
            process = None
            try:
                with log_path.open("w") as log:
                    process = subprocess.Popen([str(binary)], cwd=binary.parent, env=env,
                                               stdout=log, stderr=log)
                    pid = process.pid

                    def alive():
                        if process.poll() is not None:
                            raise RuntimeError("Demo app exited unexpectedly")

                    def key(code, flags=0, wait=0.4):
                        alive()
                        helper("chord" if isinstance(code, str) else "key", pid, code, flags)
                        time.sleep(wait)

                    def copy():
                        helper("sentinel", "DURIAN_TEST_SENTINEL_" + uuid.uuid4().hex)
                        key("a", COMMAND)
                        key("c", COMMAND)
                        return clipboard()

                    def mail_copy():
                        value = copy()
                        return ("The revised dashboard mockups are ready for review." in value
                                and "Could we take 30 minutes on Tuesday" in value
                                and "DURIAN_TEST_SENTINEL_" not in value)

                    def ax(label):
                        alive()
                        state = json.loads(helper("ax-tree", pid))
                        (here / f"{phase}-{label}.json").write_text(json.dumps(state, indent=2) + "\n")
                        return state

                    time.sleep(8)
                    alive()
                    helper("activate", pid)
                    time.sleep(1)
                    # First AX access activates AccessKit lazily; allow redraws before probing.
                    ax("cold")
                    time.sleep(1)
                    ax("warm")
                    result["startup_events"] = traces(log_path)
                    if phase == "webkit-burst":
                        # Wait for an actual document-height IPC, not a guessed
                        # extra sleep. The focus trace checks the selected view.
                        deadline = time.monotonic() + 5
                        while not any(e.get("stage") == "html.ready" for e in traces(log_path)):
                            alive()
                            if time.monotonic() >= deadline:
                                break
                            time.sleep(0.1)
                        focus_offset = log_path.stat().st_size
                        key("v", OPTION, 1)
                        result["html_copy_matches"] = mail_copy()
                        result["html_focus_events"] = traces(log_path, focus_offset)
                        result["html_boundary"] = html_boundary(result["html_focus_events"])
                        result["html_positive_control"] = (result["html_copy_matches"]
                            and result["html_boundary"] == "HTML_RESPONDER_VERIFIED")
                        if not result["html_positive_control"]:
                            result["status"] = "UNVERIFIED_HTML_FOCUS"
                            continue
                    offset = log_path.stat().st_size
                    if phase == "root-ax-press":
                        result["button"] = json.loads(helper("press-search", pid))
                        if not result["button"].get("pressed"):
                            result["status"] = "UNVERIFIED_AX_ACTION"
                            continue
                    elif phase == "webkit-burst":
                        helper("burst", pid, QUERY)
                    else:
                        helper("text", pid, "/")
                    time.sleep(1)
                    if phase != "webkit-burst":
                        helper("type", pid, QUERY)
                        time.sleep(0.5)
                    state = ax("after-search")
                    # Independent of AX labels: fresh sentinel + exact synthetic query.
                    result["query_copy_matches"] = copy() == QUERY
                    events = traces(log_path, offset)
                    tracing = any(e.get("stage") == "trace.enabled" for e in traces(log_path))
                    result["trace_available"] = tracing
                    result["status"] = classify(result["query_copy_matches"], state, events, tracing)
                    result["search_events"] = events
                    if not result["query_copy_matches"]:
                        continue  # Do not pretend subsequent focus round-trips were reached.
                    tail_offset = log_path.stat().st_size
                    key(53, wait=1)
                    after = ax("after-escape")
                    result["escape_ax_verified"] = bool(state.get("search") and
                        after.get("available") and not after.get("truncated") and not after.get("search"))
                    key("v", OPTION, 1)
                    result["html_after_escape"] = mail_copy()
                    key("t", CONTROL | SHIFT, 1)
                    key("v", OPTION, 1)
                    result["html_after_theme"] = mail_copy()
                    result["tail_events"] = traces(log_path, tail_offset)
                    result["theme_state_verified"] = any(
                        e.get("stage") == "theme.changed" and e.get("details", {}).get("dark") is True
                        for e in result["tail_events"])
                    key("q", CONTROL, 0.1)
                    try:
                        result["normal_ctrl_q_exit"] = process.wait(timeout=8)
                    except subprocess.TimeoutExpired:
                        result["normal_ctrl_q_exit"] = "UNVERIFIED_TIMEOUT"
            except (OSError, RuntimeError, subprocess.SubprocessError, ValueError) as error:
                result["status"] = "COLLECTION_ERROR"
                # Do not serialize subprocess output or clipboard data into error messages.
                result["error_type"] = type(error).__name__
            finally:
                if process is not None:
                    if process.poll() is None:
                        try:
                            helper("terminate", process.pid)
                        except (OSError, subprocess.SubprocessError):
                            pass
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.terminate()
                            try:
                                process.wait(timeout=3)
                            except subprocess.TimeoutExpired:
                                process.kill()
                                process.wait(timeout=3)
                    result["cleanup_exit"] = process.returncode
                print(json.dumps(result), flush=True)
                (here / "diagnosis.json").write_text(json.dumps(results, indent=2) + "\n")
    finally:
        # If equality verification fails, retain the private backup for recovery.
        helper("restore", backup)
        backup.unlink()
        private.rmdir()
        print("CLEANUP complete pasteboard Item/Type/Data equality verified", flush=True)
    print("Diagnostic collection finished; inspect diagnosis.json. This is not an acceptance PASS.")


if __name__ == "__main__":
    if sys.platform != "darwin" or len(sys.argv) != 2:
        sys.exit("Usage on macOS: diagnose-keyboard.py /tmp/gpui-.../durian-gpui-kit")
    main(sys.argv[1])

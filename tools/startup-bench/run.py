#!/usr/bin/python3
"""Native startup benchmark; synthetic-only data, fresh app + owned CLI each run.

Use /usr/bin/python3 with python3-pil, Xvfb/Openbox, tesseract and xdotool.
No production instrumentation, API-only substitutes, or global cache drops.
"""
import argparse
import csv
import ctypes as C
import io
import json
import mmap
import os
from pathlib import Path
import platform
import re
import shutil
import statistics
import subprocess as sp
import threading
import time

from PIL import Image, ImageChops, ImageOps

PUBLIC_FIXTURE_KEY = "0123456789abcdef" * 4
HERE = Path(__file__).resolve().parent


class XImage(C.Structure):
    _fields_ = [("width", C.c_int), ("height", C.c_int), ("xoffset", C.c_int),
                ("format", C.c_int), ("data", C.c_void_p), ("byte_order", C.c_int),
                ("bitmap_unit", C.c_int), ("bitmap_bit_order", C.c_int),
                ("bitmap_pad", C.c_int), ("depth", C.c_int),
                ("bytes_per_line", C.c_int), ("bits_per_pixel", C.c_int)]


class Screen:
    def __init__(self):
        self.x = C.CDLL("libX11.so.6")
        self.x.XOpenDisplay.argtypes = [C.c_char_p]
        self.x.XOpenDisplay.restype = C.c_void_p
        self.x.XGetImage.argtypes = [C.c_void_p, C.c_ulong, C.c_int, C.c_int,
                                    C.c_uint, C.c_uint, C.c_ulong, C.c_int]
        self.x.XGetImage.restype = C.POINTER(XImage)
        self.x.XDestroyImage.argtypes = [C.POINTER(XImage)]
        # A crashed application destroys its drawable. Turn BadDrawable into a
        # Python failure so the benchmark can preserve logs and clean up.
        error_type = C.CFUNCTYPE(C.c_int, C.c_void_p, C.c_void_p)
        self.error_handler = error_type(lambda display, event: 0)
        self.x.XSetErrorHandler.argtypes = [error_type]
        self.x.XSetErrorHandler(self.error_handler)
        self.display = self.x.XOpenDisplay(None)
        if not self.display:
            raise RuntimeError("DISPLAY must name a running X11 display")

    def image(self, window, rect):
        left, top, width, height = rect
        pointer = self.x.XGetImage(self.display, window, left, top, width, height,
                                  C.c_ulong(-1).value, 2)
        if not pointer:
            raise RuntimeError("XGetImage failed")
        try:
            image = pointer.contents
            if image.bits_per_pixel != 32 or image.byte_order != 0:
                raise RuntimeError("benchmark currently requires little-endian 32bpp X11")
            return Image.frombytes("RGB", (width, height),
                                   C.string_at(image.data, image.bytes_per_line * height),
                                   "raw", "BGRX", image.bytes_per_line)
        finally:
            self.x.XDestroyImage(pointer)


def residency(path):
    """mincore observes pages without faulting the mapping into cache."""
    size = path.stat().st_size
    if not size:
        return {"bytes": 0, "resident_pages": 0, "pages": 0}
    libc = C.CDLL(None, use_errno=True)
    libc.mmap.argtypes = [C.c_void_p, C.c_size_t, C.c_int, C.c_int, C.c_int, C.c_long]
    libc.mmap.restype = C.c_void_p
    libc.mincore.argtypes = [C.c_void_p, C.c_size_t, C.c_void_p]
    libc.munmap.argtypes = [C.c_void_p, C.c_size_t]
    with path.open("rb") as file:
        address = libc.mmap(None, size, 1, 2, file.fileno(), 0)
        if address == C.c_void_p(-1).value:
            raise OSError(C.get_errno(), "mmap")
        pages = (size + mmap.PAGESIZE - 1) // mmap.PAGESIZE
        vector = (C.c_ubyte * pages)()
        try:
            if libc.mincore(address, size, vector):
                raise OSError(C.get_errno(), "mincore")
            return {"bytes": size, "resident_pages": sum(bool(x & 1) for x in vector), "pages": pages}
        finally:
            libc.munmap(address, size)


def prepare_cache(paths, cold):
    result = {}
    for path in paths:
        if not path.is_file():
            continue
        before = residency(path)
        with path.open("rb") as file:
            if cold:
                os.fsync(file.fileno())
                os.posix_fadvise(file.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)
            else:
                while file.read(1024 * 1024):
                    pass
        result[str(path)] = {"before": before, "after": residency(path)}
    return result


def environment(root, corpus):
    root.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    for name in ("MAIL_BACKEND", "MAIL_TOKEN", "DURIAN_BIN", "MAIL_FOLDER", "MAIL_BACKGROUND"):
        env.pop(name, None)
    for name, folder in {"HOME": "home", "XDG_DATA_HOME": "data", "XDG_CONFIG_HOME": "config",
                         "XDG_STATE_HOME": "state", "XDG_CACHE_HOME": "cache",
                         "XDG_RUNTIME_DIR": "runtime", "TMPDIR": "tmp"}.items():
        directory = root / folder
        directory.mkdir(mode=0o700, exist_ok=True)
        env[name] = str(directory)
    link = root / "data/durian"
    if not link.exists():
        link.symlink_to(corpus, target_is_directory=True)
    config = root / "config/durian"
    config.mkdir(exist_ok=True)
    for name in ("config.pkl", "profiles.pkl"):
        shutil.copy2(HERE / name, config / name)
    env.update(MAIL_APPEARANCE="dark", MAIL_AVATARS="0", MAIL_TRACE="1",
               VK_ICD_FILENAMES="/dev/null", DURIAN_MASTER_KEY_HEX_SECRET=PUBLIC_FIXTURE_KEY)
    return env


def launch(app, env, output):
    events = []
    start = time.perf_counter()
    process = sp.Popen([str(app)], env=env, stdin=sp.DEVNULL, stdout=sp.DEVNULL,
                       stderr=sp.PIPE, text=True, bufsize=1)

    def logs():
        with (output / "app.log").open("w") as file:
            for line in process.stderr:
                at = (time.perf_counter() - start) * 1000
                # The owned backend suppresses raw READY/token output. Refuse to
                # export a credential if any unexpected future logging prints one.
                line = re.sub(r"(token=|Bearer )[A-Za-z0-9_-]+", r"\1[REDACTED]", line)
                events.append({"ms": round(at, 3), "line": line.rstrip()})
                file.write(f"{at:.3f}ms {line}")
                file.flush()
    reader = threading.Thread(target=logs)
    reader.start()
    deadline = start + 20
    identifier = None
    while time.perf_counter() < deadline:
        found = sp.run(["xdotool", "search", "--onlyvisible", "--name", "^Durian$"],
                       stdout=sp.PIPE, stderr=sp.DEVNULL, text=True, timeout=2)
        if found.returncode == 0 and found.stdout.strip():
            identifier = int(found.stdout.splitlines()[0])
            break
        if process.poll() is not None:
            reader.join(timeout=5)
            raise RuntimeError(f"app exited before window: {process.returncode}")
        time.sleep(0.005)
    if identifier is None:
        process.kill()
        process.wait()
        reader.join(timeout=5)
        raise RuntimeError("native window timeout")
    geometry = sp.check_output(["xdotool", "getwindowgeometry", "--shell", str(identifier)], text=True)
    values = dict(line.split("=", 1) for line in geometry.strip().splitlines())
    return process, reader, events, start, identifier, (int(values["WIDTH"]), int(values["HEIGHT"]))


def quit_app(process, reader, window):
    children = sp.check_output(["ps", "--ppid", str(process.pid), "-o", "pid=,comm="], text=True) if process.poll() is None else ""
    owned = [int(line.split()[0]) for line in children.splitlines() if line.split()[1] == "durian-cli"]
    if process.poll() is None:
        sp.run(["xdotool", "windowactivate", "--sync", str(window)], check=True, timeout=2)
        sp.run(["xdotool", "key", "--clearmodifiers", "ctrl+q"], check=True, timeout=2)
    try:
        process.wait(timeout=5)
    except sp.TimeoutExpired:
        process.kill()
        process.wait()
        raise RuntimeError("app failed normal quit")
    reader.join(timeout=5)
    if reader.is_alive():
        raise RuntimeError("owned child retained stderr after quit")
    if any(Path(f"/proc/{pid}").exists() for pid in owned):
        raise RuntimeError("owned CLI was not killed and reaped on quit")
    return {"app_exit": process.returncode, "owned_cli_pids": owned, "owned_cli_reaped": True}


def ocr(image):
    converted = ImageOps.invert(image.convert("L")).resize((image.width * 2, image.height * 2))
    buffer = io.BytesIO()
    converted.save(buffer, format="PNG")
    result = sp.run(["tesseract", "stdin", "stdout", "--psm", "11", "tsv"],
                    input=buffer.getvalue(), stdout=sp.PIPE, stderr=sp.DEVNULL, check=True)
    return result.stdout.decode()


def calibrate(app, env, screen, output, stats):
    output.mkdir(exist_ok=True)
    process, reader, events, start, window, dimensions = launch(app, env, output)
    try:
        deadline = start + 25
        while time.perf_counter() < deadline:
            image = screen.image(window, (0, 0, *dimensions))
            tsv = ocr(image)
            words = list(csv.DictReader(io.StringIO(tsv), delimiter="\t"))
            # Exclude the selected-thread toolbar/header and collapsed snippets.
            # The larger expanded plaintext font distinguishes body availability.
            # OCR sometimes inserts a digit into the numeric marker; independently
            # verify selection via the trace rather than accepting a preview snippet.
            lists = [w for w in words if w["text"].lower().startswith("benchthread")
                     and 0.17 * dimensions[0] < int(w["left"]) / 2 < 0.45 * dimensions[0]
                     and int(w["top"]) / 2 > 110]
            bodies = [w for w in words if w["text"].lower() == "benchbody"
                      and int(w["left"]) / 2 > dimensions[0] * 0.46 and int(w["height"]) >= 24]
            selected_ok = any(f"selected 0: {stats['SelectedThread']}" in e["line"] for e in events)
            if lists and bodies and selected_ok:
                selected = min(lists, key=lambda w: int(w["top"]))
                body = min(bodies, key=lambda w: int(w["top"]))
                templates = {}
                for name, word in [("useful_list", selected), ("selected_body", body)]:
                    rect = [int(word[k]) // 2 for k in ("left", "top", "width", "height")]
                    patch = image.crop((rect[0], rect[1], rect[0] + rect[2], rect[1] + rect[3]))
                    patch.save(output / f"{name}-template.png")
                    templates[name] = rect
                image.save(output / "reference.png")
                (output / "ocr.tsv").write_text(tsv)
                (output / "templates.json").write_text(json.dumps(templates, indent=2))
                return templates
            time.sleep(0.15)
        image.save(output / "calibration-failed.png")
        (output / "ocr.tsv").write_text(tsv)
        raise RuntimeError("OCR could not locate native list and selected-body anchors")
    finally:
        quit_app(process, reader, window)
        (output / "trace.json").write_text(json.dumps(events, indent=2))


def score(image, reference):
    # Compare actual foreground glyph pixels, not backgrounds/placeholders. OCR
    # establishes the reference text; timed runs perform only small X11 captures.
    mask = reference.convert("L").point(lambda p: 255 if p > 100 else 0)
    foreground = mask.histogram()[255]
    if foreground < 5:
        raise RuntimeError("reference contains too few visible glyph pixels")
    same = ImageChops.difference(image, reference).convert("L").point(lambda p: 255 if p < 18 else 0)
    return ImageChops.multiply(same, mask).histogram()[255] / foreground


def measure(app, env, screen, output, calibration, templates, mode, cache):
    output.mkdir(exist_ok=True)
    references = {name: Image.open(calibration / f"{name}-template.png").convert("RGB") for name in templates}
    process, reader, events, start, window, dimensions = launch(app, env, output)
    times, bounds = {}, {}
    last_negative = {name: 0 for name in ("first_frame", *templates)}
    try:
        deadline = start + 25
        while time.perf_counter() < deadline and len(times) < 3:
            if "first_frame" not in times:
                before = (time.perf_counter() - start) * 1000
                corner = screen.image(window, (4, 350, 8, 8))
                sampled = (time.perf_counter() - start) * 1000
                if corner.convert("L").point(lambda p: 255 if 8 < p < 100 else 0).histogram()[255] > 56:
                    times["first_frame"] = round(sampled, 3)
                    bounds["first_frame"] = [round(last_negative["first_frame"], 3), round(sampled, 3)]
                    first_image = screen.image(window, (0, 0, *dimensions))
                else:
                    last_negative["first_frame"] = before
            for name, rect in templates.items():
                if name not in times:
                    before = (time.perf_counter() - start) * 1000
                    patch = screen.image(window, rect)
                    sampled = (time.perf_counter() - start) * 1000
                    if score(patch, references[name]) >= 0.97:
                        times[name] = round(sampled, 3)
                        bounds[name] = [round(last_negative[name], 3), round(sampled, 3)]
                    else:
                        last_negative[name] = before
            time.sleep(0.015)
        if len(times) != 3:
            raise RuntimeError(f"native startup milestones missing: {times}")
        first_image.save(output / "first-frame.png")
        image = screen.image(window, (0, 0, *dimensions))
        image.save(output / "ready.png")
        text = ocr(image)
        (output / "ocr.tsv").write_text(text)
        if "BenchBody" not in text or "BenchThread" not in text:
            raise RuntimeError("timed glyph match failed independent full-window OCR")
        # Counts are a separate milestone/result, not a prerequisite for the list
        # or body. Give their trace a bounded post-timing observation interval.
        count_deadline = time.perf_counter() + 4
        while time.perf_counter() < count_deadline and not any("sidebar counts" in e["line"] for e in events):
            time.sleep(0.02)
        counts = [e for e in events if "sidebar counts" in e["line"]]
    finally:
        cleanup = quit_app(process, reader, window)
        (output / "trace.json").write_text(json.dumps(events, indent=2))
    result = {"mode": mode, "milestones_ms": times, "sampling_bounds_ms": bounds,
              "cache_residency": cache, "trace_events": events, "counts": counts,
              "cleanup": cleanup}
    (output / "result.json").write_text(json.dumps(result, indent=2))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--app", type=Path, required=True)
    parser.add_argument("--corpus", type=Path, required=True)
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--runs", type=int, default=5)
    parser.add_argument("--calibrate-only", action="store_true")
    args = parser.parse_args()
    app, corpus = args.app.resolve(), args.corpus.resolve()
    args.output.mkdir(parents=True, exist_ok=True)
    env = environment(args.work.resolve(), corpus)
    sp.run(["xdotool", "mousemove", "0", "0"], check=True)
    stats = json.loads((corpus / "stats.json").read_text())
    screen = Screen()
    calibration = args.output / "calibration"
    templates = calibrate(app, env, screen, calibration, stats)
    print(f"calibrated native anchors: {templates}", flush=True)
    if args.calibrate_only:
        return
    paths = [app, app.parent / "durian-cli", corpus / "email.db", corpus / "contacts.db",
             args.work.resolve() / "config/durian/config.pkl", args.work.resolve() / "config/durian/profiles.pkl"]
    results = []
    for mode in ("warm-filesystem", "targeted-file-cold"):
        for run in range(args.runs):
            cold = mode == "targeted-file-cold"
            if cold:
                shutil.rmtree(args.work / "cache", ignore_errors=True)
                (args.work / "cache").mkdir(mode=0o700)
            cache = prepare_cache(paths + list(corpus.glob("*.db-*")), cold)
            result = measure(app, env, screen, args.output / f"{mode}-{run+1}",
                             calibration, templates, mode, cache)
            results.append(result)
            print(f"{stats['Messages']} {mode} run {run+1}: {result['milestones_ms']}", flush=True)
    summary = {}
    for mode in ("warm-filesystem", "targeted-file-cold"):
        summary[mode] = {}
        for milestone in ("first_frame", "useful_list", "selected_body"):
            values = sorted(r["milestones_ms"][milestone] for r in results if r["mode"] == mode)
            summary[mode][milestone] = {"raw_ms": values, "p50_ms": statistics.median(values),
                                        "p95_nearest_rank_ms": values[max(0, int(len(values)*0.95 + 0.999)-1)]}
    report = {"corpus": stats, "summary": summary, "runs": results,
              "environment": {"uname": platform.uname()._asdict(), "cpus": os.cpu_count(),
                              "app_bytes": app.stat().st_size, "app_sha256": sp.check_output(["sha256sum", str(app)], text=True).split()[0],
                              "cli_sha256": sp.check_output(["sha256sum", str(app.parent / "durian-cli")], text=True).split()[0],
                              "cpuinfo": Path("/proc/cpuinfo").read_text(), "meminfo": Path("/proc/meminfo").read_text()},
              "definitions": {"first_frame": "first nonblack dark-theme native client corner",
                              "useful_list": "OCR-calibrated BenchThread glyphs visibly rendered in an actual initial-page result row (not toolbar/header)",
                              "selected_body": "OCR-calibrated BenchBody glyphs visibly rendered in selected message text; not entire HTML/attachment hydration",
                              "cold": "fresh app/CLI, empty XDG_CACHE_HOME, targeted fsync+FADV_DONTNEED on DB/config/app/CLI files verified with mincore; shared libs, kernel, WM and driver remain warm",
                              "warm": "fresh app/CLI; same private app cache, each targeted file sequentially read before timing",
                              "clock": "monotonic perf_counter, immediately before Popen (includes exec); 15ms capture loop plus observed interval bounds"}}
    (args.output / "report.json").write_text(json.dumps(report, indent=2))
    print(json.dumps(summary, indent=2), flush=True)


if __name__ == "__main__":
    main()

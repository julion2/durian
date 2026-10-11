#!/usr/bin/env python3
"""Copies the mail UI's icons from Lucide into assets/icons/lucide/<file>.svg.

tools/icons/map.tsv maps each icon file the UI names to a Lucide icon. Lucide comes from npm
without running anything: `npm pack lucide-static` and extract it under the given root.
Lucide is ISC-licensed; assets/icons/lucide/CREDITS.md carries the notice.

usage: python3 -I tools/icons/build.py /tmp/iconsets
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
LUCIDE = "lucide-static/package/icons/{}.svg"


def load(src: Path, fill: bool) -> str:
    svg = re.sub(r"<!--.*?-->\s*", "", src.read_text(), flags=re.S)
    if fill:  # stroke-only icon drawn solid (the star for "starred")
        svg = svg.replace('fill="none"', 'fill="currentColor"', 1)
    return svg


def main(packages: Path) -> None:
    rows = [line.split("\t") for line in (ROOT / "tools/icons/map.tsv").read_text().splitlines() if line and not line.startswith("#")]
    out = ROOT / "assets/icons/lucide"
    out.mkdir(parents=True, exist_ok=True)
    for file, entry in rows:
        src = packages / LUCIDE.format(entry.removesuffix("+fill"))
        (out / f"{file}.svg").write_text(load(src, entry.endswith("+fill")))
    print(f"{len(rows)} icons → assets/icons/lucide")


if __name__ == "__main__":
    main(Path(sys.argv[1]))

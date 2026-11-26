#!/usr/bin/env bash
# Documentation link check (ADR-027/7.1): every relative Markdown link must
# resolve, and every page under docs/ must be reachable from docs/README.md
# (directly or through a linked page). No network: external URLs are not
# checked here.
set -euo pipefail

exec python3 - "$@" <<'PY'
import os, re, sys
from pathlib import Path

root = Path(__file__).resolve().parent.parent if "__file__" in globals() else Path.cwd()
root = Path.cwd()
link = re.compile(r'\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)')
bad = []
pages = []

def resolve(base: Path, target: str):
    if target.startswith(("http://", "https://", "mailto:", "#")):
        return None
    path = target.split("#", 1)[0]
    if not path:
        return None
    return (base / path).resolve()

md_files = [p for p in root.rglob("*.md") if "node_modules" not in p.parts and "webui" not in p.parts]
for f in md_files:
    text = f.read_text(encoding="utf-8", errors="replace")
    for m in link.finditer(text):
        t = m.group(1)
        r = resolve(f.parent, t)
        if r is None:
            continue
        if not r.exists():
            bad.append(f"{f.relative_to(root)} → {t}")
        elif r.is_file() and r.suffix == ".md":
            pages.append((f, r))

if bad:
    print("broken documentation links:")
    for b in bad:
        print("  -", b)
    sys.exit(1)

readme = root / "docs" / "README.md"
referenced = set()
changed = True
while changed:
    changed = False
    for src, dst in pages:
        if src == readme or src in referenced:
            if dst not in referenced:
                referenced.add(dst)
                changed = True

docs = {p.resolve() for p in (root / "docs").rglob("*.md")}
excluded = {p for p in docs if "_templates" in p.parts}
unreferenced = sorted(p.relative_to(root) for p in docs - excluded - referenced - {readme})
for u in unreferenced:
    print(f"warning: {u} is not reachable from docs/README.md")
print(f"docs link check: {len(md_files)} files, {len(bad)} broken links, {len(unreferenced)} unreachable pages")
sys.exit(0 if not bad else 1)
PY

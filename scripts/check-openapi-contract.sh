#!/usr/bin/env bash
# OpenAPI contract gate (ADR-027): a removed path or operationId fails CI;
# a changed schema is reported. Removals that a release intends are declared
# in docs/api/openapi-removals.txt with a reason.
set -euo pipefail

BASE="${1:-origin/main}"
ALLOW="${2:-docs/api/openapi-removals.txt}"

if ! git rev-parse --verify "$BASE" >/dev/null 2>&1; then
  echo "openapi contract: base revision $BASE not found (fetch it first)" >&2
  exit 2
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git show "$BASE:docs/api/openapi.json" >"$tmp/base.json"
cp docs/api/openapi.json "$tmp/head.json"

python3 - "$tmp/base.json" "$tmp/head.json" "$ALLOW" <<'PY'
import json, sys
from pathlib import Path

base = json.loads(Path(sys.argv[1]).read_text())
head = json.loads(Path(sys.argv[2]).read_text())
allow_path = Path(sys.argv[3])
allowed = {}
if allow_path.exists():
    for line in allow_path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        entry, _, reason = line.partition("|")
        if not reason.strip():
            sys.exit(f"openapi contract: {entry.strip()} needs a reason in {allow_path}")
        allowed[entry.strip()] = reason.strip()

fail = []
report = []

def ops(doc):
    out = {}
    for path, item in doc.get("paths", {}).items():
        for method, op in item.items():
            if isinstance(op, dict) and "operationId" in op:
                out[f"{method.upper()} {path}"] = op["operationId"]
    return out

bops, hops = ops(base), ops(head)
for key in sorted(set(bops) - set(hops)):
    if key in allowed or bops[key] in allowed:
        continue
    fail.append(f"removed operation: {key} ({bops[key]})")
for key in sorted(set(hops) - set(bops)):
    report.append(f"added operation: {key}")

bs = base.get("components", {}).get("schemas", {})
hs = head.get("components", {}).get("schemas", {})
for name in sorted(set(bs) | set(hs)):
    if name not in hs:
        if name not in allowed:
            fail.append(f"removed schema: {name}")
    elif name not in bs:
        report.append(f"added schema: {name}")
    elif bs[name] != hs[name]:
        report.append(f"changed schema: {name}")

for line in report:
    print("openapi contract:", line)
if fail:
    for line in fail:
        print("openapi contract: FAIL:", line, file=sys.stderr)
    sys.exit(1)
print("openapi contract: OK")
PY

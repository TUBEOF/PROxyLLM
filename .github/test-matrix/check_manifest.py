#!/usr/bin/env python3
"""Gate: the conformance matrix manifest and the Go tests must agree.

Rules (a missing or stale cell is a failure, never a silent skip):
  - every pair cell (features text/tools) executes through the two runners;
    those runner tests must exist,
  - every green extra names a Go test that `go test -list` actually reports,
  - no matrix test may exist that the manifest does not account for,
  - every pending cell carries a reason.

Usage: python3 -I check_manifest.py (runs `go test -list` itself)
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
MANIFEST = REPO / "internal/e2e/testdata/matrix.json"
RUNNERS = {"text": "TestMatrix_TextPairs", "tools": "TestMatrix_ToolPairs"}


def listed_tests() -> set[str]:
    out = subprocess.run(
        ["go", "test", "./internal/e2e/", "-list", "TestMatrix_.*", "--", "-run=^$"],
        capture_output=True, text=True, cwd=REPO, check=True,
    )
    return {line.strip() for line in out.stdout.splitlines() if line.strip().startswith("Test")}


def main() -> int:
    manifest = json.loads(MANIFEST.read_text(encoding="utf-8"))
    tests = listed_tests()
    failures: list[str] = []

    claimed: set[str] = set()
    for feature, runner in RUNNERS.items():
        if runner not in tests:
            failures.append(f"runner {runner} for feature {feature!r} is missing from the test list")
        claimed.add(runner)

    for cell in manifest["cells"]:
        if cell["feature"] in RUNNERS:
            if cell["status"] == "green" and cell.get("test"):
                failures.append(f"pair cell {cell['id']} names a bespoke test; pair cells run through the runners")
            continue
        failures.extend(check_cell(cell, tests, claimed))

    for cell in manifest["extras"]:
        failures.extend(check_cell(cell, tests, claimed))

    accounted = claimed | {
        c["test"] for c in manifest["cells"] + manifest["extras"] if c.get("test")
    }
    accounted.add("TestMatrix_ManifestIsComplete")
    for name in sorted(tests - accounted):
        failures.append(f"test {name} exists but no manifest cell claims it")

    if failures:
        print("matrix manifest gate: FAIL")
        for line in failures:
            print("  -", line)
        return 1
    green = sum(1 for c in manifest["cells"] if c["status"] == "green")
    pending = sum(1 for c in manifest["cells"] if c["status"] == "pending")
    print(f"matrix manifest gate: OK ({green} green pair cells, {pending} pending, {len(tests)} tests)")
    return 0


def check_cell(cell: dict, tests: set[str], claimed: set[str]) -> list[str]:
    problems: list[str] = []
    cid = cell.get("id", "<no id>")
    if cell["status"] == "green":
        name = cell.get("test")
        if not name:
            problems.append(f"green cell {cid} names no test")
        elif name not in tests:
            problems.append(f"green cell {cid} names test {name} which does not exist")
        elif name in claimed:
            problems.append(f"cell {cid} claims the runner {name} as its own test")
        else:
            claimed.add(name)
    elif cell["status"] == "pending":
        if cell.get("test"):
            problems.append(f"pending cell {cid} names a test {cell['test']}; a pending cell cannot have one")
        if not cell.get("note"):
            problems.append(f"pending cell {cid} carries no reason")
    else:
        problems.append(f"cell {cid} status {cell['status']!r} is neither green nor pending")
    return problems


if __name__ == "__main__":
    sys.exit(main())
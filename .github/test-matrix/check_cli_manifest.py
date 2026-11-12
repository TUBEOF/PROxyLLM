#!/usr/bin/env python3
"""Gate: the M2 plain-start manifest, the driver and the workflow must agree.

Rules (a missing pair is a failure, never a silent skip):
  - schema 1; clients/scenarios non-empty and unique; every client has an adapter
  - every (client, scenario) pair appears exactly once (full cross product)
  - every green cell names no note; every skipped cell names its reason
  - every green scenario has a SCENARIOS function; every function is green
    somewhere (no dead scenario code)
  - every green scenario sits in exactly one shard; every shard has at least
    one green cell and is wired in .github/workflows/matrix.yml

Usage: python3 -I check_cli_manifest.py
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
MANIFEST = HERE / "cli_matrix.json"
WORKFLOW = REPO / ".github/workflows/matrix.yml"

sys.path.insert(0, str(HERE))
import cli_plain  # noqa: E402


def load() -> dict:
    return json.loads(MANIFEST.read_text(encoding="utf-8"))


def validate(manifest: dict, scenarios_available: set[str], workflow_text: str) -> list[str]:
    problems: list[str] = []
    if manifest.get("schema") != 1:
        problems.append(f"manifest schema = {manifest.get('schema')!r}, want 1")
    clients = manifest.get("clients") or []
    scenarios = manifest.get("scenarios") or []
    if not clients:
        problems.append("clients must not be empty")
    if not scenarios:
        problems.append("scenarios must not be empty")
    if len(set(clients)) != len(clients):
        problems.append("clients contain duplicates")
    if len(set(scenarios)) != len(scenarios):
        problems.append("scenarios contain duplicates")
    for client in clients:
        if not (REPO / ".github/test-matrix/adapters" / f"{client}.json").exists():
            problems.append(f"client {client!r} has no adapter manifest")

    pairs: dict[tuple[str, str], dict] = {}
    for cell in manifest.get("cells") or []:
        client, scenario = cell.get("client"), cell.get("scenario")
        if client not in clients:
            problems.append(f"cell names unknown client {client!r}")
            continue
        if scenario not in scenarios:
            problems.append(f"cell names unknown scenario {scenario!r}")
            continue
        key = (client, scenario)
        if key in pairs:
            problems.append(f"duplicate cell {client}/{scenario}")
            continue
        pairs[key] = cell
        status = cell.get("status")
        if status == "green":
            if cell.get("note"):
                problems.append(f"green cell {client}/{scenario} carries a note")
        elif status == "skipped":
            if not cell.get("note"):
                problems.append(f"skipped cell {client}/{scenario} carries no reason")
        else:
            problems.append(
                f"cell {client}/{scenario} status {status!r} is neither green nor skipped"
            )
    for client in clients:
        for scenario in scenarios:
            if (client, scenario) not in pairs:
                problems.append(f"missing cell {client}/{scenario} — cross product incomplete")

    green = {s for (_, s), cell in pairs.items() if cell.get("status") == "green"}
    for scenario in green:
        if scenario not in scenarios_available:
            problems.append(f"green scenario {scenario!r} has no SCENARIOS function")
    for scenario in scenarios_available:
        if scenario not in green:
            problems.append(f"SCENARIOS function {scenario!r} is green for no client")

    shards = manifest.get("shards") or {}
    owners: dict[str, list[str]] = {}
    for shard, members in shards.items():
        if not members:
            problems.append(f"shard {shard!r} is empty")
        green_here = 0
        for scenario in members:
            if scenario not in scenarios:
                problems.append(f"shard {shard!r} names unknown scenario {scenario!r}")
            owners.setdefault(scenario, []).append(shard)
            if scenario in green:
                green_here += 1
        if green_here == 0:
            problems.append(f"shard {shard!r} has no green cell")
    for scenario in green:
        if len(owners.get(scenario, [])) != 1:
            problems.append(f"green scenario {scenario!r} is in {len(owners.get(scenario, []))} shards, want 1")

    wired = set(re.findall(r"shard:\s*([A-Za-z0-9_-]+)", workflow_text))
    wired |= set(re.findall(r"--shard\s+([A-Za-z0-9_-]+)", workflow_text))
    for shard in shards:
        if shard not in wired:
            problems.append(f"shard {shard!r} is not wired in matrix.yml")
    return problems


def main() -> int:
    manifest = load()
    problems = validate(manifest, set(cli_plain.SCENARIOS), WORKFLOW.read_text(encoding="utf-8"))
    if problems:
        print("cli matrix gate: FAIL")
        for problem in problems:
            print("  -", problem)
        return 1
    green = sum(1 for c in manifest["cells"] if c["status"] == "green")
    skipped = sum(1 for c in manifest["cells"] if c["status"] == "skipped")
    print(
        f"cli matrix gate: OK ({green} green cells, {skipped} skipped, "
        f"{len(manifest['scenarios'])} scenarios)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())

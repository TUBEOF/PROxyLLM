#!/usr/bin/env python3
"""Render a test run's report.json as report.md."""

from __future__ import annotations

import argparse
import json
import os
import sys


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-dir", required=True)
    args = parser.parse_args()
    path = os.path.join(args.run_dir, "report.json")
    with open(path, encoding="utf-8") as f:
        report = json.load(f)
    lines = [
        f"# PROxyLLM live test — {report.get('run_id', 'unknown')}",
        "",
        f"- Started: {report.get('started_at', '')}",
        f"- Finished: {report.get('finished_at', '')}",
        f"- Instance: {report.get('instance', '')}",
        f"- Binary SHA: {report.get('binary_sha256', '')}",
        f"- Git HEAD: {report.get('git_head', '')}",
        f"- Models: {json.dumps(report.get('models', {}), sort_keys=True)}",
        "",
        "| Phase | Check | Status | Detail |",
        "|---|---|---|---|",
    ]
    counts: dict[str, int] = {}
    for check in report.get("checks", []):
        status = check.get("status", "fail")
        counts[status] = counts.get(status, 0) + 1
        detail = str(check.get("detail", "")).replace("|", "\\|").replace("\n", " ")
        lines.append(
            f"| {check.get('phase', '')} | {check.get('name', '')} | {status} | {detail} |"
        )
    lines += ["", "## Summary", ""]
    for status in sorted(counts):
        lines.append(f"- {status}: {counts[status]}")
    lines += ["", "## Artifacts", "", f"`{args.run_dir}`", ""]
    out = os.path.join(args.run_dir, "report.md")
    with open(out, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    print(out)
    return 0


if __name__ == "__main__":
    sys.exit(main())

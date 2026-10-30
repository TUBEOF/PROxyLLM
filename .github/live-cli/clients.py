#!/usr/bin/env python3
"""List, probe and (with explicit consent) install CLI adapters."""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path


ADAPTER_DIR = Path(__file__).resolve().parent / "adapters"


def load(name: str) -> dict:
    with open(ADAPTER_DIR / f"{name}.json", encoding="utf-8") as f:
        return json.load(f)


def version(manifest: dict) -> str:
    cmd = manifest["command"]
    path = shutil.which(cmd)
    if not path:
        return ""
    try:
        out = subprocess.run([path, "--version"], capture_output=True, text=True,
                             timeout=10, check=False)
        return (out.stdout or out.stderr).strip().splitlines()[0] if (out.stdout or out.stderr) else ""
    except (OSError, subprocess.TimeoutExpired):
        return ""


def main() -> int:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("list")
    p = sub.add_parser("probe")
    p.add_argument("name")
    p = sub.add_parser("install")
    p.add_argument("name")
    p.add_argument("--yes-install", action="store_true")
    args = parser.parse_args()

    manifests = []
    for path in sorted(ADAPTER_DIR.glob("*.json")):
        with open(path, encoding="utf-8") as f:
            manifests.append(json.load(f))

    if args.cmd == "list":
        print(json.dumps([{
            "name": m["name"],
            "label": m.get("label", m["name"]),
            "tier": m.get("tier", "tier2"),
            "installed": shutil.which(m["command"]) is not None,
            "path": shutil.which(m["command"]) or "",
            "version": version(m),
            "tested": m.get("tested", ""),
            "runnable": bool(m.get("runnable", False)),
            "install": m.get("install", ""),
            "requires_env": m.get("requires_env", []),
        } for m in manifests], indent=2))
        return 0

    manifest = load(args.name)
    if args.cmd == "probe":
        print(version(manifest) or "not installed")
        return 0

    if args.cmd == "install":
        if not args.yes_install:
            print("refusing to install without --yes-install", file=sys.stderr)
            return 2
        if shutil.which(manifest["command"]):
            print(f"{manifest['command']} already installed")
            return 0
        print(f"installing {manifest['label']}: {manifest['install']}")
        proc = subprocess.run(manifest["install"], shell=True, check=False)
        if shutil.which(manifest["command"]):
            if proc.returncode != 0:
                print(f"installer exited {proc.returncode}, but {manifest['command']} is present")
            return 0
        return proc.returncode
    return 0


if __name__ == "__main__":
    sys.exit(main())

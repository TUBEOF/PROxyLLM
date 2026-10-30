#!/usr/bin/env python3
"""Write the ephemeral live-CLI instance's .env and instance.json.

The server itself is started by the workflow; this script only prepares the
home layout admin.py expects. Secrets stay in .env (0600).
"""

from __future__ import annotations

import argparse
import json
import os
import secrets
from pathlib import Path


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--home", required=True)
    p.add_argument("--repo", default=os.getcwd())
    p.add_argument("--port", type=int, default=18080)
    p.add_argument("--chat-model", default="deepseek-v4.1-flash:1017")
    p.add_argument("--fallback-model", default="nemotron-3-ultra")
    a = p.parse_args()

    home = Path(a.home).resolve()
    for sub in ("bin", "data", "logs", "run", "tmp", "clients", "artifacts"):
        (home / sub).mkdir(parents=True, exist_ok=True)
    os.chmod(home, 0o700)

    env_path = home / ".env"
    existing: dict[str, str] = {}
    if env_path.exists():
        for line in env_path.read_text(encoding="utf-8").splitlines():
            if "=" in line and not line.lstrip().startswith("#"):
                key, value = line.split("=", 1)
                existing[key.strip()] = value.strip()
    values = {
        "OLLAMA_CLOUD_API_KEY": existing.get("OLLAMA_CLOUD_API_KEY")
        or os.environ.get("OLLAMA_CLOUD_API_KEY", ""),
        "ADMIN_USER": existing.get("ADMIN_USER") or "admin",
        "ADMIN_PASSWORD": existing.get("ADMIN_PASSWORD") or secrets.token_hex(16),
        "CLIENT_KEY": existing.get("CLIENT_KEY", ""),
    }
    env_path.write_text("".join(f"{k}={v}\n" for k, v in values.items()), encoding="utf-8")
    os.chmod(env_path, 0o600)

    inst_path = home / "instance.json"
    inst: dict = {}
    if inst_path.exists():
        try:
            inst = json.loads(inst_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            inst = {}
    inst.update({
        "schema": 1,
        "repo": str(Path(a.repo).resolve()),
        "home": str(home),
        "port": a.port,
        "chat_model": a.chat_model,
        "fallback_model": a.fallback_model,
        "web_tools": True,
    })
    inst.setdefault("resolved", {})
    inst_path.write_text(
        json.dumps(inst, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(json.dumps({"home": str(home), "port": a.port}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

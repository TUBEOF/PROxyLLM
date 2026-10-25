#!/usr/bin/env python3
"""Run one CLI adapter in an isolated, non-destructive environment."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path


def redact(text: str) -> str:
    import re
    text = re.sub(r"pk_[A-Za-z0-9_-]+", "pk_<redacted>", text)
    text = re.sub(r"Bearer\s+[A-Za-z0-9._~+/=-]+", "Bearer <redacted>", text)
    return text


def snapshot(paths: list[str]) -> dict[str, object]:
    out: dict[str, object] = {}
    for raw in paths:
        path = Path(os.path.expanduser(raw))
        if not path.exists():
            out[raw] = None
            continue
        entries: list[list[object]] = []
        if path.is_file():
            st = path.stat()
            entries.append([str(path.name), st.st_size, st.st_mtime_ns, st.st_mode])
        else:
            count = 0
            for root, dirs, files in os.walk(path):
                dirs[:] = [d for d in dirs if d not in (".git", "node_modules")]
                for name in files:
                    if count >= 5000:
                        break
                    full = Path(root) / name
                    try:
                        st = full.stat()
                    except OSError:
                        continue
                    entries.append([str(full.relative_to(path)), st.st_size,
                                    st.st_mtime_ns, st.st_mode])
                    count += 1
                if count >= 5000:
                    break
        entries.sort()
        out[raw] = hashlib.sha256(json.dumps(entries).encode()).hexdigest()
    return out


def render(value: str, ctx: dict[str, str]) -> str:
    for key, val in ctx.items():
        value = value.replace("{{" + key + "}}", str(val))
    return value


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--run-dir", required=True)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--client-key", default=os.environ.get("PROXYLLM_TEST_CLIENT_KEY", ""))
    parser.add_argument("--model", required=True)
    parser.add_argument("--prompt", required=True)
    parser.add_argument("--web", action="store_true")
    parser.add_argument("--timeout", type=int, default=120)
    args = parser.parse_args()
    if not args.client_key:
        print("client key missing: pass PROXYLLM_TEST_CLIENT_KEY or --client-key",
              file=sys.stderr)
        return 2

    with open(args.manifest, encoding="utf-8") as f:
        manifest = json.load(f)
    run_dir = Path(args.run_dir).resolve()
    dirs = {
        "RUN_DIR": str(run_dir),
        "HOME": str(run_dir / "home"),
        "CONFIG_DIR": str(run_dir / "config"),
        "DATA_DIR": str(run_dir / "data"),
        "CACHE_DIR": str(run_dir / "cache"),
        "TMP_DIR": str(run_dir / "tmp"),
        "WORK_DIR": str(run_dir / "work"),
        "BASE_URL": args.base_url.rstrip("/"),
        "CLIENT_KEY": args.client_key,
        "MODEL": args.model,
        "PROMPT": args.prompt,
    }
    for path in dirs.values():
        if path.startswith(str(run_dir)):
            Path(path).mkdir(parents=True, exist_ok=True)
    (run_dir / "home").chmod(0o700)

    skip_env = manifest.get("canary_skip_env")
    canary_skipped = bool(skip_env and os.environ.get(skip_env))
    canary_before = {} if canary_skipped else snapshot(manifest.get("canary", []))

    env: dict[str, str] = {}
    for key in manifest.get("env_keep", ["PATH", "LANG", "LC_ALL", "TERM", "TMPDIR",
                                         "SSL_CERT_FILE", "SSL_CERT_DIR",
                                         "NODE_EXTRA_CA_CERTS"]):
        if key in os.environ:
            env[key] = os.environ[key]
    for key in manifest.get("scrub_env", []):
        env[key] = ""
    env["HOME"] = dirs["HOME"]
    env["XDG_CONFIG_HOME"] = dirs["CONFIG_DIR"]
    env["XDG_DATA_HOME"] = dirs["DATA_DIR"]
    env["XDG_CACHE_HOME"] = dirs["CACHE_DIR"]
    env["TMPDIR"] = dirs["TMP_DIR"]
    env["NO_PROXY"] = "127.0.0.1,localhost"
    env["no_proxy"] = env["NO_PROXY"]
    for key, value in (manifest.get("env") or {}).items():
        env[key] = render(str(value), dirs)

    for rel, content in (manifest.get("files") or {}).items():
        target = Path(render(rel, dirs))
        if not str(target.resolve()).startswith(str(run_dir)):
            print(f"refusing file outside run dir: {target}", file=sys.stderr)
            return 2
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(render(str(content), dirs), encoding="utf-8")
        target.chmod(0o600)

    argv = [render(str(a), dirs) for a in manifest.get("args", [])]
    if args.web and manifest.get("web_args"):
        argv = [render(str(a), dirs) for a in manifest["web_args"]]
    command = manifest["command"]
    if not shutil.which(command, path=env.get("PATH", os.environ.get("PATH", ""))):
        print(f"missing CLI: {command}", file=sys.stderr)
        return 3

    started = time.time()
    try:
        proc = subprocess.run(
            [command, *argv],
            cwd=dirs["WORK_DIR"],
            env=env,
            capture_output=True,
            text=True,
            timeout=args.timeout,
            check=False,
        )
        exit_code = proc.returncode
        stdout = proc.stdout
        stderr = proc.stderr
        timed_out = False
    except subprocess.TimeoutExpired as exc:
        exit_code = 124
        stdout = (exc.stdout or "") if isinstance(exc.stdout, str) else ""
        stderr = (exc.stderr or "") if isinstance(exc.stderr, str) else ""
        timed_out = True
    duration = round(time.time() - started, 2)
    canary_after = {} if canary_skipped else snapshot(manifest.get("canary", []))
    canary_ok = True if canary_skipped else canary_before == canary_after

    result = {
        "cli": manifest.get("name", command),
        "command": ["<redacted>" if a == args.client_key else redact(a)
                    for a in [command, *argv]],
        "exit_code": exit_code,
        "timed_out": timed_out,
        "duration_seconds": duration,
        "canary_ok": canary_ok,
        "canary_skipped": canary_skipped,
        "canary_note": manifest.get("canary_note", ""),
        "canary_before": canary_before,
        "canary_after": canary_after,
        "stdout": redact(stdout[-20000:]),
        "stderr": redact(stderr[-20000:]),
        "status": "pass" if exit_code == 0 and canary_ok else (
            "client_config" if not canary_ok else "fail"
        ),
    }
    (run_dir / "result.json").write_text(json.dumps(result, indent=2), encoding="utf-8")
    print(json.dumps({
        "cli": result["cli"],
        "status": result["status"],
        "exit_code": exit_code,
        "canary_ok": canary_ok,
        "canary_skipped": canary_skipped,
        "duration_seconds": duration,
        "stdout_bytes": len(stdout),
        "stderr_bytes": len(stderr),
        "result": str(run_dir / "result.json"),
    }))
    return 0 if result["status"] == "pass" else 1


if __name__ == "__main__":
    sys.exit(main())

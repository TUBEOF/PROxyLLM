#!/usr/bin/env python3
"""Run the four live CLI phases against an ephemeral PROxyLLM instance.

Phases (English prompts only):
  1. hello       — "Hello", expect a 200 and no proxy error.
  2. vision      — a real PNG through a blind model; the proxy MUST caption it
                   (route_trace: capability_fallback vision + image_caption).
  3. web_search  — a prompt that requires web search; route_trace MUST carry
                   emulated_tool/emulated_tool_attempt to=web_search.
  4. web_fetch   — same for a fetch; to=web_fetch.

A missing expected fallback is a FAILURE, not a warning. The only exceptions
are a natively capable model (the matrix marks it, none is used here) and a CLI
that cannot perform the phase headlessly (skipped_unsupported).
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

PHASES = ("hello", "vision", "web_search", "web_fetch")
PROMPTS = {
    "hello": "Hello there",
    "vision": "What is in this image? Describe it in one sentence.",
    "web_search": (
        "Use the web search tool to find today's date and the current top "
        "news headline. Cite the source."
    ),
    "web_fetch": (
        "Use the WebFetch tool to fetch https://example.com and summarize "
        "the first paragraph. Do not answer from memory."
    ),
}


def read_env(path: Path) -> dict[str, str]:
    out: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        out[key.strip()] = value.strip()
    return out


def load_adapters(directory: Path) -> dict[str, dict]:
    out: dict[str, dict] = {}
    for path in sorted(directory.glob("*.json")):
        manifest = json.loads(path.read_text(encoding="utf-8"))
        out[manifest["name"]] = manifest
    return out


def cli_version(manifest: dict) -> str:
    """Report the exact installed CLI version — release notes must say what
    was tested, and the workflow never pins a version."""
    path = shutil.which(manifest.get("command", ""))
    if not path:
        return "missing"
    try:
        out = subprocess.run(
            [path, "--version"], capture_output=True, text=True, timeout=10, check=False
        )
        text = (out.stdout or out.stderr).strip().splitlines()
        return text[0].strip() if text else "unknown"
    except (OSError, subprocess.TimeoutExpired):
        return "unknown"


def render_markdown(report: dict) -> str:
    lines = [
        "# Live CLI compatibility",
        "",
        f"Run: `{report['run_id']}`",
        "",
        "| CLI | Version | Hello | Vision | Web Search | Web Fetch | Overall |",
        "|---|---|---|---|---|---|---|",
    ]
    for cli, info in sorted(report.get("clis", {}).items()):
        phases = info.get("phases", {})
        cells = [phases.get(p, "—") for p in PHASES]
        if any(s == "fail" for s in cells):
            overall = "FAIL"
        elif all(s == "pass" for s in cells):
            overall = "PASS"
        elif any(s == "pass" for s in cells):
            overall = "PARTIAL"
        else:
            overall = "SKIPPED"
        lines.append(
            f"| `{cli}` | {info.get('version', 'unknown')} | "
            + " | ".join(cells)
            + f" | **{overall}** |"
        )
    lines += [
        "",
        "`skipped_unsupported` means the CLI has no headless path for that "
        "phase. `PARTIAL` means at least one phase passed, but not all four.",
    ]
    return "\n".join(lines) + "\n"


def ro_connect(db: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=5)
    conn.execute("PRAGMA query_only=ON")
    return conn


def max_request_id(db: Path) -> int:
    conn = ro_connect(db)
    try:
        row = conn.execute("SELECT COALESCE(MAX(id), 0) FROM request_logs").fetchone()
        return int(row[0])
    finally:
        conn.close()


def request_rows(db: Path, since: int) -> list[tuple]:
    conn = ro_connect(db)
    try:
        return conn.execute(
            "SELECT id, status, error_redacted, route_trace, protocol, model "
            "FROM request_logs WHERE id > ? ORDER BY id",
            (since,),
        ).fetchall()
    finally:
        conn.close()


def trace_events(row: tuple) -> list[dict]:
    raw = row[3]
    if not raw:
        return []
    try:
        doc = json.loads(raw)
    except (TypeError, json.JSONDecodeError):
        return []
    events = doc.get("events") if isinstance(doc, dict) else None
    return events if isinstance(events, list) else []


def log_size(path: Path) -> int:
    try:
        return path.stat().st_size
    except FileNotFoundError:
        return 0


def proxy_errors(path: Path, offset: int) -> list[str]:
    if not path.exists():
        return []
    with path.open("rb") as f:
        f.seek(offset)
        text = f.read().decode("utf-8", "replace")
    errors: list[str] = []
    for line in text.splitlines():
        try:
            doc = json.loads(line)
            if str(doc.get("level", "")).upper() in ("ERROR", "FATAL"):
                errors.append(line)
        except json.JSONDecodeError:
            if '"level":"ERROR"' in line.replace(" ", "") or '"level":"FATAL"' in line.replace(" ", ""):
                errors.append(line)
    return errors


def has_event(events: list[dict], name: str, field: str = "", value: str = "") -> bool:
    for ev in events:
        if ev.get("type") != name:
            continue
        if not field:
            return True
        if ev.get(field) == value:
            return True
    return False


def expected_fallback(phase: str, events: list[dict]) -> tuple[bool, str]:
    if phase == "vision":
        ok = has_event(events, "capability_fallback", "capability", "vision") and has_event(
            events, "image_caption"
        )
        return ok, "capability_fallback(vision) + image_caption"
    if phase == "web_search":
        ok = has_event(events, "emulated_tool", "to", "web_search") or has_event(
            events, "emulated_tool_attempt", "to", "web_search"
        )
        return ok, "emulated_tool(to=web_search)"
    if phase == "web_fetch":
        ok = has_event(events, "emulated_tool", "to", "web_fetch") or has_event(
            events, "emulated_tool_attempt", "to", "web_fetch"
        )
        return ok, "emulated_tool(to=web_fetch)"
    return True, ""


def phase_supported(manifest: dict, phase: str) -> tuple[bool, str]:
    if phase == "hello":
        return True, ""
    if phase == "vision":
        # run_cli.py attaches an image only for Codex's exec -i today.
        if manifest.get("name") == "codex":
            return True, ""
        return False, "adapter has no headless image input"
    if phase in ("web_search", "web_fetch"):
        if not manifest.get("web_args"):
            return False, "adapter has no headless web tool"
        if phase == "web_fetch" and not manifest.get("web_fetch"):
            return False, "adapter exposes web_search only"
        return True, ""
    return False, "unknown phase"


def run_phase(
    home: Path,
    base_url: str,
    key: str,
    adapter_dir: Path,
    cli: str,
    phase: str,
    model: str,
    image: Path,
    timeout: int,
) -> tuple[int, dict, Path]:
    run_dir = home / "clients" / cli / f"{phase}-{uuid.uuid4().hex[:8]}"
    args = [
        sys.executable,
        "-u",
        "-I",
        str(Path(__file__).resolve().parent / "run_cli.py"),
        "--manifest",
        str(adapter_dir / f"{cli}.json"),
        "--run-dir",
        str(run_dir),
        "--base-url",
        base_url,
        "--client-key",
        key,
        "--model",
        model,
        "--prompt",
        PROMPTS[phase],
        "--timeout",
        str(timeout),
    ]
    if phase in ("web_search", "web_fetch"):
        args.append("--web")
    if phase == "vision":
        args += ["--image", str(image)]
    env = dict(os.environ)
    env["PROXYLLM_TEST_CLIENT_KEY"] = key
    proc = subprocess.run(args, capture_output=True, text=True, env=env, check=False)
    result: dict = {}
    result_path = run_dir / "result.json"
    if result_path.exists():
        try:
            result = json.loads(result_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            result = {}
    return proc.returncode, result, run_dir


def transient_failure(detail: str) -> bool:
    """A CLI/transport hiccup gets one retry. A model may also decline a tool
    on a single turn; retry that once too. If the expected fallback is still
    absent after the retry, the assertion fails hard — that is the suite's
    reason to exist."""
    d = detail.lower()
    if "expected fallback missing" in d:
        return True
    return (
        "cli fail" in d
        or "request status 499" in d
        or "request status 5" in d
        or "no request_logs row" in d
        or "proxy log error" in d
        or "timeout" in d
    )


def evaluate_phase(
    home: Path,
    db: Path,
    proxy_log: Path,
    base_url: str,
    key: str,
    adapter_dir: Path,
    cli: str,
    manifest: dict,
    phase: str,
    model: str,
    image: Path,
    timeout: int,
    version: str,
) -> dict:
    since = max_request_id(db)
    log_at = log_size(proxy_log)
    code, result, run_dir = run_phase(
        home, base_url, key, adapter_dir, cli, phase, model, image, timeout
    )
    # The request-log write may be off the request path; poll for the row
    # instead of relying on a fixed sleep.
    deadline = time.time() + 5
    rows: list[tuple] = []
    while time.time() < deadline:
        rows = request_rows(db, since)
        if rows:
            break
        time.sleep(0.2)
    errs = proxy_errors(proxy_log, log_at)
    detail = str(result.get("status", "")) or f"exit={code}"
    status = "pass"
    if code == 3:
        status, detail = "skipped_missing", f"CLI missing: {manifest.get('install', '')}"
    elif code == 2:
        status, detail = "skipped_unsupported", "adapter cannot run this phase"
    elif code != 0 or result.get("status") not in (None, "", "pass"):
        err = (result.get("stderr") or "").strip().splitlines()
        out = (result.get("stdout") or "").strip().splitlines()
        tail = (err[-1] if err else (out[-1] if out else ""))[-240:]
        status, detail = "fail", f"CLI {detail}: {tail}"
    elif not rows:
        status, detail = "fail", "no request_logs row"
    elif phase != "hello":
        ok = False
        for row in rows:
            ok, want = expected_fallback(phase, trace_events(row))
            if ok:
                detail = f"fallback observed: {want}"
                break
        if not ok:
            status = "fail"
            detail = f"expected fallback missing: {expected_fallback(phase, [])[1]}"
    if status == "pass":
        for row in rows:
            if int(row[1]) != 200:
                status, detail = "fail", f"request status {row[1]}"
                break
            if row[2]:
                status, detail = "fail", f"error_redacted: {row[2]}"
                break
            for ev in trace_events(row):
                if ev.get("type") in ("failed", "rejected"):
                    status, detail = "fail", f"trace {ev.get('type')}: {ev.get('reason', '')}"
                    break
            if status != "pass":
                break
    if status == "pass" and errs:
        status, detail = "fail", f"proxy log ERROR: {errs[0][:200]}"
    return {
        "phase": phase,
        "cli": cli,
        "status": status,
        "detail": detail,
        "version": version,
        "run_dir": str(run_dir.relative_to(home)),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", required=True)
    parser.add_argument("--base-url", default="")
    parser.add_argument("--clients", default="all")
    parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()

    home = Path(args.home).resolve()
    env = read_env(home / ".env")
    inst = json.loads((home / "instance.json").read_text(encoding="utf-8"))
    key = env.get("CLIENT_KEY", "")
    if not key:
        print("client key missing from .env", file=sys.stderr)
        return 2
    base_url = args.base_url or f"http://127.0.0.1:{inst['port']}"
    db = home / "data" / "proxyllm.db"
    proxy_log = home / "logs" / "proxyllm.log"

    adapter_dir = Path(__file__).resolve().parent / "adapters"
    adapters = load_adapters(adapter_dir)
    names = (
        sorted(n for n, m in adapters.items() if m.get("runnable"))
        if args.clients == "all"
        else [n.strip() for n in args.clients.split(",") if n.strip()]
    )
    versions = {cli: cli_version(adapters[cli]) for cli in names if cli in adapters}

    run_id = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-" + uuid.uuid4().hex[:6]
    out_dir = home / "artifacts" / run_id
    out_dir.mkdir(parents=True, exist_ok=True)
    image = out_dir / "image.png"
    subprocess.run(
        [sys.executable, "-I", str(Path(__file__).resolve().parent / "make_png.py"), str(image), "--seed", "42"],
        check=True,
        capture_output=True,
    )

    started = datetime.now(timezone.utc).isoformat()
    checks: list[dict] = []
    for cli in names:
        manifest = adapters.get(cli)
        if not manifest:
            checks.append({"phase": "*", "cli": cli, "status": "fail",
                           "detail": "unknown adapter", "version": "unknown"})
            continue
        for phase in PHASES:
            supported, why = phase_supported(manifest, phase)
            if not supported:
                checks.append({"phase": phase, "cli": cli, "status": "skipped_unsupported",
                               "detail": why, "version": versions.get(cli, "unknown")})
                continue
            model = inst["resolved"]["blind"] if phase == "vision" else inst["resolved"]["chat"]
            print(f"{cli:14s} {phase:11s} running", flush=True)
            for attempt in range(3):
                check = evaluate_phase(
                    home, db, proxy_log, base_url, key, adapter_dir, cli, manifest,
                    phase, model, image, args.timeout, versions.get(cli, "unknown"),
                )
                if (
                    check["status"] != "fail"
                    or not transient_failure(check["detail"])
                    or attempt == 2
                ):
                    break
                time.sleep(1)
            checks.append(check)
            print(f"{cli:14s} {phase:11s} {check['status']:20s} {check['detail']}", flush=True)

    finished = datetime.now(timezone.utc).isoformat()
    clis: dict[str, dict] = {}
    for check in checks:
        entry = clis.setdefault(check["cli"], {
            "version": versions.get(check["cli"], "unknown"), "phases": {},
        })
        entry["phases"][check["phase"]] = check["status"]
    report = {
        "schema": 1,
        "run_id": run_id,
        "started_at": started,
        "finished_at": finished,
        "instance": {"base_url": base_url, "models": inst.get("resolved", {})},
        "clis": clis,
        "checks": checks,
    }
    report_path = out_dir / "report.json"
    report_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    (out_dir / "report.md").write_text(render_markdown(report), encoding="utf-8")
    print(str(report_path), flush=True)
    return 0 if all(c["status"] != "fail" for c in checks) else 1


if __name__ == "__main__":
    raise SystemExit(main())

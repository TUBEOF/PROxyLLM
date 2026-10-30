#!/usr/bin/env python3
"""Run every supported feature through every CLI against an ephemeral instance.

Features (English prompts only, one feature per invocation):
  hello       — the CLI reaches the proxy at all.
  tools       — a client tool call (read a file) and its result.
  vision      — a real PNG through a blind model; the proxy MUST caption it
                (route_trace: capability_fallback vision + image_caption).
  web_search  — three independent tasks; each MUST end with a successful
                emulated_tool (more than zero results) AND a cited source URL
                in the CLI's own output.
  web_fetch   — two independent tasks; each MUST end with a fetched page
                (emulated_tool "fetched ...") AND the page's marker text in
                the CLI's own output.

Evidence means proxy AND answer: a green trace with a broken client answer is
exactly the failure this suite exists to catch. There is no retry — a model
that declines the tool once is a failure, not noise. A CLI without a headless
path for a feature is reported skipped WITH ITS REASON.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

FEATURES = ("hello", "tools", "vision", "web_search", "web_fetch")
MARKER = "PROXYLLM-LIVE-MARKER-7Q"

# Every task is one CLI invocation asking for exactly one feature. The web
# tasks run more than once: one green run says nothing about reliability.
TASKS: dict[str, list[dict]] = {
    "hello": [
        {"name": "hello", "prompt": "Hello there"},
    ],
    "tools": [
        {
            "name": "read-file",
            "prompt": "Read the file notes.txt in the current directory and "
                      "reply with its first line only.",
        },
    ],
    "vision": [
        {
            "name": "describe-image",
            "prompt": "What is in the image at {{IMAGE}}? Describe it in one sentence.",
        },
    ],
    "web_search": [
        {
            "name": "date-headline",
            "prompt": "Use the web search tool to find today's date and one "
                      "current news headline. Cite the source URL.",
        },
        {
            "name": "weather-cologne",
            "prompt": "Use the web search tool to find tomorrow's weather "
                      "forecast for Cologne, Germany. Cite the source URL.",
        },
        {
            "name": "news-today",
            "prompt": "Use the web search tool to find the current top story "
                      "on a major news site today. Cite the source URL.",
        },
    ],
    # The fetched pages must carry content the model cannot know from memory:
    # example.com's paragraph is in every training set, and a model that
    # answers it without calling the tool would pass a weaker assertion.
    "web_fetch": [
        {
            "name": "random-integer",
            "prompt": "You must call the web fetch tool now. Fetch "
                      "https://www.random.org/integers/?num=1&min=1000&max=9999"
                      "&col=1&base=10&format=plain&rnd=new and reply with the "
                      "number it returned. Answering from memory is not allowed.",
            "marker_re": r"\b\d{4}\b",
        },
        {
            "name": "random-six-digit",
            "prompt": "You must call the web fetch tool now. Fetch "
                      "https://www.random.org/integers/?num=1&min=100000&max=999999"
                      "&col=1&base=10&format=plain&rnd=new and reply with the "
                      "number it returned. Answering from memory is not allowed.",
            "marker_re": r"\b\d{6}\b",
        },
    ],
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
        "| CLI | Version | Hello | Tools | Vision | Web Search | Web Fetch | Overall | Notes |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for cli, info in sorted(report.get("clis", {}).items()):
        phases = info.get("phases", {})
        cells = [phases.get(feature, "skipped") for feature in FEATURES]
        supported = sum(1 for c in cells if c != "skipped")
        passed = sum(1 for c in cells if c == "pass")
        overall = "FAIL" if "fail" in cells else "PASS"
        notes = "; ".join(
            f"{feature}: {why}"
            for feature, why in sorted(info.get("skips", {}).items())
        )
        lines.append(
            f"| `{cli}` | {info.get('version', 'unknown')} | "
            + " | ".join(cells)
            + f" | **{overall} {passed}/{supported}** | {notes or '—'} |"
        )
    lines += [
        "",
        "`skipped` means the adapter has no headless path for that feature; "
        "the Notes column carries the reason. Overall counts passed/supported "
        "features and is FAIL when any supported feature failed — there is no "
        "partial status that hides a skip.",
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


def emulated_success(events: list[dict], family: str) -> tuple[bool, str]:
    """A SUCCESSFUL emulation: the account walk answered with content.

    `emulated_tool_attempt` is not evidence — it also records failed and
    empty outcomes, which is how a missing backend stayed green.
    """
    for ev in events:
        if ev.get("type") != "emulated_tool" or ev.get("to") != family:
            continue
        reason = str(ev.get("reason") or "")
        if family == "web_search":
            m = re.search(r"→\s*(\d+)\s+result", reason)
            if m and int(m.group(1)) > 0:
                return True, reason
        elif reason.startswith("fetched "):
            return True, reason
    return False, ""


def feature_supported(manifest: dict, feature: str) -> tuple[bool, str]:
    if feature == "hello":
        return True, ""
    if feature in (manifest.get("features") or {}):
        return True, ""
    return False, (manifest.get("skip_reasons") or {}).get(
        feature, "adapter declares no headless path for this feature"
    )


def run_phase(
    home: Path,
    base_url: str,
    key: str,
    adapter_dir: Path,
    cli: str,
    feature: str,
    prompt: str,
    model: str,
    image: Path,
    timeout: int,
) -> tuple[int, dict, Path]:
    run_dir = home / "clients" / cli / f"{feature}-{uuid.uuid4().hex[:8]}"
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
        prompt,
        "--feature",
        feature,
        "--timeout",
        str(timeout),
    ]
    if feature == "vision" and str(image):
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


def feature_evidence(
    feature: str, task: dict, rows: list[tuple], stdout: str
) -> tuple[str, str]:
    """Proxy evidence AND answer evidence. A trace that shows the emulated
    call while the client received nothing is the failure mode this suite
    missed until now."""
    events = [ev for row in rows for ev in trace_events(row)]
    if feature == "tools":
        if MARKER not in stdout:
            return "fail", "the CLI output does not contain the file marker"
        return "pass", "tool result observed"
    if feature == "vision":
        ok = has_event(events, "capability_fallback", "capability", "vision") and has_event(
            events, "image_caption"
        )
        if not ok:
            return "fail", "vision fallback missing (capability_fallback vision + image_caption)"
        return "pass", "vision fallback observed"
    if feature in ("web_search", "web_fetch"):
        reason = ""
        for row in rows:
            ok, why = emulated_success(trace_events(row), feature)
            if ok:
                reason = why
                break
        if not reason:
            return "fail", f"no successful emulated_tool(to={feature}) with content"
        if feature == "web_search" and not re.search(r"https?://", stdout):
            return "fail", "the CLI answer cites no source URL"
        if feature == "web_fetch":
            marker_re = task.get("marker_re") or ""
            if marker_re and not re.search(marker_re, stdout):
                return "fail", f"the CLI answer lacks a match for {marker_re!r}"
        return "pass", reason
    return "pass", "ok"


def evaluate_feature(
    home: Path,
    db: Path,
    proxy_log: Path,
    base_url: str,
    key: str,
    adapter_dir: Path,
    cli: str,
    manifest: dict,
    feature: str,
    task: dict,
    model: str,
    image: Path,
    timeout: int,
    version: str,
) -> dict:
    since = max_request_id(db)
    log_at = log_size(proxy_log)
    code, result, run_dir = run_phase(
        home, base_url, key, adapter_dir, cli, feature, task["prompt"], model, image, timeout
    )
    # The request-log write is off the request path, and one CLI run makes
    # several requests. Wait until no NEW row appeared for two polls: breaking
    # on the first row misses the tool-using request that follows it (Claude
    # Code's multi-turn runs failed exactly that way).
    deadline = time.time() + 8
    rows: list[tuple] = []
    stable = 0
    while time.time() < deadline:
        fresh = request_rows(db, since)
        if fresh and len(fresh) == len(rows):
            stable += 1
            if stable >= 2:
                break
        else:
            stable = 0
        rows = fresh
        time.sleep(0.3)
    errs = proxy_errors(proxy_log, log_at)
    stdout = result.get("stdout") or ""
    detail = str(result.get("status", "")) or f"exit={code}"
    status = "pass"
    if code == 3:
        status, detail = "skipped_missing", f"CLI missing: {manifest.get('install', '')}"
    elif code == 2:
        status, detail = "skipped_unsupported", "adapter cannot run this feature"
    elif code != 0 or result.get("status") not in (None, "", "pass"):
        err = (result.get("stderr") or "").strip().splitlines()
        out = stdout.strip().splitlines()
        tail = (err[-1] if err else (out[-1] if out else ""))[-240:]
        status, detail = "fail", f"CLI {detail}: {tail}"
    elif not rows:
        status, detail = "fail", "no request_logs row"
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
    if status == "pass" and feature != "hello":
        status, detail = feature_evidence(feature, task, rows, stdout)
    if status == "pass" and errs:
        status, detail = "fail", f"proxy log ERROR: {errs[0][:200]}"
    return {
        "feature": feature,
        "task": task["name"],
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
            checks.append({"feature": "*", "task": "*", "cli": cli, "status": "fail",
                           "detail": "unknown adapter", "version": "unknown"})
            continue
        for feature in FEATURES:
            supported, why = feature_supported(manifest, feature)
            if not supported:
                checks.append({"feature": feature, "task": "*", "cli": cli,
                               "status": "skipped_unsupported", "detail": why,
                               "version": versions.get(cli, "unknown")})
                continue
            model = inst["resolved"]["blind"] if feature == "vision" else inst["resolved"]["chat"]
            for task in TASKS[feature]:
                print(f"{cli:14s} {feature:11s} {task['name']:22s} running", flush=True)
                check = evaluate_feature(
                    home, db, proxy_log, base_url, key, adapter_dir, cli, manifest,
                    feature, task, model, image, args.timeout,
                    versions.get(cli, "unknown"),
                )
                checks.append(check)
                print(
                    f"{cli:14s} {feature:11s} {task['name']:22s} "
                    f"{check['status']:20s} {check['detail']}",
                    flush=True,
                )

    finished = datetime.now(timezone.utc).isoformat()
    clis: dict[str, dict] = {}
    for check in checks:
        entry = clis.setdefault(check["cli"], {
            "version": versions.get(check["cli"], "unknown"), "phases": {}, "skips": {},
        })
        if check["status"] == "skipped_unsupported":
            entry["skips"][check["feature"]] = check["detail"]
            entry["phases"].setdefault(check["feature"], "skipped")
            continue
        # A feature passes only when every one of its tasks passed.
        current = entry["phases"].get(check["feature"], "pass")
        entry["phases"][check["feature"]] = (
            "fail" if "fail" in (current, check["status"]) else "pass"
        )
    report = {
        "schema": 2,
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

#!/usr/bin/env python3
"""Run every supported feature through every CLI against an ephemeral instance.

Features (English prompts only, one feature per invocation):
  hello       — the CLI reaches the proxy at all.
  tools       — a client tool call (read a file) and its result.
  multi_turn  — a real conversation: two turns in one session, the second
                using the first. One task proves context is kept (a marker
                the model cannot guess); one does a web search on the second
                turn, the shape a user's follow-up actually takes.
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
that declines the tool once is a failure, not noise. The two exceptions each
run a failed web task once more — same task, fresh run — with the retry alone
deciding the check (a retry that also fails stays fail): the web-tool
backend's TRANSIENT unavailability (the proxy then answers the documented
{"error_code":"unavailable"} error result), and the model's PLAIN MISS of a
web_search call — its stream ends a completed turn without any web_search
invocation, a verdict only the two stream formats that record tool items
(codex --json, claude-code stream-json) can reach. Nothing else ever retries.
A CLI without a headless path for a feature is reported skipped WITH
ITS REASON.
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

FEATURES = ("hello", "tools", "multi_turn", "vision", "web_search", "web_fetch")
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
    "multi_turn": [
        {
            # A fact the model cannot know from memory: the second turn can
            # only answer it if the first turn reached the model.
            "name": "remember-number",
            "prompts": [
                "Remember this number: 4711. Reply with OK only.",
                "What number did I ask you to remember? Reply with the number only.",
            ],
            "marker_re": r"\b4711\b",
        },
        {
            # The user's own shape: a greeting, then a web question as the
            # follow-up. The search must happen on the SECOND turn and still
            # answer with a source.
            "name": "weather-second-turn",
            "prompts": [
                "Hallo!",
                "Use the web search tool to find tomorrow's weather forecast "
                "for Cologne, Germany. Cite the source URL.",
            ],
            "evidence": "web_search",
            "requires": "web_search",
            "marker_re": r"https?://",
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
    ]
    if report.get("run_aborted"):
        lines.append(f"**{report['run_aborted']}**")
        lines.append("")
    lines += [
        "| CLI | Version | Hello | Tools | Multi Turn | Vision | Web Search | Web Fetch | Overall | Notes |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for cli, info in sorted(report.get("clis", {}).items()):
        phases = info.get("phases", {})
        cells = [phases.get(feature, "skipped") for feature in FEATURES]
        supported = sum(1 for c in cells if c != "skipped")
        passed = sum(1 for c in cells if c == "pass")
        overall = "FAIL" if "fail" in cells else "PASS"
        notes = [f"{feature}: {why}" for feature, why in sorted(info.get("skips", {}).items())]
        notes += [f"{key}: {value}" for key, value in sorted(info.get("retries", {}).items())]
        lines.append(
            f"| `{cli}` | {info.get('version', 'unknown')} | "
            + " | ".join(cells)
            + f" | **{overall} {passed}/{supported}** | {'; '.join(notes) or '—'} |"
        )
    lines += [
        "",
        "`skipped` means the adapter has no headless path for that feature; "
        "the Notes column carries the reason. `retry 1/1` marks a web task "
        "that ran once more after the model plainly answered without calling "
        "the family's tool (plain miss) or after the emulation answered the "
        "documented transient-unavailable error — the note names the class. "
        "Overall counts passed/supported "
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
    prompts: list[str],
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
    ]
    for prompt in prompts:
        args += ["--prompt", prompt]
    args += [
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


def emulated_evidence(rows: list[tuple], family: str) -> str:
    """The first successful emulated_tool reason across the run's requests,
    or "" when no request produced one."""
    for row in rows:
        ok, why = emulated_success(trace_events(row), family)
        if ok:
            return why
    return ""


# One of the two retriable failure classes: the emulation answered the
# DOCUMENTED transient-unavailable error because the emulator's own upstream
# call failed ("emulator fetch/search failed", self-recovering in the wild).
# The other class — the model plain-missing the tool call — is plain_miss_web
# below. Both are single-shot; a missing canary or a protocol defect always
# fails a task on the first try.
UNAVAILABLE_RE = re.compile(r'error_code["\']?\s*[:=]\s*["\']?unavailable\b')


def transient_unavailable_web(feature: str, detail: str, rows: list[tuple], stdout: str) -> bool:
    """True when a web task failed on exactly that documented transient
    unavailability: no successful emulated call, a trace showing the
    emulator's failed upstream call, and the CLI's own output echoing the
    same error_code shape the proxy sent the model. The trace is ground
    truth; the text must agree, which keeps the two retry classes distinct —
    a plain miss (no emulated call at all) never matches here, and a
    transient failure (the tool ran, the emulated backend failed) is no
    answer defect."""
    if detail != f"no successful emulated_tool(to={feature}) with content":
        return False
    traced = any(
        ev.get("type") == "emulated_tool"
        and ev.get("to") == feature
        and ("emulator fetch failed" in ev.get("reason", "")
             or "emulator search failed" in ev.get("reason", ""))
        for row in rows
        for ev in trace_events(row)
    )
    return traced and UNAVAILABLE_RE.search(stdout) is not None


def _stream_docs(stdout: str) -> list[dict]:
    """The JSON stream formats write one object per line; skip anything else.
    A line that does not parse is noise and only ever keeps the detector
    conservative (it cannot make a retry more likely)."""
    docs: list[dict] = []
    for line in stdout.splitlines():
        s = line.strip()
        if not (s.startswith("{") and s.endswith("}")):
            continue
        try:
            docs.append(json.loads(s))
        except json.JSONDecodeError:
            continue
    return docs


def plain_miss_web(cli: str, feature: str, detail: str, stdout: str) -> bool:
    """True when a web task failed on a MODEL plain miss: the CLI's own
    stream ended a completed turn without ever recording an invocation of
    the family's tool — the model says "I'll search…" and then answers
    without calling it (run 41684909774, codex weather-cologne). Known only
    for the two stream formats that record tool items:

      codex --json        web_search items (item.started/completed) per
                          invocation, `turn.completed` ending the turn
      claude-code
      stream-json         a `WebSearch` tool_use or `web_search`
                          server_tool_use block inside assistant events, a
                          `result` event with subtype `success` per turn

    The web-fetch CLIs (cline, continue, kimi, opencode) record no tool
    items in their streams, so their stdout can never prove a miss and they
    stay unretried. A stream whose completed turn is missing is parse
    uncertainty and never retries; the presence of any tool item disproves
    the miss."""
    detail_miss = f"no successful emulated_tool(to={feature}) with content"
    if feature != "web_search" or detail != detail_miss:
        return False
    docs = _stream_docs(stdout)
    if cli == "codex":
        done = any(e.get("type") == "turn.completed" for e in docs)
        invoked = any(
            (e.get("item") or {}).get("type", "").startswith("web_search")
            for e in docs
        )
    elif cli == "claude-code":
        done = any(
            e.get("type") == "result" and e.get("subtype") == "success" for e in docs
        )
        invoked = any(
            (b.get("name") or "").replace("_", "").lower() == "websearch"
            for e in docs
            if e.get("type") == "assistant"
            for b in (e.get("message") or {}).get("content") or []
            if isinstance(b, dict) and b.get("type") in ("tool_use", "server_tool_use")
        )
    else:
        return False
    return done and not invoked


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
    if feature == "multi_turn":
        # The second turn must show it saw the first: a marker the model
        # cannot guess, and (for the web task) a search that really ran.
        marker_re = task.get("marker_re") or ""
        if marker_re and not re.search(marker_re, stdout):
            return "fail", f"the second turn did not use the first (no match for {marker_re!r})"
        if task.get("evidence") == "web_search":
            why = emulated_evidence(rows, "web_search")
            if not why:
                return "fail", "no successful emulated_tool(web_search) in the conversation"
            return "pass", why
        return "pass", "second turn used the first"
    if feature == "vision":
        ok = has_event(events, "capability_fallback", "capability", "vision") and has_event(
            events, "image_caption"
        )
        if not ok:
            return "fail", "vision fallback missing (capability_fallback vision + image_caption)"
        return "pass", "vision fallback observed"
    if feature in ("web_search", "web_fetch"):
        reason = emulated_evidence(rows, feature)
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
    def attempt() -> tuple[str, str, list[tuple], str, Path]:
        since = max_request_id(db)
        log_at = log_size(proxy_log)
        code, result, run_dir = run_phase(
            home, base_url, key, adapter_dir, cli, feature,
            task.get("prompts") or [task["prompt"]], model, image, timeout
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
        # A 499 row is the CLIENT abandoning its own request (the proxy's own
        # rule: abandoned is neither success nor failure). CLIs fire-and-cancel
        # auxiliary calls (titles, prewarm); only a run with no completed row at
        # all is a failure.
        completed = [row for row in rows if int(row[1]) != 499]
        if status == "pass" and not completed:
            status, detail = "fail", "no request_logs row completed (every row was client-abandoned)"
        if status == "pass":
            for row in completed:
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
        return status, detail, rows, stdout, run_dir

    status, detail, rows, stdout, run_dir = attempt()
    attempts = 1
    # The suite's only retries, each running the task once more with the
    # retry alone deciding (a retry that fails keeps its own detail): the
    # web-tool backend's transient failure the proxy answered with the
    # documented unavailable error (transient_unavailable_web), and the
    # model's plain tool miss (plain_miss_web).
    retry_kind = ""
    if transient_unavailable_web(feature, detail, rows, stdout):
        retry_kind = "transient unavailability"
    elif plain_miss_web(cli, feature, detail, stdout):
        retry_kind = "plain miss"
    if retry_kind:
        print(
            f"{cli:14s} {feature:11s} {task['name']:22s} "
            f"{retry_kind} — retrying once",
            flush=True,
        )
        attempts = 2
        status, detail, _, _, run_dir = attempt()
    check = {
        "feature": feature,
        "task": task["name"],
        "cli": cli,
        "status": status,
        "detail": detail,
        "version": version,
        "run_dir": str(run_dir.relative_to(home)),
        "attempts": attempts,
    }
    if retry_kind:
        check["retry"] = retry_kind
    return check


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
    cooldown_streak = 0
    aborted = ""
    for cli in names:
        manifest = adapters.get(cli)
        if not manifest:
            checks.append({"feature": "*", "task": "*", "cli": cli, "status": "fail",
                           "detail": "unknown adapter", "version": "unknown",
                           "attempts": 0})
            continue
        for feature in FEATURES:
            supported, why = feature_supported(manifest, feature)
            if not supported:
                checks.append({"feature": feature, "task": "*", "cli": cli,
                               "status": "skipped_unsupported", "detail": why,
                               "version": versions.get(cli, "unknown"),
                               "attempts": 0})
                continue
            model = inst["resolved"]["blind"] if feature == "vision" else inst["resolved"]["chat"]
            for task in TASKS[feature]:
                # A task that needs another feature (the second-turn web task
                # needs web_search) is skipped for a CLI that does not declare
                # it — the rest of multi_turn still runs.
                needed = task.get("requires")
                if needed and not feature_supported(manifest, needed)[0]:
                    checks.append({
                        "feature": feature, "task": task["name"], "cli": cli,
                        "status": "skipped_unsupported",
                        "detail": f"needs {needed}: {feature_supported(manifest, needed)[1]}",
                        "version": versions.get(cli, "unknown"),
                        "attempts": 0,
                    })
                    continue
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
                # A provider in health cooldown fails every following task at
                # its own per-CLI retry budget — three tasks of that burn
                # minutes per CLI and the job dies in the 90-minute cap long
                # before the suite is honest about anything. Fail fast: abort
                # the run; the report carries the reason and the already-run
                # failures stay verbatim.
                if "health cooldown" in check["detail"]:
                    cooldown_streak += 1
                else:
                    cooldown_streak = 0
                if cooldown_streak >= 3:
                    for rest in FEATURES[FEATURES.index(feature):][1:]:
                        if feature_supported(manifest, rest)[0]:
                            for rest_task in TASKS[rest]:
                                checks.append({
                                    "feature": rest, "task": rest_task["name"], "cli": cli,
                                    "status": "skipped_unsupported",
                                    "detail": "aborted: proxy provider in health cooldown (upstream quota/availability)",
                                    "version": versions.get(cli, "unknown"),
                                    "attempts": 0,
                                })
                    aborted = (
                        "Run aborted after repeated health-cooldown failures of the proxy's "
                        "provider (upstream quota/availability). Tasks that already ran keep "
                        "their own result; the report is NOT a clean pass."
                    )
                    print("abort: proxy provider in health cooldown; aborted early", flush=True)

        if aborted:
            break

    finished = datetime.now(timezone.utc).isoformat()
    clis: dict[str, dict] = {}
    for check in checks:
        entry = clis.setdefault(check["cli"], {
            "version": versions.get(check["cli"], "unknown"), "phases": {}, "skips": {},
        })
        if check["status"] == "skipped_unsupported":
            if check["task"] not in ("*", ""):
                # One task of a supported feature (the second-turn web task on
                # a CLI without a web tool): a note, not a skipped feature.
                entry["skips"][f"{check['feature']} ({check['task']})"] = check["detail"]
            else:
                entry["skips"][check["feature"]] = check["detail"]
                entry["phases"].setdefault(check["feature"], "skipped")
            continue
        # A feature passes only when every one of its tasks passed.
        current = entry["phases"].get(check["feature"], "pass")
        entry["phases"][check["feature"]] = (
            "fail" if "fail" in (current, check["status"]) else "pass"
        )
        # The Notes column names every web task that needed its one retry,
        # and how that retry ended, with the failure class that triggered it.
        if check.get("attempts", 1) >= 2:
            kind = check.get("retry", "transient unavailability")
            entry.setdefault("retries", {})[f"{check['feature']} ({check['task']})"] = (
                f"retry 1/1 {'passed' if check['status'] == 'pass' else 'still failed'}"
                f" ({kind})"
            )
    report = {
        "schema": 4,
        "run_id": run_id,
        "started_at": started,
        "finished_at": finished,
        "instance": {"base_url": base_url, "models": inst.get("resolved", {})},
        "run_aborted": aborted,
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

#!/usr/bin/env python3
"""CLI-only live test for the PROxyLLM test instance.

Every stimulus is a real client CLI process against the local instance. The
harness never speaks /v1/messages itself, never starts a stub upstream, and
never replays a fixture: the evidence comes from the proxy's request log and
from the CLI's own session transcript. Phases 0-2 are preflight/evidence;
3-7 drive the CLIs; the error-path regression runs last because it lowers the
header wall and can open the breaker.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import re
import sqlite3
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from admin import AdminClient, load_env, redact  # noqa: E402

ADAPTERS = HERE / "adapters"
CANARY = "PROXYLLM-TOOL-CANARY-42"
CLIENT_PROTOCOL = {
    "claude-code": "anthropic",
    "codex": "openai-responses",
    "opencode": "openai",
    "goose": "openai",
    "kimi": "openai",
    "aider": "openai",
    "qwen": "openai",
    "continue": "openai",
    "cline": "openai",
    "factory": "openai",
}
# CLIs that expose an agent file tool in their non-interactive mode. Aider's
# --message mode is a chat, not an agent; Factory needs FACTORY_API_KEY.
TOOL_CAPABLE = {
    "claude-code", "codex", "opencode", "goose", "kimi", "qwen", "continue", "cline",
}


class Harness:
    def __init__(self, home: Path, run_id: str, strict_upstream: bool):
        self.home = home
        self.run_id = run_id
        self.strict_upstream = strict_upstream
        self.artifacts = home / "artifacts" / run_id
        self.artifacts.mkdir(parents=True, exist_ok=True)
        self.env = load_env(str(home / ".env"))
        with open(home / "instance.json", encoding="utf-8") as f:
            self.inst = json.load(f)
        self.base = f"http://127.0.0.1:{self.inst['port']}"
        self.db = home / "data" / "proxyllm.db"
        self.checks: list[dict] = []
        self.watermark_request = 0
        self.watermark_child = 0
        log = home / "logs" / "proxyllm.log"
        self.log_offset = log.stat().st_size if log.exists() else 0
        self.admin = AdminClient(self.base, str(home / "run" / "admin-cookies.txt"))
        self._login()

    def _login(self) -> None:
        self.admin.login(str(self.home / ".env"),
                         self.env.get("ADMIN_USER", "admin"),
                         self.env.get("ADMIN_PASSWORD", ""))

    def check(self, phase: int, name: str, status: str, detail: str = "") -> None:
        self.checks.append({"phase": phase, "name": name, "status": status,
                            "detail": detail})
        print(f"[{status:17}] phase {phase}: {name}" + (f" — {detail}" if detail else ""))

    def admin_call(self, method: str, path: str, body: dict | None = None,
                   timeout: int = 60) -> tuple[int, dict]:
        return self.admin.call(method, path, body, timeout=timeout,
                               retry_login=self._relogin)

    def _relogin(self) -> bool:
        try:
            self._login()
            return True
        except SystemExit:
            return False

    def db_rows(self, sql: str, args: tuple = ()) -> list[dict]:
        con = sqlite3.connect(f"file:{self.db}?mode=ro", uri=True)
        con.row_factory = sqlite3.Row
        con.execute("PRAGMA query_only=ON")
        try:
            return [dict(r) for r in con.execute(sql, args).fetchall()]
        finally:
            con.close()

    def max_ids(self) -> tuple[int, int]:
        rows = self.db_rows(
            "SELECT (SELECT COALESCE(MAX(id),0) FROM request_logs) AS r,"
            "       (SELECT COALESCE(MAX(id),0) FROM request_child_logs) AS c"
        )
        return rows[0]["r"], rows[0]["c"]

    def wait_request(self, after: int, predicate, timeout: float = 8.0) -> dict | None:
        deadline = time.time() + timeout
        while time.time() < deadline:
            rows = self.db_rows(
                "SELECT id, ts, model, provider_id, status, protocol, route_trace, "
                "error_redacted FROM request_logs WHERE id > ? ORDER BY id DESC",
                (after,),
            )
            for row in rows:
                if predicate(row):
                    return row
            time.sleep(0.25)
        return None

    def events(self, request_id: int) -> list[dict]:
        return self.db_rows(
            """SELECT json_extract(e.value,'$.type') AS type,
                      json_extract(e.value,'$.from') AS from_ref,
                      json_extract(e.value,'$.to') AS to_ref,
                      json_extract(e.value,'$.provider_id') AS provider_id,
                      json_extract(e.value,'$.model') AS model,
                      json_extract(e.value,'$.reason') AS reason,
                      json_extract(e.value,'$.capability') AS capability
               FROM request_logs r, json_each(r.route_trace,'$.events') e
               WHERE r.id = ? ORDER BY e.key""",
            (request_id,),
        )

    def children(self, since: int) -> list[dict]:
        return self.db_rows(
            """SELECT id, parent_id, kind, provider_id, model, from_ref, to_ref,
                      detail, latency_ms FROM request_child_logs
               WHERE id > ? ORDER BY id""",
            (since,),
        )

    def run_cli(self, name: str, prompt: str, web: bool = False,
                timeout: int = 120, model: str = "proxy-chat",
                image: Path | None = None,
                work_file: tuple[str, str] | None = None) -> tuple[dict, Path]:
        manifest = ADAPTERS / f"{name}.json"
        label = re.sub(r"[^a-zA-Z0-9_-]+", "-", prompt)[:32].strip("-") or "call"
        run_dir = self.home / "clients" / name / self.run_id / f"{label}-{time.time_ns()}"
        run_dir.mkdir(parents=True, exist_ok=True)
        if work_file:
            work = run_dir / "work"
            work.mkdir(parents=True, exist_ok=True)
            (work / work_file[0]).write_text(work_file[1], encoding="utf-8")
        cmd = [
            sys.executable, "-I", str(HERE / "run_cli.py"),
            "--manifest", str(manifest),
            "--run-dir", str(run_dir),
            "--base-url", self.base,
            "--model", model,
            "--prompt", prompt,
            "--timeout", str(timeout),
        ]
        if web:
            cmd.append("--web")
        if image:
            cmd += ["--image", str(image)]
        env = dict(os.environ)
        env["PROXYLLM_TEST_CLIENT_KEY"] = self.env.get("CLIENT_KEY", "")
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 60,
                              check=False, env=env)
        (self.artifacts / "cli" / name).mkdir(parents=True, exist_ok=True)
        (self.artifacts / "cli" / name / "runner.stdout").write_text(
            redact(proc.stdout), encoding="utf-8")
        (self.artifacts / "cli" / name / "runner.stderr").write_text(
            redact(proc.stderr), encoding="utf-8")
        try:
            summary = json.loads(proc.stdout.strip().splitlines()[-1])
        except (IndexError, json.JSONDecodeError):
            summary = {"cli": name, "status": "fail",
                       "detail": redact(proc.stderr[-500:]) or "runner produced no JSON"}
        return summary, run_dir

    def cli_output(self, run_dir: Path) -> dict:
        path = run_dir / "result.json"
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return {}

    def transcript(self, run_dir: Path) -> str:
        chunks = []
        for path in sorted(run_dir.glob("config/claude/projects/*/*.jsonl")):
            try:
                chunks.append(path.read_text(encoding="utf-8", errors="replace"))
            except OSError:
                continue
        return "\n".join(chunks)

    def write_png(self, name: str, seed: int) -> Path:
        path = self.artifacts / "images" / name
        path.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run([sys.executable, "-I", str(HERE / "make_png.py"),
                        str(path), "--seed", str(seed)],
                       check=True, capture_output=True)
        return path


def scan_claude_transcript(text: str) -> tuple[list[str], list[str]]:
    """Return (leaked family tool_use names, unknown-tool error texts).

    Claude Code's transcript records a model call as a client tool_use only
    when the CLI executes it. A client tool_use named lowercase web_fetch /
    web_search is the proxy leak: the CLI never declared those names.
    """
    leaks, errors = [], []
    for line in text.splitlines():
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        message = row.get("message") or {}
        for block in message.get("content") or []:
            if not isinstance(block, dict):
                continue
            if block.get("type") == "tool_use" and block.get("name") in (
                    "web_fetch", "web_search"):
                leaks.append(block.get("name", ""))
            if block.get("type") == "tool_result":
                body = str(block.get("content") or "")
                if "No such tool available" in body and any(
                        tool in body for tool in ("web_fetch", "web_search")):
                    errors.append(body[:200])
    return leaks, errors


def cli_names(spec: str) -> list[str]:
    if spec.strip().lower() == "all":
        return sorted(p.stem for p in ADAPTERS.glob("*.json"))
    return [c.strip() for c in spec.split(",") if c.strip()]


def cli_inventory() -> dict[str, dict]:
    proc = subprocess.run([sys.executable, "-I", str(HERE / "clients.py"), "list"],
                          capture_output=True, text=True, check=False)
    try:
        return {row["name"]: row for row in json.loads(proc.stdout)}
    except json.JSONDecodeError:
        return {}


def phase0(h: Harness, clients: list[str], inventory: dict[str, dict]) -> None:
    try:
        with urllib.request.urlopen(h.base + "/healthz", timeout=5) as resp:
            h.check(0, "healthz", "pass" if resp.status == 200 else "fail",
                    f"HTTP {resp.status}")
    except OSError as exc:
        h.check(0, "healthz", "fail", redact(str(exc))[:200])

    env_path = h.home / ".env"
    mode = env_path.stat().st_mode & 0o777 if env_path.exists() else 0
    h.check(0, ".env 0600", "pass" if mode == 0o600 else "fail", oct(mode))
    home_mode = h.home.stat().st_mode & 0o777
    h.check(0, "home 0700", "pass" if home_mode == 0o700 else "fail", oct(home_mode))

    status, payload = h.admin_call("GET", "/api/admin/providers")
    providers = [p.get("id") for p in payload.get("providers", [])] \
        if status == 200 else []
    h.check(0, "providers", "pass" if status == 200 else "fail", str(providers))

    key = h.env.get("CLIENT_KEY", "")
    h.check(0, "client key", "pass" if key.startswith("pk_") else "fail",
            f"prefix {key[:8]}…" if key else "missing")

    pid_path = h.home / "run" / "proxyllm.pid"
    pid = int(pid_path.read_text().strip()) if pid_path.exists() else 0
    cmdline = Path(f"/proc/{pid}/cmdline").read_bytes() if pid and \
        Path(f"/proc/{pid}/cmdline").exists() else b""
    owns = b"proxyllm" in cmdline
    h.check(0, "pid ownership", "pass" if owns else "fail", f"pid {pid}")

    binary = h.home / "bin" / "proxyllm"
    actual = hashlib.sha256(binary.read_bytes()).hexdigest() if binary.exists() else ""
    recorded = h.inst.get("binary_sha256", "")
    h.check(0, "binary sha",
            "pass" if actual and actual == recorded else "warn",
            f"recorded {recorded[:16]}… actual {actual[:16]}…"
            + ("" if actual == recorded else " — rerun restart --build"))

    status, payload = h.admin_call("GET", "/api/admin/capability-fallbacks")
    h.check(0, "fallbacks", "pass" if status == 200 else "fail",
            json.dumps(payload)[:200] if status == 200 else f"HTTP {status}")

    for name in clients:
        row = inventory.get(name, {})
        if not row.get("installed"):
            h.check(0, f"cli {name}", "skipped_missing", row.get("install", ""))
        elif not row.get("runnable"):
            h.check(0, f"cli {name}", "skipped_unsupported", "adapter unverified")
        else:
            h.check(0, f"cli {name}", "pass",
                    f"{row.get('version')} (tested {row.get('tested')})")


def phase1(h: Harness, clients: list[str], inventory: dict[str, dict],
           timeout: int) -> None:
    for name in clients:
        row = inventory.get(name, {})
        if not row.get("installed"):
            h.check(1, f"{name} hallo", "skipped_missing", row.get("install", ""))
            continue
        if not row.get("runnable"):
            h.check(1, f"{name} hallo", "skipped_unsupported", "adapter unverified")
            continue
        before = h.max_ids()[0]
        result, _ = h.run_cli(name, "Sag nur Hallo.", timeout=min(timeout, 180))
        if result.get("status") == "client_config":
            h.check(1, f"{name} hallo", "client_config", json.dumps(result)[:400])
            continue
        if result.get("status") != "pass" or int(result.get("stdout_bytes", 0)) <= 0:
            h.check(1, f"{name} hallo", "fail", json.dumps(result)[:400])
            continue
        expected = CLIENT_PROTOCOL.get(name, "")
        request = h.wait_request(before,
                                 lambda r: r["status"] == 200 and
                                 (not expected or r["protocol"] == expected))
        if not request:
            h.check(1, f"{name} hallo", "fail",
                    f"no 200 request with protocol {expected}")
            continue
        if name == "claude-code":
            events = h.events(request["id"])
            has_alias = any(e["type"] == "alias" for e in events)
            has_resolved = any(e["type"] == "model_resolved" for e in events)
            h.check(1, f"{name} alias trace",
                    "pass" if has_alias and has_resolved else "fail",
                    f"request {request['id']} alias={has_alias} "
                    f"model_resolved={has_resolved}")
        h.check(1, f"{name} hallo", "pass",
                f"request {request['id']} protocol {request['protocol']}")


def phase2(h: Harness) -> None:
    bad = h.db_rows(
        "SELECT id, status, error_redacted FROM request_logs "
        "WHERE id > ? AND status >= 500 ORDER BY id",
        (h.watermark_request,),
    )
    h.check(2, "request statuses", "pass" if not bad else "fail",
            f"{len(bad)} failure(s)" + (f": {bad[:3]}" if bad else ""))
    versions = h.db_rows(
        "SELECT DISTINCT json_extract(route_trace,'$.version') AS v "
        "FROM request_logs WHERE id > ?", (h.watermark_request,))
    unknown = [r["v"] for r in versions if r["v"] not in (None, 9)]
    h.check(2, "trace versions", "pass" if not unknown else "fail",
            f"versions {[r['v'] for r in versions]}")
    log = h.home / "logs" / "proxyllm.log"
    errors = []
    if log.exists():
        with open(log, encoding="utf-8", errors="replace") as f:
            f.seek(h.log_offset)
            for line in f.read().splitlines():
                if re.search(r'level=(error|ERROR)|"level":"error"|panic:', line):
                    errors.append(line[:300])
    h.check(2, "proxy log errors", "pass" if not errors else "fail",
            f"{len(errors)} error line(s): {errors[:2]}")
    status, _ = h.admin_call("GET", "/api/admin/logs?limit=5")
    h.check(2, "admin logs read", "pass" if status == 200 else "fail", f"HTTP {status}")


def phase3(h: Harness, clients: list[str], inventory: dict[str, dict],
           timeout: int) -> None:
    prompt = ("Read the file README.txt in the current working directory and reply "
              "with its exact first line. You must use a file-reading tool.")
    for name in clients:
        row = inventory.get(name, {})
        if not row.get("installed"):
            h.check(3, f"{name} tools", "skipped_missing", row.get("install", ""))
            continue
        if not row.get("runnable"):
            h.check(3, f"{name} tools", "skipped_unsupported", "adapter unverified")
            continue
        if name not in TOOL_CAPABLE:
            h.check(3, f"{name} tools", "skipped_unsupported",
                    "no agent file tool in this CLI's non-interactive mode")
            continue
        before = h.max_ids()[0]
        result, run_dir = h.run_cli(name, prompt, timeout=min(timeout, 180),
                                    work_file=("README.txt", CANARY + "\n"))
        if result.get("status") == "client_config":
            h.check(3, f"{name} tools", "client_config", json.dumps(result)[:400])
            continue
        output = json.dumps(h.cli_output(run_dir))
        request = h.wait_request(before, lambda r: r["status"] == 200,
                                 timeout=min(timeout, 180))
        if CANARY not in output:
            h.check(3, f"{name} tools", "fail",
                    f"answer did not contain the canary; result={json.dumps(result)[:300]}")
            continue
        if not request:
            h.check(3, f"{name} tools", "fail", "no 200 request row")
            continue
        h.check(3, f"{name} tools", "pass",
                f"request {request['id']} protocol {request['protocol']}")


def check_web_run(h: Harness, name: str, prompt: str, timeout: int,
                  phase: int, label: str) -> None:
    before_req = h.max_ids()[0]
    before_child = h.max_ids()[1]
    result, run_dir = h.run_cli(name, prompt, web=True, timeout=timeout,
                                work_file=("README.txt", CANARY + "\n"))
    if result.get("status") == "client_config":
        h.check(phase, label, "client_config", json.dumps(result)[:400])
        return
    time.sleep(1)
    children = h.children(before_child)
    searches = [c for c in children if c["kind"] == "emulated_tool" and
                c["to_ref"] == "web_search"]
    fetches = [c for c in children if c["kind"] == "emulated_tool" and
               c["to_ref"] == "web_fetch"]
    leaks, unknown = scan_claude_transcript(h.transcript(run_dir))
    detail = (f"search={len(searches)}, fetch={len(fetches)}, "
              f"leaks={leaks}, unknown={len(unknown)}")
    if result.get("status") != "pass":
        h.check(phase, label, "fail", json.dumps(result)[:300] + " " + detail)
        return
    if leaks or unknown:
        h.check(phase, label, "fail",
                f"client-side emulated name/unknown-tool error: {detail}")
        return
    if searches or fetches:
        h.check(phase, label, "pass", detail)
        return
    # The CLI's ToolSearch may have executed its own WebSearch/WebFetch (no
    # proxy child row) or the model chose not to use the tools. Neither is a
    # proxy failure; the leak scanner above is the failing condition.
    request = h.wait_request(before_req, lambda r: r["status"] == 200, timeout=5)
    h.check(phase, label, "model_behavior" if request else "fail",
            "no proxy emulated call; " + detail)


def phase4(h: Harness, clients: list[str], timeout: int) -> None:
    if "claude-code" not in clients:
        h.check(4, "web tools via Claude Code", "skipped_unsupported",
                "claude-code not selected")
        return
    search_fetch = ("Use the WebSearch tool to search for 'PROxyLLM web tool test', "
                    "then use the WebFetch tool on https://example.com and quote its "
                    "main heading. You must use both tools.")
    mixed = ("In the same assistant turn, call WebFetch on https://example.com and "
             "Read README.txt. Report both results.")
    web_timeout = min(max(timeout, 240), 300)
    for i in range(1, 4):
        check_web_run(h, "claude-code", search_fetch, web_timeout, 4,
                      f"web search+fetch run {i}")
    for i in range(1, 7):
        check_web_run(h, "claude-code", mixed, web_timeout, 4,
                      f"mixed web+file run {i}")


def phase5(h: Harness, clients: list[str], timeout: int) -> None:
    if "claude-code" not in clients:
        h.check(5, "neon session via Claude Code", "skipped_unsupported",
                "claude-code not selected")
        return
    prompt = ("Suche ein Bild einer guten und modernen UX Web App in Neon Farben. "
              "Zeige mir verschiedene Versionen, sodass du diese im Anschluss auf "
              "Basis darauf bauen kannst.")
    before_req = h.max_ids()[0]
    before_child = h.max_ids()[1]
    result, run_dir = h.run_cli("claude-code", prompt, web=True,
                                timeout=max(timeout, 900))
    if result.get("status") == "client_config":
        h.check(5, "neon session via Claude Code", "client_config",
                json.dumps(result)[:400])
        return
    if result.get("status") != "pass":
        h.check(5, "neon session via Claude Code", "fail",
                json.dumps(result)[:400])
        return
    rows = h.db_rows(
        "SELECT id, status, error_redacted FROM request_logs WHERE id > ? ORDER BY id",
        (before_req,))
    bad = [r for r in rows if r["status"] >= 500 or r["status"] == 529]
    if bad:
        h.check(5, "neon session via Claude Code", "fail",
                f"{len(bad)} 5xx/529 row(s): {bad[:3]}")
        return
    if not any(r["status"] == 200 for r in rows):
        h.check(5, "neon session via Claude Code", "fail", "no 200 request row")
        return
    leaks, unknown = scan_claude_transcript(h.transcript(run_dir))
    if leaks or unknown:
        h.check(5, "neon session via Claude Code", "fail",
                f"web-tool error in transcript: {leaks} {unknown[:2]}")
        return
    children = h.children(before_child)
    searches = [c for c in children if c["kind"] == "emulated_tool" and
                c["to_ref"] == "web_search"]
    h.check(5, "neon session web flow",
            "pass" if searches else "model_behavior",
            f"search={len(searches)}")
    h.check(5, "neon session via Claude Code", "pass",
            f"{len(rows)} request(s), no 5xx/529")


def phase6(h: Harness, inventory: dict[str, dict], timeout: int) -> None:
    """Failover driven by a real CLI; the first upstream is a real public
    endpoint that refuses connections (no stub, no private-URL opt-in)."""
    row = inventory.get("codex", {})
    if not row.get("installed") or not row.get("runnable"):
        h.check(6, "failover via Codex", "skipped_missing",
                row.get("install", "codex not installed"))
        return
    chat_model = h.inst.get("chat_model", "deepseek-v4.1-flash:1017")
    try:
        h.admin_call("PUT", "/api/admin/providers/acc-unreachable-test", {
            "type": "custom-openai", "name": "Unreachable transport (test)",
            "config": {"base_url": "https://example.com:1/v1"},
            "api_key": "dummy-test-key",
        })
        h.admin_call("PUT", "/api/admin/models/failover-cli-test", {
            "provider_id": "acc-unreachable-test",
            "name": chat_model,
            "capabilities": {"streaming": True, "tools": True, "vision": False},
            "priority": 0,
        })
        h.admin_call("PUT", "/api/admin/routing/failover-cli-test", {
            "match_model": "failover-cli-test",
            "chain": ["acc-unreachable-test", "acc-ollama-cloud"],
            "priority": 0, "enabled": True, "strategy": "fill_first",
        })
        before = h.max_ids()[0]
        result, _ = h.run_cli("codex", "Sag nur Hallo.", model="failover-cli-test",
                              timeout=min(max(timeout, 180), 300))
        request = h.wait_request(before,
                                 lambda r: r["model"] == "failover-cli-test",
                                 timeout=60)
        events = h.events(request["id"]) if request else []
        types = {e["type"] for e in events}
        ok = result.get("status") == "pass" and request is not None and \
            request["status"] == 200 and "failover" in types
        h.check(6, "failover via Codex", "pass" if ok else "fail",
                f"status={result.get('status')} HTTP={request['status'] if request else '-'} "
                f"events={sorted(types)}")
    finally:
        h.admin_call("DELETE", "/api/admin/routing/failover-cli-test")
        h.admin_call("DELETE", "/api/admin/models/failover-cli-test")
        h.admin_call("DELETE", "/api/admin/providers/acc-unreachable-test")


def phase7(h: Harness, inventory: dict[str, dict], timeout: int) -> None:
    row = inventory.get("codex", {})
    if not row.get("installed") or not row.get("runnable"):
        h.check(7, "vision via Codex image", "skipped_missing",
                row.get("install", "codex not installed"))
        return
    image = h.write_png("vision-cli.png", 11)
    before_req = h.max_ids()[0]
    before_child = h.max_ids()[1]
    result, _ = h.run_cli("codex", "Describe the image and use the analyze_image tool.",
                          model="fallback-test", image=image,
                          timeout=min(max(timeout, 180), 300))
    request = h.wait_request(before_req, lambda r: r["model"] == "fallback-test",
                             timeout=60)
    time.sleep(0.5)
    children = h.children(before_child)
    kinds = {c["kind"] for c in children}
    ok = (result.get("status") == "pass" and request is not None and
          request["status"] == 200 and
          bool(kinds & {"image_caption", "analyze_image"}))
    h.check(7, "vision via Codex image", "pass" if ok else "fail",
            f"status={result.get('status')} "
            f"HTTP={request['status'] if request else '-'} kinds={sorted(kinds)}")


def setting_value(h: Harness, key: str) -> str | None:
    status, payload = h.admin_call("GET", "/api/admin/settings")
    if status != 200:
        return None
    for entry in payload.get("entries", []):
        if entry.get("key") == key:
            return entry.get("value", "")
    return None


def phase8(h: Harness, clients: list[str]) -> None:
    """Error-path regression, last: a lowered header wall must produce 504,
    never 500, and the instance must recover after the setting is restored."""
    key = "upstream_response_header_timeout_ms"
    old = setting_value(h, key)
    if "claude-code" not in clients:
        h.check(8, "transport timeout mapping", "skipped_unsupported",
                "claude-code not selected")
        return
    try:
        h.admin_call("PUT", "/api/admin/settings", {"key": key, "value": "100"})
        h.admin_call("PUT", "/api/admin/providers/acc-ollama-cloud/enabled",
                     {"enabled": True})
        before = h.max_ids()[0]
        h.run_cli("claude-code",
                  "Use the WebFetch tool on https://example.com and quote its heading.",
                  web=True, timeout=90)
        rows = h.db_rows(
            "SELECT id, status, error_redacted FROM request_logs "
            "WHERE id > ? ORDER BY id", (before,))
        statuses = [r["status"] for r in rows]
        has504 = 504 in statuses
        has500 = 500 in statuses
        h.check(8, "transport timeout mapping",
                "pass" if has504 and not has500 else "fail",
                f"statuses={statuses}, errors={[r['error_redacted'] for r in rows][:2]}")
    finally:
        if old is None:
            h.admin_call("DELETE", f"/api/admin/settings/{key}")
        else:
            h.admin_call("PUT", "/api/admin/settings", {"key": key, "value": old})
        h.admin_call("PUT", "/api/admin/providers/acc-ollama-cloud/enabled",
                     {"enabled": True})
    # Wait out the breaker (5 s base, possibly escalated) and prove recovery.
    recovered = None
    for _ in range(20):
        before = h.max_ids()[0]
        h.run_cli("claude-code", "Sag nur Hallo.", timeout=60)
        recovered = h.wait_request(before, lambda r: r["status"] == 200,
                                   timeout=10)
        if recovered:
            break
        time.sleep(3)
    h.check(8, "recovery after restore",
            "pass" if recovered else "fail",
            f"request {recovered['id']}" if recovered else "no 200 after 60s")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", default=os.environ.get(
        "PROXYLLM_TEST_HOME", os.path.expanduser("~/proxyllm-test-instance")))
    parser.add_argument("--phases", default="0,1,2,3,4,5,6,7,8")
    parser.add_argument("--clients", default="all")
    parser.add_argument("--strict-upstream", action="store_true")
    parser.add_argument("--run-id", default=None)
    parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()
    phases = {int(p) for p in args.phases.split(",") if p.strip()}
    clients = cli_names(args.clients)
    home = Path(args.home).expanduser().resolve()
    run_id = args.run_id or time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-" + \
        base64.urlsafe_b64encode(os.urandom(3)).decode().rstrip("=")
    h = Harness(home, run_id, args.strict_upstream)
    inventory = cli_inventory()
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    h.watermark_request, h.watermark_child = h.max_ids()

    try:
        if 0 in phases:
            phase0(h, clients, inventory)
        if 1 in phases:
            phase1(h, clients, inventory, args.timeout)
        if 2 in phases:
            phase2(h)
        if 3 in phases:
            phase3(h, clients, inventory, args.timeout)
        if 4 in phases:
            phase4(h, clients, args.timeout)
        if 5 in phases:
            phase5(h, clients, args.timeout)
        if 6 in phases:
            phase6(h, inventory, args.timeout)
        if 7 in phases:
            phase7(h, inventory, args.timeout)
        if 8 in phases:
            phase8(h, clients)
    except Exception as exc:  # keep the report even on a harness error
        h.check(99, "harness", "fail", redact(f"{type(exc).__name__}: {exc}"))
    finally:
        report = {
            "schema": 1,
            "run_id": run_id,
            "started_at": started,
            "finished_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "instance": str(home),
            "binary_sha256": h.inst.get("binary_sha256", ""),
            "git_head": h.inst.get("git_head", ""),
            "models": h.inst.get("resolved", {}),
            "checks": h.checks,
        }
        (h.artifacts / "report.json").write_text(
            json.dumps(report, indent=2), encoding="utf-8")
        subprocess.run([sys.executable, "-I", str(HERE / "report.py"),
                        "--run-dir", str(h.artifacts)], check=False)

    failures = [c for c in h.checks
                if c["status"] in ("fail", "client_config")
                or (args.strict_upstream and c["status"] == "upstream")]
    print(f"\nreport: {h.artifacts / 'report.md'}")
    print(f"checks: {len(h.checks)}, failures: {len(failures)}")
    for check in failures:
        print(f"  FAIL {check['name']}: {check['detail']}")
    return 0 if not failures else 1


if __name__ == "__main__":
    sys.exit(main())

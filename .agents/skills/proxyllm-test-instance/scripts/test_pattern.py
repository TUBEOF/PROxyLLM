#!/usr/bin/env python3
"""End-to-end live test for the PROxyLLM test instance (phases 0-6)."""

from __future__ import annotations

import argparse
import base64
import hashlib
import http.server
import json
import os
import re
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from admin import AdminClient, load_env, redact  # noqa: E402

ADAPTERS = HERE / "adapters"
CLIENT_PROTOCOL = {
    "claude-code": "anthropic",
    "codex": "openai-responses",
    "opencode": "openai",
    "goose": "openai",
    "kimi": "openai",
    "aider": "openai",
    "qwen": "openai",
    "continue": "openai",
    "cline": "anthropic",
    "factory": "openai",
}


def upstreamish(status: int, payload: dict) -> bool:
    """True when a non-200 carries an upstream-failure marker."""
    text = json.dumps(payload).lower()
    return any(key in text for key in (
        "transport", "timeout", "chain exhausted", "http2",
        "connection refused", "upstream error", "upstream_error",
        "routing: ",
    ))


class FailureHandler(http.server.BaseHTTPRequestHandler):
    """A local upstream that fails fast with a retryable 500."""

    def _fail(self) -> None:
        body = json.dumps({
            "error": {"message": "injected upstream failure", "type": "server_error"}
        }).encode()
        self.send_response(500)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_POST = _fail
    do_GET = _fail

    def log_message(self, *_args) -> None:  # keep the test output clean
        pass


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

    def v1_messages(self, model: str, content, tools: list | None = None,
                    max_tokens: int = 512, timeout: int = 120) -> tuple[int, dict]:
        body = {"model": model, "max_tokens": max_tokens, "messages": [
            {"role": "user", "content": content}]}
        if tools:
            body["tools"] = tools
        req = urllib.request.Request(
            self.base + "/v1/messages",
            data=json.dumps(body).encode(),
            headers={
                "Content-Type": "application/json",
                "x-api-key": self.env.get("CLIENT_KEY", ""),
                "anthropic-version": "2023-06-01",
            },
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return resp.status, json.loads(resp.read() or b"{}")
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                payload = json.loads(raw)
            except json.JSONDecodeError:
                payload = {"error": raw.decode("utf-8", "replace")[:400]}
            return exc.code, payload

    def run_cli(self, name: str, prompt: str, web: bool = False,
                timeout: int = 120) -> dict:
        manifest = ADAPTERS / f"{name}.json"
        run_dir = self.home / "clients" / name / self.run_id
        run_dir.mkdir(parents=True, exist_ok=True)
        cmd = [
            sys.executable, "-I", str(HERE / "run_cli.py"),
            "--manifest", str(manifest),
            "--run-dir", str(run_dir),
            "--base-url", self.base,
            "--model", "proxy-chat",
            "--prompt", prompt,
            "--timeout", str(timeout),
        ]
        if web:
            cmd.append("--web")
        env = dict(os.environ)
        env["PROXYLLM_TEST_CLIENT_KEY"] = self.env.get("CLIENT_KEY", "")
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 30,
                              check=False, env=env)
        (self.artifacts / "cli" / name).mkdir(parents=True, exist_ok=True)
        (self.artifacts / "cli" / name / "runner.stdout").write_text(
            redact(proc.stdout), encoding="utf-8")
        (self.artifacts / "cli" / name / "runner.stderr").write_text(
            redact(proc.stderr), encoding="utf-8")
        try:
            return json.loads(proc.stdout.strip().splitlines()[-1])
        except (IndexError, json.JSONDecodeError):
            return {"cli": name, "status": "fail",
                    "detail": redact(proc.stderr[-500:]) or "runner produced no JSON"}

    def write_png(self, name: str, seed: int) -> Path:
        path = self.artifacts / "images" / name
        path.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run([sys.executable, "-I", str(HERE / "make_png.py"),
                        str(path), "--seed", str(seed)],
                       check=True, capture_output=True)
        return path

    def image_content(self, path: Path, question: str) -> list[dict]:
        data = base64.b64encode(path.read_bytes()).decode()
        return [
            {"type": "text", "text": question},
            {"type": "image", "source": {
                "type": "base64", "media_type": "image/png", "data": data}},
        ]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", default=os.environ.get(
        "PROXYLLM_TEST_HOME", os.path.expanduser("~/proxyllm-test-instance")))
    parser.add_argument("--phases", default="0,1,2,3,4,5,6")
    parser.add_argument("--clients", default="claude-code,codex,opencode")
    parser.add_argument("--strict-upstream", action="store_true")
    parser.add_argument("--run-id", default=None)
    parser.add_argument("--timeout", type=int, default=120)
    parser.add_argument("--no-failover-injection", action="store_true")
    args = parser.parse_args()
    phases = {int(p) for p in args.phases.split(",") if p.strip()}
    clients = [c.strip() for c in args.clients.split(",") if c.strip()]
    home = Path(args.home).expanduser().resolve()
    run_id = args.run_id or time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-" + \
        base64.urlsafe_b64encode(os.urandom(3)).decode().rstrip("=")
    h = Harness(home, run_id, args.strict_upstream)
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    h.watermark_request, h.watermark_child = h.max_ids()

    try:
        if 0 in phases:
            phase0(h, clients)
        if 1 in phases:
            phase1(h, clients, args.timeout)
        if 2 in phases:
            phase2(h)
        if 3 in phases:
            phase3(h, args.no_failover_injection)
        if 4 in phases:
            phase4(h, clients, args.timeout)
        if 5 in phases:
            phase5(h)
        if 6 in phases:
            phase6(h, clients, args.timeout)
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


def phase0(h: Harness, clients: list[str]) -> None:
    try:
        with urllib.request.urlopen(h.base + "/healthz", timeout=5) as resp:
            h.check(0, "healthz", "pass" if resp.status == 200 else "fail",
                    f"HTTP {resp.status}")
    except Exception as exc:
        h.check(0, "healthz", "fail", redact(str(exc)))
    h.check(0, ".env 0600", "pass" if oct((h.home / ".env").stat().st_mode)[-3:] == "600"
            else "fail")
    h.check(0, "home 0700", "pass" if oct(h.home.stat().st_mode)[-3:] == "700" else "fail")
    status, payload = h.admin_call("GET", "/api/admin/providers")
    providers = payload.get("providers", []) if status == 200 else []
    ids = {p.get("id") for p in providers}
    h.check(0, "providers", "pass" if {"acc-ollama-cloud", "acc-ollama-web"} <= ids
            else "fail", f"{sorted(ids)}")
    status, payload = h.admin.call("GET", "/v1/models",
                                   bearer=h.env.get("CLIENT_KEY", ""), timeout=15)
    h.check(0, "client key", "pass" if status == 200 else "fail", f"HTTP {status}")
    pid_file = h.home / "run" / "proxyllm.pid"
    bin_path = h.home / "bin" / "proxyllm"
    if pid_file.exists():
        pid = pid_file.read_text().strip()
        try:
            cmdline = Path(f"/proc/{pid}/cmdline").read_bytes().replace(b"\0", b" ").decode(
                errors="replace")
        except OSError:
            cmdline = ""
        h.check(0, "pid ownership", "pass" if str(bin_path) in cmdline else "fail",
                f"pid {pid}")
    else:
        h.check(0, "pid ownership", "fail", "pid file missing")
    expected_sha = str(h.inst.get("binary_sha256", ""))
    actual_sha = (hashlib.sha256(bin_path.read_bytes()).hexdigest()
                  if bin_path.exists() else "")
    h.check(0, "binary sha",
            "pass" if expected_sha and actual_sha == expected_sha else "fail",
            actual_sha[:16])
    status, payload = h.admin_call("GET", "/api/admin/capability-fallbacks")
    ok = status == 200 and payload.get("vision") == [h.inst.get("resolved", {}).get("chat")] \
        and payload.get("web_search") == ["acc-ollama-web"]
    h.check(0, "fallbacks", "pass" if ok else "fail", json.dumps(payload)[:300])
    proc = subprocess.run([sys.executable, "-I", str(HERE / "clients.py"), "list"],
                          capture_output=True, text=True, check=False)
    try:
        inventory = json.loads(proc.stdout)
    except json.JSONDecodeError:
        inventory = []
    for row in inventory:
        if row["name"] in clients:
            if row["installed"]:
                h.check(0, f"cli {row['name']}", "pass",
                        f"{row['version']} (tested {row['tested']})")
            else:
                h.check(0, f"cli {row['name']}", "skipped_missing", row["install"])


def phase1(h: Harness, clients: list[str], timeout: int) -> None:
    proc = subprocess.run([sys.executable, "-I", str(HERE / "clients.py"), "list"],
                          capture_output=True, text=True, check=False)
    try:
        inventory = {row["name"]: row for row in json.loads(proc.stdout)}
    except json.JSONDecodeError:
        inventory = {}
    for name in clients:
        row = inventory.get(name, {})
        if not row.get("installed"):
            h.check(1, f"{name} hallo", "skipped_missing",
                    row.get("install", "CLI not installed"))
            continue
        if not row.get("runnable"):
            h.check(1, f"{name} hallo", "skipped_unsupported", "adapter unverified")
            continue
        before = h.max_ids()[0]
        result = h.run_cli(name, "Hallo", timeout=timeout)
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
        detail = f"request {request['id']} protocol {request['protocol']}"
        if name == "claude-code":
            events = h.events(request["id"])
            has_alias = any(e["type"] == "alias" for e in events)
            has_resolved = any(e["type"] == "model_resolved" for e in events)
            h.check(1, f"{name} alias trace",
                    "pass" if has_alias and has_resolved else "fail",
                    f"request {request['id']} alias={has_alias} "
                    f"model_resolved={has_resolved}")
        h.check(1, f"{name} hallo", "pass", detail)


def phase2(h: Harness) -> None:
    proc = subprocess.run(
        [sys.executable, "-I", str(HERE / "db.py"), "--db", str(h.db), "scan",
         "--since-request", str(h.watermark_request),
         "--since-child", str(h.watermark_child)],
        capture_output=True, text=True, check=False)
    try:
        scan = json.loads(proc.stdout)
    except json.JSONDecodeError:
        h.check(2, "db scan", "fail", redact(proc.stderr[-400:]))
        return
    failures = scan.get("failures", [])
    h.check(2, "request statuses", "pass" if not failures else "fail",
            f"{len(failures)} failure(s)")
    h.check(2, "trace versions", "pass" if not scan.get("unknown_trace_versions")
            else "fail", f"versions {scan.get('trace_versions')}")
    log = h.home / "logs" / "proxyllm.log"
    bad = []
    if log.exists():
        with open(log, encoding="utf-8", errors="replace") as f:
            f.seek(h.log_offset)
            text = f.read()
        for line in text.splitlines():
            if re.search(r'level=(error|ERROR)|"level":"error"|panic:', line):
                bad.append(line[:300])
    h.check(2, "proxy log errors", "pass" if not bad else "fail",
            f"{len(bad)} error line(s): {bad[:2]}")
    # Exercise the console read path.
    status, payload = h.admin_call(
        "GET", f"/api/admin/logs?limit=5&after={h.watermark_request}")
    h.check(2, "admin logs read", "pass" if status == 200 else "fail", f"HTTP {status}")


def phase3(h: Harness, no_failover: bool) -> None:
    # 3a alias
    before = h.max_ids()[0]
    status, payload = h.v1_messages("claude-sonnet-4-6", "Hallo", max_tokens=16)
    request = h.wait_request(before, lambda r: r["model"] == "claude-sonnet-4-6")
    ok = status == 200 and request is not None
    alias = any(e["type"] == "alias" for e in h.events(request["id"])) if request else False
    h.check(3, "alias request", "pass" if ok and alias else "fail",
            f"HTTP {status}, alias={alias}")

    # 3b vision fallback
    image = h.write_png("vision-fallback.png", 11)
    before_req = h.max_ids()[0]
    before = h.max_ids()[1]
    status, payload = h.v1_messages(
        "fallback-test", h.image_content(image, "Beschreibe das Bild und nutze analyze_image."),
        max_tokens=400)
    request = h.wait_request(before_req, lambda r: r["model"] == "fallback-test",
                             timeout=30)
    time.sleep(0.5)
    children = h.children(before)
    kinds = {c["kind"] for c in children}
    events = h.events(request["id"]) if request else []
    event_types = {e["type"] for e in events}
    vision_ok = (status == 200 and bool(kinds & {"image_caption", "analyze_image"})
                 and "capability_fallback" in event_types)
    if vision_ok:
        outcome = "pass"
    elif upstreamish(status, payload):
        outcome = "upstream"
    else:
        outcome = "fail"
    h.check(3, "vision fallback", outcome,
            f"HTTP {status}, child kinds={sorted(kinds)}, "
            f"events={sorted(event_types)}")

    if no_failover:
        h.check(3, "failover injection", "skipped", "--no-failover-injection")
        return
    if not h.inst.get("allow_private_provider_urls"):
        h.check(3, "failover injection", "skipped",
                "start the instance with --allow-private-provider-urls to inject a local 500")
        return
    # 3c failover injection: a local upstream returns 500, the chain must walk on.
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FailureHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    broken_port = server.server_address[1]
    try:
        h.admin_call("PUT", "/api/admin/providers/acc-broken-transport", {
            "type": "custom-openai", "name": "Broken transport (test)",
            "config": {"base_url": f"http://127.0.0.1:{broken_port}/v1"},
            "api_key": "dummy-test-key",
        })
        h.admin_call("PUT", "/api/admin/models/fallback-chain-test", {
            "provider_id": "acc-broken-transport",
            "name": h.inst.get("chat_model", "deepseek-v4.1-flash:1017"),
            "capabilities": {"streaming": True, "tools": True, "vision": True},
            "priority": 0,
        })
        h.admin_call("PUT", "/api/admin/routing/fallback-chain-test", {
            "match_model": "fallback-chain-test",
            "chain": ["acc-broken-transport", "acc-ollama-cloud"],
            "priority": 0, "enabled": True, "strategy": "fill_first",
        })
        before = h.max_ids()[0]
        status, _ = h.v1_messages("fallback-chain-test", "Hallo", max_tokens=16)
        request = h.wait_request(before, lambda r: r["model"] == "fallback-chain-test",
                                 timeout=60)
        events = h.events(request["id"]) if request else []
        types = {e["type"] for e in events}
        child_rows = h.db_rows(
            "SELECT kind FROM request_child_logs WHERE parent_id = ?", (request["id"],)
        ) if request else []
        kinds = {c["kind"] for c in child_rows}
        ok = status == 200 and "failover" in types and "failover" in kinds
        h.check(3, "failover injection", "pass" if ok else "fail",
                f"HTTP {status}, events={sorted(types)}, children={sorted(kinds)}")
    finally:
        h.admin_call("DELETE", "/api/admin/routing/fallback-chain-test")
        h.admin_call("DELETE", "/api/admin/models/fallback-chain-test")
        h.admin_call("DELETE", "/api/admin/providers/acc-broken-transport")
        server.shutdown()
        server.server_close()


def phase4(h: Harness, clients: list[str], timeout: int) -> None:
    before = h.max_ids()[1]
    status, payload = h.v1_messages(
        "proxy-chat", "Suche nach aktuellen Neon-UI-Designbeispielen.",
        tools=[{"type": "web_search_20250305", "name": "web_search", "max_uses": 3}])
    time.sleep(0.5)
    children = h.children(before)
    search = [c for c in children if c["kind"] == "emulated_tool" and
              c["to_ref"] == "web_search"]
    outcome = ("pass" if status == 200 and search else
               "upstream" if upstreamish(status, payload) else
               "model_behavior" if status == 200 else "fail")
    h.check(4, "web_search protocol", outcome,
            f"HTTP {status}, emulated={len(search)}")

    before = h.max_ids()[1]
    status, payload = h.v1_messages(
        "proxy-chat", "Fasse https://example.com zusammen.",
        tools=[{"type": "web_fetch_20250910", "name": "web_fetch", "max_uses": 3}])
    time.sleep(0.5)
    children = h.children(before)
    fetch = [c for c in children if c["kind"] == "emulated_tool" and
             c["to_ref"] == "web_fetch"]
    outcome = ("pass" if status == 200 and fetch else
               "upstream" if upstreamish(status, payload) else
               "model_behavior" if status == 200 else "fail")
    h.check(4, "web_fetch protocol", outcome,
            f"HTTP {status}, emulated={len(fetch)}")
    if fetch:
        bad = [c for c in fetch if re.search(r"\b(202|403|404|500|0 bytes)\b",
                                             c.get("detail", ""))]
        h.check(4, "web_fetch upstream visibility",
                "upstream" if bad else "pass",
                f"{len(bad)} upstream failure(s) visible in child detail")

    for name in clients:
        if name != "claude-code":
            h.check(4, f"web tools via {name}", "skipped_unsupported",
                    "client-side web tools or not claimed by the proxy")
    if "claude-code" not in clients:
        return
    before = h.max_ids()[1]
    result = h.run_cli("claude-code",
                       "Nutze WebSearch nach Neon-UI-Design und WebFetch auf "
                       "https://example.com; fasse kurz zusammen.",
                       web=True, timeout=max(timeout, 360))
    time.sleep(1)
    children = h.children(before)
    emulated = [c for c in children if c["kind"] == "emulated_tool"]
    h.check(4, "web tools via Claude Code",
            "pass" if emulated else "warn",
            f"runner={result.get('status')}, emulated={len(emulated)}")


def phase5(h: Harness) -> None:
    questions = [
        "Welche Farben dominieren? Nutze analyze_image.",
        "Beschreibe das Layout und die Formen.",
        "Welche Farbflächen erkennst du?",
    ]
    analyze_seen = False
    for index, question in enumerate(questions, start=1):
        image = h.write_png(f"vision-{index}.png", 20 + index)
        before = h.max_ids()[1]
        status, payload = h.v1_messages("fallback-test", h.image_content(image, question),
                                        max_tokens=400)
        time.sleep(0.5)
        children = h.children(before)
        kinds = {c["kind"] for c in children}
        if "analyze_image" in kinds:
            analyze_seen = True
        if status == 200 and kinds & {"image_caption", "analyze_image"}:
            outcome = "pass"
        elif upstreamish(status, payload):
            outcome = "upstream"
        else:
            outcome = "fail"
        h.check(5, f"vision run {index}", outcome,
                f"HTTP {status}, kinds={sorted(kinds)}")
    h.check(5, "analyze_image called", "pass" if analyze_seen else "warn",
            "called in at least one run" if analyze_seen
            else "the model did not call the injected tool in any run")

def phase6(h: Harness, clients: list[str], timeout: int) -> None:
    prompt = ("Suche ein Bild einer guten und modernen UX Web App in Neon Farben. "
              "Zeige mir verschiedene Versionen, sodass du diese im Anschluss auf "
              "Basis darauf bauen kannst.")
    before = h.max_ids()[1]
    claude_status = None
    if "claude-code" in clients:
        proc = subprocess.run(
            [sys.executable, "-I", str(HERE / "clients.py"), "list"],
            capture_output=True, text=True, check=False)
        try:
            installed = {r["name"]: r["installed"] for r in json.loads(proc.stdout)}
        except json.JSONDecodeError:
            installed = {}
        if installed.get("claude-code"):
            result = h.run_cli("claude-code", prompt, web=True,
                               timeout=max(timeout, 360))
            claude_status = result.get("status", "fail")
            if claude_status == "client_config":
                h.check(6, "neon prompt via Claude Code", "client_config",
                        json.dumps(result)[:400])
            else:
                h.check(6, "neon prompt via Claude Code",
                        "pass" if claude_status == "pass" else "fail",
                        json.dumps(result)[:400])
        else:
            h.check(6, "neon prompt via Claude Code", "skipped_missing",
                    "claude not installed")
    if claude_status != "pass":
        status, payload = h.v1_messages(
            "proxy-chat", prompt,
            tools=[{"type": "web_search_20250305", "name": "web_search",
                    "max_uses": 3},
                   {"type": "web_fetch_20250910", "name": "web_fetch",
                    "max_uses": 3}], max_tokens=2048, timeout=360)
        outcome = ("pass" if status == 200 else
                   "upstream" if upstreamish(status, payload) else "fail")
        h.check(6, "neon prompt protocol", outcome, f"HTTP {status}")
    time.sleep(1)
    children = h.children(before)
    emulated = [c for c in children if c["kind"] == "emulated_tool"]
    searches = [c for c in emulated if c["to_ref"] == "web_search"]
    fetches = [c for c in emulated if c["to_ref"] == "web_fetch"]
    detail = (f"search={len(searches)}, fetch={len(fetches)}"
              + (f", fetch details={[c['detail'][:80] for c in fetches[:3]]}"
                 if fetches else ""))
    h.check(6, "neon prompt web flow",
            "pass" if searches else "model_behavior", detail)


if __name__ == "__main__":
    sys.exit(main())

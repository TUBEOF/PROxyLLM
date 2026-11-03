#!/usr/bin/env python3
"""M2 — the plain-start CLI matrix.

Every CLI runs the way a user starts it: the bare binary in a pseudo-terminal
(tmux), only its own config pointing at PROxyLLM — no -p, no --search, no
flags. The upstream is MockServer (deterministic, keyless), so the matrix is
reproducible on every PR.

Scenarios per CLI x upstream:
  answer  — send one prompt, the pane must show the simulated answer.
  recall  — turn 1 "remember 4711", turn 2 "which number?"; the second
            upstream request must carry the whole history (asserted from
            MockServer's request log), and the pane must show the answer.

Evidence is the pane text AND the upstream request log AND the proxy's
request_logs rows — a green cell proves the full loop.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import secrets
import shutil
import sqlite3
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]

# run_cli.py owns the adapter-isolation contract (env scrub, config rendering,
# canary); the plain driver reuses its renderer and mirrors its bootstrap.
_spec = importlib.util.spec_from_file_location("run_cli", REPO / ".github/live-cli/run_cli.py")
run_cli = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_cli)

_spec = importlib.util.spec_from_file_location("admin", REPO / ".github/live-cli/admin.py")
admin = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(admin)

MODEL = "sim-model"
PANE_COLS, PANE_ROWS = 140, 44
UPSTREAM_PATH = "/v1/chat/completions"


# --- proxy lifecycle -------------------------------------------------------


def start_proxy(home: Path, binary: Path, port: int) -> subprocess.Popen:
    data = home / "data"
    logs = home / "logs"
    run = home / "run"
    for d in (data, logs, run):
        d.mkdir(parents=True, exist_ok=True)
    key_path = data / "master.key"
    key_path.write_text(secrets.token_hex(32) + "\n")
    env = dict(os.environ)
    env.update({
        "PROXYLLM_SERVER_ADDR": f"127.0.0.1:{port}",
        "PROXYLLM_STORAGE_PATH": str(data / "proxyllm.db"),
        "PROXYLLM_MASTER_KEY_PATH": str(key_path),
        "PROXYLLM_LOG_FORMAT": "json",
        "PROXYLLM_LOG_LEVEL": "info",
        # The simulated upstream is a loopback MockServer.
        "PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS": "1",
    })
    log = open(logs / "proxyllm.log", "ab")
    proc = subprocess.Popen([str(binary)], env=env, stdout=log, stderr=log)
    deadline = time.time() + 30
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=1) as r:
                if r.status == 200:
                    return proc
        except Exception:
            time.sleep(0.25)
    proc.kill()
    raise SystemExit("proxy did not become healthy; see logs/proxyllm.log")


def configure(home: Path, port: int, mockserver: str) -> str:
    """Admin-configure the instance against MockServer; returns the client key."""
    client = admin.AdminClient(f"http://127.0.0.1:{port}", str(home / "run" / "admin-cookies.txt"))
    user = "matrix-admin"
    password = secrets.token_hex(16)
    env_path = home / ".env"
    env_path.write_text(f"ADMIN_USER={user}\nADMIN_PASSWORD={password}\n")
    client.login(str(env_path), user, password)
    admin.put(client, "/api/admin/providers/acc-sim", {
        "type": "custom-openai",
        "name": "Simulated upstream",
        "config": {"base_url": mockserver + "/v1"},
        "api_key": "simulated-key",
    })
    admin.put(client, "/api/admin/providers/acc-sim/enabled", {"enabled": True})
    admin.put(client, "/api/admin/models/" + MODEL, {
        "provider_id": "acc-sim", "name": MODEL, "capabilities": {}, "compat": {},
    })
    # Claude Code's env pins model names (claude-sonnet-4-6, ...); the alias
    # row routes every claude-* name onto the simulated upstream.
    admin.put(client, "/api/admin/settings", {
        "key": "model_aliases",
        "value": json.dumps({"claude-*": MODEL, MODEL: MODEL}, separators=(",", ":"), sort_keys=True),
    })
    payload = admin.post(client, "/api/admin/keys", {"name": "cli-plain"})
    key = payload.get("key", "")
    if not key:
        raise SystemExit("client key creation returned no key")
    return key


# --- MockServer expectations ------------------------------------------------


def ms_reset(mockserver: str) -> None:
    req = urllib.request.Request(mockserver + "/mockserver/reset", data=b"{}", method="PUT")
    with urllib.request.urlopen(req, timeout=5):
        pass


# Observed wire facts (2026-11 codex 0.166 / claude-code 2.1.316 / crush
# 0.99.0 / openhands 0.59.0): codex buffers /v1/responses turns upstream (no
# "stream" key; plain string message content) and drives utility calls —
# task-title generation — as STREAMED requests quoting the conversation;
# Claude streams every Messages turn and sends the user prompt as its own text
# part; crush streams chat completions with the user prompt as a plain string,
# and its title calls stream too; openhands (with native_tool_calling in its
# [llm] config) posts closed chat completions with the prompt as a plain
# string and never streams — its agent loop has no utility calls, but the
# CodeAct runtime appends a <RUNTIME_INFORMATION> user message after the
# prompt on the first turn only. MockServer's LLM response follows the
# expectation's streaming flag only, and its JSON matcher wants exact leaf
# values, so each turn is pinned by its exact last user message in the CLI's
# own shape, later turns ranked higher (their request carries earlier prompts
# in history). The fallback buckets catch the utility traffic so no upstream
# call 404s (the proxy would log the failure and fail the clean-rows check).
def _mock_expectation(mockserver: str, text: str, streaming: bool,
                      priority: int, times: int | None, matcher: dict | None) -> None:
    req: dict = {"method": "POST", "path": UPSTREAM_PATH}
    if matcher:
        req["body"] = {"type": "JSON", "json": matcher, "matchType": "LOOSE"}
    body: dict = {"httpRequest": req,
                  "httpLlmResponse": {"provider": "OPENAI", "model": MODEL,
                                      "completion": {"text": text, "streaming": streaming}},
                  "priority": priority}
    if times is not None:
        body["times"] = {"remainingTimes": times, "unlimited": False}
    try:
        call = urllib.request.Request(
            mockserver + "/mockserver/expectation",
            data=json.dumps(body).encode(), method="PUT",
            headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(call, timeout=5) as r:
            if r.status != 201:
                raise SystemExit(f"expectation {text!r} -> HTTP {r.status}")
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "replace")[:300]
        raise SystemExit(f"expectation {text!r} -> HTTP {exc.code}: {detail}") from exc


def mock_pinned(mockserver: str, cli: str, turns: list[tuple[str, str]], streaming: bool) -> None:
    for rank, (prompt, text) in enumerate(turns):
        matcher: dict = {"messages": [{"role": "user", "content": prompt}]}
        if streaming:
            matcher["stream"] = True
            priority = 150 + 10 * rank
        else:
            priority = 140 + 10 * rank
        if cli == "claude-code":  # the only part-array sender
            matcher["messages"][0]["content"] = [{"text": prompt}]
        _mock_expectation(mockserver, text, streaming, priority, None, matcher)


def mock_fallback(mockserver: str, text: str, streaming: bool) -> None:
    _mock_expectation(mockserver, text, streaming, 10, None,
                      {"stream": True} if streaming else None)


def turn_requests(reqs: list[dict], prompt: str, cli: str = "") -> list[dict]:
    """Real model turns, not utility calls: upstream requests whose LAST
    message carries the prompt as an exact message or text part (codex's
    title calls quote the conversation, so containment would count them).
    Runtime-metadata user messages some CLIs append after the prompt —
    openhands's CodeAct runtime inserts one <RUNTIME_INFORMATION> turn —
    never carry the prompt, so the cli's hint drops them from the LAST
    message check."""
    out = []
    for r in reqs:
        if r.get("path") != UPSTREAM_PATH:
            continue
        payload = (r.get("body") or {}).get("json") or {}
        msgs = payload.get("messages") or []
        hint = RUNTIME_HINTS.get(cli)
        if hint:
            msgs = [m for m in msgs if hint not in str(m.get("content") or "")]
        if not msgs or msgs[-1].get("role") != "user":
            continue
        last = msgs[-1]
        content = last.get("content")
        if isinstance(content, str):
            if content == prompt:
                out.append(r)
        elif isinstance(content, list):
            if any(isinstance(p, dict) and p.get("type") == "text" and p.get("text") == prompt
                   for p in content):
                out.append(r)
    return out


def ms_requests(mockserver: str) -> list[dict]:
    req = urllib.request.Request(
        mockserver + "/mockserver/retrieve?type=REQUESTS&format=JSON",
        data=b"{}", method="PUT", headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read().decode())


# plain_command is the user's start: the bare binary, no arguments.
def plain_command(manifest: Path) -> str:
    return json.loads(manifest.read_text()).get("command", "")


# A CLI is "ready" once its own chrome painted; each TUI names itself.
READY_MARKERS = {"codex": "codex", "claude-code": "Claude", "crush": "crush",
                 "openhands": "OpenHands CLI v"}

# How each CLI's model turns stream upstream, and what the catch-all bucket
# answer must look like to absorb the utility traffic (title generation):
# codex buffers turns but streams its title calls, claude-code streams
# nothing but turns, crush streams everything, openhands never streams its
# turns and makes no utility calls.
STREAMING = {"codex": False, "claude-code": True, "crush": True, "openhands": False}
FALLBACK_STREAMING = {"codex": True, "claude-code": False, "crush": True,
                      "openhands": False}

# User-message markers that are runtime metadata, not model turns; see
# turn_requests.
RUNTIME_HINTS = {"openhands": "<RUNTIME_INFORMATION>"}

# First-run dialogs some TUIs paint before the composer; each entry waits for
# its pane text, then presses keys. claude-code 2.1.316's trust dialog
# defaults to "No, exit" — the Down moves it to "Yes, I trust this folder";
# a blind Enter there exits the CLI with an empty pane.
ONBOARDING = {
    "claude-code": [
        ("Choose the text style", ["Enter"]),
        ("Press Enter to continue", ["Enter"]),
        ("Quick safety check", ["Down", "Enter"]),
    ],
}


def kill_codex_daemons() -> None:
    """codex 0.166 spawns a per-CODEX_HOME app-server daemon whose control
    socket lives in /tmp/codex-daemon-1000. The driver wipes the home every
    run but the daemon and its socket survive; the next run's TUI then dies
    with 'app-server control socket is already in use' and an empty pane.
    A fresh CI box has neither; killing the strays restores that state."""
    out = subprocess.run(["pgrep", "-f", "codex app-server"],
                         capture_output=True, text=True).stdout
    for pid in out.split():
        subprocess.run(["kill", pid], check=False)
    sockets = Path("/tmp/codex-daemon-1000")
    if sockets.is_dir():
        for entry in sockets.iterdir():
            entry.unlink(missing_ok=True)


# --- tmux driver ------------------------------------------------------------


def tmux(*args: str, check: bool = True) -> str:
    out = subprocess.run(["tmux", *args], capture_output=True, text=True, check=check)
    return out.stdout


def tmux_spawn(name: str, cwd: Path, env: dict, command: list[str]) -> None:
    argv = ["env", *[f"{k}={v}" for k, v in env.items()], *command]
    quoted = " ".join("'" + a.replace("'", "'\\''") + "'" for a in argv)
    tmux("new-session", "-d", "-s", name, "-x", str(PANE_COLS), "-y", str(PANE_ROWS), "-c", str(cwd), quoted)


def tmux_send(name: str, text: str) -> None:
    tmux("send-keys", "-t", name, "-l", text)
    tmux("send-keys", "-t", name, "Enter")


def tmux_pane(name: str) -> str:
    return tmux("capture-pane", "-p", "-t", name, check=False)


def submit(name: str, text: str) -> None:
    """Send one prompt and submit it. The first Enter can race the TUI's own
    async input handling, so a second Enter follows while the composer still
    holds the text; an Enter on an empty composer is a no-op."""
    tmux_send(name, text)
    time.sleep(1.0)
    tmux_send(name, "")
    pane = tmux_pane(name)
    if text in pane.split("›")[-1] if "›" in pane else False:
        pass  # composer still holds it — the extra Enter above was submitted
    # Give the TUI a beat; a second Enter is harmless either way.
    time.sleep(1.5)
    tmux_send(name, "")
    time.sleep(1.0)


def wait_pane_count(name: str, marker: str, want: int, timeout: float, what: str) -> str:
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        last = tmux_pane(name)
        if last.count(marker) >= want:
            return last
        time.sleep(0.5)
    raise AssertionError(f"{what}: pane shows {marker!r} {last.count(marker)}x, want {want}; last pane:\n{last}")


def wait_pane(name: str, marker: str, timeout: float, what: str) -> str:
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        last = tmux_pane(name)
        if marker in last:
            return last
        time.sleep(0.5)
    raise AssertionError(f"{what}: pane never showed {marker!r}; last pane:\n{last}")


def onboard(name: str, cli: str) -> None:
    """Walk the CLI's first-run dialogs; nothing to do when it starts clean."""
    for marker, keys in ONBOARDING.get(cli, ()):
        wait_pane(name, marker, 45, f"onboarding: {marker!r}")
        for key in keys:
            tmux("send-keys", "-t", name, key)
            time.sleep(0.4)
    time.sleep(1.5)


# --- CLI bootstrap (mirrors run_cli's isolation contract) --------------------


def prepare(manifest_path: Path, run_dir: Path, base_url: str, key: str, model: str) -> tuple[dict, Path]:
    manifest = json.loads(manifest_path.read_text())
    dirs = {
        "RUN_DIR": str(run_dir),
        "HOME": str(run_dir / "home"),
        "CONFIG_DIR": str(run_dir / "config"),
        "DATA_DIR": str(run_dir / "data"),
        "CACHE_DIR": str(run_dir / "cache"),
        "TMP_DIR": str(run_dir / "tmp"),
        "WORK_DIR": str(run_dir / "work"),
        "BASE_URL": base_url,
        "CLIENT_KEY": key,
        "MODEL": model,
        "PROMPT": "",
        "IMAGE": "",
        "FILE": str(run_dir / "work" / "notes.txt"),
        "MARKER": "PROXYLLM-LIVE-MARKER-7Q",
    }
    for k, path in dirs.items():
        if k in ("FILE", "IMAGE", "PROMPT"):
            continue
        Path(path).mkdir(parents=True, exist_ok=True)
    os.chmod(dirs["HOME"], 0o700)
    Path(dirs["FILE"]).write_text("PROXYLLM-LIVE-MARKER-7Q\nsecond line\n")

    env: dict[str, str] = {}
    for keep in manifest.get("env_keep", ["PATH", "LANG", "LC_ALL", "TERM", "TMPDIR"]):
        if keep in os.environ:
            env[keep] = os.environ[keep]
        elif keep == "TERM":
            # A TUI cannot run without a terminal type; a driver context
            # frequently has none.
            env["TERM"] = "xterm-256color"
    for k, v in (manifest.get("env") or {}).items():
        env[k] = run_cli.render(str(v), dirs)
    env.update({
        "HOME": dirs["HOME"],
        "XDG_CONFIG_HOME": dirs["CONFIG_DIR"],
        "XDG_DATA_HOME": dirs["DATA_DIR"],
        "XDG_CACHE_HOME": dirs["CACHE_DIR"],
        "TMPDIR": dirs["TMP_DIR"],
        "NO_PROXY": "127.0.0.1,localhost",
        "no_proxy": "127.0.0.1,localhost",
    })
    for rel, content in (manifest.get("files") or {}).items():
        target = Path(run_cli.render(rel, dirs))
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(run_cli.render(str(content), dirs))
        target.chmod(0o600)
    return env, Path(dirs["WORK_DIR"])


# --- proxy evidence ----------------------------------------------------------


def proxy_rows(home: Path) -> list[tuple]:
    db = home / "data" / "proxyllm.db"
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=5)
    try:
        conn.execute("PRAGMA query_only=ON")
        return conn.execute(
            "SELECT id, status, error_redacted FROM request_logs ORDER BY id").fetchall()
    finally:
        conn.close()


def wait_proxy_rows(home: Path, at_least: int, timeout: float = 15) -> list[tuple]:
    """The request-log write is off the request path, so the row can lag the
    answer the CLI already rendered (the live suite polls for the same
    reason)."""
    deadline = time.time() + timeout
    rows = []
    while time.time() < deadline:
        rows = proxy_rows(home)
        if len(rows) >= at_least:
            return rows
        time.sleep(0.3)
    return rows


# --- scenarios ----------------------------------------------------------------


def scenario_answer(cli: str, manifest: Path, home: Path, base_url: str, key: str, mockserver: str, port: int) -> None:
    marker = "ECHO-" + secrets.token_hex(4)
    prompt = f"Reply with exactly: {marker}."
    ms_reset(mockserver)
    mock_pinned(mockserver, cli, [(prompt, marker)], STREAMING[cli])
    mock_fallback(mockserver, "ok", FALLBACK_STREAMING[cli])
    run_dir = home / "clients" / cli / "answer"
    if run_dir.exists():
        shutil.rmtree(run_dir)
    env, cwd = prepare(manifest, run_dir, base_url, key, MODEL)
    name = f"plain-{cli}-answer"
    tmux_spawn(name, cwd, env, [plain_command(manifest)])
    try:
        wait_pane(name, READY_MARKERS.get(cli, cli), 45, "startup")  # the TUI painted its chrome
        onboard(name, cli)
        submit(name, prompt)
        # The pane echoes the prompt AND the answer once; the answer marker
        # must appear on both, so two occurrences prove the model replied.
        wait_pane_count(name, marker, 2, 120, "answer")
    finally:
        tmux("kill-session", "-t", name, check=False)
    rows = wait_proxy_rows(home, 1)
    bad = [r for r in rows if r[1] not in (200, 499)]
    if not rows:
        raise AssertionError("answer: the proxy logged no request row")
    if bad:
        raise AssertionError(f"answer: proxy rows not clean: {bad}")
    real = turn_requests(ms_requests(mockserver), prompt, cli)
    if len(real) != 1:
        raise AssertionError(f"answer: upstream turn requests = {len(real)}, want exactly 1")


def scenario_recall(cli: str, manifest: Path, home: Path, base_url: str, key: str, mockserver: str, port: int) -> None:
    t1 = "Please remember the number 4711."
    t2 = "Which number did I ask you to remember?"
    ms_reset(mockserver)
    mock_pinned(mockserver, cli, [(t1, "Noted."), (t2, "The number is 4711")], STREAMING[cli])
    mock_fallback(mockserver, "ok", FALLBACK_STREAMING[cli])

    run_dir = home / "clients" / cli / "recall"
    if run_dir.exists():
        shutil.rmtree(run_dir)
    env, cwd = prepare(manifest, run_dir, base_url, key, MODEL)
    name = f"plain-{cli}-recall"
    tmux_spawn(name, cwd, env, [plain_command(manifest)])
    try:
        wait_pane(name, READY_MARKERS.get(cli, cli), 45, "startup")
        onboard(name, cli)
        submit(name, t1)
        wait_pane(name, "Noted.", 120, "recall turn 1")
        submit(name, t2)
        # The turn-1 echo and the answer together prove the history came back;
        # the follow-up request carrying 4711 is asserted below from MockServer
        # regardless.
        wait_pane_count(name, "4711", 2, 120, "recall turn 2")
    finally:
        tmux("kill-session", "-t", name, check=False)

    rows = wait_proxy_rows(home, 2)
    bad = [r for r in rows if r[1] not in (200, 499) or (r[1] != 499 and r[2])]
    if len(rows) < 2:
        raise AssertionError(f"recall: the proxy logged {len(rows)} row(s), want 2")
    if bad:
        raise AssertionError(f"recall: proxy rows not clean: {bad}")
    reqs = ms_requests(mockserver)
    real1 = turn_requests(reqs, t1, cli)
    real2 = turn_requests(reqs, t2, cli)
    if len(real1) != 1 or len(real2) != 1:
        raise AssertionError(
            f"recall: upstream turn requests: turn1={len(real1)}, turn2={len(real2)}, want 1 each")
    if "4711" not in json.dumps(real2[0].get("body")):
        raise AssertionError("recall: the follow-up upstream request carries no history (4711 missing)")


SCENARIOS = {"answer": scenario_answer, "recall": scenario_recall}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", required=True)
    parser.add_argument("--mockserver", required=True)
    parser.add_argument("--clients", default="codex,claude-code,crush,openhands")
    parser.add_argument("--scenarios", default="answer,recall")
    parser.add_argument("--binary", default=str(REPO / "proxyllm"))
    parser.add_argument("--port", type=int, default=18190)
    args = parser.parse_args()

    if not shutil.which("tmux"):
        raise SystemExit("tmux is required (the plain-start driver runs CLIs in a pseudo-terminal)")

    home = Path(args.home).resolve()
    if home.exists():
        shutil.rmtree(home)
    home.mkdir(parents=True)

    binary = Path(args.binary)
    if not binary.exists():
        raise SystemExit(f"proxy binary {binary} missing; run make build first")

    kill_codex_daemons()
    proxy = start_proxy(home, binary, args.port)
    try:
        key = configure(home, args.port, args.mockserver)
        failures = []
        for cli in [c.strip() for c in args.clients.split(",") if c.strip()]:
            manifest = REPO / ".github/live-cli/adapters" / f"{cli}.json"
            if not manifest.exists():
                raise SystemExit(f"no adapter manifest for {cli}")
            for scenario in [s.strip() for s in args.scenarios.split(",") if s.strip()]:
                label = f"{cli}/{scenario}"
                print(f"{label:32s} running", flush=True)
                try:
                    SCENARIOS[scenario](cli, manifest, home, f"http://127.0.0.1:{args.port}", key, args.mockserver, args.port)
                except AssertionError as exc:
                    print(f"{label:32s} FAIL  {exc}", flush=True)
                    failures.append(label)
                else:
                    print(f"{label:32s} pass", flush=True)
        if failures:
            print(f"plain-start matrix: FAIL ({', '.join(failures)})")
            return 1
        print("plain-start matrix: OK")
        return 0
    finally:
        proxy.kill()


if __name__ == "__main__":
    sys.exit(main())
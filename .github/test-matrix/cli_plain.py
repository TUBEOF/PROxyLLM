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
    # row routes every claude-* name onto the simulated upstream. opencode's
    # seed config names its provider model "proxy-chat" (the id the live
    # suite aliases the same way), so it rides the same row.
    admin.put(client, "/api/admin/settings", {
        "key": "model_aliases",
        "value": json.dumps({"claude-*": MODEL, "proxy-chat": MODEL, MODEL: MODEL}, separators=(",", ":"), sort_keys=True),
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
# 0.99.0 / openhands 0.59.0 / gptme 0.36.0 / open-interpreter 0.0.58): codex
# buffers /v1/responses turns
# upstream (no "stream" key; plain string message content) and drives utility
# calls — task-title generation — as STREAMED requests quoting the
# conversation; Claude streams every Messages turn and sends the user prompt
# as its own text part; crush streams chat completions with the user prompt
# as a plain string, and its title calls stream too; openhands (with
# native_tool_calling in its [llm] config) posts closed chat completions with
# the prompt as a plain string and never streams — its agent loop has no
# utility calls, but the CodeAct runtime appends a <RUNTIME_INFORMATION> user
# message after the prompt on the first turn only; gptme wraps every message
# content (system included) in typed text parts, streams its model turns and
# fires one closed auto-naming call after the first reply; open-interpreter
# (the Codex fork, wire_api = "chat") streams every upstream call it makes —
# model turns carry its guidance stacks (<skills_instructions>,
# <environment_context> user messages before the prompt) plus one streamed
# title call after the first turn. aider 0.86.2 streams every call it makes
# (with stream_options.include_usage) and makes no utility calls; it appends
# its whole-edit instructions to each user message at request time, so its
# pinned matcher content is the prompt plus that fixed scaffold (see
# AIDER_MESSAGE_SUFFIX). MockServer's LLM response follows the expectation's
# streaming flag only, and its JSON matcher wants exact leaf values, so each
# turn is pinned by its exact last user message in the CLI's own shape, later
# turns ranked higher (their request carries earlier prompts in history). The
# fallback buckets catch the utility traffic so no upstream call 404s (the
# proxy would log the failure and fail the clean-rows check). Continue (cn)
# 1.5.47 streams every model turn (plain-string user content after its system
# agent prompt) and fires one CLOSED title-naming call after each turn; its
# plain start has no onboarding keys — the seeded ~/.continue/
# .onboarding_complete marker (a normal post-onboarding user's state) skips
# the first-run Anthropic-key gate, and the config's custom openai model is
# used with no Continue account. goose 1.57.0 streams every call it makes
# (model turns and one title call per turn, always with stream_options); its
# last user message carries the typed prompt plus a <turn-context> block
# whose per-turn timestamp cannot be pinned, so its turns are pinned
# STRUCTURALLY — a real turn is the request that carries tool definitions
# (the title calls carry none), and from turn 2 on the previous turn's
# assistant reply in the history (stored verbatim) discriminates the turns.
# Its wizard-shaped config seed (GOOSE_TELEMETRY_ENABLED: false plus
# active_provider/providers.openai, the state a user's completed configure
# run has) makes the plain start skip every first-run dialog. kimi 2.4.1
# streams every model turn (plain string user content, Kimi Code's own agent
# system prompt and 24 function tools around it, the <system-reminder> date
# turn after the first prompt — see RUNTIME_HINTS) and makes no utility
# calls; no Kimi account: the seed config's custom OpenAI provider
# (`type = "openai"`) is the documented no-login path, and the fresh home's
# workspace trust dialog preselects "Trust and continue", accepted with
# Enter (ONBOARDING). qwen 0.26.6 streams every call it makes — the model
# turns plus one [SUGGESTION MODE] call (next-input suggestion) and one
# managed-memory extraction call ([Managed memory has TWO directories])
# right after each turn; those utility calls quote the conversation but
# their LAST message never carries the prompt, so turn_requests sees
# exactly the real turns, and the fallback bucket absorbs them. The first
# turn's user message is a stack of <system-reminder> text parts around the
# typed prompt (pinned as a part-array sender — MockServer's LOOSE matching
# accepts the one-part subset); later turns carry the typed prompt as a
# plain string message (PART_ARRAY_FIRST_TURN). No account and no dialogs:
# the env-only OpenAI-compatible config (OPENAI_API_KEY + OPENAI_BASE_URL +
# OPENAI_MODEL, the documented no-login path) selects auth type "openai" and
# skips the auth picker, folder trust is disabled by default, and
# OPENAI_MODEL fills the model slot (/model changes it). opencode 1.19.5
# streams every call it makes — the model turns (11 function tools around
# its system prompt, the typed prompt as a plain-string last message) plus
# one title call right after the first prompt; the title call quotes the
# conversation with the prompt as its OWN last message, so content pinning
# cannot tell the two apart, but it sends no tool definitions — turns are
# pinned STRUCTURALLY like goose's, and turn_requests requires tools for it
# (TOOLS_PINNED). No opencode account and no first-run dialogs: the config's
# custom provider (@ai-sdk/openai-compatible with options.apiKey) is the
# documented no-login path, and the fresh TUI starts straight into the
# composer with the seeded model; assistant replies are stored verbatim, so
# the structural rank matcher's history pin holds.
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


# Senders wrapping message content in an array of typed parts instead of a
# plain string; the pinned matcher holds the prompt in the text part.
# gptme sends arrays of typed parts to the OpenAI endpoint, but the proxy
# forwards its turns as plain strings — the matcher must match the forwarded
# shape, so only claude-code and qwen are pinned as part-array senders here
# (for qwen the prompt is one of several <system-reminder> text parts in the
# array; the LOOSE one-part subset still pins the turn).
PART_ARRAY = {"claude-code", "qwen"}


# CLIs that wrap the typed prompt in a larger user message on the wire:
# aider 0.86.2 appends its whole-edit instructions verbatim to every user
# message at request time (captured from the pinned version's wire), so the
# pinned MockServer matcher must carry prompt + scaffold while the real-turn
# check accepts the prompt as its prefix.
AIDER_MESSAGE_SUFFIX = (
    "\n\n"
    "To suggest changes to a file you MUST return the entire content of the updated file.\n"
    "You MUST use this *file listing* format:\n"
    "\n"
    "path/to/filename.js\n"
    "```\n"
    "// entire file content ...\n"
    "// ... goes in between\n"
    "```\n"
    "\n"
    "Every *file listing* MUST use this format:\n"
    "- First line: the filename with any originally provided path; no extra markup, punctuation, comments, etc. **JUST** the filename with path.\n"
    "- Second line: opening ```\n"
    "- ... entire content of the file ...\n"
    "- Final line: closing ```\n"
    "\n"
    "To suggest changes to a file you MUST return a *file listing* that contains the entire content of the file.\n"
    '*NEVER* skip, omit or elide content from a *file listing* using "..." or by adding comments like "... rest of code..."!\n'
    "Create a new file you MUST return a *file listing* which includes an appropriate filename, including any appropriate path.\n"
    "\n"
    "Reply in English.\n"
    "\n"
)
WRAPS_PROMPT = {"aider": True}

# CLIs whose sent user message carries the typed prompt as a PREFIX (goose
# appends its <turn-context> block after it; see STRUCTURE_PINNED).
PREFIX_TURN = {"goose"}


# CLIs whose typed prompt reaches the wire as a part ARRAY on the first turn
# but as a plain string afterwards (qwen 0.26.6 stacks its startup
# <system-reminder> context around the first prompt only): the part-array
# matcher pins the first turn, later turns pin the plain string.
PART_ARRAY_FIRST_TURN = {"qwen"}


def last_user_content(cli: str, prompt: str) -> str:
    """The user message content the CLI puts on the wire for one typed prompt."""
    if WRAPS_PROMPT.get(cli):
        return prompt + AIDER_MESSAGE_SUFFIX
    return prompt


# CLIs whose turns cannot be pinned by exact user-message content (goose
# appends <turn-context> with a per-turn timestamp; opencode's title call
# quotes the prompt as its own last message but sends no tools): their
# matcher is the structural discriminator instead — a real turn is the
# request that carries tool definitions, and turn >= 2 carries the previous
# turn's (verbatim) assistant reply in the history. See the wire-facts note
# above.
STRUCTURE_PINNED = {"goose", "opencode"}


# CLIs whose ONLY upstream requests carrying tool definitions are their real
# model turns (see STRUCTURE_PINNED): turn_requests skips the tool-less
# utility calls whose last message is the prompt itself.
TOOLS_PINNED = {"opencode"}


def mock_pinned(mockserver: str, cli: str, turns: list[tuple[str, str]], streaming: bool) -> None:
    for rank, (prompt, text) in enumerate(turns):
        matcher: dict = {"messages": [{"role": "user", "content": last_user_content(cli, prompt)}]}
        if cli in STRUCTURE_PINNED:
            matcher = {"tools": [{"type": "function"}]}
            if streaming:
                matcher["stream"] = True
            if rank:
                matcher["messages"] = [{"role": "assistant", "content": turns[rank - 1][1]}]
        if streaming:
            matcher.setdefault("stream", True)
            priority = 150 + 10 * rank
        else:
            priority = 140 + 10 * rank
        first = cli in PART_ARRAY_FIRST_TURN
        if cli in PART_ARRAY and (rank == 0 or not first):
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
        if cli in TOOLS_PINNED and not (payload.get("tools") or []):
            continue  # a utility call without tools (opencode's title call)
        msgs = payload.get("messages") or []
        hint = RUNTIME_HINTS.get(cli)
        if hint:
            msgs = [m for m in msgs if hint not in str(m.get("content") or "")]
        if not msgs or msgs[-1].get("role") != "user":
            continue
        last = msgs[-1]
        content = last.get("content")
        if isinstance(content, str):
            if content == prompt or (WRAPS_PROMPT.get(cli) and content.startswith(prompt)) \
                    or (cli in PREFIX_TURN and content.startswith(prompt)):
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
# gptme flushes stdin before entering its read loop, so its ready marker must
# be the composer prompt itself — text typed earlier is discarded. aider
# (prompt_toolkit, no TUI chrome) repaints its composer per token too, so its
# ready state is its banner and the composer waits are the bare prompt.
READY_MARKERS = {"codex": "codex", "claude-code": "Claude", "crush": "crush",
                 "openhands": "OpenHands CLI v",
                 "gptme": "User:",
                 "aider": "Aider v",
                 # cline's fresh composer placeholder; the pane names the model
                 # only in its status strip, and the ClinePass modal paints the
                 # first chrome.
                 "cline": "What can I do for you?",
                 # continue: the fresh composer's placeholder (the banner and
                 # the status strip name the CLI; the placeholder is stable
                 # across turns).
                 "continue": "Ask anything, @ for context",
                 # goose: the first-run banner's start state, printed once
                 # before the composer paints; "goose is ready" stays.
                 "goose": "goose is ready",
                 # kimi: the welcome box names the CLI right after the trust
                 # dialog is accepted; the composer's own ">" never closes a
                 # turn, so the banner is the stable fresh-start chrome.
                 "kimi": "Welcome to Kimi Code!",
                 # qwen paints its welcome box before the composer can take
                 # keys — typed text is dropped at that point — so the ready
                 # marker is the fresh composer's placeholder (stable across
                 # turns), like continue's.
                 "qwen": "Type your message or @path/to/file",
                 # opencode paints its banner then the composer directly (no
                 # auth, no theme picker with a config-seeded provider), so
                 # the fresh composer's placeholder is the ready state.
                 "opencode": "Ask anything",
                 "open-interpreter": "Open Interpreter (v"}

# How each CLI's model turns stream upstream, and what the catch-all bucket
# answer must look like to absorb the utility traffic (title generation):
# codex buffers turns but streams its title calls, claude-code streams
# nothing but turns, crush streams everything, openhands never streams its
# turns and makes no utility calls, gptme streams turns but not its one
# auto-naming call (a closed chat completion after the first reply).
# open-interpreter is a Codex fork and streams everything it sends upstream
# (model turns and title calls alike, over wire_api = "chat"), as does aider
# (which makes no utility calls at all). cline 3.0.74 streams every model
# turn it makes (plain-string user content) and makes no utility calls; its
# only first-run screen is the ClinePass intro modal, closed by any key other
# than Enter (ONBOARDING); with the modal gone the fresh composer's
# placeholder is the ready state, and it accepts typed prompts without a
# prompt-tail gate.
# continue 1.5.47 (see the wire-facts note above) streams its turns and its
# only utility call (an auto-title) is CLOSED, like gptme's — the marker seed
# means the driver never answers an onboarding dialog.
STREAMING = {"codex": False, "claude-code": True, "crush": True, "openhands": False,
             "gptme": True, "aider": True, "cline": True,
             "continue": True, "goose": True,
             # kimi streams its model turns and makes no utility calls (no
             # title generation upstream); the fallback bucket streams too.
             "kimi": True,
             # qwen streams everything it sends upstream, including the
             # managed-memory extraction call that follows each turn.
             "qwen": True,
             # opencode streams its model turns and its one title call.
             "opencode": True,
             "open-interpreter": True}
FALLBACK_STREAMING = {"codex": True, "claude-code": False, "crush": True,
                      "openhands": False, "gptme": False, "aider": True,
                      "cline": True, "continue": False, "goose": True,
                      "kimi": True,
                      "open-interpreter": True,
                      "qwen": True,
                      "opencode": True}

# User-message markers that are runtime metadata, not model turns; see
# turn_requests. openhands's CodeAct runtime inserts a <RUNTIME_INFORMATION>
# user turn after the prompt; kimi 2.4.1 appends its <system-reminder>
# date-restatement user turn after the prompt on the session's first turn.
RUNTIME_HINTS = {"openhands": "<RUNTIME_INFORMATION>",
                 "kimi": "<system-reminder>"}

# First-run dialogs some TUIs paint before the composer; each entry waits for
# its pane text, then presses keys. claude-code 2.1.316's trust dialog
# defaults to "No, exit" — the Down moves it to "Yes, I trust this folder";
# a blind Enter there exits the CLI with an empty pane. gptme's picker
# ("New conversation") is preselected, so Enter starts the chat.
# cline's ClinePass intro modal closes on any key EXCEPT Enter — Enter opens
# the subscription URL in a browser, so Escape closes it.
ONBOARDING = {
    "claude-code": [
        ("Choose the text style", ["Enter"]),
        ("Press Enter to continue", ["Enter"]),
        ("Quick safety check", ["Down", "Enter"]),
    ],
    "gptme": [
        ("New conversation or load previous?", ["Enter"]),
    ],
    # open-interpreter's trust dialog is Codex's folder gate; "Trust and
    # continue" is preselected, so Enter accepts it.
    "open-interpreter": [
        ("Trust this folder", ["Enter"]),
    ],
    "cline": [
        ("Try ClinePass", ["Escape"]),
    ],
    # kimi's workspace trust gate on every fresh home: "Trust and continue" is
    # the preselected row, so Enter accepts it (Esc would exit the CLI).
    "kimi": [
        ("Trust this folder?", ["Enter"]),
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


def tmux_spawn(name: str, cwd: Path, env: dict, command: list[str], run_dir: Path) -> None:
    argv = ["env", *[f"{k}={v}" for k, v in env.items()], *command]
    quoted = " ".join("'" + a.replace("'", "'\\''") + "'" for a in argv)
    # The evidence must be attached before the CLI starts, or a client that
    # dies instantly wins the race (and a dead pane takes its tmux session
    # down, leaving the job nothing to upload): the window is created on a
    # placeholder process, remain-on-exit keeps the dead screen capturable,
    # pipe-pane tees every byte the pty got (stdout+stderr as the user sees
    # it) into the run dir, and only then is the placeholder swapped for the
    # real command — so a boot-time crash or stall leaves its console output
    # behind. A crashed earlier driver run may have left this cell's session
    # behind; kill it so the name is free again (no-op on a fresh CI box).
    tmux("kill-session", "-t", name, check=False)
    tmux("new-session", "-d", "-s", name, "-x", str(PANE_COLS), "-y", str(PANE_ROWS), "-c", str(cwd), "sleep 30")
    tmux("set-option", "-t", name, "remain-on-exit", "on")
    pane_log = run_dir / "pane.log"
    tmux("pipe-pane", "-t", name, f"cat >> '{pane_log}'")
    tmux("respawn-pane", "-k", "-t", name, quoted)


def collect_logs(run_dir: Path) -> None:
    """Copy the log files each CLI wrote under its own home/config tree into
    the cell's run dir, so the artifact carries a client's own boot log
    (e.g. continue's core log) next to the pane stream."""
    logs = run_dir / "client-logs"
    logs.mkdir(parents=True, exist_ok=True)
    for base in ("home", "config", "data", "cache"):
        root = run_dir / base
        if not root.is_dir():
            continue
        for log in root.rglob("*.log"):
            try:
                rel = str(log.relative_to(root))
                target = logs / f"{base}--{rel}".replace(os.sep, "-")
                shutil.copy2(log, target)
            except OSError:
                pass  # a mid-write log file is not worth failing the cell over


def tmux_send(name: str, text: str) -> None:
    tmux("send-keys", "-t", name, "-l", text)
    tmux("send-keys", "-t", name, "Enter")


def tmux_pane(name: str) -> str:
    return tmux("capture-pane", "-p", "-t", name, check=False)


# Some CLIs flush stdin when they repaint the composer after every turn, so
# typed-ahead input is discarded: wait until the composer prompt itself is
# the pane's last line before typing.
PROMPT_TAIL = {"gptme": "User:", "aider": ">"}


def wait_pane_tail(name: str, tail: str, timeout: float, what: str) -> None:
    """Wait until the pane's last non-empty line is the fresh composer prompt."""
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        last = tmux_pane(name)
        lines = [ln.rstrip() for ln in last.splitlines() if ln.strip()]
        if lines and lines[-1] == tail:
            return
        time.sleep(0.5)
    raise AssertionError(f"{what}: composer prompt {tail!r} is not the pane's last line; last pane:\n{last}")


def submit(name: str, cli: str, text: str) -> None:
    """Send one prompt and submit it. The first Enter can race the TUI's own
    async input handling, so a second Enter follows while the composer still
    holds the text; an Enter on an empty composer is a no-op."""
    if tail := PROMPT_TAIL.get(cli):
        wait_pane_tail(name, tail, 30, "composer")
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
        elif keep in ("LANG", "LC_ALL"):
            # A CI runner has neither set, and some CLIs branch on the
            # locale: aider builds its "Reply in <language>." line from it
            # at runtime, so with none the line vanishes from the wire and
            # the pinned matcher falls through. The default must resolve to
            # a real language name — C.UTF-8 does not (aider's
            # normalize_language maps C to None, verified against the
            # installed aider 0.86), so use the standard Ubuntu user session
            # locale, only when nothing is set (never overriding an
            # operator's real locale).
            env[keep] = "en_US.UTF-8"
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
    tmux_spawn(name, cwd, env, [plain_command(manifest)], run_dir)
    try:
        onboard(name, cli)
        wait_pane(name, READY_MARKERS.get(cli, cli), 45, "startup")  # the TUI painted its chrome
        submit(name, cli, prompt)
        # The pane echoes the prompt AND the answer once; the answer marker
        # must appear on both, so two occurrences prove the model replied.
        wait_pane_count(name, marker, 2, 120, "answer")
    finally:
        collect_logs(run_dir)
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
    tmux_spawn(name, cwd, env, [plain_command(manifest)], run_dir)
    try:
        onboard(name, cli)
        wait_pane(name, READY_MARKERS.get(cli, cli), 45, "startup")
        submit(name, cli, t1)
        wait_pane(name, "Noted.", 120, "recall turn 1")
        submit(name, cli, t2)
        # The turn-1 echo and the answer together prove the history came back;
        # the follow-up request carrying 4711 is asserted below from MockServer
        # regardless.
        wait_pane_count(name, "4711", 2, 120, "recall turn 2")
    finally:
        collect_logs(run_dir)
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
    parser.add_argument("--clients",
                        default="codex,claude-code,crush,openhands,gptme,open-interpreter,aider,cline,continue,goose,kimi,qwen,opencode")
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
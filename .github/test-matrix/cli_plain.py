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
import re
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

# The admin API client lives next to this driver; importlib keeps it loadable
# under `python3 -I`, which does not put the script directory on sys.path.
_spec = importlib.util.spec_from_file_location("admin", HERE / "admin.py")
admin = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(admin)

MODEL = "sim-model"
WEB_MODEL = "web-sim"
PANE_COLS, PANE_ROWS = 140, 44
UPSTREAM_PATH = "/v1/chat/completions"
CLI_MANIFEST = HERE / "cli_matrix.json"
ADMIN_CLIENT: admin.AdminClient | None = None
ALIAS_MODEL = ""


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
    global ADMIN_CLIENT
    client = admin.AdminClient(f"http://127.0.0.1:{port}", str(home / "run" / "admin-cookies.txt"))
    user = "matrix-admin"
    password = secrets.token_hex(16)
    env_path = home / ".env"
    env_path.write_text(f"ADMIN_USER={user}\nADMIN_PASSWORD={password}\n")
    client.login(str(env_path), user, password)
    ADMIN_CLIENT = client
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
    # The web scenario's model injects the emulated web tools into the model
    # request; MockServer also plays the Firecrawl v2 backend.
    admin.put(client, "/api/admin/models/" + WEB_MODEL, {
        "provider_id": "acc-sim", "name": WEB_MODEL,
        "capabilities": {}, "compat": {"inject_web_tools": True},
    })
    admin.put(client, "/api/admin/providers/acc-firecrawl", {
        "type": "firecrawl",
        "name": "Simulated web tools",
        "config": {"base_url": mockserver},
        "api_key": "simulated-key",
    })
    admin.put(client, "/api/admin/providers/acc-firecrawl/enabled", {"enabled": True})
    # Claude Code's env pins model names (claude-sonnet-4-6, ...); the alias
    # row routes every claude-* name onto the simulated upstream. opencode's
    # seed config names its provider model "proxy-chat" (the id the alias
    # row maps the same way), so it rides the same row.
    admin.put(client, "/api/admin/settings", {
        "key": "model_aliases",
        "value": json.dumps({"claude-*": MODEL, "proxy-chat": MODEL, MODEL: MODEL}, separators=(",", ":"), sort_keys=True),
    })
    payload = admin.post(client, "/api/admin/keys", {"name": "cli-plain"})
    key = payload.get("key", "")
    if not key:
        raise SystemExit("client key creation returned no key")
    return key


def fresh_upstream(mockserver: str, scenario: str, cli: str) -> str:
    """One provider account per fault cell: the breaker and quota cooldowns are
    keyed by account, so sharing one would poison the next cell."""
    if ADMIN_CLIENT is None:
        raise SystemExit("admin client not configured")
    tag = f"{scenario}-{cli}".replace("_", "-")
    account = f"acc-{tag}"
    model = f"{tag}-sim"
    admin.put(ADMIN_CLIENT, f"/api/admin/providers/{account}", {
        "type": "custom-openai",
        "name": f"Simulated {tag}",
        "config": {"base_url": mockserver + "/v1"},
        "api_key": "simulated-key",
    })
    admin.put(ADMIN_CLIENT, f"/api/admin/providers/{account}/enabled", {"enabled": True})
    admin.put(ADMIN_CLIENT, "/api/admin/models/" + model, {
        "provider_id": account, "name": model, "capabilities": {}, "compat": {},
    })
    return model


def set_aliases(model: str) -> None:
    """Point the claude-* names at one model. Claude Code validates its model
    names locally, so the cell maps a recognized name instead of overriding
    the env with a synthetic one. The GET refreshes the CSRF token the
    client must echo on the write."""
    if ADMIN_CLIENT is None:
        raise SystemExit("admin client not configured")
    admin.get_json(ADMIN_CLIENT, "/api/admin/settings")
    aliases = {"claude-*": model, "proxy-chat": model, MODEL: model, WEB_MODEL: WEB_MODEL}
    admin.put(ADMIN_CLIENT, "/api/admin/settings", {
        "key": "model_aliases",
        "value": json.dumps(aliases, separators=(",", ":"), sort_keys=True),
    })


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
# proxy would log the failure and fail the clean-rows check). goose 1.57.0
# streams every call it makes
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
def _put_expectation(mockserver: str, body: dict, label: str) -> None:
    try:
        call = urllib.request.Request(
            mockserver + "/mockserver/expectation",
            data=json.dumps(body).encode(), method="PUT",
            headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(call, timeout=5) as r:
            if r.status != 201:
                raise SystemExit(f"expectation {label!r} -> HTTP {r.status}")
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "replace")[:300]
        raise SystemExit(f"expectation {label!r} -> HTTP {exc.code}: {detail}") from exc


def _mock_expectation(mockserver: str, completion: dict, priority: int,
                      times: int | None = None, matcher: dict | None = None,
                      path: str = UPSTREAM_PATH, provider: str = "OPENAI",
                      model: str = MODEL, chaos: dict | None = None) -> None:
    req: dict = {"method": "POST", "path": path}
    if matcher:
        req["body"] = {"type": "JSON", "json": matcher, "matchType": "LOOSE"}
    llm: dict = {"provider": provider, "model": model, "completion": completion}
    if chaos:
        llm["chaos"] = chaos
    body: dict = {"httpRequest": req, "httpLlmResponse": llm, "priority": priority}
    if times is not None:
        body["times"] = {"remainingTimes": times, "unlimited": False}
    _put_expectation(mockserver, body, str(completion.get("text", "")))


def mock_http_response(mockserver: str, path: str, status: int, body: str,
                       priority: int, times: int | None = None,
                       matcher: dict | None = None,
                       headers: list[dict] | None = None) -> None:
    req: dict = {"method": "POST", "path": path}
    if matcher:
        req["body"] = {"type": "JSON", "json": matcher, "matchType": "LOOSE"}
    response: dict = {"statusCode": status, "body": body}
    if headers:
        response["headers"] = headers
    doc: dict = {"httpRequest": req, "httpResponse": response, "priority": priority}
    if times is not None:
        doc["times"] = {"remainingTimes": times, "unlimited": False}
    _put_expectation(mockserver, doc, f"HTTP {status} {path}")


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


def pinned_matcher(cli: str, prompt: str, rank: int,
                   turns: list[tuple[str, str]], streaming: bool) -> dict:
    """The MockServer matcher a real turn's last user message must satisfy."""
    matcher: dict = {"messages": [{"role": "user", "content": last_user_content(cli, prompt)}]}
    if cli in STRUCTURE_PINNED:
        matcher = {"tools": [{"type": "function"}]}
        if streaming:
            matcher["stream"] = True
        if rank:
            matcher["messages"] = [{"role": "assistant", "content": turns[rank - 1][1]}]
    if streaming:
        matcher.setdefault("stream", True)
    first = cli in PART_ARRAY_FIRST_TURN
    if cli in PART_ARRAY and (rank == 0 or not first):
        matcher["messages"][0]["content"] = [{"text": prompt}]
    return matcher


def mock_pinned(mockserver: str, cli: str, turns: list[tuple[str, str]], streaming: bool,
                model: str = MODEL) -> None:
    for rank, (prompt, text) in enumerate(turns):
        matcher = pinned_matcher(cli, prompt, rank, turns, streaming)
        priority = (150 if streaming else 140) + 10 * rank
        _mock_expectation(mockserver, {"text": text, "streaming": streaming}, priority,
                          matcher=matcher, model=model)


def mock_pinned_dual(mockserver: str, cli: str, turns: list[tuple[str, str]],
                     model: str) -> None:
    """Install both wire modes for a synthetic model: the CLI's stream choice
    is not guaranteed, and a stream matcher must not pin the wrong shape."""
    for rank, (prompt, text) in enumerate(turns):
        _mock_expectation(mockserver, {"text": text, "streaming": True},
                          170 + 10 * rank, model=model,
                          matcher=pinned_matcher(cli, prompt, rank, turns, True))
        _mock_expectation(mockserver, {"text": text, "streaming": False},
                          165 + 10 * rank, model=model,
                          matcher=pinned_matcher(cli, prompt, rank, turns, False))


def mock_fallback(mockserver: str, text: str, streaming: bool, model: str = MODEL) -> None:
    _mock_expectation(mockserver, {"text": text, "streaming": streaming}, 10,
                      matcher={"stream": True} if streaming else None, model=model)


def turn_requests(reqs: list[dict], prompt: str, cli: str = "",
                  path: str = UPSTREAM_PATH, tool_result: bool = False) -> list[dict]:
    """Real model turns, not utility calls: upstream requests whose LAST
    message carries the prompt as an exact message or text part (codex's
    title calls quote the conversation, so containment would count them).
    Runtime-metadata user messages some CLIs append after the prompt —
    openhands's CodeAct runtime inserts one <RUNTIME_INFORMATION> turn —
    never carry the prompt, so the cli's hint drops them from the LAST
    message check. With tool_result, the turn after a tool call is the one
    whose last message is the tool result (OpenAI role=tool, Anthropic
    tool_result part) and whose body carries the prompt somewhere."""
    out = []
    for r in reqs:
        if r.get("path") != path:
            continue
        payload = (r.get("body") or {}).get("json") or {}
        if cli in TOOLS_PINNED and not (payload.get("tools") or []):
            continue  # a utility call without tools (opencode's title call)
        msgs = payload.get("messages") or []
        hint = RUNTIME_HINTS.get(cli)
        if hint:
            msgs = [m for m in msgs if hint not in str(m.get("content") or "")]
        if not msgs:
            continue
        last = msgs[-1]
        content = last.get("content")
        if tool_result:
            is_tool = last.get("role") in ("tool", "function") or (
                isinstance(content, list)
                and any(isinstance(p, dict) and p.get("type") == "tool_result" for p in content)
            )
            if is_tool and prompt in json.dumps(payload):
                out.append(r)
            continue
        if last.get("role") != "user":
            continue
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
                 # turns).
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
STREAMING = {"codex": False, "claude-code": True, "crush": True, "openhands": False,
             "gptme": True, "aider": True, "cline": True,
             "goose": True,
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
                      "cline": True, "goose": True,
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
    tmux("set-option", "-t", name, "history-limit", "10000")
    pane_log = run_dir / "pane.log"
    tmux("pipe-pane", "-t", name, f"cat >> '{pane_log}'")
    tmux("respawn-pane", "-k", "-t", name, quoted)


def collect_logs(run_dir: Path) -> None:
    """Copy the log files each CLI wrote under its own home/config tree into
    the cell's run dir, so the artifact carries a client's own boot log (e.g.
    a TUI's core log) next to the pane stream."""
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


def tmux_pane_scroll(name: str) -> str:
    """Scrollback capture with wrapped lines joined, for large-output cells."""
    return tmux("capture-pane", "-p", "-J", "-S", "-4000", "-t", name, check=False)


ANSI_RE = re.compile(
    r"\x1b\[[0-9;?<=>$]*[ -/]*[@-~]"        # CSI (cursor, colour, modes)
    r"|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)"   # OSC (titles, links)
    r"|\x1b[()][A-Za-z0-9]"                 # charset selection
    r"|\x1b[=>]"                            # keypad modes
    r"|\r")


def pane_stream(run_dir: Path) -> str:
    """Every byte the TUI painted, escapes stripped. Virtualized renderers
    (claude-code, codex, opencode) paint only the window they show, so the
    visible pane proves the tail and this stream proves the unicode line
    around it — not the full 20 KB prefix."""
    return ANSI_RE.sub("", (run_dir / "pane.log").read_text(encoding="utf-8", errors="replace"))


# A mid-stream error must reach the user: every TUI paints some error text
# (each in its own words), and a client that silently re-sent the turn has
# equally seen the failure. Shared by the two mid-stream fault cells.
STREAM_ERROR = re.compile(r"(?i)(error|fail|exception|issue|not found|exceed|unavailable)")

# Some TUIs report a mid-stream failure behind a counter instead of painting
# the error (open-interpreter: "⚠ N warnings · f2 to view"); a count that grew
# after the fault is their terminal-outcome evidence.
WARNING_COUNT = re.compile(r"(\d+)\s+warnings?")


def warning_count(pane: str) -> int:
    return max((int(m.group(1)) for m in WARNING_COUNT.finditer(pane)), default=0)


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


def wait_pane_optional(name: str, marker: str, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if marker in tmux_pane(name):
            return True
        time.sleep(0.5)
    return False


def onboard(name: str, cli: str) -> None:
    """Walk the CLI's first-run dialogs; nothing to do when it starts clean."""
    for marker, keys in ONBOARDING.get(cli, ()):
        wait_pane(name, marker, 45, f"onboarding: {marker!r}")
        for key in keys:
            tmux("send-keys", "-t", name, key)
            time.sleep(0.4)
    time.sleep(1.5)


def start_cell(cli: str, manifest: Path, home: Path, base_url: str, key: str,
               label: str, model: str = MODEL) -> tuple[str, Path]:
    """Spawn one plain-start cell and wait for its chrome and composer."""
    global ALIAS_MODEL
    if cli in ("claude-code", "opencode"):
        # Both send a fixed wire model name the proxy resolves through the
        # alias map; every other adapter renders the cell's model directly.
        set_aliases(model)
        ALIAS_MODEL = model
    run_dir = home / "clients" / cli / label
    if run_dir.exists():
        shutil.rmtree(run_dir)
    env, cwd = prepare(manifest, run_dir, base_url, key, model)
    name = f"plain-{cli}-{label}"
    tmux_spawn(name, cwd, env, [plain_command(manifest)], run_dir)
    try:
        onboard(name, cli)
        wait_pane(name, READY_MARKERS.get(cli, cli), 45, "startup")
    except BaseException:
        # A failed startup owes the runner the same cleanup as a finished
        # cell: kill the session, collect its logs, drain in-flight traffic.
        finish_cell(name, run_dir)
        raise
    return name, run_dir


def finish_cell(name: str, run_dir: Path) -> None:
    global ALIAS_MODEL
    collect_logs(run_dir)
    tmux("kill-session", "-t", name, check=False)
    if run_dir.parent.name == "codex":
        kill_codex_daemons()
    if ALIAS_MODEL:
        set_aliases(MODEL)
        ALIAS_MODEL = ""
    # Let the killed client's in-flight retries drain before the next cell
    # resets MockServer; otherwise one cell's retry can consume the next
    # cell's one-shot fault expectation and mis-attribute its request row.
    try:
        wait_proxy_quiet(run_dir.parents[2])
    except Exception:
        pass


# --- CLI bootstrap -----------------------------------------------------------


def render(value: str, ctx: dict[str, str]) -> str:
    for key, val in ctx.items():
        value = value.replace("{{" + key + "}}", str(val))
    return value


def prepare(manifest_path: Path, run_dir: Path, base_url: str, key: str,
            model: str) -> tuple[dict, Path]:
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
        env[k] = render(str(v), dirs)
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
        target = Path(render(rel, dirs))
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(render(str(content), dirs))
        target.chmod(0o600)
    return env, Path(dirs["WORK_DIR"])


# --- proxy evidence ----------------------------------------------------------


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


def proxy_rows(home: Path, since: int = 0) -> list[tuple]:
    """Request-log rows after the per-cell baseline; a deliberate fault row
    from an earlier cell can therefore never poison a later clean-row check."""
    db = home / "data" / "proxyllm.db"
    conn = ro_connect(db)
    try:
        return conn.execute(
            "SELECT id, status, error_redacted FROM request_logs WHERE id > ? ORDER BY id",
            (since,)).fetchall()
    finally:
        conn.close()


def wait_proxy_quiet(home: Path, timeout: float = 10, quiet: float = 3.0) -> int:
    """Wait until the request log stops growing; return its current max id."""
    db = home / "data" / "proxyllm.db"
    deadline = time.time() + timeout
    last = max_request_id(db)
    stable_since = time.time()
    while time.time() < deadline:
        time.sleep(0.2)
        now = max_request_id(db)
        if now != last:
            last = now
            stable_since = time.time()
            continue
        if time.time() - stable_since >= quiet:
            break
    return last


def wait_proxy_rows(home: Path, since: int, at_least: int, timeout: float = 15) -> list[tuple]:
    """The request-log write is off the request path, so the row can lag the
    answer the CLI already rendered (the write is asynchronous). Only rows
    after `since` count."""
    deadline = time.time() + timeout
    rows = []
    while time.time() < deadline:
        rows = proxy_rows(home, since)
        if len(rows) >= at_least:
            return rows
        time.sleep(0.3)
    return rows


# --- scenarios ----------------------------------------------------------------


def scenario_answer(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                    mockserver: str, port: int, since: int) -> None:
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
    rows = wait_proxy_rows(home, since, 1)
    bad = [r for r in rows if r[1] not in (200, 499)]
    if not rows:
        raise AssertionError("answer: the proxy logged no request row")
    if bad:
        raise AssertionError(f"answer: proxy rows not clean: {bad}")
    real = turn_requests(ms_requests(mockserver), prompt, cli)
    if len(real) != 1:
        raise AssertionError(f"answer: upstream turn requests = {len(real)}, want exactly 1")


def scenario_recall(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                    mockserver: str, port: int, since: int) -> None:
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

    rows = wait_proxy_rows(home, since, 2)
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


# --- proxy-driven scenarios ---------------------------------------------------


def direct_chat_probe(base_url: str, key: str, model: str) -> int:
    """One raw client request through the proxy; returns the HTTP status."""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "probe"}],
    }).encode()
    req = urllib.request.Request(
        base_url + "/v1/chat/completions", data=body, method="POST",
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + key})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status
    except urllib.error.HTTPError as exc:
        return exc.code


def scenario_fault_429(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                       mockserver: str, port: int, since: int) -> None:
    """A one-shot 429 on the real turn: the CLI must either recover through a
    retry or surface an honest error — never fabricate an answer."""
    marker = "RETRY-" + secrets.token_hex(4)
    prompt = "Tell me the answer."
    streaming = STREAMING[cli]
    scenario_model = fresh_upstream(mockserver, "fault_429", cli)
    ms_reset(mockserver)
    matcher = pinned_matcher(cli, prompt, 0, [(prompt, marker)], streaming)
    _mock_expectation(mockserver, {"text": "never delivered", "streaming": streaming},
                      200, times=1, matcher=matcher, model=scenario_model,
                      chaos={"errorStatus": 429, "errorProbability": 1.0, "retryAfter": "1"})
    mock_pinned(mockserver, cli, [(prompt, marker)], streaming, model=scenario_model)
    wrong = "WRONG-" + secrets.token_hex(4)
    mock_fallback(mockserver, wrong, FALLBACK_STREAMING[cli], model=scenario_model)

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "fault_429",
                               model=scenario_model)
    try:
        submit(name, cli, prompt)
        # The turn must reach the proxy before the drain check means
        # anything; then the CLI's own retry loop drains. The old 180 s pane
        # wait ran in full on every honest-error cell, which alone approaches
        # the shard budget.
        wait_proxy_rows(home, since, 1, timeout=90)
        wait_proxy_quiet(home, timeout=60, quiet=5)
        recovered = wait_pane_optional(name, marker, 30)
        pane = tmux_pane(name)
    finally:
        finish_cell(name, run_dir)

    rows = wait_proxy_rows(home, since, 1, timeout=90)
    if not any(r[1] == 200 for r in rows):
        rows = wait_proxy_rows(home, since, 2, timeout=90)
    has_success = any(r[1] == 200 for r in rows)
    has_rate_limit = any(r[1] == 429 for r in rows)
    if recovered and not has_success:
        raise AssertionError(f"fault_429: the CLI rendered an answer with no successful row: {rows}")
    if not recovered and wrong in pane:
        raise AssertionError("fault_429: the CLI rendered the catch-all answer instead of the 429 error")
    if not recovered and not has_rate_limit:
        raise AssertionError(f"fault_429: no recovery and no honest 429 row: {rows}")
    if not turn_requests(ms_requests(mockserver), prompt, cli):
        raise AssertionError("fault_429: the upstream saw no turn request")


def scenario_fault_529(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                       mockserver: str, port: int, since: int) -> None:
    """A one-shot 529 is a server error: fail over, never retry the same
    provider, never fabricate an answer."""
    marker = "OVERLOAD-" + secrets.token_hex(4)
    prompt = "Tell me the answer."
    streaming = STREAMING[cli]
    scenario_model = fresh_upstream(mockserver, "fault_529", cli)
    ms_reset(mockserver)
    # No matcher: whichever request the CLI makes first gets the 529; a retry
    # gets 404 (no fallback), so a fabricated answer is impossible.
    _mock_expectation(mockserver, {"text": marker, "streaming": streaming},
                      200, times=1, model=scenario_model,
                      chaos={"errorStatus": 529, "errorProbability": 1.0, "retryAfter": "1"})

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "fault_529",
                               model=scenario_model)
    try:
        submit(name, cli, prompt)
        rows = wait_proxy_rows(home, since, 1, timeout=90)
        time.sleep(2)
        pane = tmux_pane(name)
    finally:
        finish_cell(name, run_dir)

    if marker in pane:
        raise AssertionError("fault_529: the CLI rendered a fabricated answer")
    if not any(r[1] == 529 for r in rows):
        raise AssertionError(f"fault_529: no 529 request-log row: {rows}")
    real = turn_requests(ms_requests(mockserver), prompt, cli)
    if not real:
        raise AssertionError("fault_529: the upstream saw no turn request")


def scenario_fault_quota(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                         mockserver: str, port: int, since: int) -> None:
    """An insufficient_quota rejection cools the account: the CLI's first call
    surfaces 402, and a direct probe of the same model is refused by the
    cooldown (503 on OpenAI surfaces, 529 on Anthropic) without reaching the
    upstream again."""
    p1 = "Hello there."
    scenario_model = fresh_upstream(mockserver, "fault_quota", cli)
    ms_reset(mockserver)
    # No matcher: whichever request the CLI makes first gets the 402; the
    # direct probe then proves the account cooldown, independent of whether
    # the TUI survives the error.
    mock_http_response(mockserver, UPSTREAM_PATH, 402,
                       '{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}',
                       200, times=1,
                       headers=[{"name": "Retry-After", "values": ["30"]}])

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "fault_quota",
                               model=scenario_model)
    try:
        submit(name, cli, p1)
        rows = wait_proxy_rows(home, since, 1, timeout=90)
        if not any(r[1] == 402 for r in rows):
            raise AssertionError(f"fault_quota: no 402 request-log row: {rows}")
        # Probe inside the 30 s cooldown; a CLI that keeps retrying can push a
        # later probe past the window and get a plain upstream 404 instead.
        probe = direct_chat_probe(base_url, key, scenario_model)
        if probe not in (503, 529):
            raise AssertionError(f"fault_quota: cooldown probe = {probe}, want 503/529")
    finally:
        finish_cell(name, run_dir)

    rows = wait_proxy_rows(home, since, 2, timeout=30)
    if not any(r[1] in (503, 529) for r in rows):
        raise AssertionError(f"fault_quota: no cooldown request-log row: {rows}")


def scenario_fault_truncated(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                             mockserver: str, port: int, since: int) -> None:
    """A mid-stream truncation must surface as an error, never a complete
    answer; the tail marker must never render."""
    marker = "TRUNC-TAIL-" + secrets.token_hex(4)
    prompt = "Give me a long answer."
    text = ("partial-start " * 20) + marker
    scenario_model = fresh_upstream(mockserver, "fault_truncated", cli)
    ms_reset(mockserver)
    matcher = pinned_matcher(cli, prompt, 0, [(prompt, marker)], True)
    _mock_expectation(mockserver, {"text": text, "streaming": True}, 150, times=1,
                      matcher=matcher, model=scenario_model,
                      chaos={"truncateMode": "MID_STREAM", "truncateAtFraction": 0.5,
                             "malformedSse": False})

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "fault_truncated",
                               model=scenario_model)
    try:
        before = tmux_pane(name)
        submit(name, cli, prompt)
        # The turn must reach the proxy, then the CLI's own retry loop drains:
        # a still-retrying TUI would be sampled mid-spinner.
        wait_proxy_rows(home, since, 1, timeout=90)
        wait_proxy_quiet(home, timeout=60, quiet=5)
        time.sleep(2)
        pane = tmux_pane(name)
    finally:
        finish_cell(name, run_dir)

    rows = proxy_rows(home, since)
    if marker in pane:
        raise AssertionError("fault_truncated: the CLI rendered the truncated tail as a complete answer")
    if not any(r[1] not in (200, 499) or r[2] for r in rows):
        raise AssertionError(f"fault_truncated: no error request-log row: {rows}")
    real = turn_requests(ms_requests(mockserver), prompt, cli)
    if not real:
        raise AssertionError("fault_truncated: the upstream saw no turn request")
    surfaced = STREAM_ERROR.search(pane) or warning_count(pane) > warning_count(before)
    if not surfaced and len(real) < 2:
        raise AssertionError("fault_truncated: the CLI neither surfaced the stream error nor retried the turn")


def scenario_fault_malformed_sse(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                                 mockserver: str, port: int, since: int) -> None:
    """A malformed SSE frame mid-stream must terminate the answer with an
    error; the raw body also proves the proxy reads every frame, not only the
    final sentinel."""
    prompt = "Give me a long answer."
    scenario_model = fresh_upstream(mockserver, "fault_malformed_sse", cli)
    body = (
        'data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,'
        '"model":"' + scenario_model + '","choices":[{"index":0,"delta":{"content":"partial-"},'
        '"finish_reason":null}]}\n\n'
        "data: {not json\n\n"
    )
    ms_reset(mockserver)
    # No answer text is pinned: the body below is the fixture, and the
    # matcher's second tuple element is unused at rank 0.
    matcher = pinned_matcher(cli, prompt, 0, [(prompt, "")], True)
    mock_http_response(mockserver, UPSTREAM_PATH, 200, body, 150, times=1, matcher=matcher,
                       headers=[{"name": "Content-Type", "values": ["text/event-stream"]}])

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "fault_malformed_sse",
                               model=scenario_model)
    try:
        before = tmux_pane(name)
        submit(name, cli, prompt)
        # The turn must reach the proxy, then the CLI's own retry loop drains:
        # a still-retrying TUI would be sampled mid-spinner.
        wait_proxy_rows(home, since, 1, timeout=90)
        wait_proxy_quiet(home, timeout=60, quiet=5)
        time.sleep(2)
        pane = tmux_pane(name)
    finally:
        finish_cell(name, run_dir)

    rows = proxy_rows(home, since)
    if not any(r[1] not in (200, 499) or r[2] for r in rows):
        raise AssertionError(f"fault_malformed_sse: no error request-log row: {rows}")
    real = turn_requests(ms_requests(mockserver), prompt, cli)
    if not real:
        raise AssertionError("fault_malformed_sse: the upstream saw no turn request")
    surfaced = STREAM_ERROR.search(pane) or warning_count(pane) > warning_count(before)
    if not surfaced and len(real) < 2:
        raise AssertionError("fault_malformed_sse: the CLI neither surfaced the stream error nor retried the turn")


def scenario_web_turn2(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                       mockserver: str, port: int, since: int) -> None:
    """A web search on turn 2: the model row injects the web tools, the proxy
    serves the model's web_search through the Firecrawl emulator (MockServer),
    re-dispatches with the result, and the CLI renders the cited answer."""
    marker = "WEB-" + secrets.token_hex(4)
    t1 = "Hello there."
    t2 = "Search the web for the weather in Cologne tomorrow."
    final = f"{marker} see https://example.test/cologne"
    streaming = STREAMING[cli]
    ms_reset(mockserver)
    mock_pinned_dual(mockserver, cli, [(t1, "Hello.")], WEB_MODEL)
    tool_call = {
        "stopReason": "tool_calls",
        "toolCalls": [{
            "id": "call_web_1",
            "name": "web_search",
            "arguments": '{"query":"weather in Cologne"}',
        }],
    }
    turns = [(t1, "Hello."), (t2, "")]
    _mock_expectation(mockserver, dict(tool_call, streaming=True), 210, times=1,
                      matcher=pinned_matcher(cli, t2, 1, turns, True), model=WEB_MODEL)
    _mock_expectation(mockserver, dict(tool_call, streaming=False), 209, times=1,
                      matcher=pinned_matcher(cli, t2, 1, turns, False), model=WEB_MODEL)
    mock_http_response(mockserver, "/v2/search", 200,
                       '{"success":true,"data":{"web":[{"title":"Cologne forecast",'
                       '"url":"https://example.test/cologne","description":"12C sunny tomorrow"}]}}',
                       100)
    # The re-dispatch is the request whose history carries the tool result.
    # The proxy's OpenAI-chat rewrite uses a user message (Anthropic tool
    # results) or a tool message (OpenAI tool results), so pin both roles.
    tool_result = json.dumps({
        "tool_use_id": "call_web_1",
        "content": [{
            "url": "https://example.test/cologne",
            "title": "Cologne forecast",
            "content": "12C sunny tomorrow",
        }],
    }, separators=(",", ":"))
    for priority, role, stream in ((220, "user", True), (219, "user", False),
                                   (218, "tool", True), (217, "tool", False)):
        matcher = {"messages": [{"role": role, "content": tool_result}]}
        if stream:
            matcher["stream"] = True
        _mock_expectation(mockserver, {"text": final, "streaming": stream},
                          priority, matcher=matcher, model=WEB_MODEL)
    _mock_expectation(mockserver, {"text": "ok", "streaming": True}, 12, model=WEB_MODEL,
                      matcher={"stream": True})
    _mock_expectation(mockserver, {"text": "ok", "streaming": False}, 11, model=WEB_MODEL)

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "web_turn2", model=WEB_MODEL)
    try:
        submit(name, cli, t1)
        wait_pane(name, "Hello.", 120, "web turn 1")
        submit(name, cli, t2)
        wait_pane(name, marker, 180, "web turn 2")
    finally:
        finish_cell(name, run_dir)

    reqs = ms_requests(mockserver)
    real1 = turn_requests(reqs, t1, cli)
    real2 = turn_requests(reqs, t2, cli)
    if not real1 or not real2:
        raise AssertionError(
            f"web_turn2: upstream turn requests: turn1={len(real1)}, turn2={len(real2)}, want at least 1 each")
    if not any(r.get("path") == "/v2/search" for r in reqs):
        raise AssertionError("web_turn2: the emulated search backend received no request")
    redispatch = [r for r in reqs if r.get("path") == UPSTREAM_PATH
                  and "example.test/cologne" in json.dumps(r.get("body"))]
    if not redispatch:
        raise AssertionError("web_turn2: no re-dispatched chat request carries the search result")
    rows = wait_proxy_rows(home, since, 1)
    bad = [r for r in rows if r[1] not in (200, 499) or (r[1] != 499 and r[2])]
    if not rows:
        raise AssertionError("web_turn2: the proxy logged no request row")
    if bad:
        raise AssertionError(f"web_turn2: proxy rows not clean: {bad}")


# The CLI's own tool that reads the seeded work/notes.txt. The file's marker
# must ride the tool result back into the re-dispatched conversation.
TOOL_LOOP_SEEDS = {
    "crush": {
        "name": "view",
        "arguments": json.dumps({"file_path": "notes.txt"}),
    },
    "openhands": {
        "name": "execute_bash",
        "arguments": json.dumps({"command": "cat notes.txt"}),
    },
}


def scenario_tool_loop(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                       mockserver: str, port: int, since: int) -> None:
    """The CLI executes its own tool (read notes.txt), the tool result rides
    back into the conversation, and the model answers from it."""
    seed = TOOL_LOOP_SEEDS[cli]
    marker = "TOOL-" + secrets.token_hex(4)
    prompt = "Read notes.txt and tell me the marker."
    streaming = STREAMING[cli]
    ms_reset(mockserver)
    matcher = pinned_matcher(cli, prompt, 0, [(prompt, marker)], streaming)
    _mock_expectation(mockserver, {
        "stopReason": "tool_calls",
        "toolCalls": [{
            "id": "call_tool_1",
            "name": seed["name"],
            "arguments": seed["arguments"],
        }],
        "streaming": streaming,
    }, 150, times=1, matcher=matcher, model=MODEL)
    _mock_expectation(mockserver, {"text": marker, "streaming": streaming}, 130, model=MODEL)
    mock_fallback(mockserver, "ok", FALLBACK_STREAMING[cli], model=MODEL)

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "tool_loop", model=MODEL)
    try:
        submit(name, cli, prompt)
        wait_pane(name, marker, 180, "tool loop answer")
    finally:
        finish_cell(name, run_dir)

    reqs = ms_requests(mockserver)
    with_result = turn_requests(reqs, prompt, cli, tool_result=True)
    if not with_result:
        raise AssertionError("tool_loop: no re-dispatched request carries a tool result")
    if not any("PROXYLLM-LIVE-MARKER-7Q" in json.dumps(r.get("body")) for r in with_result):
        raise AssertionError("tool_loop: the tool result carries no file marker (the CLI did not run the tool)")
    rows = wait_proxy_rows(home, since, 1)
    if not rows:
        raise AssertionError("tool_loop: the proxy logged no request row")
    bad = [r for r in rows if r[1] not in (200, 499) or (r[1] != 499 and r[2])]
    if bad:
        raise AssertionError(f"tool_loop: proxy rows not clean: {bad}")


TURN_WORDS = ["ALPHA", "BRAVO", "CHARLIE", "DELTA", "ECHO",
              "FOXTROT", "GOLF", "HOTEL", "INDIA"]


def scenario_multi_turn(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                        mockserver: str, port: int, since: int) -> None:
    """Nine remembered words plus a final recall: the last upstream request
    must carry the whole history, not just the final prompt."""
    tail = "DONE-" + secrets.token_hex(4)
    prompts = [f"Remember this word: {word}." for word in TURN_WORDS]
    answers = [f"Noted {word}." for word in TURN_WORDS]
    final_prompt = "List every word I asked you to remember."
    final_answer = " ".join(TURN_WORDS) + " " + tail
    ms_reset(mockserver)
    turns = list(zip(prompts, answers)) + [(final_prompt, final_answer)]
    mock_pinned(mockserver, cli, turns, STREAMING[cli])
    mock_fallback(mockserver, "ok", FALLBACK_STREAMING[cli])

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "multi_turn", model=MODEL)
    try:
        for prompt, answer in zip(prompts, answers):
            submit(name, cli, prompt)
            wait_pane(name, answer, 120, "multi_turn note")
        submit(name, cli, final_prompt)
        wait_pane(name, tail, 180, "multi_turn final")
    finally:
        finish_cell(name, run_dir)

    finals = turn_requests(ms_requests(mockserver), final_prompt, cli)
    if not finals:
        raise AssertionError("multi_turn: no upstream request carries the final prompt")
    body = json.dumps(finals[-1].get("body"))
    missing = [word for word in TURN_WORDS if word not in body]
    if missing:
        raise AssertionError(f"multi_turn: the final upstream request is missing history: {missing}")
    rows = wait_proxy_rows(home, since, 1)
    if not rows:
        raise AssertionError("multi_turn: the proxy logged no request row")
    bad = [r for r in rows if r[1] not in (200, 499) or (r[1] != 499 and r[2])]
    if bad:
        raise AssertionError(f"multi_turn: proxy rows not clean: {bad}")


def scenario_large_output(cli: str, manifest: Path, home: Path, base_url: str, key: str,
                          mockserver: str, port: int, since: int) -> None:
    """A large, unicode-heavy answer ends with a marker; the CLI must render
    the tail and the unicode line, not truncate silently. Virtualized
    renderers paint only the window they show, so the 20 000-char prefix
    cannot be counted from the pane stream: the tail proves the proxy relayed
    the end, the unicode line proves nothing in between was mangled."""
    tail = "TAIL-" + secrets.token_hex(4)
    text = ("L" * 20000) + "\nUnicode: 日本語 émoji 🚀 combining é\n" + tail
    prompt = "Give me the large text."
    ms_reset(mockserver)
    _mock_expectation(mockserver, {"text": text, "streaming": STREAMING[cli]}, 150,
                      matcher=pinned_matcher(cli, prompt, 0, [(prompt, tail)], STREAMING[cli]),
                      model=MODEL)
    mock_fallback(mockserver, "ok", FALLBACK_STREAMING[cli])

    name, run_dir = start_cell(cli, manifest, home, base_url, key, "large_output", model=MODEL)
    try:
        submit(name, cli, prompt)
        wait_pane(name, tail, 180, "large output tail")
        stream = pane_stream(run_dir)
    finally:
        finish_cell(name, run_dir)

    missing = [s for s in ("日本語", "émoji", "🚀", "é") if s not in stream]
    if missing:
        raise AssertionError(f"large_output: the pane stream is missing {missing}")
    if not turn_requests(ms_requests(mockserver), prompt, cli):
        raise AssertionError("large_output: the upstream saw no turn request")
    rows = wait_proxy_rows(home, since, 1)
    if not rows:
        raise AssertionError("large_output: the proxy logged no request row")
    bad = [r for r in rows if r[1] not in (200, 499) or (r[1] != 499 and r[2])]
    if bad:
        raise AssertionError(f"large_output: proxy rows not clean: {bad}")


SCENARIOS.update({
    "fault_429": scenario_fault_429,
    "fault_529": scenario_fault_529,
    "fault_quota": scenario_fault_quota,
    "fault_truncated": scenario_fault_truncated,
    "fault_malformed_sse": scenario_fault_malformed_sse,
    "web_turn2": scenario_web_turn2,
    "tool_loop": scenario_tool_loop,
    "multi_turn": scenario_multi_turn,
    "large_output": scenario_large_output,
})


def load_cli_manifest(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def select_cells(manifest: dict, shard: str | None,
                 clients: str | None, scenarios: str | None) -> list[tuple[str, str]]:
    """The manifest is the source of truth; explicit --clients/--scenarios are
    a local override that ignores cell statuses. A missing shard or scenario is
    a failure, never a silent empty run."""
    by_key = {(c["client"], c["scenario"]): c for c in manifest["cells"]}
    if clients or scenarios:
        wanted_clients = [c.strip() for c in
                          (clients or ",".join(manifest["clients"])).split(",") if c.strip()]
        wanted_scenarios = [s.strip() for s in
                           (scenarios or ",".join(manifest["scenarios"])).split(",") if s.strip()]
        unknown = [s for s in wanted_scenarios if s not in SCENARIOS]
        if unknown:
            raise SystemExit(f"unknown scenario(s): {', '.join(unknown)}")
        out = []
        for cli in wanted_clients:
            for scenario in wanted_scenarios:
                if (cli, scenario) not in by_key:
                    raise SystemExit(f"no manifest cell for {cli}/{scenario}")
                out.append((cli, scenario))
        return out

    if shard:
        shard_scenarios = manifest["shards"].get(shard)
        if not shard_scenarios:
            raise SystemExit(f"unknown or empty shard {shard!r}")
    else:
        shard_scenarios = None
    out = []
    for cell in manifest["cells"]:
        if cell["status"] != "green":
            continue
        if shard_scenarios is not None and cell["scenario"] not in shard_scenarios:
            continue
        out.append((cell["client"], cell["scenario"]))
    if not out:
        raise SystemExit("manifest selection is empty")
    return out


def upstream_tool_names(reqs: list[dict]) -> list[str]:
    """The first upstream request's declared tool names; the seed-discovery
    evidence for the CLI tool-loop scenario."""
    for request in reqs:
        payload = (request.get("body") or {}).get("json") or {}
        tools = payload.get("tools") or []
        if tools:
            return [t.get("function", {}).get("name") or t.get("name")
                    for t in tools if isinstance(t, dict)]
    return []


def write_result(cli: str, scenario: str, home: Path, mockserver: str,
                 since: int, started: float, error: str) -> None:
    """One machine-readable cell report next to the pane and client logs."""
    run_dir = home / "clients" / cli / scenario
    run_dir.mkdir(parents=True, exist_ok=True)
    pane_tail = ""
    pane_log = run_dir / "pane.log"
    if pane_log.exists():
        pane_tail = pane_log.read_bytes()[-4096:].decode("utf-8", "replace")
    requests = ms_requests(mockserver)
    rows = proxy_rows(home, since)
    result = {
        "cli": cli,
        "scenario": scenario,
        "status": "fail" if error else "pass",
        "error": error,
        "duration_seconds": round(time.time() - started, 2),
        "pane_tail": pane_tail,
        "upstream_requests": len(requests),
        "upstream_tools": upstream_tool_names(requests),
        "proxy_rows": [{"status": row[1], "error": row[2]} for row in rows],
    }
    (run_dir / "result.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--home", required=True)
    parser.add_argument("--mockserver", required=True)
    parser.add_argument("--manifest", default=str(CLI_MANIFEST))
    parser.add_argument("--shard", default=None)
    parser.add_argument("--clients", default=None,
                        help="local override: comma-separated clients, ignores manifest statuses")
    parser.add_argument("--scenarios", default=None,
                        help="local override: comma-separated scenarios, ignores manifest statuses")
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

    manifest_doc = load_cli_manifest(Path(args.manifest))
    cells = select_cells(manifest_doc, args.shard, args.clients, args.scenarios)

    kill_codex_daemons()
    proxy = start_proxy(home, binary, args.port)
    try:
        key = configure(home, args.port, args.mockserver)
        failures = []
        for cli, scenario in cells:
            adapter = REPO / ".github/test-matrix/adapters" / f"{cli}.json"
            if not adapter.exists():
                raise SystemExit(f"no adapter manifest for {cli}")
            label = f"{cli}/{scenario}"
            print(f"{label:32s} running", flush=True)
            since = wait_proxy_quiet(home)
            started = time.time()
            error = ""
            try:
                SCENARIOS[scenario](cli, adapter, home, f"http://127.0.0.1:{args.port}",
                                    key, args.mockserver, args.port, since)
            except AssertionError as exc:
                error = str(exc)
                print(f"{label:32s} FAIL  {error}", flush=True)
                failures.append(label)
            except BaseException as exc:
                error = f"driver abort: {exc}"
                raise
            else:
                print(f"{label:32s} pass", flush=True)
            finally:
                try:
                    write_result(cli, scenario, home, args.mockserver, since, started, error)
                except Exception as exc:  # evidence must never mask the cell verdict
                    print(f"{label:32s} result.json failed: {exc}", flush=True)
        if failures:
            print(f"plain-start matrix: FAIL ({', '.join(failures)})")
            return 1
        print("plain-start matrix: OK")
        return 0
    finally:
        proxy.kill()


if __name__ == "__main__":
    sys.exit(main())
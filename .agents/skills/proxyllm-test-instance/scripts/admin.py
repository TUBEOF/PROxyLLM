#!/usr/bin/env python3
"""Admin client + setup for the PROxyLLM test instance.

Runs with `python3 -I`; stdlib only. Secrets are read from the instance .env and
are never printed. Bodies go through the request body, never argv.
"""

from __future__ import annotations

import argparse
import http.cookiejar
import json
import os
import re
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def redact(text: str) -> str:
    text = re.sub(r"pk_[A-Za-z0-9_-]+", "pk_<redacted>", text)
    text = re.sub(r"Bearer\s+[A-Za-z0-9._~+/=-]+", "Bearer <redacted>", text)
    text = re.sub(
        r"(api_key|password|token|authorization|csrf)([=:]\s*)[^\",}\s]+",
        r"\1\2<redacted>",
        text,
        flags=re.I,
    )
    return text


def load_env(path: str) -> dict[str, str]:
    out: dict[str, str] = {}
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                key, value = line.split("=", 1)
                if re.fullmatch(r"[A-Z_][A-Z0-9_]*", key):
                    out[key] = value
    except FileNotFoundError:
        pass
    return out


def write_env(path: str, values: dict[str, str]) -> None:
    """Replace/insert keys atomically, preserving comments and other keys."""
    lines: list[str] = []
    seen: set[str] = set()
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            lines = f.read().splitlines()
    except FileNotFoundError:
        lines = ["# PROxyLLM test instance secrets. Do not commit."]
    out: list[str] = []
    for line in lines:
        if "=" in line and not line.lstrip().startswith("#"):
            key = line.split("=", 1)[0]
            if key in values:
                out.append(f"{key}={values[key]}")
                seen.add(key)
                continue
        out.append(line)
    for key, value in values.items():
        if key not in seen:
            out.append(f"{key}={value}")
    tmp = f"{path}.tmp.{os.getpid()}"
    old_umask = os.umask(0o077)
    try:
        with open(tmp, "w", encoding="utf-8") as f:
            f.write("\n".join(out).rstrip("\n") + "\n")
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    finally:
        os.umask(old_umask)


class AdminClient:
    def __init__(self, base: str, cookie_path: str):
        self.base = base.rstrip("/")
        self.csrf = ""
        self.jar = http.cookiejar.MozillaCookieJar(cookie_path)
        try:
            self.jar.load(ignore_discard=True, ignore_expires=True)
        except (FileNotFoundError, http.cookiejar.LoadError):
            pass
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar)
        )
        self.cookie_path = cookie_path

    def save(self) -> None:
        try:
            self.jar.save(ignore_discard=True, ignore_expires=True)
            os.chmod(self.cookie_path, 0o600)
        except OSError:
            pass

    def call(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        *,
        timeout: int = 60,
        bearer: str | None = None,
        retry_login=None,
    ) -> tuple[int, dict]:
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if bearer:
            headers["Authorization"] = f"Bearer {bearer}"
        if self.csrf and method not in ("GET", "HEAD"):
            headers["X-CSRF-Token"] = self.csrf
        req = urllib.request.Request(
            self.base + path, data=data, headers=headers, method=method
        )
        try:
            with self.opener.open(req, timeout=timeout) as resp:
                raw = resp.read()
                token = resp.headers.get("X-CSRF-Token")
                if token:
                    self.csrf = token
                try:
                    payload = json.loads(raw) if raw else {}
                except json.JSONDecodeError:
                    payload = {"raw": raw.decode("utf-8", "replace")[:400]}
                self.save()
                return resp.status, payload
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                payload = json.loads(raw) if raw else {}
            except json.JSONDecodeError:
                payload = {"error": raw.decode("utf-8", "replace")[:400]}
            token = exc.headers.get("X-CSRF-Token") if exc.headers else None
            if token:
                self.csrf = token
            self.save()
            if exc.code == 401 and retry_login is not None:
                if retry_login():
                    return self.call(method, path, body, timeout=timeout,
                                     bearer=bearer, retry_login=None)
            return exc.code, payload

    def login(self, env_path: str, user: str, password: str) -> None:
        status, payload = self.call(
            "POST", "/api/admin/login", {"username": user, "password": password}
        )
        if status == 200 and payload.get("csrf_token"):
            self.csrf = payload["csrf_token"]
            return
        # Fresh DB: create the first admin.
        new_password = secrets.token_hex(16)
        status, payload = self.call(
            "POST",
            "/api/admin/bootstrap",
            {"username": user, "password": new_password},
        )
        if status == 200 and payload.get("csrf_token"):
            self.csrf = payload["csrf_token"]
            write_env(env_path, {"ADMIN_PASSWORD": new_password})
            print(f"bootstrapped admin {user!r} (password stored in .env)")
            return
        if status == 409:
            raise SystemExit(
                "admin exists but the stored password is wrong; refusing to guess"
            )
        raise SystemExit(
            f"login/bootstrap failed: HTTP {status}: "
            f"{redact(json.dumps(payload))[:300]}"
        )


def get_json(client: AdminClient, path: str) -> dict:
    status, payload = client.call("GET", path)
    if status != 200:
        raise SystemExit(f"GET {path} -> HTTP {status}: {redact(str(payload))[:300]}")
    return payload


def put(client: AdminClient, path: str, body: dict, *, ok: tuple[int, ...] = (200, 201)) -> dict:
    status, payload = client.call("PUT", path, body)
    if status not in ok:
        raise SystemExit(
            f"PUT {path} -> HTTP {status}: {redact(json.dumps(payload))[:400]}"
        )
    return payload


def post(client: AdminClient, path: str, body: dict | None = None, *,
         timeout: int = 120, ok: tuple[int, ...] = (200, 201)) -> dict:
    status, payload = client.call("POST", path, body or {}, timeout=timeout)
    if status not in ok:
        raise SystemExit(
            f"POST {path} -> HTTP {status}: {redact(json.dumps(payload))[:400]}"
        )
    return payload


def norm(value: str) -> str:
    return re.sub(r"[^a-z0-9]+", "", value.lower())


def tokens(value: str) -> list[str]:
    return [t for t in re.split(r"[^a-z0-9]+", value.lower()) if t]


def resolve_model(models: list[dict], query: str) -> dict:
    scored: list[tuple[int, dict]] = []
    qn = norm(query)
    qt = tokens(query)
    for model in models:
        mid = str(model.get("id", ""))
        name = str(model.get("name", ""))
        score = 0
        if query == mid or query == name:
            score = 100
        elif query.lower() == mid.lower() or query.lower() == name.lower():
            score = 95
        elif qn and qn in (norm(mid), norm(name)):
            score = 90
        else:
            hay = norm(mid + " " + name)
            if qt and all(t in hay for t in qt):
                score = 50 + len([t for t in qt if len(t) > 2])
                if any(t.isdigit() for t in qt) and any(t in hay for t in qt if t.isdigit()):
                    score += 5
        if score:
            scored.append((score, model))
    scored.sort(key=lambda pair: (-pair[0], pair[1].get("id", "")))
    if not scored:
        raise SystemExit(
            f"model not found: {query!r}. Candidates:\n"
            + "\n".join(f"  {m.get('id')}" for m in models[:5])
        )
    best = scored[0][0]
    ties = [m for s, m in scored if s == best]
    if len(ties) > 1:
        raise SystemExit(
            f"model ambiguous: {query!r}. Candidates:\n"
            + "\n".join(f"  {m.get('id')} ({m.get('name')})" for m in ties[:5])
        )
    return scored[0][1]


def model_id_path(mid: str) -> str:
    return urllib.parse.quote(mid, safe="/:._-")


def main() -> int:
    parser = argparse.ArgumentParser(description="PROxyLLM test-instance setup")
    parser.add_argument("--home", default=os.environ.get("PROXYLLM_TEST_HOME",
                                                         os.path.expanduser("~/proxyllm-test-instance")))
    parser.add_argument("--base", default=None)
    parser.add_argument("command", choices=["setup", "validate", "print-config",
                                            "resolve-models", "dry-run"])
    parser.add_argument("--force-aliases", action="store_true")
    parser.add_argument("--skip-probe", action="store_true")
    args = parser.parse_args()

    home = os.path.abspath(os.path.expanduser(args.home))
    env_path = os.path.join(home, ".env")
    instance_path = os.path.join(home, "instance.json")
    env = load_env(env_path)
    try:
        with open(instance_path, encoding="utf-8") as f:
            inst = json.load(f)
    except FileNotFoundError:
        raise SystemExit(f"instance.json missing: {instance_path}")
    base = args.base or f"http://127.0.0.1:{inst['port']}"
    api_key = env.get("OLLAMA_CLOUD_API_KEY", "")
    if not api_key:
        raise SystemExit(f"OLLAMA_CLOUD_API_KEY missing in {env_path}")
    user = env.get("ADMIN_USER", "admin")
    password = env.get("ADMIN_PASSWORD", "")
    if not password:
        raise SystemExit(f"ADMIN_PASSWORD missing in {env_path}")

    client = AdminClient(base, os.path.join(home, "run", "admin-cookies.txt"))
    client.login(env_path, user, password)

    if args.command == "print-config":
        print(json.dumps(_public_config(inst, env), indent=2, sort_keys=True))
        return 0

    if args.command == "dry-run":
        providers = get_json(client, "/api/admin/providers")
        models = get_json(client, "/api/admin/models")
        settings = get_json(client, "/api/admin/settings")
        fallbacks = get_json(client, "/api/admin/capability-fallbacks")
        print("providers:", ", ".join(p.get("id", "") for p in providers.get("providers", [])))
        print("models:", len(models.get("models", [])))
        print("settings keys:", ", ".join(e.get("key", "") for e in settings.get("entries", [])))
        print("fallbacks:", json.dumps(fallbacks))
        chat = resolve_model(models.get("models", []), inst["chat_model"])
        fallback = resolve_model(models.get("models", []), inst["fallback_model"])
        print(f"chat:     {chat['id']} (vision={chat.get('capabilities', {}).get('vision')})")
        print(f"fallback: {fallback['id']} (vision={fallback.get('capabilities', {}).get('vision')})")
        return 0

    if args.command == "resolve-models":
        models = get_json(client, "/api/admin/models").get("models", [])
        for query in (inst["chat_model"], inst["fallback_model"]):
            model = resolve_model(models, query)
            print(f"{query} -> {model['id']}")
        return 0

    if args.command == "validate":
        _validate(client, inst, env, args.skip_probe)
        return 0

    # setup
    _setup(client, inst, env, home, args.force_aliases, args.skip_probe)
    return 0


def _setup(client: AdminClient, inst: dict, env: dict, home: str,
           force_aliases: bool, skip_probe: bool) -> None:
    api_key = env["OLLAMA_CLOUD_API_KEY"]
    put(client, "/api/admin/providers/acc-ollama-cloud", {
        "type": "ollama-cloud",
        "name": "Ollama Cloud (test instance)",
        "config": {},
        "api_key": api_key,
    })
    put(client, "/api/admin/providers/acc-ollama-web", {
        "type": "ollama-cloud-web",
        "name": "Ollama Web Tools (test instance)",
        "config": {},
        "api_key": api_key,
    })
    put(client, "/api/admin/providers/acc-ollama-cloud/enabled", {"enabled": True})
    put(client, "/api/admin/providers/acc-ollama-web/enabled", {"enabled": True})
    print("providers: acc-ollama-cloud, acc-ollama-web")

    sync = post(client, "/api/admin/model-sync/run", timeout=180)
    providers = sync.get("last_run", {}).get("providers", [])
    for row in providers:
        if row.get("provider_id") == "acc-ollama-cloud" and row.get("skipped"):
            raise SystemExit(
                "model sync skipped the chat provider: "
                + str(row.get("skipped_reason", ""))
            )
    print(f"model sync: {len(providers)} provider row(s)")

    models = get_json(client, "/api/admin/models").get("models", [])
    chat = resolve_model(models, inst["chat_model"])
    fallback = resolve_model(models, inst["fallback_model"])
    print(f"chat model:     {chat['id']}")
    print(f"fallback model: {fallback['id']}")

    # Pin the fallback-test row blind.
    caps = dict(fallback.get("capabilities") or {})
    caps["vision"] = False
    pinned = dict(caps.get("pinned") or {})
    pinned["vision"] = True
    caps["pinned"] = pinned
    put(client, "/api/admin/models/" + model_id_path(fallback["id"]), {
        "provider_id": fallback.get("provider_id"),
        "name": fallback.get("name"),
        "capabilities": caps,
        "priority": fallback.get("priority", 0),
    })
    print("fallback model pinned vision=false")

    # Aliases: merge, never clobber an existing different target silently.
    settings = get_json(client, "/api/admin/settings").get("entries", [])
    raw = next((e.get("value", "{}") for e in settings if e.get("key") == "model_aliases"), "{}")
    try:
        aliases = json.loads(raw or "{}")
    except json.JSONDecodeError:
        raise SystemExit("model_aliases is not valid JSON; refusing to overwrite")
    wanted = {
        "claude-*": chat["id"],
        "proxy-chat": chat["id"],
        "sonnet": chat["id"],
        "opus": chat["id"],
        "haiku": chat["id"],
        "fable": chat["id"],
        inst["chat_model"]: chat["id"],
        "fallback-test": fallback["id"],
        inst["fallback_model"]: fallback["id"],
    }
    changed = False
    for key, target in wanted.items():
        current = aliases.get(key)
        if current is not None and current != target and not force_aliases:
            print(f"alias conflict: {key} -> {current} (keeping; use --force-aliases)",
                  file=sys.stderr)
            continue
        if current != target:
            aliases[key] = target
            changed = True
    if changed:
        put(client, "/api/admin/settings", {
            "key": "model_aliases",
            "value": json.dumps(aliases, separators=(",", ":"), sort_keys=True),
        })
    print(f"aliases: {len(aliases)} entr(y/ies)")

    # One live write for all three fallback lists.
    put(client, "/api/admin/capability-fallbacks", {
        "vision": [chat["id"]],
        "web_search": ["acc-ollama-web"],
        "web_fetch": ["acc-ollama-web"],
    })
    print("capability fallbacks: vision=chat, web=acc-ollama-web")

    # Client key: verify an existing one, otherwise mint a new one.
    key = env.get("CLIENT_KEY", "")
    if key and _key_works(client, key):
        print("client key: existing key works")
    else:
        payload = post(client, "/api/admin/keys", {"name": "proxyllm-test-instance"})
        key = payload.get("key", "")
        if not key:
            raise SystemExit("key creation returned no key")
        write_env(os.path.join(home, ".env"), {"CLIENT_KEY": key})
        env["CLIENT_KEY"] = key
        print(f"client key: created (prefix {key[:8]}…)")
        if not _key_works(client, key):
            raise SystemExit("new client key does not authenticate against /v1/models")

    # Persist resolved ids.
    inst["resolved"] = {
        "chat": chat["id"],
        "fallback": fallback["id"],
        "vision": chat["id"],
        "web": "acc-ollama-web",
    }
    with open(os.path.join(home, "instance.json"), "w", encoding="utf-8") as f:
        json.dump(inst, f, indent=2, sort_keys=True)
        f.write("\n")

    if not skip_probe:
        for model in (chat, fallback):
            body = post(client, "/api/admin/models/test/" + model_id_path(model["id"]),
                        timeout=30)
            ok = bool(body.get("ok"))
            print(f"model test {model['id']}: {'ok' if ok else 'FAILED'}")
            if not ok:
                detail = body.get("error") or body.get("detail")
                print(f"  {redact(str(detail))[:300]}", file=sys.stderr)

    _validate(client, inst, env, skip_probe=True)
    print("setup complete")


def _key_works(client: AdminClient, key: str) -> bool:
    status, _ = client.call("GET", "/v1/models", bearer=key, timeout=15)
    return status == 200


def _validate(client: AdminClient, inst: dict, env: dict, skip_probe: bool) -> None:
    providers = get_json(client, "/api/admin/providers").get("providers", [])
    by_id = {p.get("id"): p for p in providers}
    for pid, ptype in (("acc-ollama-cloud", "ollama-cloud"),
                       ("acc-ollama-web", "ollama-cloud-web")):
        row = by_id.get(pid)
        if not row or row.get("type") != ptype:
            raise SystemExit(f"provider missing or wrong type: {pid}")
        if not row.get("enabled"):
            raise SystemExit(f"provider disabled: {pid}")
    models = get_json(client, "/api/admin/models").get("models", [])
    resolved = inst.get("resolved") or {}
    chat = next((m for m in models if m.get("id") == resolved.get("chat")), None)
    fallback = next((m for m in models if m.get("id") == resolved.get("fallback")), None)
    if not chat or not fallback:
        raise SystemExit("resolved models are missing from /api/admin/models")
    caps = fallback.get("capabilities") or {}
    if caps.get("vision") is not False or (caps.get("pinned") or {}).get("vision") is not True:
        raise SystemExit("fallback model is not pinned vision=false")
    fallbacks = get_json(client, "/api/admin/capability-fallbacks")
    if fallbacks.get("vision") != [chat["id"]]:
        raise SystemExit(f"vision fallback mismatch: {fallbacks.get('vision')}")
    for key in ("web_search", "web_fetch"):
        if fallbacks.get(key) != ["acc-ollama-web"]:
            raise SystemExit(f"{key} fallback mismatch: {fallbacks.get(key)}")
    settings = get_json(client, "/api/admin/settings").get("entries", [])
    raw = next((e.get("value", "{}") for e in settings if e.get("key") == "model_aliases"), "{}")
    aliases = json.loads(raw or "{}")
    for key, target in (("claude-*", chat["id"]), ("proxy-chat", chat["id"]),
                        ("fallback-test", fallback["id"])):
        if aliases.get(key) != target:
            raise SystemExit(f"alias mismatch: {key} -> {aliases.get(key)} (want {target})")
    key = env.get("CLIENT_KEY", "")
    if not key or not _key_works(client, key):
        raise SystemExit("client key missing or invalid")
    if not skip_probe:
        for model in (chat, fallback):
            body = post(client, "/api/admin/models/test/" + model_id_path(model["id"]),
                        timeout=30)
            if not body.get("ok"):
                raise SystemExit(f"model test failed: {model['id']}")
    print("validation: ok")


def _public_config(inst: dict, env: dict) -> dict:
    return {
        "home": inst.get("home"),
        "port": inst.get("port"),
        "chat_model": inst.get("chat_model"),
        "fallback_model": inst.get("fallback_model"),
        "resolved": inst.get("resolved"),
        "env": {
            key: ("<set>" if env.get(key) else "<missing>")
            for key in ("OLLAMA_CLOUD_API_KEY", "ADMIN_USER", "ADMIN_PASSWORD", "CLIENT_KEY")
        },
    }


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)

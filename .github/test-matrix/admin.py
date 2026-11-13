#!/usr/bin/env python3
"""Admin API client for the plain-start CLI matrix.

Runs with `python3 -I`; stdlib only. Bodies go through the request body, never
argv. Secrets are masked before any error leaves this module.
"""

from __future__ import annotations

import http.cookiejar
import json
import os
import re
import secrets
import urllib.error
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

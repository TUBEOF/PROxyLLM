#!/usr/bin/env python3
"""Read-only SQLite inspection for the PROxyLLM test-instance harness."""

from __future__ import annotations

import argparse
import json
import sqlite3
import sys


def connect(path: str) -> sqlite3.Connection:
    con = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    con.execute("PRAGMA query_only=ON")
    return con


def rows(con: sqlite3.Connection, sql: str, args: tuple = ()) -> list[dict]:
    return [dict(r) for r in con.execute(sql, args).fetchall()]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--db", required=True)
    sub = parser.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("maxids")
    p = sub.add_parser("recent")
    p.add_argument("--limit", type=int, default=20)
    p = sub.add_parser("children")
    p.add_argument("--limit", type=int, default=50)
    p.add_argument("--kind", default=None)
    p = sub.add_parser("trace")
    p.add_argument("--id", type=int, required=True)
    p = sub.add_parser("events")
    p.add_argument("--id", type=int, required=True)
    p = sub.add_parser("config")
    p.add_argument("--key", default=None)
    p = sub.add_parser("inventory")
    p = sub.add_parser("scan")
    p.add_argument("--since-request", type=int, default=0)
    p.add_argument("--since-child", type=int, default=0)

    args = parser.parse_args()
    con = connect(args.db)

    if args.cmd == "maxids":
        print(json.dumps({
            "request": con.execute("SELECT COALESCE(MAX(id),0) FROM request_logs").fetchone()[0],
            "child": con.execute("SELECT COALESCE(MAX(id),0) FROM request_child_logs").fetchone()[0],
        }))
    elif args.cmd == "recent":
        print(json.dumps(rows(con, """
            SELECT id, ts, model, provider_id, status, latency_ms, error_redacted,
                   protocol, features
            FROM request_logs ORDER BY id DESC LIMIT ?
        """, (args.limit,)), indent=2))
    elif args.cmd == "children":
        sql = """SELECT id, parent_id, ts, kind, provider_id, model, from_ref,
                        to_ref, detail, latency_ms, usage
                 FROM request_child_logs"""
        params: tuple = ()
        if args.kind:
            sql += " WHERE kind = ?"
            params = (args.kind,)
        sql += " ORDER BY id DESC LIMIT ?"
        params += (args.limit,)
        print(json.dumps(rows(con, sql, params), indent=2))
    elif args.cmd == "trace":
        row = con.execute("SELECT route_trace FROM request_logs WHERE id = ?",
                          (args.id,)).fetchone()
        print(row[0] if row else "{}")
    elif args.cmd == "events":
        print(json.dumps(rows(con, """
            SELECT json_extract(e.value,'$.type')        AS type,
                   json_extract(e.value,'$.from')        AS from_ref,
                   json_extract(e.value,'$.to')          AS to_ref,
                   json_extract(e.value,'$.provider_id') AS provider_id,
                   json_extract(e.value,'$.model')       AS model,
                   json_extract(e.value,'$.class')       AS class,
                   json_extract(e.value,'$.reason')      AS reason,
                   json_extract(e.value,'$.capability')  AS capability
            FROM request_logs r, json_each(r.route_trace,'$.events') e
            WHERE r.id = ? ORDER BY e.key
        """, (args.id,)), indent=2))
    elif args.cmd == "config":
        if args.key:
            print(json.dumps(rows(con, "SELECT key, value FROM config WHERE key = ?",
                                  (args.key,)), indent=2))
        else:
            print(json.dumps(rows(con, "SELECT key, value FROM config ORDER BY key"),
                             indent=2))
    elif args.cmd == "inventory":
        print(json.dumps({
            "providers": rows(con, "SELECT id, type, name, enabled FROM providers ORDER BY id"),
            "models": rows(con, "SELECT id, provider_id, name, capabilities, origin FROM models ORDER BY id"),
            "rules": rows(con, "SELECT id, match_model, chain, priority, enabled FROM routing_rules ORDER BY id"),
            "keys": rows(con, "SELECT id, name, key_prefix, created_at FROM client_keys ORDER BY created_at"),
        }, indent=2))
    elif args.cmd == "scan":
        logs = rows(con, """
            SELECT id, ts, model, provider_id, status, latency_ms, error_redacted
            FROM request_logs WHERE id > ? ORDER BY id
        """, (args.since_request,))
        children = rows(con, """
            SELECT id, parent_id, kind, provider_id, model, from_ref, to_ref,
                   detail, latency_ms
            FROM request_child_logs WHERE id > ? ORDER BY id
        """, (args.since_child,))
        versions = []
        for row in rows(con, "SELECT route_trace FROM request_logs WHERE id > ?",
                        (args.since_request,)):
            try:
                versions.append(json.loads(row["route_trace"]).get("version"))
            except (TypeError, json.JSONDecodeError):
                versions.append(None)
        kinds: dict[str, int] = {}
        for child in children:
            kinds[child["kind"]] = kinds.get(child["kind"], 0) + 1
        print(json.dumps({
            "requests": len(logs),
            "failures": [r for r in logs if (r["status"] or 0) >= 400],
            "children": len(children),
            "child_kinds": kinds,
            "trace_versions": sorted({v for v in versions if v is not None}),
            "unknown_trace_versions": [v for v in versions if v is None],
            "logs": logs,
            "child_rows": children,
        }, indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except sqlite3.Error as exc:
        print(f"sqlite error: {exc}", file=sys.stderr)
        sys.exit(2)

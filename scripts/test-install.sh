#!/usr/bin/env bash
# End-to-end test for scripts/install.sh (ADR-024): a real proxy process, a
# real admin session and client key, a temporary HOME, and assertions on the
# files the installer writes. Runs in the CI "installer" job and locally.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
HOME_DIR="$WORK/home"
mkdir -p "$HOME_DIR"
PORT="${PROXYLLM_TEST_PORT:-18080}"
BASE="http://127.0.0.1:$PORT"
JAR="$WORK/cookies"
PROXY_PID=""

cleanup() {
  [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

json_field() { python3 -c 'import json,sys;print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }

cd "$ROOT"
go build -trimpath -o "$WORK/proxyllm" ./cmd/proxyllm

PROXYLLM_SERVER_ADDR="127.0.0.1:$PORT" \
  PROXYLLM_STORAGE_PATH="$WORK/proxy.db" \
  PROXYLLM_MASTER_KEY="$(printf 'a%.0s' {1..64})" \
  "$WORK/proxyllm" >"$WORK/proxy.log" 2>&1 &
PROXY_PID=$!

ready=0
for _ in $(seq 1 100); do
  if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" = 1 ] || { cat "$WORK/proxy.log"; fail "proxy did not become healthy"; }

csrf=$(curl -fsS -c "$JAR" -X POST "$BASE/api/admin/bootstrap" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"installer-test"}' | json_field csrf_token)
[ -n "$csrf" ] || fail "bootstrap returned no CSRF token"

authed() {
  curl -fsS -b "$JAR" -X "$1" "$BASE$2" \
    -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" -d "$3"
}

authed PUT /api/admin/providers/prv-test \
  '{"type":"custom-openai","name":"Test","config":{"base_url":"https://example.com/v1"}}' >/dev/null
authed PUT /api/admin/models/anthropic/glm-5.3 \
  '{"provider_id":"prv-test","name":"glm-5.3","capabilities":{"streaming":true,"tools":true},"priority":0}' >/dev/null
KEY=$(authed POST /api/admin/keys '{"name":"installer-test"}' | json_field key)
case "$KEY" in pk_*) ;; *) fail "key creation did not return a pk_ key" ;; esac

run_installer() {
  HOME="$HOME_DIR" PROXYLLM_URL="$BASE" PROXYLLM_API_KEY="$KEY" \
    PROXYLLM_CLIS=claude,codex,opencode bash "$ROOT/scripts/install.sh" "$@"
}

# A real Codex config: a user top-level key, a [features] table that is not
# the last table, and a profile that owns its own `model` key. The merge must
# keep all three intact.
mkdir -p "$HOME_DIR/.codex"
cat >"$HOME_DIR/.codex/config.toml" <<'EOF'
approval_policy = "never"

[features]
web_search = true

[profiles.p]
model = "gpt-5"
EOF

# --- install ---
run_installer --yes >"$WORK/install.log" 2>&1 || { cat "$WORK/install.log"; fail "installer failed"; }

CLAUDE="$HOME_DIR/.claude/settings.json"
CODEX="$HOME_DIR/.codex/config.toml"
OPENCODE="$HOME_DIR/.config/opencode/opencode.json"
for f in "$CLAUDE" "$CODEX" "$OPENCODE"; do
  [ -f "$f" ] || fail "missing $f"
done
grep -q "$BASE" "$CLAUDE" || fail "Claude settings lack ANTHROPIC_BASE_URL"
grep -q 'CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY' "$CLAUDE" || fail "Claude settings lack the discovery flag"
grep -q 'ANTHROPIC_DEFAULT_HAIKU_MODEL' "$CLAUDE" || fail "Claude settings lack the small-fast model"
[ "$(stat -c %a "$CLAUDE")" = 600 ] || fail "Claude settings are not mode 600"

grep -q '^model = "anthropic/glm-5.3"' "$CODEX" || fail "Codex model not written"
grep -q '^\[model_providers.proxyllm\]' "$CODEX" || fail "Codex provider table missing"
grep -q "model_catalog_url = \"$BASE/v1/model_catalog\"" "$CODEX" || fail "Codex catalog URL missing"
grep -q '^api_key_model_discovery = true' "$CODEX" || fail "Codex discovery switch missing"
grep -q 'env_key = "PROXYLLM_API_KEY"' "$CODEX" || fail "Codex env_key missing"
if grep -q "$KEY" "$CODEX"; then fail "Codex config contains the client key"; fi
[ "$(grep -c '^\[features\]' "$CODEX")" = 1 ] || fail "Codex [features] table duplicated"
awk '/^approval_policy = "never"/{a=NR} /^\[model_providers\.proxyllm\]/{p=NR} END{exit !(a && p && a<p)}' "$CODEX" \
  || fail "Codex: a user top-level key ended up inside the provider table"
awk '/^\[features\]/{f=1;next} /^\[/{f=0} f && /^web_search = true/{found=1} END{exit !found}' "$CODEX" \
  || fail "Codex: an unrelated [features] key was lost"
awk '/^\[profiles\.p\]/{f=1;next} /^\[/{f=0} f && /^model = "gpt-5"/{found=1} END{exit !found}' "$CODEX" \
  || fail "Codex: a profile's own model key was removed"

grep -q '"proxyllm"' "$OPENCODE" || fail "OpenCode provider block missing"
grep -q '"model": "proxyllm/anthropic/glm-5.3"' "$OPENCODE" || fail "OpenCode default model missing"
if grep -q "$KEY" "$OPENCODE"; then fail "OpenCode config contains the client key"; fi
pass "installer wrote the three configs"

# --- idempotency ---
before=$(sha256sum "$CLAUDE" "$CODEX" "$OPENCODE")
run_installer --yes >"$WORK/install2.log" 2>&1 || { cat "$WORK/install2.log"; fail "second run failed"; }
after=$(sha256sum "$CLAUDE" "$CODEX" "$OPENCODE")
[ "$before" = "$after" ] || fail "a second run changed the files"
pass "second run is idempotent"

# --- dry run writes nothing ---
before=$(sha256sum "$CLAUDE" "$CODEX" "$OPENCODE")
run_installer --yes --dry-run >"$WORK/dry.log" 2>&1 || { cat "$WORK/dry.log"; fail "dry run failed"; }
after=$(sha256sum "$CLAUDE" "$CODEX" "$OPENCODE")
[ "$before" = "$after" ] || fail "--dry-run wrote files"
grep -q '^--- ' "$WORK/dry.log" || fail "--dry-run printed no diff"
pass "--dry-run writes nothing"

# --- the documented `curl … | bash` shape ---
# The script arrives on stdin, so the prompts must read the terminal instead
# of consuming it; before this was fixed the first prompt exited silently.
HOME2="$WORK/home-pipe"
mkdir -p "$HOME2" "$WORK/bin"
printf '#!/bin/sh\nexit 0\n' >"$WORK/bin/claude"
chmod +x "$WORK/bin/claude"
if command -v script >/dev/null 2>&1; then
  printf '1\ny\n\n' | PATH="$WORK/bin:$PATH" HOME="$HOME2" \
    PROXYLLM_URL="$BASE" PROXYLLM_API_KEY="$KEY" \
    script -qec "cat '$ROOT/scripts/install.sh' | bash" /dev/null \
    >"$WORK/pipe.log" 2>&1 || { cat "$WORK/pipe.log"; fail "piped interactive run failed"; }
  grep -q "$BASE" "$HOME2/.claude/settings.json" 2>/dev/null \
    || { cat "$WORK/pipe.log"; fail "piped interactive run wrote no Claude settings"; }
  pass "the piped form prompts on the terminal"
else
  printf 'skip: piped-form check (no script(1))\n'
fi

# --- uninstall ---
run_installer --uninstall >"$WORK/uninstall.log" 2>&1 || { cat "$WORK/uninstall.log"; fail "uninstall failed"; }
if grep -q 'ANTHROPIC_BASE_URL' "$CLAUDE"; then fail "uninstall left the Claude env keys"; fi
if grep -q 'model_providers.proxyllm' "$CODEX"; then fail "uninstall left the Codex provider"; fi
if grep -q '"proxyllm"' "$OPENCODE"; then fail "uninstall left the OpenCode provider"; fi
grep -q '^approval_policy = "never"' "$CODEX" || fail "uninstall removed a user top-level key"
grep -q '^model = "gpt-5"' "$CODEX" || fail "uninstall removed a profile's model key"
pass "uninstall removed the managed blocks"

printf 'installer test: OK\n'

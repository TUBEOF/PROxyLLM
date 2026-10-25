#!/usr/bin/env bash
# Shared helpers for the PROxyLLM test-instance harness.
# Sourced, never executed. Bash + python3 -I only; no third-party tools.

set -Eeuo pipefail
umask 077
export PYTHONDONTWRITEBYTECODE=1

SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPTS_DIR="$SKILL_DIR/scripts"
REPO="${PROXYLLM_TEST_REPO:-$(cd "$SKILL_DIR/../../.." && pwd)}"
INSTANCE_HOME="${PROXYLLM_TEST_HOME:-$HOME/proxyllm-test-instance}"
HOME_EXPLICIT=0
[ -n "${PROXYLLM_TEST_HOME:-}" ] && HOME_EXPLICIT=1
ENV_FILE="" INSTANCE_JSON="" MARKER="" PID_FILE="" LOCK_FILE=""
COOKIE_JAR="" PROXY_LOG="" INSTANCE_LOG="" PORT="${PROXYLLM_TEST_PORT:-18080}"
BASE_URL="" BIN="" DB="" MASTER_KEY="" TMP_DIR=""

set_instance_home() {
  INSTANCE_HOME="$1"
  ENV_FILE="$INSTANCE_HOME/.env"
  INSTANCE_JSON="$INSTANCE_HOME/instance.json"
  MARKER="$INSTANCE_HOME/.proxyllm-test-instance"
  PID_FILE="$INSTANCE_HOME/run/proxyllm.pid"
  LOCK_FILE="$INSTANCE_HOME/run/instance.lock"
  COOKIE_JAR="$INSTANCE_HOME/run/admin-cookies.txt"
  PROXY_LOG="$INSTANCE_HOME/logs/proxyllm.log"
  INSTANCE_LOG="$INSTANCE_HOME/logs/instance.log"
  BASE_URL="http://127.0.0.1:$PORT"
  BIN="$INSTANCE_HOME/bin/proxyllm"
  DB="$INSTANCE_HOME/data/proxyllm.db"
  MASTER_KEY="$INSTANCE_HOME/data/master.key"
  TMP_DIR="$INSTANCE_HOME/run/tmp"
}

set_instance_home "$INSTANCE_HOME"

log_info()  { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$INSTANCE_LOG"; }
log_warn()  { printf '%s WARN %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$INSTANCE_LOG" >&2; }
log_error() { printf '%s ERROR %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$INSTANCE_LOG" >&2; }
die()       { log_error "$*"; exit 2; }

# Mask the secret shapes the harness may encounter in a log line.
redact() {
  sed -E \
    -e 's/pk_[A-Za-z0-9_-]+/pk_<redacted>/g' \
    -e 's/Bearer [A-Za-z0-9._~+\/=-]+/Bearer <redacted>/g' \
    -e 's/(api_key|password|token|authorization|csrf)([=:][[:space:]]*)[^",} ]+/\1\2<redacted>/Ig'
}

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command missing: $1"; }

json_get() {
  python3 -I -c 'import json,sys
try:
    v=json.load(sys.stdin)
except Exception:
    sys.exit(1)
cur=v
for part in sys.argv[1].split("."):
    if isinstance(cur, dict):
        cur=cur.get(part)
    else:
        cur=None
        break
print("" if cur is None else cur)' "$1"
}

# Strict dotenv reader: NAME=VALUE only, no source/eval, no whitespace export.
load_env_file() {
  local file="$1"
  [ -r "$file" ] || return 0
  local line key value
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      ''|'#'*) continue ;;
    esac
    case "$line" in
      *=*) ;;
      *) continue ;;
    esac
    key="${line%%=*}"
    value="${line#*=}"
    [[ "$key" =~ ^[A-Z_][A-Z0-9_]*$ ]] || continue
    export "$key=$value"
  done < "$file"
}

require_instance() {
  [ -d "$INSTANCE_HOME" ] || die "instance home missing: $INSTANCE_HOME (run init)"
  [ -f "$INSTANCE_JSON" ] || die "instance.json missing: $INSTANCE_JSON"
  load_env_file "$ENV_FILE"
  PORT="$(json_get port < "$INSTANCE_JSON" 2>/dev/null || echo "$PORT")"
  BASE_URL="http://127.0.0.1:$PORT"
  DB="$INSTANCE_HOME/data/proxyllm.db"
}

atomic_write() {
  local path="$1" mode="${2:-600}"
  local dir tmp
  dir="$(dirname "$path")"
  mkdir -p "$dir"
  tmp="$(mktemp "$dir/.write.XXXXXX")"
  cat > "$tmp"
  chmod "$mode" "$tmp"
  mv -f "$tmp" "$path"
}

port_free() {
  local port="$1"
  python3 -I - "$port" <<'PY'
import socket, sys
p = int(sys.argv[1])
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(("127.0.0.1", p))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
}

wait_health() {
  local tries="${1:-80}"
  for _ in $(seq 1 "$tries"); do
    if curl -sf -m 2 "$BASE_URL/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

pid_alive() {
  local pid="$1"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

pid_is_ours() {
  local pid="$1"
  [ -r "/proc/$pid/cmdline" ] || return 1
  tr '\0' ' ' < "/proc/$pid/cmdline" | grep -Fq "$BIN"
}

with_lock() {
  mkdir -p "$(dirname "$LOCK_FILE")"
  if command -v flock >/dev/null 2>&1; then
    exec 9>"$LOCK_FILE"
    flock -n 9 || die "another instance operation is running (lock: $LOCK_FILE)"
    "$@"
    local rc=$?
    flock -u 9 || true
    exec 9>&-
    return $rc
  fi
  local lockdir="$LOCK_FILE.d"
  mkdir "$lockdir" 2>/dev/null || die "another instance operation is running (lock: $lockdir)"
  trap 'rmdir "$lockdir" 2>/dev/null || true' RETURN
  "$@"
}

instance_running() {
  [ -f "$PID_FILE" ] || return 1
  local pid
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  pid_alive "$pid" && pid_is_ours "$pid"
}

cleanup_tmp() {
  [ -d "$TMP_DIR" ] && find "$TMP_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
}

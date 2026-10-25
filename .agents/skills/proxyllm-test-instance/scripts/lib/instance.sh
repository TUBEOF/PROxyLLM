#!/usr/bin/env bash
# Instance lifecycle: init, build, start, stop, status, logs, doctor.
# Sourced by test-instance.sh after common.sh.

instance_init() {
  local key_file="" prompt_key=0 chat_model="" fallback_model="" force=0 allow_private=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --home) INSTANCE_HOME="$2"; shift 2 ;;
      --repo) REPO="$2"; shift 2 ;;
      --port) PORT="$2"; shift 2 ;;
      --key-file) key_file="$2"; shift 2 ;;
      --prompt-key) prompt_key=1; shift ;;
      --chat-model) chat_model="$2"; shift 2 ;;
      --fallback-model) fallback_model="$2"; shift 2 ;;
      --web-tools) shift 2 ;;
      --allow-private-provider-urls) allow_private=1; shift ;;
      --force) force=1; shift ;;
      *) die "init: unknown option $1" ;;
    esac
  done
  if [ "$HOME_EXPLICIT" -eq 0 ] && [ -t 0 ]; then
    local answer
    printf 'Instance path [%s]: ' "$INSTANCE_HOME" >&2
    read -r answer
    if [ -n "$answer" ]; then
      INSTANCE_HOME="$answer"
      HOME_EXPLICIT=1
    fi
  fi
  set_instance_home "$INSTANCE_HOME"

  mkdir -p "$INSTANCE_HOME"/{bin,data,logs,run,tmp,clients,artifacts}
  chmod 700 "$INSTANCE_HOME"

  if [ -f "$ENV_FILE" ] && [ "$force" -eq 0 ]; then
    log_info "keeping existing $ENV_FILE (use --force to replace)"
    load_env_file "$ENV_FILE"
  else
    local key=""
    if [ -n "$key_file" ]; then
      [ -r "$key_file" ] || die "key file not readable: $key_file"
      key="$(
        python3 -I - "$key_file" <<'PY'
import sys
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    line = line.strip()
    if line.startswith("OLLAMA_CLOUD_API_KEY="):
        print(line.split("=", 1)[1].strip().strip('"').strip("'"))
        break
PY
      )"
      [ -n "$key" ] || die "OLLAMA_CLOUD_API_KEY not found in $key_file"
    elif [ "$prompt_key" -eq 1 ] || [ -t 0 ]; then
      printf 'Ollama Cloud API key: ' >&2
      read -r -s key
      printf '\n' >&2
      [ -n "$key" ] || die "empty API key"
    else
      die "no key source: pass --key-file PATH or --prompt-key"
    fi
    if [ -z "$chat_model" ]; then
      if [ -t 0 ]; then
        printf 'Chat/model alias target [deepseek-v4.1-flash:1017]: ' >&2
        read -r chat_model
      fi
      chat_model="${chat_model:-deepseek-v4.1-flash:1017}"
    fi
    if [ -z "$fallback_model" ]; then
      if [ -t 0 ]; then
        printf 'Fallback-test model (no vision) [nemotron-3-ultra]: ' >&2
        read -r fallback_model
      fi
      fallback_model="${fallback_model:-nemotron-3-ultra}"
    fi
    local admin_password
    admin_password="$(python3 -I -c 'import secrets;print(secrets.token_hex(16))')"
    {
      printf '# PROxyLLM test instance secrets. Do not commit.\n'
      printf 'OLLAMA_CLOUD_API_KEY=%s\n' "$key"
      printf 'ADMIN_USER=admin\n'
      printf 'ADMIN_PASSWORD=%s\n' "$admin_password"
    } | atomic_write "$ENV_FILE" 600
    log_info "wrote $ENV_FILE (0600)"
  fi

  if [ ! -f "$INSTANCE_JSON" ] || [ "$force" -eq 1 ]; then
    python3 -I - "$INSTANCE_JSON" "$REPO" "$INSTANCE_HOME" "$PORT" \
      "$chat_model" "$fallback_model" "$allow_private" <<'PY'
import json, sys, time
path, repo, home, port, chat, fallback, allow_private = sys.argv[1:8]
data = {
    "schema": 1,
    "repo": repo,
    "home": home,
    "port": int(port),
    "chat_model": chat or "deepseek-v4.1-flash:1017",
    "fallback_model": fallback or "nemotron-3-ultra",
    "web_tools": "ollama-cloud-web",
    "allow_private_provider_urls": allow_private == "1",
    "created_at": int(time.time()),
    "resolved": {},
}
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
    chmod 644 "$INSTANCE_JSON"
  fi
  if [ "$allow_private" -eq 1 ]; then
    python3 -I - "$INSTANCE_JSON" <<'PY'
import json, sys
path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    data = json.load(f)
data["allow_private_provider_urls"] = True
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
  fi
  python3 -I - "$MARKER" "$INSTANCE_HOME" <<'PY'
import json, sys
with open(sys.argv[1], "w", encoding="utf-8") as f:
    json.dump({"skill": "proxyllm-test-instance", "home": sys.argv[2]}, f)
    f.write("\n")
PY
  log_info "instance initialized at $INSTANCE_HOME (port $PORT)"
  log_info "next: test-instance.sh start --build, then setup"
}

instance_build() {
  local force="${1:-0}"
  [ -d "$REPO" ] || die "repo missing: $REPO"
  if [ "$force" -eq 0 ] && [ -x "$BIN" ] && [ "$BIN" -nt "$REPO/cmd/proxyllm/main.go" ] \
     && [ "$BIN" -nt "$REPO/go.mod" ]; then
    log_info "binary is newer than the sources; keeping $BIN"
    return 0
  fi
  log_info "building $REPO/proxyllm"
  ( cd "$REPO" && env -u GOROOT make build )
  [ -x "$REPO/proxyllm" ] || die "build did not produce $REPO/proxyllm"
  mkdir -p "$(dirname "$BIN")"
  cp -f "$REPO/proxyllm" "$BIN"
  chmod 755 "$BIN"
  sha256sum "$BIN" | awk '{print $1}' > "$BIN.sha256"
  python3 -I - "$INSTANCE_JSON" "$(sha256sum "$BIN" | awk '{print $1}')" \
    "$(git -C "$REPO" rev-parse HEAD 2>/dev/null || true)" <<'PY'
import json, sys
path, sha, head = sys.argv[1:4]
with open(path, encoding="utf-8") as f:
    data = json.load(f)
data["binary_sha256"] = sha
data["git_head"] = head
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
  log_info "built $BIN ($(cut -c1-12 "$BIN.sha256"))"
}

instance_start() {
  local build=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --build) build=1; shift ;;
      --no-build) build=0; shift ;;
      --binary) BIN="$2"; shift 2 ;;
      --wait) shift 2 ;;
      --log-level) shift 2 ;;
      *) die "start: unknown option $1" ;;
    esac
  done
  require_instance
  if instance_running; then
    log_info "already running (pid $(cat "$PID_FILE")) on $BASE_URL"
    return 0
  fi
  if [ "$build" -eq 1 ] || [ ! -x "$BIN" ]; then
    instance_build 1
  fi
  if ! port_free "$PORT"; then
    die "port $PORT is in use; choose another with --port or init --port"
  fi
  mkdir -p "$(dirname "$PID_FILE")" "$(dirname "$PROXY_LOG")"
  local allow_private
  allow_private="$(python3 -I -c 'import json,sys
try:
    print("1" if json.load(open(sys.argv[1])).get("allow_private_provider_urls") else "")
except Exception:
    print("")' "$INSTANCE_JSON")"
  ( cd "$REPO" && \
      PROXYLLM_SERVER_ADDR="127.0.0.1:$PORT" \
      PROXYLLM_STORAGE_PATH="$DB" \
      PROXYLLM_MASTER_KEY_PATH="$MASTER_KEY" \
      PROXYLLM_LOG_FORMAT=text \
      PROXYLLM_LOG_LEVEL="${PROXYLLM_LOG_LEVEL:-info}" \
      PROXYLLM_ALLOW_PRIVATE_PROVIDER_URLS="$allow_private" \
      setsid sh -c 'echo $$ > "$0"; exec "$1"' "$PID_FILE" "$BIN" \
      >>"$PROXY_LOG" 2>&1 </dev/null & )
  if wait_health 80; then
    log_info "started (pid $(cat "$PID_FILE")) on $BASE_URL"
  else
    log_error "start failed; log tail:"
    tail -n 30 "$PROXY_LOG" | redact >&2 || true
    return 1
  fi
}

instance_stop() {
  if ! instance_running; then
    log_info "not running"
    rm -f "$PID_FILE"
    return 0
  fi
  local pid
  pid="$(cat "$PID_FILE")"
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 40); do
    pid_alive "$pid" || break
    sleep 0.25
  done
  if pid_alive "$pid"; then
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
  log_info "stopped"
}

instance_status() {
  require_instance
  if instance_running && curl -sf -m 2 "$BASE_URL/healthz" >/dev/null 2>&1; then
    printf 'running (pid %s) on %s\n' "$(cat "$PID_FILE")" "$BASE_URL"
    printf 'home:   %s\n' "$INSTANCE_HOME"
    printf 'db:     %s\n' "$DB"
    printf 'binary: %s (%s)\n' "$BIN" "$(cut -c1-12 "$BIN.sha256" 2>/dev/null || echo unknown)"
    return 0
  fi
  printf 'not running\n'
  return 1
}

instance_logs() {
  require_instance
  local n="${1:-50}"
  [ -f "$PROXY_LOG" ] || die "no proxy log at $PROXY_LOG"
  tail -n "$n" "$PROXY_LOG" | redact
}

instance_doctor() {
  require_instance
  local failures=0
  check() {
    local label="$1"; shift
    if "$@" >/dev/null 2>&1; then log_info "ok   $label"; else log_error "FAIL $label"; failures=$((failures + 1)); fi
  }
  check "python3" command -v python3
  check "curl" command -v curl
  check "instance home 0700" test "$(stat -c '%a' "$INSTANCE_HOME")" = "700"
  check ".env 0600" test "$(stat -c '%a' "$ENV_FILE" 2>/dev/null || echo none)" = "600"
  check "instance.json" test -f "$INSTANCE_JSON"
  check "storage path" test -f "$DB"
  check "binary" test -x "$BIN"
  check "health" curl -sf -m 2 "$BASE_URL/healthz"
  if [ -f "$BIN.sha256" ]; then
    check "binary sha" test "$(sha256sum "$BIN" | awk '{print $1}')" = "$(cat "$BIN.sha256")"
  fi
  [ "$failures" -eq 0 ] || return 1
}

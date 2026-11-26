#!/usr/bin/env bash
# PROxyLLM CLI installer (ADR-024).
#
# Detects Claude Code, Codex and OpenCode, asks for the proxy URL and a client
# key, and writes each CLI's configuration. It never installs a CLI, never
# prints the key, and every change is reversible:
#
#   curl -fsSL https://raw.githubusercontent.com/TUBEOF/PROxyLLM/main/scripts/install.sh | bash
#   bash install.sh --dry-run     show the changes, write nothing
#   bash install.sh --uninstall    remove only what this script wrote
#
# Non-interactive:
#   PROXYLLM_URL=http://proxy:8080 PROXYLLM_API_KEY=pk_... \
#   PROXYLLM_CLIS=claude,codex bash install.sh --yes
#
# Requires bash and curl. JSON editing prefers python3, falls back to jq for
# reading, and refuses with a clear message when neither is present.
set -euo pipefail

readonly MARK_BEGIN="# managed by PROxyLLM: begin"
readonly MARK_END="# managed by PROxyLLM: end"
readonly PROXY_URL_DEFAULT="http://localhost:8080"

PROXY_URL="${PROXYLLM_URL:-}"
PROXY_KEY="${PROXYLLM_API_KEY:-}"
CLIS_ENV="${PROXYLLM_CLIS:-}"
DRY_RUN=0
UNINSTALL=0
ASSUME_YES=0
MODELS=""

log() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: install.sh [--dry-run] [--uninstall] [--yes]

  --dry-run    print the file changes instead of writing them
  --uninstall  remove the blocks this script wrote (backups are kept)
  --yes        accept defaults; use PROXYLLM_URL / PROXYLLM_API_KEY /
               PROXYLLM_CLIS for the answers

Environment: PROXYLLM_URL, PROXYLLM_API_KEY, PROXYLLM_CLIS (comma list).
EOF
}

for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --yes|-y) ASSUME_YES=1 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $arg (try --help)" ;;
  esac
done

# --- prerequisites and JSON helpers --------------------------------------

command -v curl >/dev/null 2>&1 || die "curl is required"
have_python=0
if command -v python3 >/dev/null 2>&1; then
  have_python=1
elif ! command -v jq >/dev/null 2>&1; then
  die "python3 or jq is required to read the proxy model list"
fi

json_ids() {
  if [ "$have_python" = 1 ]; then
    python3 -c 'import json,sys
d=json.load(sys.stdin)
for m in d.get("data", []):
    if isinstance(m, dict) and m.get("id"):
        print(m["id"])'
  else
    jq -r '.data[].id // empty'
  fi
}

# --- input --------------------------------------------------------------

ask_url() {
  if [ -n "$PROXY_URL" ]; then return; fi
  if [ "$ASSUME_YES" = 1 ]; then PROXY_URL="$PROXY_URL_DEFAULT"; return; fi
  printf 'Proxy URL [%s]: ' "$PROXY_URL_DEFAULT"
  read -r PROXY_URL
  PROXY_URL="${PROXY_URL:-$PROXY_URL_DEFAULT}"
}

ask_key() {
  if [ -n "$PROXY_KEY" ]; then return; fi
  [ "$ASSUME_YES" = 1 ] && die "PROXYLLM_API_KEY is required with --yes"
  printf 'Client API key (pk_...): '
  read -rs PROXY_KEY
  printf '\n'
  [ -n "$PROXY_KEY" ] || die "an API key is required"
}

probe() {
  local body
  body=$(printf 'header = "Authorization: Bearer %s"\n' "$PROXY_KEY" |
    curl -fsS --config - --max-time 10 "$PROXY_URL/v1/models") ||
    die "cannot reach $PROXY_URL/v1/models — check the URL, the key and that the proxy is running"
  MODELS=$(printf '%s' "$body" | json_ids)
  [ -n "$MODELS" ] || die "the proxy answered no models — create a model row in the console first"
}

# --- selection ----------------------------------------------------------

DETECTED=()
for cli in claude codex opencode; do
  if command -v "$cli" >/dev/null 2>&1; then DETECTED+=("$cli"); fi
done

SELECTED=()
select_clis() {
  if [ -n "$CLIS_ENV" ]; then
    IFS=',' read -r -a wanted <<<"$CLIS_ENV"
    for w in "${wanted[@]}"; do
      w="${w//[[:space:]]/}"
      [ -n "$w" ] || continue
      case "$w" in
        claude|codex|opencode) SELECTED+=("$w") ;;
        *) die "unknown CLI in PROXYLLM_CLIS: $w (claude, codex, opencode)" ;;
      esac
    done
    return
  fi
  if [ "${#DETECTED[@]}" -eq 0 ]; then
    log "No supported CLI found (claude, codex, opencode). Install one first:"
    log "  Claude Code  https://docs.anthropic.com/en/docs/claude-code"
    log "  Codex        npm install -g @openai/codex"
    log "  OpenCode     https://opencode.ai/docs"
    exit 0
  fi
  if [ "$ASSUME_YES" = 1 ]; then
    SELECTED=("${DETECTED[@]}")
    return
  fi
  log "Detected CLIs:"
  i=1
  for c in "${DETECTED[@]}"; do log "  $i) $c"; i=$((i + 1)); done
  log 'Select one or more (e.g. "1 3", "all", "none"):'
  read -r choice
  case "$choice" in
    all) SELECTED=("${DETECTED[@]}") ;;
    none|"") return ;;
    *)
      for n in $choice; do
        case "$n" in ''|*[!0-9]*) die "invalid selection: $n" ;; esac
        [ "$n" -ge 1 ] && [ "$n" -le "${#DETECTED[@]}" ] || die "selection out of range: $n"
        SELECTED+=("${DETECTED[$((n - 1))]}")
      done
      ;;
  esac
}

# --- model choice -------------------------------------------------------

pick_claude_model() {
  local m
  while IFS= read -r m; do
    case "$m" in
      *claude*|*Claude*|*anthropic*|*Anthropic*) printf '%s\n' "$m"; return ;;
    esac
  done <<<"$MODELS"
  printf '%s\n' "$MODELS" | head -n 1
}

pick_haiku_model() {
  local main="$1" m
  while IFS= read -r m; do
    [ "$m" = "$main" ] && continue
    case "$m" in
      *haiku*|*Haiku*|*flash*|*Flash*|*mini*|*Mini*|*small*|*Small*) printf '%s\n' "$m"; return ;;
    esac
  done <<<"$MODELS"
  printf '%s\n' "$MODELS" | head -n 1
}

choose_model() {
  local default="$1" m i n cand
  if [ "$ASSUME_YES" = 1 ] || [ -n "$CLIS_ENV" ]; then
    printf '%s\n' "$default"
    return
  fi
  log "Available models:" >&2
  i=1
  while IFS= read -r m; do log "  $i) $m" >&2; i=$((i + 1)); done <<<"$MODELS"
  printf 'Default model [%s]: ' "$default" >&2
  read -r m
  [ -n "$m" ] || { printf '%s\n' "$default"; return; }
  case "$m" in
    *[!0-9]*) printf '%s\n' "$m" ;;
    *)
      n=1
      while IFS= read -r cand; do
        if [ "$n" = "$m" ]; then printf '%s\n' "$cand"; return; fi
        n=$((n + 1))
      done <<<"$MODELS"
      printf '%s\n' "$default"
      ;;
  esac
}

# --- file application ---------------------------------------------------

TMPDIR_WORK=$(mktemp -d)
cleanup() { rm -rf "$TMPDIR_WORK"; }
trap cleanup EXIT

# propose TARGET TRANSFORM: TRANSFORM edits the temp copy in "$1"; dry-run
# diffs, a real run backs up and replaces.
propose() {
  local target="$1" transform="$2"
  local tmp
  tmp="$TMPDIR_WORK/$(basename "$target")"
  if [ -f "$target" ]; then cp "$target" "$tmp"; else : >"$tmp"; fi
  "$transform" "$tmp"
  if [ "$DRY_RUN" = 1 ]; then
    log "--- $target"
    if [ -f "$target" ]; then diff -u "$target" "$tmp" || true; else sed 's/^/+/' "$tmp"; fi
    return
  fi
  if [ -f "$target" ]; then
    cp -p "$target" "$target.bak.$(date +%s)"
  else
    mkdir -p "$(dirname "$target")"
  fi
  cp "$tmp" "$target"
  chmod 600 "$target" 2>/dev/null || true
  log "wrote $target"
}

# --- Claude Code --------------------------------------------------------

claude_settings() { printf '%s/.claude/settings.json' "$HOME"; }

claude_merge() {
  CLAUDE_MAIN_MODEL="$CLAUDE_MAIN" CLAUDE_HAIKU_MODEL="$CLAUDE_HAIKU" \
    ANTHROPIC_BASE_URL="$PROXY_URL" ANTHROPIC_AUTH_TOKEN="$PROXY_KEY" \
    python3 - "$1" <<'PY'
import json, os, sys
path = sys.argv[1]
try:
    text = open(path, encoding="utf-8").read()
except FileNotFoundError:
    text = ""
if not text.strip():
    data = {}
else:
    try:
        data = json.loads(text)
    except ValueError:
        sys.exit("error: existing settings.json is not valid JSON")
env = data.setdefault("env", {})
env.update({
    "ANTHROPIC_BASE_URL": os.environ["ANTHROPIC_BASE_URL"],
    "ANTHROPIC_AUTH_TOKEN": os.environ["ANTHROPIC_AUTH_TOKEN"],
    "ANTHROPIC_MODEL": os.environ["CLAUDE_MAIN_MODEL"],
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": os.environ["CLAUDE_HAIKU_MODEL"],
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
})
with open(path, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
PY
}

claude_unmerge() {
  python3 - "$1" <<'PY'
import json, sys
path = sys.argv[1]
try:
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
except (FileNotFoundError, ValueError):
    sys.exit(0)
env = data.get("env")
if isinstance(env, dict):
    for key in ("ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL",
                "ANTHROPIC_DEFAULT_HAIKU_MODEL",
                "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"):
        env.pop(key, None)
    if not env:
        data.pop("env", None)
with open(path, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
PY
}

install_claude() {
  if [ "$have_python" != 1 ]; then
    warn "Claude Code settings need python3 for the JSON merge; skipping"
    return
  fi
  CLAUDE_MAIN=$(pick_claude_model)
  CLAUDE_HAIKU=$(pick_haiku_model "$CLAUDE_MAIN")
  CLAUDE_MAIN=$(choose_model "$CLAUDE_MAIN")
  propose "$(claude_settings)" claude_merge
  log "Claude Code: model $CLAUDE_MAIN, small-fast $CLAUDE_HAIKU"
}

uninstall_claude() {
  local f
  f=$(claude_settings)
  [ -f "$f" ] || return 0
  [ "$have_python" = 1 ] || { warn "python3 required to edit $f"; return 0; }
  if [ "$DRY_RUN" = 1 ]; then log "--- would remove the PROxyLLM env keys from $f"; return; fi
  cp -p "$f" "$f.bak.$(date +%s)"
  claude_unmerge "$f"
  chmod 600 "$f" 2>/dev/null || true
  log "cleaned $f"
}

# --- Codex --------------------------------------------------------------

codex_config() { printf '%s/.codex/config.toml' "$HOME"; }

# codex_merge rewrites the file: managed keys first (TOML requires top-level
# keys before tables), then the provider table, then the feature switch.
codex_merge() {
  local file="$1" model="$CODEX_MODEL" effort="$CODEX_EFFORT" base="$PROXY_URL"
  local stripped="$file.stripped"
  awk -v model="$model" -v effort="$effort" -v base="$base" '
    BEGIN { managed = 0; provider = 0; features = 0 }
    /^# managed by PROxyLLM: begin$/ { managed = 1; next }
    /^# managed by PROxyLLM: end$/ { managed = 0; next }
    managed { next }
    /^\[model_providers\.proxyllm\]/ { provider = 1; next }
    provider && /^\[/ { provider = 0 }
    provider { next }
    /^api_key_model_discovery[[:space:]]*=/ { next }
    /^model[[:space:]]*=/ { next }
    /^model_provider[[:space:]]*=/ { next }
    /^model_reasoning_effort[[:space:]]*=/ { next }
    /^\[features\]/ {
      features = 1
      print
      print "api_key_model_discovery = true  # managed by PROxyLLM"
      next
    }
    /^\[/ { features = 0 }
    { print }
    END { if (!features) { print ""; print "[features]"; print "api_key_model_discovery = true  # managed by PROxyLLM" } }
  ' "$file" >"$stripped"

  {
    printf '%s\n' "$MARK_BEGIN"
    printf 'model = "%s"\n' "$model"
    printf 'model_provider = "proxyllm"\n'
    printf 'model_reasoning_effort = "%s"\n' "$effort"
    printf '%s\n\n' "$MARK_END"
    printf '[model_providers.proxyllm]\n'
    printf 'name = "PROxyLLM"\n'
    printf 'base_url = "%s/v1"\n' "$base"
    printf 'wire_api = "responses"\n'
    printf 'env_key = "PROXYLLM_API_KEY"\n'
    printf 'model_catalog_url = "%s/v1/model_catalog"\n' "$base"
    printf '\n'
    cat "$stripped"
  } >"$file.new"
  mv "$file.new" "$file"
  rm -f "$stripped"
}

codex_unmerge() {
  awk '
    BEGIN { managed = 0; provider = 0 }
    /^# managed by PROxyLLM: begin$/ { managed = 1; next }
    /^# managed by PROxyLLM: end$/ { managed = 0; next }
    managed { next }
    /^\[model_providers\.proxyllm\]/ { provider = 1; next }
    provider && /^\[/ { provider = 0 }
    provider { next }
    /managed by PROxyLLM/ { next }
    { print }
  ' "$1" >"$1.new" && mv "$1.new" "$1"
}

install_codex() {
  CODEX_MODEL=$(choose_model "$(printf '%s\n' "$MODELS" | head -n 1)")
  CODEX_EFFORT="medium"
  if [ "$ASSUME_YES" != 1 ] && [ -z "$CLIS_ENV" ]; then
    printf 'Codex reasoning effort [low/medium/high/xhigh] (medium): ' >&2
    read -r answer
    case "$answer" in low|medium|high|xhigh) CODEX_EFFORT="$answer" ;; esac
  fi
  propose "$(codex_config)" codex_merge
  log "Codex: model $CODEX_MODEL, effort $CODEX_EFFORT"
}

# --- OpenCode -----------------------------------------------------------

opencode_config() { printf '%s/.config/opencode/opencode.json' "$HOME"; }

opencode_merge() {
  OPENCODE_BASE="$PROXY_URL" OPENCODE_MODEL="$OPENCODE_MAIN" OPENCODE_MODELS="$MODELS" \
    python3 - "$1" <<'PY'
import json, os, sys
path = sys.argv[1]
try:
    text = open(path, encoding="utf-8").read()
except FileNotFoundError:
    text = ""
if not text.strip():
    data = {}
else:
    try:
        data = json.loads(text)
    except ValueError:
        sys.exit("error: existing opencode.json is not valid JSON")
providers = data.setdefault("provider", {})
models = {}
for name in os.environ["OPENCODE_MODELS"].splitlines()[:20]:
    if name:
        models[name] = {}
providers["proxyllm"] = {
    "npm": "@opencode-ai/openai-compatible",
    "name": "PROxyLLM",
    "options": {"baseURL": os.environ["OPENCODE_BASE"] + "/v1"},
    "models": models,
}
data["model"] = "proxyllm/" + os.environ["OPENCODE_MODEL"]
with open(path, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
PY
}

opencode_unmerge() {
  python3 - "$1" <<'PY'
import json, sys
path = sys.argv[1]
try:
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
except (FileNotFoundError, ValueError):
    sys.exit(0)
providers = data.get("provider")
if isinstance(providers, dict):
    providers.pop("proxyllm", None)
    if not providers:
        data.pop("provider", None)
model = data.get("model")
if isinstance(model, str) and model.startswith("proxyllm/"):
    data.pop("model", None)
with open(path, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
PY
}

install_opencode() {
  if [ "$have_python" != 1 ]; then
    warn "OpenCode config needs python3 for the JSON merge; skipping"
    return
  fi
  OPENCODE_MAIN=$(choose_model "$(printf '%s\n' "$MODELS" | head -n 1)")
  propose "$(opencode_config)" opencode_merge
  log "OpenCode: model proxyllm/$OPENCODE_MAIN"
  log "OpenCode: export PROXYLLM_API_KEY=<your pk key>, or run 'opencode auth login proxyllm'"
}

uninstall_opencode() {
  local f
  f=$(opencode_config)
  [ -f "$f" ] || return 0
  [ "$have_python" = 1 ] || { warn "python3 required to edit $f"; return 0; }
  if [ "$DRY_RUN" = 1 ]; then log "--- would remove the proxyllm provider from $f"; return; fi
  cp -p "$f" "$f.bak.$(date +%s)"
  opencode_unmerge "$f"
  chmod 600 "$f" 2>/dev/null || true
  log "cleaned $f"
}

# --- main ---------------------------------------------------------------

main() {
  if [ "$UNINSTALL" = 1 ]; then
    uninstall_claude
    if [ -f "$(codex_config)" ]; then
      if [ "$DRY_RUN" = 1 ]; then
        log "--- would remove the PROxyLLM blocks from $(codex_config)"
      else
        cp -p "$(codex_config)" "$(codex_config).bak.$(date +%s)"
        codex_unmerge "$(codex_config)"
        log "cleaned $(codex_config)"
      fi
    fi
    uninstall_opencode
    return
  fi

  ask_url
  PROXY_URL="${PROXY_URL%/}"
  ask_key
  probe
  select_clis

  if [ "${#SELECTED[@]}" -eq 0 ]; then log "Nothing selected."; return; fi
  if [ "$DRY_RUN" != 1 ] && [ "$ASSUME_YES" != 1 ] && [ -z "$CLIS_ENV" ]; then
    log "About to configure: ${SELECTED[*]}"
    printf 'Continue? [Y/n] '
    read -r ok
    case "$ok" in n|N|no|NO) log "aborted"; exit 0 ;; esac
  fi

  for cli in "${SELECTED[@]}"; do
    case "$cli" in
      claude) install_claude ;;
      codex) install_codex ;;
      opencode) install_opencode ;;
    esac
  done

  log ""
  log "Done. Start each CLI and check its first request in the PROxyLLM Logs page."
  case " ${SELECTED[*]} " in *" claude "*) log "  Claude Code: claude" ;; esac
  case " ${SELECTED[*]} " in *" codex "*) log "  Codex:       export PROXYLLM_API_KEY=<pk key>; codex" ;; esac
  case " ${SELECTED[*]} " in *" opencode "*) log "  OpenCode:    opencode" ;; esac
}

main

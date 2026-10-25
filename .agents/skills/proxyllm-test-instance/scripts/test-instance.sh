#!/usr/bin/env bash
# PROxyLLM local test instance — one entry point for the whole harness.
#
#   test-instance.sh init|start|stop|restart|status|logs|setup|dry-run|
#                    validate|test|report|clients|config|doctor|teardown
#
# Runtime state lives outside the repo (default ~/proxyllm-test-instance).
# Secrets live in <home>/.env (0600) and are never printed.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=lib/instance.sh
source "$SCRIPT_DIR/lib/instance.sh"

usage() {
  cat >&2 <<'EOF'
usage: test-instance.sh <command> [options] [--home PATH]

  --home PATH applies to any command (default ~/proxyllm-test-instance or
  $PROXYLLM_TEST_HOME).

  init       [--home PATH] [--key-file PATH | --prompt-key] [--port N]
             [--chat-model NAME] [--fallback-model NAME]
             [--allow-private-provider-urls] [--force]
  start      [--build] [--no-build] [--binary PATH]
  stop | restart | status
  logs       [N]
  setup      [--skip-probe] [--force-aliases]
  dry-run
  validate   [--skip-probe]
  test       [--phases 0,1,2,3,4,5,6] [--clients LIST] [--strict-upstream]
             [--timeout SECONDS] [--run-id ID] [--no-failover-injection]
  report     [--run-id ID]
  clients    list | probe NAME | install NAME --yes-install
  config     print
  doctor
  teardown   [--purge --yes]
EOF
}

cmd="${1:-}"
[ -n "$cmd" ] || { usage; exit 2; }
shift || true

# --home is global: consume it before dispatching to any command.
args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --home)
      [ $# -ge 2 ] || die "--home needs a path"
      PROXYLLM_TEST_HOME="$2"
      export PROXYLLM_TEST_HOME
      HOME_EXPLICIT=1
      set_instance_home "$2"
      shift 2
      ;;
    *)
      args+=("$1")
      shift
      ;;
  esac
done
set -- "${args[@]}"

case "$cmd" in
  init)
    with_lock instance_init "$@"
    ;;
  start)
    with_lock instance_start "$@"
    ;;
  stop)
    with_lock instance_stop
    ;;
  restart)
    with_lock instance_stop
    with_lock instance_start "$@"
    ;;
  status)
    instance_status || true
    ;;
  logs)
    instance_logs "${1:-50}"
    ;;
  setup)
    require_instance
    with_lock python3 -I "$SCRIPTS_DIR/admin.py" --home "$INSTANCE_HOME" setup "$@"
    ;;
  dry-run)
    require_instance
    python3 -I "$SCRIPTS_DIR/admin.py" --home "$INSTANCE_HOME" dry-run
    ;;
  validate)
    require_instance
    python3 -I "$SCRIPTS_DIR/admin.py" --home "$INSTANCE_HOME" validate "$@"
    ;;
  test)
    require_instance
    python3 -I "$SCRIPTS_DIR/test_pattern.py" --home "$INSTANCE_HOME" "$@"
    ;;
  report)
    require_instance
    run_id=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --run-id) run_id="$2"; shift 2 ;;
        *) die "report: unknown option $1" ;;
      esac
    done
    if [ -n "$run_id" ]; then
      dir="$INSTANCE_HOME/artifacts/$run_id"
    else
      dir="$(ls -1dt "$INSTANCE_HOME"/artifacts/* 2>/dev/null | head -1 || true)"
    fi
    [ -n "$dir" ] && [ -d "$dir" ] || die "no report found under $INSTANCE_HOME/artifacts"
    python3 -I "$SCRIPTS_DIR/report.py" --run-dir "$dir"
    ;;
  clients)
    python3 -I "$SCRIPTS_DIR/clients.py" "$@"
    ;;
  config)
    require_instance
    python3 -I "$SCRIPTS_DIR/admin.py" --home "$INSTANCE_HOME" print-config
    ;;
  doctor)
    instance_doctor
    ;;
  teardown)
    purge=0
    yes=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --purge) purge=1; shift ;;
        --yes) yes=1; shift ;;
        *) die "teardown: unknown option $1" ;;
      esac
    done
    with_lock instance_stop
    if [ "$purge" -eq 1 ]; then
      [ "$yes" -eq 1 ] || die "refusing purge without --yes"
      [ -f "$MARKER" ] || die "refusing purge: marker missing at $MARKER"
      [ "$INSTANCE_HOME" != "/" ] || die "refusing purge: instance home is /"
      [ "$INSTANCE_HOME" != "$HOME" ] || die "refusing purge: instance home is \$HOME"
      case "$INSTANCE_HOME" in
        "$HOME"/*) ;;
        *) die "refusing purge outside the home directory: $INSTANCE_HOME" ;;
      esac
      [ -d "$INSTANCE_HOME/data" ] || die "refusing purge: data dir missing"
      find "$INSTANCE_HOME" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
      rmdir "$INSTANCE_HOME" 2>/dev/null || true
      log_info "purged $INSTANCE_HOME"
    fi
    ;;
  *)
    usage
    exit 2
    ;;
esac

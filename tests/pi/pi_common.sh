#!/bin/bash
# Pi-specific test common: start_pi / stop_pi + isolated pi config.
# Mirrors tests/codex/codex_common.sh, but pi's startup is simpler: an interactive pi pane renders in
# ~3s with NO trust/hooks/update dialogs (verified 2026-08-08), so start_pi has no dialog handler — it
# only gates on session registration (the pi SessionStart payload landing in /session/list, MAJOR-3).
PI_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${PI_COMMON_DIR}/../e2e_common.sh"

# Resolve the pi binary once (operator's shell has nvm; the fallback is this machine's install path).
PI_BIN="${PI_BIN:-$(command -v pi 2>/dev/null || echo /home/seraphli/.nvm/versions/node/v24.11.0/bin/pi)}"
export PI_BIN

# require_pi_key: the E2E cannot run without the NewAPI token. Fail loud (never hardcode a key).
require_pi_key() {
  if [ -z "${NEWAPI_E2E_KEY:-}" ]; then
    fail "pi E2E requires NEWAPI_E2E_KEY (the NewAPI proxy token) exported in the environment — it is env-var only, never committed. Export it and re-run."
  fi
}

# ensure_pi_config: write the isolated pi config (models.json custom provider + empty auth.json) into
# PI_CODING_AGENT_DIR. models.json declares the `newapi` openai-completions provider; the apiKey is the
# LITERAL string "$NEWAPI_E2E_KEY" so pi interpolates it from the pane env at request time — the real key
# never lands on disk or in argv. auth.json is seeded empty so the key resolves purely via models.json.
ensure_pi_config() {
  mkdir -p "$PI_CODING_AGENT_DIR"
  PI_PROVIDER="$PI_E2E_PROVIDER" PI_MODEL="$PI_E2E_MODEL" PI_BASE_URL="$PI_E2E_BASE_URL" python3 -c '
import json, os
provider = os.environ["PI_PROVIDER"]
cfg = {"providers": {provider: {
    "baseUrl": os.environ["PI_BASE_URL"],
    "api": "openai-completions",
    "apiKey": "$NEWAPI_E2E_KEY",
    "models": [{
        "id": os.environ["PI_MODEL"],
        "name": os.environ["PI_MODEL"],
        "reasoning": False,
        "input": ["text"],
        "contextWindow": 65536,
        "maxTokens": 8192,
        "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
    }],
}}}
with open(os.path.join(os.environ["PI_CODING_AGENT_DIR"], "models.json"), "w") as f:
    json.dump(cfg, f, indent=2)
'
  [ -f "$PI_CODING_AGENT_DIR/auth.json" ] || printf '{}' > "$PI_CODING_AGENT_DIR/auth.json"
  # Round-1 context phase (phase10): configure a NON-default compaction.reserveTokens so the extension's
  # configured-reserve read path is observable. 32768 != the 16384 pi default, so phase10's
  # `context_total == contextWindow - 32768` assert fails loudly under a default fallback or raw-window denominator.
  printf '{"compaction":{"reserveTokens":32768}}' > "$PI_CODING_AGENT_DIR/settings.json"
}

# pi_api_idle: query /session/idle for the pi pane via the bot HTTP API (product IsSessionRunning, single
# source of truth). Echoes True/False. Mirrors codex_api_idle.
pi_api_idle() {
  local enc
  enc=$(printf '%s' "$E2E_PANE" | python3 -c "import sys,urllib.parse; print(urllib.parse.quote(sys.stdin.read()))")
  curl -sf "http://127.0.0.1:$TEST_PORT/session/idle?target=$enc" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('idle',False))" 2>/dev/null || echo "False"
}

# pi_session_registered: True iff /session/list contains a session whose target matches $1 (the pane).
# This is the pi readiness signal (MAJOR-3): the extension's SessionStart POST has been received.
pi_session_registered() {
  local pane="$1"
  curl -s "http://127.0.0.1:$TEST_PORT/session/list" 2>/dev/null | python3 -c '
import sys, json
pane = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    print("False"); sys.exit(0)
for s in d.get("sessions", []):
    t = s.get("target", "")
    if t == pane or t.startswith(pane.split("@")[0] + "@"):
        print("True"); sys.exit(0)
print("False")
' "$pane" 2>/dev/null || echo "False"
}

# build_pi_launch: echo the exact command string to type into the pi pane. NO -e (the extension is
# globally auto-discovered from PI_CODING_AGENT_DIR/extensions). $1 = session-dir (empty for a plain/hand
# launch, per SPEC §6: getSessionFile() returns an absolute path even without --session-dir).
build_pi_launch() {
  local session_dir="${1:-}"
  local -a argv=("$PI_BIN" --provider "$PI_E2E_PROVIDER" --model "$PI_E2E_MODEL")
  if [ -n "$session_dir" ]; then
    argv+=(--session-dir "$session_dir")
  fi
  local q
  printf -v q '%q ' "${argv[@]}"
  printf '%s' "$q"
}

# _launch_pi_pane: create an isolated tmux session, export the pi env into it via `-e` (so the NewAPI
# token never appears in argv), and start pi. Sets E2E_SESSION / E2E_PANE. $1=session name,
# $2=session-dir (empty for hand launch). PI_OFFLINE=1 skips the one-time fd download so startup is
# deterministic (verified: "fd not found. Offline mode enabled, skipping download.", no hang).
_launch_pi_pane() {
  local session_name="$1"
  local session_dir="${2:-}"
  E2E_SESSION="$session_name"
  export E2E_SESSION
  rm -f "$TEST_CONFIG_DIR/sessions.json" 2>/dev/null || true
  $TMUX_TEST kill-session -t "=$E2E_SESSION" 2>/dev/null || true
  $TMUX_TEST new-session -d -s "$E2E_SESSION" -x 220 -y 50 \
    -e "NEWAPI_E2E_KEY=$NEWAPI_E2E_KEY" \
    -e "PI_CODING_AGENT_DIR=$PI_CODING_AGENT_DIR" \
    -e "PI_OFFLINE=1"
  E2E_PANE=$($TMUX_TEST list-panes -t "$E2E_SESSION" -F '#{pane_id}@#{socket_path}')
  export E2E_PANE
  $TMUX_TEST send-keys -t "$E2E_SESSION" "$(build_pi_launch "$session_dir")"
  sleep 1
  $TMUX_TEST send-keys -t "$E2E_SESSION" Enter
}

# start_pi: launch pi through the harness and gate on session registration, then a warmup turn.
#   $1 = session name (default $E2E_SESSION)
#   $2 = install cleanup trap (default true)
# The readiness gate keys on /session/list registration (MAJOR-3 / phase g), NOT the idle API: an
# unregistered pi pane and an idle one are wire-identical on /session/idle.
start_pi() {
  local session_name="${1:-$E2E_SESSION}"
  local install_trap="${2:-true}"
  require_pi_key
  ensure_pi_config
  local session_dir="$TEST_CONFIG_DIR/pi-sessions/$session_name"
  mkdir -p "$session_dir"
  _launch_pi_pane "$session_name" "$session_dir"
  echo "Waiting for pi to start and register (session dir: $session_dir)..."
  local log_before_reg
  log_before_reg=$(wc -l < "$LOG_FILE" 2>/dev/null || echo 0)
  local elapsed=0
  local registered=false
  while [ $elapsed -lt 90 ]; do
    if [ "$(pi_session_registered "$E2E_PANE")" = "True" ]; then
      registered=true
      echo "pi session registered in /session/list at t=$elapsed"
      break
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  pane_log "[start_pi] after registration wait (registered=$registered t=$elapsed)"
  if [ "$registered" != "true" ]; then
    echo "WARN: pi session registration not detected within 90s"
  fi
  # Warmup: one full turn so the session is proven idle-capable before the phase's real asserts, and so a
  # subsequent inject cannot race startup. Wait on the store-driven idle API (SetIdle fires at Stop).
  echo "pi warmup: injecting warmup prompt..."
  inject_prompt "Reply with exactly one word: ready"
  wait_for_idle "$TIMEOUT" "$E2E_PANE"
  pane_log "[start_pi] after warmup"
  _PI_PHASE_SESSION="$E2E_SESSION"
  if [ "$install_trap" = "true" ]; then
    trap '_pi_phase_cleanup' EXIT
  fi
}

stop_pi() {
  local session_name="${1:-$E2E_SESSION}"
  # pi TUI: ctrl+c clears, ctrl+d exits on empty input. Send both, then kill-session as a backstop.
  if $TMUX_TEST has-session -t "=$session_name" 2>/dev/null; then
    $TMUX_TEST send-keys -t "$session_name" C-c 2>/dev/null || true
    sleep 1
    $TMUX_TEST send-keys -t "$session_name" C-d 2>/dev/null || true
    sleep 2
    $TMUX_TEST kill-session -t "=$session_name" 2>/dev/null || true
  fi
  E2E_PANE=""
}

_pi_phase_cleanup() {
  local rc=$?
  if [ -z "${_PI_PHASE_SESSION:-}" ]; then
    if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
      cleanup_sessions 2>/dev/null || true
    fi
    return $rc
  fi
  if [ $rc -eq 0 ]; then
    stop_pi "$_PI_PHASE_SESSION"
  else
    echo "  [cleanup] pi phase exited with rc=$rc, capturing pane and killing session $_PI_PHASE_SESSION"
    pane_log "[cleanup] abnormal exit rc=$rc - $_PI_PHASE_SESSION"
    $TMUX_TEST kill-session -t "=$_PI_PHASE_SESSION" 2>/dev/null || true
    E2E_PANE=""
  fi
  _PI_PHASE_SESSION=""
  if [ "${E2E_ORCHESTRATED:-}" != "1" ]; then
    cleanup_sessions 2>/dev/null || true
  fi
  exit $rc
}

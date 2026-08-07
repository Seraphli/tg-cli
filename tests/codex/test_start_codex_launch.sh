#!/bin/bash
# Deterministic test for build_codex_launch (codex custom-model launch string), exercising the FULL
# shell/tmux send-keys quoting boundary with a fake `codex` argv-recorder. No network, no real codex.
# Covers: all-3-set (custom provider argv incl name/base_url/wire_api; key via ENV inheritance, NOT argv);
# zero vars (default, no warning); all 6 partial combos (default + warning); metacharacter preservation.
set -uo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# A dummy sentinel key — the fake recorder only records present/absent, NEVER the value, and the real
# token is never involved here.
DUMMY_KEY='SENTINEL_KEY_abc123_DO_NOT_USE'

# Source build_codex_launch (pulls in e2e_common.sh setup; isolate its side effects to a throwaway run id).
export E2E_RUN_ID="codexlaunchtest-$$"
source "$DIR/codex_common.sh"

WORK="$(mktemp -d)"
FAKE_TMUX="codex-launch-test-$$"
trap 'tmux -L "$FAKE_TMUX" kill-server 2>/dev/null || true; rm -rf "$WORK" "${TEST_CONFIG_DIR:-}" 2>/dev/null || true' EXIT

mkdir -p "$WORK/bin"
cat > "$WORK/bin/codex" <<'FAKE'
#!/bin/bash
printf '%s\n' "$@" > "$ARGV_OUT"
if [ -n "${CODEX_API_KEY:-}" ]; then echo present > "$KEY_OUT"; else echo absent > "$KEY_OUT"; fi
printf '%s\n' "${CODEX_HOME:-}" > "$HOME_OUT"
FAKE
chmod +x "$WORK/bin/codex"

# Fake codex must win PATH resolution in every pane (servers inherit this env at creation).
export PATH="$WORK/bin:$PATH"
export ARGV_OUT="$WORK/argv" KEY_OUT="$WORK/key" HOME_OUT="$WORK/home"
export CODEX_HOME="$WORK/codex-home"; mkdir -p "$CODEX_HOME"

pass=0; fail=0
ok()   { echo "  PASS: $1"; pass=$((pass+1)); }
bad()  { echo "  FAIL: $1"; fail=$((fail+1)); }
has()  { case "$2" in *"$3"*) ok "$1";; *) bad "$1 (missing [$3])";; esac; }
hasnt(){ case "$2" in *"$3"*) bad "$1 (found [$3])";; *) ok "$1";; esac; }
eq()   { [ "$2" = "$3" ] && ok "$1" || bad "$1 (got [$2] want [$3])"; }

# run_launch: correction #3 sequencing — the CODEX_* env is exported by the CALLER, THEN we create a fresh
# unique tmux server (which inherits that env), THEN generate+send the command. Proves inheritance is real,
# not accidental. Populates: LAUNCH (string), ARGV (recorded, newline-joined), KEY (present/absent),
# RCV_HOME (CODEX_HOME the fake saw), WARN (stderr from build_codex_launch).
run_launch() {
  rm -f "$ARGV_OUT" "$KEY_OUT" "$HOME_OUT" "$WORK/warn"
  LAUNCH="$(build_codex_launch 2>"$WORK/warn")"
  tmux -L "$FAKE_TMUX" kill-server 2>/dev/null || true
  tmux -L "$FAKE_TMUX" new-session -d -s s "bash --norc --noprofile"
  tmux -L "$FAKE_TMUX" send-keys -t s "$LAUNCH"; tmux -L "$FAKE_TMUX" send-keys -t s Enter
  local i
  for i in $(seq 1 60); do [ -f "$ARGV_OUT" ] && break; sleep 0.1; done
  tmux -L "$FAKE_TMUX" kill-server 2>/dev/null || true
  ARGV="$(cat "$ARGV_OUT" 2>/dev/null || true)"
  KEY="$(cat "$KEY_OUT" 2>/dev/null || true)"
  RCV_HOME="$(cat "$HOME_OUT" 2>/dev/null || true)"
  WARN="$(cat "$WORK/warn" 2>/dev/null || true)"
}

echo "== Case 1: all 3 set → custom provider argv; key via ENV, not argv =="
export CODEX_MODEL='grok-4.5' CODEX_BASE_URL='https://newapi.seraphli.eu.cc/v1' CODEX_API_KEY="$DUMMY_KEY"
run_launch
has  "argv has --model grok-4.5"           "$ARGV" $'--model\ngrok-4.5'
has  "argv sets custom provider"           "$ARGV" 'model_provider="custom"'
has  "argv sets provider name (required)"  "$ARGV" 'model_providers.custom.name="Custom E2E"'
has  "argv sets base_url (quoted TOML)"     "$ARGV" 'model_providers.custom.base_url="https://newapi.seraphli.eu.cc/v1"'
has  "argv sets env_key"                    "$ARGV" 'model_providers.custom.env_key="CODEX_API_KEY"'
has  "argv sets wire_api=responses"         "$ARGV" 'model_providers.custom.wire_api="responses"'
has  "argv keeps --yolo --enable hooks"     "$ARGV" $'--yolo\n--enable\nhooks'
eq   "API key present in codex ENV (inherited)" "$KEY" "present"
hasnt "API key value ABSENT from argv"      "$ARGV" "$DUMMY_KEY"
eq   "CODEX_HOME received by codex"         "$RCV_HOME" "$CODEX_HOME"
eq   "no warning when all 3 set"            "$WARN" ""

echo "== Case 2: zero vars → default launch, NO warning =="
unset CODEX_MODEL CODEX_BASE_URL CODEX_API_KEY
run_launch
eq   "default argv (no --model/-c)"         "$ARGV" $'--yolo\n--enable\nhooks'
hasnt "no --model in default"               "$ARGV" "--model"
hasnt "no -c in default"                    "$ARGV" "-c"
eq   "zero vars → NO warning"               "$WARN" ""

echo "== Case 3: all 6 partial combos → default + warning =="
for combo in M B K MB MK BK; do
  unset CODEX_MODEL CODEX_BASE_URL CODEX_API_KEY
  case "$combo" in
    *M*) export CODEX_MODEL='grok-4.5' ;; esac
  case "$combo" in
    *B*) export CODEX_BASE_URL='https://x/v1' ;; esac
  case "$combo" in
    *K*) export CODEX_API_KEY="$DUMMY_KEY" ;; esac
  run_launch
  eq  "partial[$combo] default argv"        "$ARGV" $'--yolo\n--enable\nhooks'
  [ -n "$WARN" ] && ok "partial[$combo] warning emitted" || bad "partial[$combo] warning missing"
done

echo "== Case 4: metacharacters in model/URL preserved byte-exact =="
unset CODEX_MODEL CODEX_BASE_URL CODEX_API_KEY
export CODEX_MODEL='meta model"x' CODEX_BASE_URL='https://h.example/v1?a=1&b="q"' CODEX_API_KEY="$DUMMY_KEY"
run_launch
has  "model metachar preserved"             "$ARGV" 'meta model"x'
# json.dumps escapes the inner quotes → TOML basic string; codex receives the value with escaped quotes.
has  "URL metachar preserved (json-escaped quotes)" "$ARGV" 'model_providers.custom.base_url="https://h.example/v1?a=1&b=\"q\""'
hasnt "API key value still absent from argv" "$ARGV" "$DUMMY_KEY"

echo ""
echo "RESULT: $pass PASS / $fail FAIL"
[ "$fail" -eq 0 ]

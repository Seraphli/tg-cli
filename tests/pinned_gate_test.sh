#!/bin/bash
# codexcheck#6 deterministic negative control for the SER-62 S2 pinned-tool gate.
# Standalone (NOT part of the normal e2e run path). Two DISTINCT, deterministic cases — no network
# (npm is stubbed via PATH), everything isolated under a throwaway temp dir so the real
# $HOME/.tg-cli-test is never touched:
#   Case A (fail):    a wrong-version binary is pre-placed AND npm is a NO-OP, so the repair cannot fix
#                     it -> the gate prints `[pinned-versions] <tool> expected=<pin> got=<actual>` to
#                     STDERR and exits 1.
#   Case B (proceed): npm is a WORKING stub that drops correct-version binaries, so the gate installs
#                     all three pins and PROCEEDS (exit 0, no `[pinned-versions]` error line).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HELPER="$SCRIPT_DIR/pinned_tools.sh"

# Load the pins so the test asserts against the same numbers the gate uses.
source "$SCRIPT_DIR/pinned-tool-versions.env"

# Everything lives under one temp dir; the trap removes it (nothing real is touched).
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/pinned-gate-test.XXXXXX")"
trap 'rm -rf "$TMP_ROOT"' EXIT

FAILURES=0
report() {  # report <PASS|FAIL> <message>
  echo "  $1: $2"
  [ "$1" = "FAIL" ] && FAILURES=$((FAILURES + 1))
  return 0
}

# write_fake_tool <path> <version-line> — a fake tool binary whose `--version` echoes <version-line>.
write_fake_tool() {
  local path="$1" line="$2"
  mkdir -p "$(dirname "$path")"
  cat > "$path" <<EOF
#!/bin/bash
[ "\$1" = "--version" ] && echo "$line"
exit 0
EOF
  chmod +x "$path"
}

# run_gate <home> <prefix> <npm_stub_dir> — drive ensure_pinned_tools in a clean bash subprocess with
# the pinned prefix, temp HOME, and stubbed npm on PATH. Echoes the gate's STDERR; exit code propagates.
run_gate() {
  local home="$1" prefix="$2" npmdir="$3"
  HOME="$home" PINNED_PREFIX="$prefix" PATH="$npmdir:$PATH" \
    bash -c 'source "'"$HELPER"'"; ensure_pinned_tools' 2>&1 >/dev/null
}

# --- Case A: wrong binary + NO-OP npm -> gate fails deterministically ---
caseA() {
  local home="$TMP_ROOT/A/home" prefix="$TMP_ROOT/A/prefix" npmdir="$TMP_ROOT/A/npm"
  mkdir -p "$home" "$npmdir"
  # Pre-place a WRONG-version claude (claude is checked first, so the gate fails here) — strips to 9.9.9.
  write_fake_tool "$prefix/bin/claude" "9.9.9 (fake)"
  # NO-OP npm: install "succeeds" but changes nothing, so the wrong version survives the repair attempt.
  cat > "$npmdir/npm" <<'EOF'
#!/bin/bash
exit 0
EOF
  chmod +x "$npmdir/npm"
  local stderr rc
  stderr=$(run_gate "$home" "$prefix" "$npmdir"); rc=$?
  local want="[pinned-versions] claude expected=$CLAUDE_CODE_VERSION got=9.9.9"
  if [ "$rc" -eq 1 ] && grep -qF "$want" <<< "$stderr"; then
    report PASS "Case A: gate failed (exit 1) with expected stderr line: $want"
  else
    report FAIL "Case A: rc=$rc stderr=<<<$stderr>>> (wanted exit 1 + line: $want)"
  fi
}

# --- Case B: working npm drops correct binaries -> gate proceeds ---
caseB() {
  local home="$TMP_ROOT/B/home" prefix="$TMP_ROOT/B/prefix" npmdir="$TMP_ROOT/B/npm"
  mkdir -p "$home" "$prefix/bin" "$npmdir"
  # WORKING npm stub: parse `install -g --prefix <prefix> <pkg>@<ver>`, map pkg->tool, then drop a
  # correct-version fake binary in the per-tool --version format the gate's strip expects.
  cat > "$npmdir/npm" <<'EOF'
#!/bin/bash
prefix=""; spec=""
while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) prefix="$2"; shift 2 ;;
    install|-g) shift ;;
    *) spec="$1"; shift ;;
  esac
done
pkg="${spec%@*}"; ver="${spec##*@}"
case "$pkg" in
  @anthropic-ai/claude-code)       tool=claude; line="$ver (Claude Code)" ;;
  @earendil-works/pi-coding-agent) tool=pi;     line="$ver" ;;
  @openai/codex)                   tool=codex;  line="codex-cli $ver" ;;
  *) exit 0 ;;
esac
mkdir -p "$prefix/bin"
cat > "$prefix/bin/$tool" <<INNER
#!/bin/bash
[ "\$1" = "--version" ] && echo "$line"
exit 0
INNER
chmod +x "$prefix/bin/$tool"
exit 0
EOF
  chmod +x "$npmdir/npm"
  local stderr rc
  stderr=$(run_gate "$home" "$prefix" "$npmdir"); rc=$?
  if [ "$rc" -eq 0 ] && ! grep -q '^\[pinned-versions\]' <<< "$stderr"; then
    report PASS "Case B: gate proceeded (exit 0, no [pinned-versions] error line)"
  else
    report FAIL "Case B: rc=$rc stderr=<<<$stderr>>> (wanted exit 0 + no error line)"
  fi
}

echo "=== pinned-tool gate negative control ==="
caseA
caseB
echo ""
if [ "$FAILURES" -eq 0 ]; then
  echo "pinned_gate_test PASSED (2/2)"
  exit 0
else
  echo "pinned_gate_test FAILED ($FAILURES failing)"
  exit 1
fi

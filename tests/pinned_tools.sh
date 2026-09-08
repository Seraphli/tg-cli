#!/bin/bash
# Shared pinned-tool gate helper (SER-62 S2). Sourced by tests/e2e.sh (the live gate) and by
# tests/pinned_gate_test.sh (the codexcheck#6 deterministic negative control). Everything the test
# needs to override is env-driven: PINNED_PREFIX (where the pinned tools install), HOME (the default
# prefix root), PINNED_VERSIONS_ENV (the pins file), and PINNED_NPM (the install command — default
# `npm`, resolved via PATH so a test can stub it). Nothing here writes outside PINNED_PREFIX, so a test
# pointing PINNED_PREFIX/HOME at a temp dir touches nothing real.

# Guard against double-sourcing.
[ -n "${_PINNED_TOOLS_LOADED:-}" ] && return 0
_PINNED_TOOLS_LOADED=1

_PINNED_TOOLS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Pins: CLAUDE_CODE_VERSION / PI_VERSION / CODEX_VERSION. Overridable file via PINNED_VERSIONS_ENV.
source "${PINNED_VERSIONS_ENV:-$_PINNED_TOOLS_DIR/pinned-tool-versions.env}"

# Where the pinned tools live. Overridable (the negative control points this at a temp dir).
PINNED_PREFIX="${PINNED_PREFIX:-$HOME/.tg-cli-test/tools/pinned}"

# Strip a tool's `--version` output down to its bare version string. The three tools format --version
# differently: claude -> first whitespace field; codex -> second field; pi -> the whole trimmed line.
_pinned_strip_version() {
  local tool="$1" raw="$2"
  case "$tool" in
    claude) awk 'NR==1{print $1; exit}' <<< "$raw" ;;
    codex)  awk 'NR==1{print $2; exit}' <<< "$raw" ;;
    pi)     awk 'NR==1{gsub(/^[ \t]+|[ \t]+$/,""); print; exit}' <<< "$raw" ;;
  esac
}

# Record a MAIN tool's resolved path + mtime (via command -v) into namespaced globals, so
# verify_main_untouched can later confirm the user's main install was not modified. Must run BEFORE
# PINNED_PREFIX goes on PATH (the caller guarantees this) so command -v resolves the real install.
_pinned_record_main() {
  local tool="$1" p m=""
  p=$(command -v "$tool" 2>/dev/null || true)
  if [ -n "$p" ]; then m=$(stat -c %Y "$p" 2>/dev/null || true); fi
  printf -v "_PINNED_MAIN_PATH_$tool" '%s' "$p"
  printf -v "_PINNED_MAIN_MTIME_$tool" '%s' "$m"
}

# Install (if needed) and verify one pinned tool. Installs into PINNED_PREFIX via PINNED_NPM (default
# npm) when the pinned binary is missing or off-pin, then RE-VERIFIES. If it is STILL off-pin after the
# install attempt, emit the machine-readable line to STDERR and return 1 (fail() in e2e_common.sh only
# echoes to stdout, so the gate must write this line to >&2 itself).
_pinned_check_one() {
  local tool="$1" pkg="$2" pin="$3"
  local bin="$PINNED_PREFIX/bin/$tool"
  local raw="" stripped="" need_install=0
  if [ ! -x "$bin" ]; then
    need_install=1
  else
    raw=$("$bin" --version 2>/dev/null || true)
    stripped=$(_pinned_strip_version "$tool" "$raw")
    if [ "$stripped" != "$pin" ]; then need_install=1; fi
  fi
  if [ "$need_install" -eq 1 ]; then
    "${PINNED_NPM:-npm}" install -g --prefix "$PINNED_PREFIX" "$pkg@$pin" || true
    raw=$("$bin" --version 2>/dev/null || true)
    stripped=$(_pinned_strip_version "$tool" "$raw")
  fi
  if [ "$stripped" != "$pin" ]; then
    echo "[pinned-versions] $tool expected=$pin got=$stripped" >&2
    return 1
  fi
  return 0
}

# The gate: record the main installs, then install/verify all three pinned backends. On any version
# mismatch that survives the install attempt, _pinned_check_one already wrote the stderr line, so exit 1.
ensure_pinned_tools() {
  _pinned_record_main claude
  _pinned_record_main pi
  _pinned_record_main codex
  _pinned_check_one claude "@anthropic-ai/claude-code"       "$CLAUDE_CODE_VERSION" || exit 1
  _pinned_check_one pi     "@earendil-works/pi-coding-agent" "$PI_VERSION"          || exit 1
  _pinned_check_one codex  "@openai/codex"                   "$CODEX_VERSION"        || exit 1
}

# Re-check one main install recorded by ensure_pinned_tools: the file at the recorded path must still
# exist with the same mtime. A change means the gate/install touched the user's main install -> fail.
_pinned_verify_main() {
  local tool="$1"
  local pvar="_PINNED_MAIN_PATH_$tool" mvar="_PINNED_MAIN_MTIME_$tool"
  local old_path="${!pvar:-}" old_mtime="${!mvar:-}"
  [ -z "$old_path" ] && return 0
  local now_mtime=""
  now_mtime=$(stat -c %Y "$old_path" 2>/dev/null || true)
  if [ "$now_mtime" != "$old_mtime" ]; then
    echo "[pinned-versions] $tool main-install modified at $old_path (mtime was $old_mtime, now ${now_mtime:-missing})" >&2
    exit 1
  fi
}

verify_main_untouched() {
  _pinned_verify_main claude
  _pinned_verify_main pi
  _pinned_verify_main codex
}

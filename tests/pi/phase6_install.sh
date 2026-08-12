#!/bin/bash
# Phase 6 = SPEC (f): `tg-cli install` registers the pi extension into a TEST pi config dir, and
# `tg-cli install --uninstall` removes it (file present then absent; port substituted; idempotent; the
# extensions/ dir survives uninstall). Uses a dedicated isolated dir so the running suite's pi dir is
# untouched. This is the E2E of the product install path (TC-install covers the Go unit level).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/pi_common.sh"

echo ""
echo "--- pi (f) install / uninstall lifecycle ---"

ensure_infrastructure

FDIR="$TEST_CONFIG_DIR/pi-ftest"
rm -rf "$FDIR"
EXT="$FDIR/extensions/tg-cli.ts"

pi_install() {
  PI_CODING_AGENT_DIR="$FDIR" ./tg-cli --config-dir "$TEST_CONFIG_DIR" install --port "$TEST_PORT" \
    --settings "$TEST_SETTINGS" --skip-tmux > /dev/null 2>&1
}
pi_uninstall() {
  PI_CODING_AGENT_DIR="$FDIR" ./tg-cli --config-dir "$TEST_CONFIG_DIR" install --uninstall \
    --settings "$TEST_SETTINGS" --skip-tmux > /dev/null 2>&1
}

# (f.1) fresh install into a dir with no extensions/ subdir creates the dir + writes tg-cli.ts.
pi_install
if [ -f "$EXT" ] && [ -d "$FDIR/extensions" ]; then
  pass "pi (f): install created extensions/tg-cli.ts in the test pi dir"
else
  fail "pi (f): install did not create $EXT"
fi

# (f.2) the written file has the substituted bot port and NO leftover __HOOK_PORT__ placeholder.
set +eo pipefail
grep -F "$TEST_PORT" "$EXT" > /dev/null 2>&1
_ps_port=("${PIPESTATUS[@]}")
grep -F "__HOOK_PORT__" "$EXT" > /dev/null 2>&1
_ps_ph=("${PIPESTATUS[@]}")
set -eo pipefail
if [ "${_ps_port[0]}" -eq 0 ] && [ "${_ps_ph[0]}" -ne 0 ]; then
  pass "pi (f): extension carries the substituted port ($TEST_PORT) and no __HOOK_PORT__ placeholder"
else
  fail "pi (f): port substitution wrong (port_found=${_ps_port[0]} placeholder_found=${_ps_ph[0]})"
fi

# (f.3) idempotent: re-install with the same port is byte-identical.
cp "$EXT" "$TEST_CONFIG_DIR/pi-ext-snapshot.ts"
pi_install
if cmp -s "$EXT" "$TEST_CONFIG_DIR/pi-ext-snapshot.ts"; then
  pass "pi (f): re-install is byte-idempotent (same port -> identical bytes)"
else
  fail "pi (f): re-install produced different bytes (not idempotent)"
fi

# (f.4) uninstall removes ONLY the file; the extensions/ dir survives.
pi_uninstall
if [ ! -f "$EXT" ] && [ -d "$FDIR/extensions" ]; then
  pass "pi (f): uninstall removed tg-cli.ts and left the extensions/ dir intact"
else
  fail "pi (f): uninstall wrong (file_present=$([ -f "$EXT" ] && echo yes || echo no) dir_present=$([ -d "$FDIR/extensions" ] && echo yes || echo no))"
fi

echo "  pi (f) install/uninstall test complete."

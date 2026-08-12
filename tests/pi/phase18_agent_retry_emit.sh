#!/bin/bash
# Phase 18 = Round-4 Item 7 (extension half): the pi extension must POST agent_retry ONLY when the previous run
# of the SAME turn ended in a retryable provider error and pi is auto-continuing that turn — so the Go side can
# mark the already-rendered (truncated) bubble interrupted-and-retrying. The detection lives entirely in
# cmd/config/pi-extension.ts (agent_start captures the previous run's stopReason + turn BEFORE the reset;
# lastRunTurnId set in agent_end distinguishes a same-turn retry from a new turn after a retries-exhausted error).
#
# FULLY SYNTHETIC + DETERMINISTIC — no bot, no live pi, no network. It loads the REAL extension via node's native
# TypeScript support with a mock `pi` (captures the event handlers) and a mock `fetch` (records the POSTs), then
# drives event sequences and asserts which POSTs carry event=agent_retry. This exercises the exact production
# detection gate; the pi/cc E2E suites cover the end-to-end TG edit.
#   S1 retry:        input(t1) agent_start agent_end(error) agent_start  -> exactly one agent_retry (turn t1, error carried)
#   S2 exhausted:    input(t1) agent_start agent_end(error) input(t2) agent_start -> NO agent_retry (turn advanced)
#   S3 normal:       input agent_start agent_end(stop) input agent_start -> NO agent_retry
#   S4 aborted:      input agent_start agent_end(aborted) agent_start    -> NO agent_retry (ESC abort is not a retry)
#   S5 overflow:     input(t1) agent_start agent_end(error) session_compact(overflow) agent_start -> NO agent_retry
#                    (context overflow ARRIVES as stopReason=error and re-runs the SAME turn after compaction, but pi
#                    does NOT treat it as a retry — _isRetryableError returns false for isContextOverflow. The
#                    extension's session_compact discriminator must suppress the mark for a compaction-driven re-run.)
# RED (revert the `if (prevErrorRetry) post("agent_retry", ...)` emission in the extension): S1 fails (zero
# agent_retry). RED (before the session_compact discriminator is added): S5 fails (the compaction re-run wrongly
# emits agent_retry). GREEN (current): all five scenarios pass.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/../e2e_common.sh"

echo ""
echo "--- pi Item7: extension POSTs agent_retry only on a same-turn retry-after-error (synthetic) ---"

EXT_SRC="${SCRIPT_DIR}/../../cmd/config/pi-extension.ts"
if ! command -v node >/dev/null 2>&1; then
  record_fail "phase18: node not found (required to load the pi extension)"
  echo "--- pi Item7 agent_retry emission phase complete ---"
  exit 0
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/phase18-retry-XXXXXX")
cleanup18() { rm -rf "$WORK" 2>/dev/null || true; }
trap cleanup18 EXIT

# The extension carries a literal __HOOK_PORT__ placeholder (substituted at install). Fill it so node can load it.
sed 's/__HOOK_PORT__/12500/' "$EXT_SRC" > "$WORK/pi-ext.mts"

cat > "$WORK/harness.mjs" <<'HARNESS'
const mod = await import(process.argv[2]);
const setup = mod.default;
let fails = 0;
function check(cond, msg) { if (!cond) { console.log("FAIL: " + msg); fails++; } else { console.log("ok: " + msg); } }
const ctx = {
  sessionManager: { getCwd: () => "/home/seraphli/proj", getSessionId: () => "sess-1", getSessionFile: () => "/tmp/sess-1.jsonl" },
  getContextUsage: () => ({ tokens: 100, contextWindow: 200000 }),
  isProjectTrusted: () => false,
};
function newInstance() {
  const posts = [];
  globalThis.fetch = (url, opts) => { posts.push({ event: url.split("/hook/")[1], body: JSON.parse(opts.body) }); return Promise.resolve({ ok: true }); };
  const h = {};
  setup({ on: (e, f) => { h[e] = f; } });
  return { posts, fire: async (e, ev) => { if (h[e]) await h[e](ev || {}, ctx); } };
}
const retriesOf = (posts) => posts.filter((p) => p.event === "agent_retry");

// S1 — retryable error then SAME-turn retry -> exactly one agent_retry (turn t1, carrying the error).
{
  const { posts, fire } = newInstance();
  await fire("input", { text: "do X" });
  await fire("agent_start", {});
  await fire("agent_end", { messages: [{ role: "assistant", stopReason: "error", errorMessage: "overloaded (429)" }] });
  await fire("agent_start", {});
  const r = retriesOf(posts);
  check(r.length === 1, "S1 retry: exactly one agent_retry (got " + r.length + ")");
  if (r.length === 1) {
    check(r[0].body.turn_id === "t1", "S1 retry: agent_retry turn_id=t1 (got " + r[0].body.turn_id + ")");
    check(r[0].body.error_message === "overloaded (429)", "S1 retry: error_message carried");
  }
}
// S2 — retries exhausted then a NEW user turn -> NO agent_retry (turn advanced).
{
  const { posts, fire } = newInstance();
  await fire("input", { text: "do X" });
  await fire("agent_start", {});
  await fire("agent_end", { messages: [{ role: "assistant", stopReason: "error", errorMessage: "overloaded" }] });
  await fire("input", { text: "next question" });
  await fire("agent_start", {});
  check(retriesOf(posts).length === 0, "S2 new-turn-after-exhausted-error: NO agent_retry");
}
// S3 — normal completion then a new turn -> NO agent_retry.
{
  const { posts, fire } = newInstance();
  await fire("input", { text: "hi" });
  await fire("agent_start", {});
  await fire("agent_end", { messages: [{ role: "assistant", stopReason: "stop", errorMessage: "" }] });
  await fire("input", { text: "again" });
  await fire("agent_start", {});
  check(retriesOf(posts).length === 0, "S3 normal completion: NO agent_retry");
}
// S4 — an ESC abort ('aborted', not 'error') then the same turn continues -> NO agent_retry.
{
  const { posts, fire } = newInstance();
  await fire("input", { text: "hi" });
  await fire("agent_start", {});
  await fire("agent_end", { messages: [{ role: "assistant", stopReason: "aborted", errorMessage: "" }] });
  await fire("agent_start", {});
  check(retriesOf(posts).length === 0, "S4 aborted (not error): NO agent_retry");
}
// S5 — a context-OVERFLOW compaction re-runs the SAME turn: the errored run (overflow arrives as stopReason=error)
//      is followed by a session_compact BEFORE the re-run's agent_start. pi does not treat overflow as a retry, so
//      the extension's session_compact discriminator must suppress agent_retry. Firing session_compact is a no-op on
//      a build without the handler (RED there: the same-turn error still emits agent_retry).
{
  const { posts, fire } = newInstance();
  await fire("input", { text: "do X" });
  await fire("agent_start", {});
  await fire("agent_end", { messages: [{ role: "assistant", stopReason: "error", errorMessage: "context length exceeded" }] });
  await fire("session_compact", { reason: "overflow" });
  await fire("agent_start", {});
  check(retriesOf(posts).length === 0, "S5 overflow-compaction re-run: NO agent_retry (session_compact discriminator)");
}
console.log(fails === 0 ? "ALL PASS" : (fails + " FAILED"));
process.exit(fails === 0 ? 0 : 1);
HARNESS

if h_out=$(node "$WORK/harness.mjs" "$WORK/pi-ext.mts" 2>&1); then h_rc=0; else h_rc=$?; fi
printf '%s\n' "$h_out" | sed 's/^/    /'
if [ "$h_rc" -eq 0 ] && printf '%s\n' "$h_out" | grep -q "ALL PASS"; then
  pass "phase18: extension emits agent_retry only on a same-turn retry-after-error (5/5 scenarios)"
else
  record_fail "phase18: agent_retry emission harness failed (rc=$h_rc)"
fi

echo ""
echo "--- pi Item7 agent_retry emission phase complete ---"

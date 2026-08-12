/**
 * tg-cli pi extension — a SILENT event forwarder installed by tg-cli.
 *
 * pi auto-loads this file (via jiti, no build step). It subscribes to pi
 * events and fire-and-forget POSTs them to the tg-cli bot's existing
 * /hook/<Event> HTTP endpoint, mapping pi events onto tg-cli's fixed hook
 * event set.
 *
 * `__HOOK_PORT__` is a literal placeholder token substituted with the bot's
 * HTTP port (e.g. 12500) at install time — it is NOT read from env/credentials.
 *
 * NORMATIVE: this extension MUST NOT touch the pi UI in any way (no
 * ctx.ui.setTitle / notify / setWidget / setFooter / any ctx.ui.* / any
 * dialog). ~/.pi/agent is the user's daily driver, so the forwarder must be
 * invisible to them; busy state is signaled ONLY via agent_start/agent_settled
 * POSTs. Do not add any UI call here.
 */
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

export default function (pi) {
  const HOOK_PORT = __HOOK_PORT__;
  const BASE = "http://127.0.0.1:" + HOOK_PORT;

  let promptCounter = 0;
  let promptId = "p0";
  let turnId = "t0";
  let msgCounter = 0;
  let curMsgId = null; // current assistant message id, null between messages
  let curIdx = 0; // 0-based CONTIGUOUS index over forwarded TEXT deltas (NOT contentIndex)
  let curText = ""; // accumulated text of the current assistant message
  let lastAssistantText = ""; // last non-empty assistant text, for the Stop dedup contract
  let lastStopReason = null; // last assistant stopReason this run — forwarded VERBATIM in agent_idle.stop_reason; drives the settled branch
  let lastErrorMessage = ""; // last assistant errorMessage this run — forwarded in agent_idle.error_message for the Go handler
  let lastRunTurnId = ""; // turn_id of the run whose agent_end last latched lastStopReason — lets the next agent_start tell a SAME-turn retry from a new turn (Item 7)
  let compactedInWindow = false; // set by session_compact within a (agent_end -> next agent_start) window; suppresses the Item 7 agent_retry for a compaction-driven re-run (overflow/threshold), which pi does NOT treat as a retry. Reset at every agent_end so only a compaction in THIS window counts.
  let reserveCache = null; // resolved reserveTokens, cached per session

  function tmuxTarget() {
    const pane = process.env.TMUX_PANE;
    if (!pane) return "";
    const tmux = process.env.TMUX;
    if (tmux) return pane + "@" + tmux.split(",")[0];
    return pane;
  }

  function common(ctx) {
    const sm = ctx.sessionManager;
    const cwd = (sm && sm.getCwd && sm.getCwd()) || process.cwd() || "";
    const parts = cwd.split("/").filter(Boolean);
    return {
      session_id: (sm && sm.getSessionId && sm.getSessionId()) || "",
      cwd: cwd,
      project: parts.length ? parts[parts.length - 1] : "",
      transcript_path: (sm && sm.getSessionFile && sm.getSessionFile()) || "",
      backend: "pi",
      tmux_target: tmuxTarget(),
    };
  }

  function post(event, ctx, extra) {
    const body = Object.assign({ hook_event_name: event }, common(ctx), extra || {});
    try {
      fetch(BASE + "/hook/" + event, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      }).catch(() => {});
    } catch (e) {}
  }

  function readReserveFrom(file) {
    try {
      const j = JSON.parse(fs.readFileSync(file, "utf8"));
      if (j && j.compaction && typeof j.compaction.reserveTokens === "number") {
        return j.compaction.reserveTokens;
      }
    } catch (e) {}
    return null;
  }

  // Resolve pi's compaction reserveTokens once per session (cached): default 16384,
  // overridden by the global settings.json, then by the project's .pi/settings.json
  // ONLY when the project is trusted (mirrors pi's own settings-load gating).
  function resolveReserve(ctx) {
    if (reserveCache !== null) return reserveCache;
    let reserve = 16384;
    const agentDir = process.env.PI_CODING_AGENT_DIR || path.join(os.homedir(), ".pi", "agent");
    const g = readReserveFrom(path.join(agentDir, "settings.json"));
    if (g !== null) reserve = g;
    try {
      if (ctx.isProjectTrusted && ctx.isProjectTrusted()) {
        const sm = ctx.sessionManager;
        const cwd = ctx.cwd || (sm && sm.getCwd && sm.getCwd()) || process.cwd() || "";
        if (cwd) {
          const p = readReserveFrom(path.join(cwd, ".pi", "settings.json"));
          if (p !== null) reserve = p;
        }
      }
    } catch (e) {}
    reserveCache = reserve;
    return reserve;
  }

  // Write the tg-cli context-usage file (the pi analog of CC's statusline output).
  // tokens null/undefined -> 0 (renders 0% per the boss ruling); window undefined -> 0
  // (the Go effLimit guard then yields no segment). NEVER touches ctx.ui.
  function writeContext(ctx) {
    try {
      const sm = ctx.sessionManager;
      const sid = (sm && sm.getSessionId && sm.getSessionId()) || "";
      if (!sid) return;
      const u = (ctx.getContextUsage && ctx.getContextUsage()) || null;
      const tokens = (u && typeof u.tokens === "number") ? u.tokens : 0;
      const win = (u && typeof u.contextWindow === "number") ? u.contextWindow : 0;
      const dir = path.join(os.tmpdir(), "tg-cli", "context");
      fs.mkdirSync(dir, { recursive: true });
      fs.writeFileSync(
        path.join(dir, sid + ".json"),
        JSON.stringify({
          backend: "pi",
          context_tokens: tokens,
          context_window: win,
          reserve_tokens: resolveReserve(ctx),
        })
      );
    } catch (e) {}
  }

  pi.on("session_start", (event, ctx) => {
    post("SessionStart", ctx, {});
  });

  pi.on("input", (event, ctx) => {
    promptCounter++;
    promptId = "p" + promptCounter;
    turnId = "t" + promptCounter;
    post("UserPromptSubmit", ctx, { prompt: event.text, prompt_id: promptId, turn_id: turnId });
  });

  pi.on("agent_start", (event, ctx) => {
    // Item 7 (retry-after-error mark): capture the PREVIOUS run's outcome BEFORE the reset below. A retryable
    // provider error (stream truncation, overloaded, 429/5xx) makes pi auto-continue the SAME turn — a fresh
    // agent_start with NO new user input, so turnId is unchanged (turnId advances only in `input`). When the
    // previous run of THIS turn ended in "error", POST agent_retry so the Go side marks the already-rendered
    // (truncated) bubble interrupted-and-retrying. lastRunTurnId guards against a NEW turn after a retries-
    // exhausted error, where lastStopReason is still "error" but turnId has already advanced (NOT a retry).
    // compacted guards against a context-OVERFLOW re-run: overflow ALSO arrives as stopReason=error
    // (overflow.js isContextOverflow) and pi re-runs the SAME turn after compacting it — but pi does NOT treat
    // that as a retry (_isRetryableError returns false for isContextOverflow). A session_compact fires in THIS
    // (agent_end -> agent_start) window ONLY for a compaction re-run, never for a _prepareRetry network retry,
    // so it discriminates the two. Without it the guard would be wider than pi's own retry predicate.
    const compacted = compactedInWindow;
    const prevErrorRetry = lastStopReason === "error" && lastRunTurnId === turnId && !compacted;
    const prevErrorMessage = lastErrorMessage;
    // Reset the Stop-dedup carry at the START of every run. lastAssistantText survives across runs in the
    // extension closure and is written ONLY in message_end; without this reset, a run that ends with no
    // assistant text (e.g. an aborted tool call) leaves the PREVIOUS turn's text here, and agent_settled would
    // POST it as the Stop body — which the Go side re-sends (FinalizeNoEntry + direct_send) since this run's own
    // UserPromptSubmit already Rotated the stream. agent_start is emitted before any throwable work in the loop,
    // so it is the correct and sufficient reset point (no second reset).
    lastAssistantText = "";
    lastStopReason = null;
    lastErrorMessage = "";
    writeContext(ctx);
    post("agent_start", ctx, { prompt_id: promptId, turn_id: turnId });
    if (prevErrorRetry) {
      post("agent_retry", ctx, { prompt_id: promptId, turn_id: turnId, error_message: prevErrorMessage });
    }
  });

  pi.on("message_start", (event, ctx) => {
    if (event.message && event.message.role === "assistant") {
      msgCounter++;
      curMsgId = String(msgCounter);
      curIdx = 0;
      curText = "";
    }
  });

  pi.on("message_update", (event, ctx) => {
    const ame = event.assistantMessageEvent;
    if (!ame || ame.type !== "text_delta") return; // text deltas ONLY; thinking_*/toolcall_*/text_start/text_end excluded
    if (curMsgId === null) {
      // defensive: message_start not seen
      msgCounter++;
      curMsgId = String(msgCounter);
      curIdx = 0;
      curText = "";
    }
    const delta = ame.delta || "";
    // index is the adapter's own contiguous 0-based counter over forwarded text deltas — NEVER ame.contentIndex.
    post("MessageDisplay", ctx, {
      message_id: curMsgId,
      turn_id: turnId,
      prompt_id: promptId,
      index: curIdx,
      final: false,
      delta: delta,
    });
    curText += delta;
    curIdx++;
  });

  pi.on("message_end", (event, ctx) => {
    if (!event.message || event.message.role !== "assistant") return;
    if (curMsgId !== null && curIdx > 0) {
      // Seal: final:true on a synthetic empty delta at the next contiguous index. AssembledText then walks
      // 0..curIdx and completes; the empty delta leaves the assembled text unchanged. lastAssistantText is
      // the exact concatenation of the forwarded deltas, so the Stop body TrimSpace-equals it (no duplicate send).
      post("MessageDisplay", ctx, {
        message_id: curMsgId,
        turn_id: turnId,
        prompt_id: promptId,
        index: curIdx,
        final: true,
        delta: "",
      });
      lastAssistantText = curText;
    }
    curMsgId = null;
  });

  pi.on("tool_call", (event, ctx) => {
    post("PreToolUse", ctx, {
      tool_name: event.toolName,
      tool_input: event.input,
      tool_use_id: event.toolCallId,
      prompt_id: promptId,
      turn_id: turnId,
    });
  });

  pi.on("tool_result", (event, ctx) => {
    post("PostToolUse", ctx, {
      tool_name: event.toolName,
      tool_response: event.content,
      tool_use_id: event.toolCallId,
      prompt_id: promptId,
      turn_id: turnId,
    });
  });

  pi.on("session_compact", (event, ctx) => {
    // Item 7 discriminator (state-only, NO hook POST). pi emits session_compact to extensions when it compacts
    // the transcript — manual /compact, threshold, or context overflow (agent-session.js _runAutoCompaction). An
    // overflow ARRIVES as stopReason=error and drives a SAME-turn re-run that would otherwise satisfy the
    // agent_start retry gate and falsely POST agent_retry. pi itself excludes overflow from retry, so record that
    // a compaction happened in THIS (agent_end -> next agent_start) window; agent_start then suppresses the mark.
    // A network retry (_prepareRetry) fires NO session_compact, so it stays a real agent_retry.
    compactedInWindow = true;
  });

  pi.on("agent_end", (event, ctx) => {
    // State-only latch (NO hook POST). agent_end fires for every agent run (prompt + each continue) BEFORE
    // agent_settled (agent-session.js emits agent_settled in a finally AFTER the run loop). Scan event.messages
    // for assistant messages and record the LAST assistant stopReason + errorMessage this run. Both measured
    // abort shapes carry the outcome on the last assistant message (during-TEXT -> "aborted", during-TOOL ->
    // "error"), so the last-writer-wins scan is sufficient — no sticky flag needed (Occam).
    const msgs = (event && event.messages) || [];
    for (const m of msgs) {
      if (m && m.role === "assistant") {
        lastStopReason = m.stopReason;
        lastErrorMessage = m.errorMessage || "";
      }
    }
    // Record the turn this run belonged to, so the NEXT agent_start can tell a same-turn retry (turnId
    // unchanged) from a new turn after a retries-exhausted error (turnId advanced). Item 7.
    lastRunTurnId = turnId;
    // Open a fresh compaction window for the (this agent_end -> next agent_start) gap: only a session_compact
    // fired WITHIN this window suppresses the next agent_start's agent_retry. Resetting here keeps a stale
    // compaction flag from an earlier sub-run from leaking into a later network retry of the same turn. Item 7.
    compactedInWindow = false;
  });

  pi.on("agent_settled", (event, ctx) => {
    writeContext(ctx);
    // SCOPE GUARD: branch on EXACTLY two stopReasons. "aborted" (ESC) and "error" (retries exhausted) must NOT
    // post the normal Stop — Stop relabels the last bubble to "✅ Task Completed", a misreport for an
    // interrupted/failed turn (CC posts no Stop on an interrupt). Every OTHER stopReason
    // (stop/length/toolUse/deferred/pending) posts Stop exactly as before. NOT an "anything-not-stop" policy.
    // R2/R3: agent_idle forwards pi's OWN stopReason VERBATIM in stop_reason (never interpreted here) plus the
    // errorMessage. The Go handler dispatches the notification off stop_reason: "aborted" -> a standalone
    // Interrupted notification, "error" -> the error notification carrying error_message. Notifications unify
    // upward — every interrupt notifies; no errorMessage string-match anywhere.
    if (lastStopReason === "aborted" || lastStopReason === "error") {
      post("agent_idle", ctx, {
        prompt_id: promptId,
        turn_id: turnId,
        stop_reason: lastStopReason,
        error_message: lastErrorMessage,
      });
      return;
    }
    post("Stop", ctx, { last_assistant_message: lastAssistantText, prompt_id: promptId, turn_id: turnId });
  });

  pi.on("turn_end", (event, ctx) => {
    // turn_end forwards NO hook POST (round-0 event-mapping contract) — it only
    // refreshes the local context-usage file so long multi-tool runs show a current
    // number (per-LLM-response freshness). A local /tmp write is not a hook POST.
    writeContext(ctx);
  });
}

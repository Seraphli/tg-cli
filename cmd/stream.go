package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// setSent grows e.SentText to index i and assigns text.
func setSent(e *stores.StreamEntry, i int, text string) {
	for len(e.SentText) <= i {
		e.SentText = append(e.SentText, "")
	}
	e.SentText[i] = text
}

// alignToParagraph truncates text to everything up to AND including the LAST "\n\n" paragraph boundary (F1).
// ok=false when text contains no "\n\n". Byte-index based; the returned string is a prefix of text. Used by the
// ticker flush to render only whole paragraphs (boss ruling: send/edit only at a paragraph boundary).
func alignToParagraph(text string) (string, bool) {
	idx := strings.LastIndex(text, "\n\n")
	if idx < 0 {
		return "", false
	}
	return text[:idx+2], true
}

// renderStreamChunks renders the entry's FROZEN plan (nd + rich/legacy chunk lists, built once in flushSession
// p2) into TG message(s): send new chunks, edit changed ones, delete surplus old continuation messages if the
// re-render shrank the chunk count. f25 MAJOR: it renders the frozen plan VERBATIM (no recompute of nd or
// chunks) so the snapshot-time prediction equals the render exactly.
func renderStreamChunks(bs *BotState, sessionID string, e *stores.StreamEntry, nd notify.NotificationData, chunks, legacyChunks []string, relabel, interrupt bool) {
	chat := &tele.Chat{ID: e.ChatID}
	for i, chunk := range chunks {
		nd.Body = chunk
		nd.Finalized = relabel && i == len(chunks)-1
		// Item 7: the "🔄 Interrupted — retrying…" mark rides on the LAST chunk's header (like Finalized), so a
		// multi-chunk truncated bubble is marked at its tail. Interrupted takes precedence over Finalized in
		// BuildNotificationText — but a to-be-marked errored bubble is never a Stop-relabel target anyway.
		nd.Interrupted = interrupt && i == len(chunks)-1
		if len(chunks) > 1 {
			nd.Page = i + 1
			nd.TotalPages = len(chunks)
		} else {
			nd.Page = 0
		}
		out := notify.BuildNotificationText(nd)
		// Build legacy parallel output for G2 fallback.
		legacyBody := ""
		if i < len(legacyChunks) {
			legacyBody = legacyChunks[i]
		}
		ndLegacy := nd
		ndLegacy.Body = legacyBody
		outLegacy := notify.BuildNotificationText(ndLegacy)
		if i < len(e.Msgs) {
			if i < len(e.SentText) && e.SentText[i] == out {
				continue
			}
			helpers.RetryEditRich(bs.Bot, &tele.Message{ID: e.Msgs[i], Chat: chat}, out, helpers.RichSendOpts{SkipEntityDetection: false, LegacyHTML: outLegacy, CCMessageID: e.MessageID})
			setSent(e, i, out)
			logger.Debug(fmt.Sprintf("Stream edit: msg_id=%d message_id=%s turn_id=%s chunk=%d final=%v fmt=rich full_text:\n%s", e.Msgs[i], e.MessageID, e.TurnID, i, nd.Finalized, helpers.FinalizeRichHTML(out)))
		} else {
			if sent, err := helpers.RetrySendRich(bs.Bot, chat, out, helpers.RichSendOpts{TopicID: e.TopicID, SkipEntityDetection: false, LegacyHTML: outLegacy, CCMessageID: e.MessageID}); err == nil && sent != nil {
				e.Msgs = append(e.Msgs, sent.ID)
				e.Rich = true
				setSent(e, i, out)
				logger.Debug(fmt.Sprintf("Stream send: msg_id=%d message_id=%s turn_id=%s chunk=%d fmt=rich full_text:\n%s", sent.ID, e.MessageID, e.TurnID, i, helpers.FinalizeRichHTML(out)))
			}
		}
	}
	if relabel && len(e.Msgs) > 0 {
		logger.Info(fmt.Sprintf("Stream relabel ✅: last_msg_id=%d message_id=%s session=%s", e.Msgs[len(e.Msgs)-1], e.MessageID, sessionID))
	}
	// Delete surplus old continuation messages if the re-render shrank the chunk count.
	for i := len(chunks); i < len(e.Msgs); i++ {
		if err := bs.Bot.Delete(&tele.Message{ID: e.Msgs[i], Chat: chat}); err != nil {
			helpers.RetryEditRich(bs.Bot, &tele.Message{ID: e.Msgs[i], Chat: chat}, "<i>(obsolete)</i>", helpers.RichSendOpts{LegacyHTML: "<i>(obsolete)</i>"})
		}
		logger.Info(fmt.Sprintf("Stream surplus removed: msg_id=%d message_id=%s chunk=%d fmt=rich", e.Msgs[i], e.MessageID, i))
	}
	if len(chunks) < len(e.Msgs) {
		e.Msgs = e.Msgs[:len(chunks)]
		if len(e.SentText) > len(chunks) {
			e.SentText = e.SentText[:len(chunks)]
		}
	}
}

// flushDispatchMode selects how the render op produced by flushSession is enqueued onto the Message FIFO.
type flushDispatchMode int

const (
	flushAsync flushDispatchMode = iota // DispatchAsync (guaranteed enqueue, blocks at cap) — non-stop boundaries
	flushTry                            // TryDispatchAsync (nonblocking) — the ticker (S4)
	flushSync                           // Dispatch (sync, awaits the render) — Stop boundary
)

// flushJob is one snapshotted renderable entry captured under DataMu, plus its immutable FROZEN render plan
// (f25 MAJOR): nd + rich/legacy chunk lists are computed ONCE in flushSession p2 (outside DataMu) so the
// snapshot-time chunk prediction and renderStreamChunks use the IDENTICAL data — prediction == render by
// construction (GetPaneCLICommand is unbounded, so a recompute at render time could disagree). willSendBelow
// (planned chunks > positionedAtSnapshot) means a NEW TG message will be sent below -> SendBelowSinceTool.
type flushJob struct {
	e                    *stores.StreamEntry
	mid                  string
	text                 string
	complete             bool
	finalize             bool
	relabel              bool
	interrupt            bool // Item 7: render this entry's header as "🔄 Interrupted — retrying…" (one-shot)
	nd                   notify.NotificationData
	chunks               []string
	legacyChunks         []string
	positionedAtSnapshot int
	willSendBelow        bool
}

// flushSession snapshots the session's stream under DataMu then enqueues ONE render op onto the Message FIFO
// (dual-FIFO, S3). FlushMu is held across BOTH the snapshot AND the enqueue so ops enqueue in Hook-FIFO order
// (INV3/R1); NO Message-FIFO op takes FlushMu, so a full-queue enqueue while holding FlushMu cannot deadlock
// (INV6). stop=true means a Stop boundary (relabel the last message); force=true is the deadline degradation
// that finalizes even an INCOMPLETE last message. mode picks the enqueue path (S3/S4). Returns true if a render
// op was enqueued (false when nothing is renderable, or when a flushTry enqueue failed on a full queue).
func flushSession(bs *BotState, sessionID string, stop, force bool, mode flushDispatchMode) bool {
	ss := bs.Streams.Session(sessionID)
	ss.FlushMu.Lock()
	defer ss.FlushMu.Unlock()
	cfg, _ := config.LoadAppConfig()
	// p1: snapshot order + dirty-clear + read PositionedChunks under DataMu (no I/O here). Abort if reset.
	ss.DataMu.Lock()
	if ss.Closed {
		ss.DataMu.Unlock()
		return false
	}
	order := append([]string(nil), ss.Order...)
	lastIdx := len(order) - 1
	var jobs []flushJob
	for i, mid := range order {
		e := ss.Msgs[mid]
		// Skip already-finalized messages — EXCEPT the turn's last one at Stop, which still needs the 💬→✅
		// header relabel even though its content is sealed.
		needRelabel := stop && i == lastIdx && !e.Relabeled
		// Item 7: a sealed bubble newly flagged Interrupted (a retryable-error retry on the same turn) must be
		// re-rendered ONCE to add the "🔄 Interrupted — retrying…" header — the SAME sealed-entry re-render
		// exception as needRelabel, one-shot via InterruptRendered.
		needInterrupt := e.Interrupted && !e.InterruptRendered
		if e.Sealed && !needRelabel && !needInterrupt {
			continue
		}
		text, complete := e.AssembledText()
		// Drop empty-text jobs from the snapshot itself so the "no renderable job" check below is accurate
		// (an unconditional pre-tool flush is a cheap no-op when nothing renderable, rev 14 BLOCKER3).
		if strings.TrimSpace(text) == "" {
			continue
		}
		isLast := i == lastIdx
		// Finalize (seal + table + ✅) ONLY when the message is actually complete, or on a forced deadline close.
		// A non-forced Stop on an incomplete last message just best-effort flushes the partial 💬 — no seal/relabel,
		// so a slightly-late final delta is still accepted on the next loop.
		finalize := complete || (force && isLast)
		relabel := stop && isLast && finalize
		// Ticker paragraph alignment (boss ruling): ONLY the ticker flush (flushTry — its sole production call
		// site is startStreamLoop) aligns to a paragraph boundary; every other flush and every finalize render
		// the FULL text. F1: render only up to & incl the last "\n\n"; the remainder is NOT consumed (Deltas
		// untouched — a later flush re-reads it). F3/F4: finalize / non-ticker render full (block skipped).
		renderText := text
		if mode == flushTry && !finalize {
			aligned, ok := alignToParagraph(text)
			// F2: no boundary yet, or the only boundary encloses whitespace (e.g. a leading "\n\n"). The
			// TrimSpace clause is note3's interpretation call INSIDE ruling #2 ("no boundary yet -> do not
			// send"), mirroring the existing empty-text guard above — not a change to the ruling.
			if !ok || strings.TrimSpace(aligned) == "" {
				continue // Dirty & LastFlush preserved (skip is BEFORE the assignments below) — retry next tick/delta
			}
			// F5: the rendered body must never shrink. Skip when the aligned body is not longer than the body
			// already committed for this entry (RenderedLen, the enqueue-time high-water). Same skip semantics as
			// F2 (Dirty preserved). This is the SOLE guard against renderStreamChunks surplus-deletion.
			if len(aligned) <= e.RenderedLen {
				continue
			}
			renderText = aligned
		}
		e.Dirty = false
		e.LastFlush = time.Now()
		jobs = append(jobs, flushJob{e: e, mid: mid, text: renderText, complete: complete, finalize: finalize, relabel: relabel, interrupt: needInterrupt, positionedAtSnapshot: e.PositionedChunks})
	}
	ss.DataMu.Unlock()
	// No renderable job (all sealed / empty text) — return WITHOUT enqueuing so an unconditional pre-tool flush
	// is a cheap no-op (S3).
	if len(jobs) == 0 {
		return false
	}
	// p2 (outside DataMu, FlushMu still held): build the FROZEN render plan per job (f25 MAJOR). Freeze nd
	// (incl. the UNBOUNDED GetPaneCLICommand output + context usage) AND the rich/legacy chunk lists once, so
	// the snapshot-time chunk prediction and renderStreamChunks use the IDENTICAL data — no TOCTOU, the -100
	// budget margin is no longer load-bearing. planned=len(chunks) > positionedAtSnapshot => a new TG message
	// will be SENT below (new bubble or pagination growth) => willSendBelow.
	richMax := 30000
	if cfg.RichMaxRunes > 0 {
		richMax = cfg.RichMaxRunes
	}
	for i := range jobs {
		j := &jobs[i]
		nd := notify.NotificationData{
			Event:          "Message",
			Project:        j.e.Project,
			CWD:            j.e.CWD,
			TmuxTarget:     j.e.TmuxTarget,
			AgentName:      j.e.AgentName,
			Backend:        j.e.Backend,
			CLICommand:     helpers.GetPaneCLICommand(j.e.TmuxTarget),
			ContextUsedPct: -1,
		}
		if pct, used, win, ok := helpers.ReadContextUsage(sessionID); ok {
			nd.ContextUsedPct, nd.ContextUsedTokens, nd.ContextWindowSize = pct, used, win
		}
		budget := richMax - notify.HeaderLen(nd) - 100
		j.nd = nd
		j.chunks = helpers.SplitBody(markdown.RenderRichHTML(j.text), budget)
		j.legacyChunks = helpers.SplitBody(markdown.RenderTelegramHTML(j.text), budget)
		j.willSendBelow = len(j.chunks) > j.positionedAtSnapshot
	}
	// The render op: takes DataMu ONLY (never FlushMu), revalidates each entry (ss.Closed / ss.Msgs[mid]==e),
	// renders the FROZEN plan via renderStreamChunks (TG I/O), and sets Sealed/Relabeled under DataMu.
	// TG I/O runs OUTSIDE DataMu; Msgs/SentText are Message-FIFO-only (INV4).
	renderOp := func() error {
		for _, j := range jobs {
			ss.DataMu.Lock()
			if ss.Closed || ss.Msgs[j.mid] != j.e {
				ss.DataMu.Unlock()
				continue // session reset / entry rotated away between snapshot and render
			}
			ss.DataMu.Unlock()
			renderStreamChunks(bs, sessionID, j.e, j.nd, j.chunks, j.legacyChunks, j.relabel, j.interrupt)
			if j.finalize || j.interrupt {
				ss.DataMu.Lock()
				if j.finalize {
					j.e.Sealed = true
					if j.relabel {
						j.e.Relabeled = true
					}
				}
				if j.interrupt {
					j.e.InterruptRendered = true // one-shot: the sealed bubble carries the mark; do not re-render it
				}
				ss.DataMu.Unlock()
			}
		}
		return nil
	}
	switch mode {
	case flushSync:
		bs.MessageQueue.Dispatch(sessionID, "msg:stream-render", renderOp)
	case flushTry:
		if !bs.MessageQueue.TryDispatchAsync(sessionID, "msg:stream-render", renderOp) {
			// Queue full (S4): re-mark the snapshotted entries Dirty under DataMu so the next tick retries;
			// Dirty dedups (a concurrent delta may also have set it). Never inline the render. NO p3 commit —
			// PositionedChunks/SendBelowSinceTool stay untouched so the retry recomputes willSendBelow against the
			// unchanged count (f25: a full-queue miss must never produce a false split).
			ss.DataMu.Lock()
			for i := range jobs {
				if e := ss.Msgs[jobs[i].mid]; e == jobs[i].e {
					e.Dirty = true
				}
			}
			ss.DataMu.Unlock()
			return false
		}
	default: // flushAsync
		bs.MessageQueue.DispatchAsync(sessionID, "msg:stream-render", renderOp)
	}
	// p3 (f25): reached ONLY on a SUCCESSFUL enqueue (the flushTry full-queue miss returned above). For flushSync
	// the render already COMPLETED before this point, but FlushMu is held through p3, so no concurrent snapshot
	// can observe an intermediate placement state — the commit still happens exactly once, at (post-)enqueue,
	// totally ordered by FlushMu (B1: a later snapshot never re-splits).
	commitPositioned(ss, jobs)
	return true
}

// commitPositioned is flushSession p3 (f25): under DataMu, for each STILL-LIVE snapshotted job (revalidating
// ss.Closed / ss.Msgs[mid]==e exactly as the render op does, so a Rotate/EndSession between the p1 release and
// here leaves no stale commit or signal — B2), commit PositionedChunks=len(chunks) and OR the placement signal
// into ss.SendBelowSinceTool. Extracted so the lifecycle-revalidation path is unit-testable deterministically.
func commitPositioned(ss *stores.SessionStream, jobs []flushJob) {
	ss.DataMu.Lock()
	sendBelow := false
	for i := range jobs {
		j := &jobs[i]
		if ss.Closed || ss.Msgs[j.mid] != j.e {
			continue // entry rotated away / session closed between snapshot and commit (B2)
		}
		j.e.PositionedChunks = len(j.chunks)
		j.e.RenderedLen = len(j.text) // F5 high-water: byte length of the body just committed for this entry
		if j.willSendBelow {
			sendBelow = true
		}
	}
	if sendBelow {
		ss.SendBelowSinceTool = true
	}
	ss.DataMu.Unlock()
}

// streamFlush is the Callbacks.StreamFlush implementation. S10 (no MD-waits): the Stop path NO LONGER drains
// (drainUntilComplete removed) — the terminal outcome was already decided on the Hook FIFO (register.go S10).
// It runs ONE SYNC stop render op (flushSession with stop=true, force=true) — the needRelabel snapshot
// exception (:195) relabels a ticker-SEALED-!Relabeled last entry 💬→✅, force-finalizes an INCOMPLETE last
// entry once, and renders nothing for a COMPLETE/already-relabeled/absent last entry — then TryClose falls
// back to MarkStopped so the turn is always closed. The stop parameter is always true at every call site;
// the non-stop branch is kept as a plain single async flush for any future non-terminal caller.
func streamFlush(bs *types.BotState, sessionID string, stop bool) {
	if !stop {
		flushSession(bs, sessionID, false, false, flushAsync)
		return
	}
	bs.Streams.MarkStopRequested(sessionID)
	// Single SYNC stop render op (force=true seals+relabels the last message once). No drain loop.
	flushSession(bs, sessionID, true, true, flushSync)
	if !bs.Streams.TryClose(sessionID) {
		bs.Streams.MarkStopped(sessionID)
	}
}

// streamFlushAwaitNewText is the Callbacks.StreamFlushAwaitNewText implementation: the AskUserQuestion
// boundary flush. f23 (boss-ordered restoration): restore the in-flight pre-question text wait that 10de4d8
// removed (drainForNewFinal 3s). A LITERAL sleep-poll of CompleteCount would SELF-STARVE now that
// MessageDisplay is processed on the SAME Hook FIFO as this send:pending job (S0): the worker runs jobs one
// at a time, so a blocking poll would starve the very pre-question MD it waits for. So, mirroring the S6 tool
// boundary, DRAIN the in-flight pre-question MD on THIS worker (DrainAndRunMatching + an event-driven wait)
// until a NEW text bubble completes (CompleteCount rises above the entry baseline) or the 3s budget elapses,
// then flush. Prompt-agnostic (faithful to the original drainForNewFinal) — any MessageDisplay before the next
// tool / turn-terminal is eligible. Runs in the send:pending Hook-FIFO job BEFORE the msg:ask-send question
// dispatch (pending.go), so the text renders before the question. The baseline is captured at entry, so a
// stale already-complete prior bubble does NOT satisfy it.
func streamFlushAwaitNewText(bs *types.BotState, sessionID string) {
	const askBudget = 3 * time.Second
	start := time.Now()
	deadline := start.Add(askBudget)
	baseline := bs.Streams.CompleteCount(sessionID)
	// eligible: any MessageDisplay arriving before the deadline (prompt-agnostic). boundary: the next tool /
	// turn-terminal, or anything past the deadline — the front-scan stops there so a later-turn MD is not drained.
	eligible := func(m stores.JobMeta) bool {
		return m.Event == "MessageDisplay" && !m.ArrivedAt.After(deadline)
	}
	boundary := func(m stores.JobMeta) bool {
		return m.Event == "PreToolUse" || m.Event == "Stop" || m.Event == "UserPromptSubmit" ||
			m.Event == "SessionEnd" || m.ArrivedAt.After(deadline)
	}
	// (a) drain any already-queued pre-question MD now (runs its handler on THIS worker -> appends the text).
	bs.SessionEvents.DrainAndRunMatching(sessionID, eligible, boundary)
	branch := "drained_new_final"
	// (b) if no NEW completed bubble yet, park (event-driven) until one completes (CompleteCount>baseline), a
	// boundary is queued, or the 3s budget. No floor (zero) — an already-queued boundary resolves immediately.
	if bs.Streams.CompleteCount(sessionID) <= baseline {
		branch = bs.SessionEvents.WaitForMatchOrDeadlineFloored(sessionID, "", eligible, boundary,
			func() bool { return bs.Streams.CompleteCount(sessionID) > baseline }, time.Time{}, deadline)
	}
	logger.Info(fmt.Sprintf("askq drain done: session=%s branch=%s elapsed_ms=%d baseline=%d budget_ms=%d",
		sessionID, branch, time.Since(start).Milliseconds(), baseline, askBudget.Milliseconds()))
	// (c) flush the pre-question text SYNC onto the Message FIFO BEFORE the msg:ask-send question dispatch, so
	// the text renders before the question (FIFO order preserved).
	flushSession(bs, sessionID, false, false, flushSync)
}

// flushStreamOp is the Callbacks.FlushStreamOp implementation: the plain NON-DRAINING pre-tool flush (S6).
// It just snapshots the stream and enqueues ONE async render op — NO drainUntilComplete (the S6 queue-
// lookahead already drained/waited for the pre-tool MessageDisplay on the Hook FIFO). No-op when nothing
// is renderable (flushSession returns without enqueuing on an empty/sealed snapshot).
func flushStreamOp(bs *types.BotState, sessionID string) {
	flushSession(bs, sessionID, false, false, flushAsync)
}

// startStreamLoop is the background worker that throttle-flushes dirty sessions.
func startStreamLoop(ctx context.Context, bs *BotState) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg, _ := config.LoadAppConfig()
			throttle := 1000 * time.Millisecond
			if cfg.StreamThrottleMs > 0 {
				throttle = time.Duration(cfg.StreamThrottleMs) * time.Millisecond
			}
			for _, sid := range bs.Streams.DueSessions(throttle) {
				// S4: NONBLOCKING ticker flush. flushSession snapshots under DataMu then enqueues ONE render op
				// onto the Message FIFO via TryDispatchAsync; on a full queue it re-marks the snapshot entries
				// Dirty under DataMu and returns false, so the next tick retries (Dirty dedups). It NEVER inlines
				// the render — a full session queue no longer stalls other sessions (MAJOR 3). Boundary flushes
				// keep the blocking Dispatch/DispatchAsync paths.
				flushSession(bs, sid, false, false, flushTry)
			}
			bs.Streams.SweepEnded(60 * time.Second)           // drop ended-session tombstones older than TTL
			bs.Streams.SweepPostToolArrived(30 * time.Second) // drop never-consumed PostToolUse (B2) signals
		}
	}
}

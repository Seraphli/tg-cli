package cmd

import (
	"bytes"
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

// renderTableImages renders each table in mdBody as a PNG image (shared by streaming finalize and sendEventNotification).
func renderTableImages(mdBody, tableMode string) [][]byte {
	if tableMode == "" {
		tableMode = "image"
	}
	if tableMode != "image" {
		return nil
	}
	var imgs [][]byte
	for _, t := range markdown.ExtractTableData(mdBody) {
		img, err := markdown.RenderTableImageChromeFormatted(t.Headers, t.Rows, t.HeadersHTML, t.RowsHTML)
		if err != nil {
			if img, err = markdown.RenderTableImage(t.Headers, t.Rows); err != nil {
				logger.Error(fmt.Sprintf("Table image render failed: %v", err))
				continue
			}
		}
		imgs = append(imgs, img)
	}
	return imgs
}

// sendTableImages sends pre-rendered table images as TG photo messages.
func sendTableImages(bs *BotState, chat *tele.Chat, topicID int, imgs [][]byte, logCtx string) {
	for i, b := range imgs {
		var opts []interface{}
		if topicID > 0 {
			opts = append(opts, &tele.SendOptions{ThreadID: topicID})
		}
		sent, err := helpers.RetrySend(bs.Bot, chat, &tele.Photo{File: tele.FromReader(bytes.NewReader(b))}, opts...)
		if err != nil {
			logger.Error(fmt.Sprintf("Table image %d send failed (%s): %v", i+1, logCtx, err))
			continue
		}
		mid := 0
		if sent != nil {
			mid = sent.ID
		}
		logger.Info(fmt.Sprintf("Stream table image sent: %s idx=%d photo_msg_id=%d chat=%d", logCtx, i+1, mid, chat.ID))
	}
}

// drainUntilComplete waits for the session's last message to become complete (or timeout).
// Allows async deltas to catch up before a boundary action (PreToolUse, Ask, Stop).
func drainUntilComplete(bs *BotState, sessionID string, timeout time.Duration) {
	start := time.Now()
	grace := 400 * time.Millisecond // initial wait for the FIRST delta to appear at all
	for {
		has, complete := bs.Streams.LastStatus(sessionID)
		if has && complete {
			logger.Info(fmt.Sprintf("drain done: session=%s elapsed=%dms reason=text_complete", sessionID, time.Since(start).Milliseconds()))
			return
		}
		elapsed := time.Since(start)
		if !has && elapsed >= grace {
			logger.Info(fmt.Sprintf("drain done: session=%s elapsed=%dms reason=no_text_within_grace grace=%dms", sessionID, elapsed.Milliseconds(), grace.Milliseconds()))
			return // no streamed text before this boundary
		}
		if elapsed >= timeout {
			logger.Info(fmt.Sprintf("drain done: session=%s elapsed=%dms reason=incomplete_within_timeout has=%v", sessionID, elapsed.Milliseconds(), has))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// drainForNewFinal waits until a NEW text bubble completes (CompleteCount rises above the count captured at
// entry) or the timeout elapses. Used at the AskUserQuestion boundary so the text bubble preceding the
// question is flushed before the question is sent. Unlike drainUntilComplete it does NOT check whether the
// current last bubble is already complete — a stale prior bubble would make that return instantly and miss a
// bubble arriving over the off-FIFO MessageDisplay channel just before the boundary.
func drainForNewFinal(bs *BotState, sessionID string, timeout time.Duration) {
	start := time.Now()
	baseline := bs.Streams.CompleteCount(sessionID)
	for {
		if bs.Streams.CompleteCount(sessionID) > baseline {
			logger.Info(fmt.Sprintf("drain done: session=%s elapsed=%dms reason=new_final_arrived baseline=%d", sessionID, time.Since(start).Milliseconds(), baseline))
			return
		}
		if time.Since(start) >= timeout {
			logger.Info(fmt.Sprintf("drain done: session=%s elapsed=%dms reason=timeout_no_new_final baseline=%d", sessionID, time.Since(start).Milliseconds(), baseline))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// setSent grows e.SentText to index i and assigns text.
func setSent(e *stores.StreamEntry, i int, text string) {
	for len(e.SentText) <= i {
		e.SentText = append(e.SentText, "")
	}
	e.SentText[i] = text
}

// renderStreamChunks renders the entry's text into TG message(s): send new chunks, edit changed ones,
// delete surplus old continuation messages if the re-render shrank the chunk count.
func renderStreamChunks(bs *BotState, sessionID string, e *stores.StreamEntry, text string, cfg config.AppConfig, relabel bool) {
	rendered := markdown.RenderTelegramHTML(text)
	nd := notify.NotificationData{
		Event:          "Message",
		Project:        e.Project,
		CWD:            e.CWD,
		TmuxTarget:     e.TmuxTarget,
		AgentName:      e.AgentName,
		Backend:        e.Backend,
		CLICommand:     helpers.GetPaneCLICommand(e.TmuxTarget),
		ContextUsedPct: -1,
	}
	if pct, used, win, ok := helpers.ReadContextUsage(sessionID); ok {
		nd.ContextUsedPct, nd.ContextUsedTokens, nd.ContextWindowSize = pct, used, win
	}
	paginationMax := 4000
	if cfg.PaginationMaxRunes > 0 {
		paginationMax = cfg.PaginationMaxRunes
	}
	chunks := helpers.SplitBody(rendered, paginationMax-notify.HeaderLen(nd)-100)
	chat := &tele.Chat{ID: e.ChatID}
	for i, chunk := range chunks {
		nd.Body = chunk
		nd.Finalized = relabel && i == len(chunks)-1
		if len(chunks) > 1 {
			nd.Page = i + 1
			nd.TotalPages = len(chunks)
		} else {
			nd.Page = 0
		}
		out := notify.BuildNotificationText(nd)
		if i < len(e.Msgs) {
			if i < len(e.SentText) && e.SentText[i] == out {
				continue
			}
			helpers.RetryEdit(bs.Bot, &tele.Message{ID: e.Msgs[i], Chat: chat}, out, tele.ModeHTML)
			setSent(e, i, out)
			logger.Info(fmt.Sprintf("Stream edit: msg_id=%d message_id=%s turn_id=%s chunk=%d final=%v full_text:\n%s", e.Msgs[i], e.MessageID, e.TurnID, i, nd.Finalized, out))
		} else {
			opts := []interface{}{tele.ModeHTML}
			if e.TopicID > 0 {
				opts = append(opts, &tele.SendOptions{ThreadID: e.TopicID})
			}
			if sent, err := helpers.RetrySend(bs.Bot, chat, out, opts...); err == nil && sent != nil {
				e.Msgs = append(e.Msgs, sent.ID)
				setSent(e, i, out)
				logger.Info(fmt.Sprintf("Stream send: msg_id=%d message_id=%s turn_id=%s chunk=%d full_text:\n%s", sent.ID, e.MessageID, e.TurnID, i, out))
			}
		}
	}
	if relabel && len(e.Msgs) > 0 {
		logger.Info(fmt.Sprintf("Stream relabel ✅: last_msg_id=%d message_id=%s session=%s", e.Msgs[len(e.Msgs)-1], e.MessageID, sessionID))
	}
	// Delete surplus old continuation messages if the re-render shrank the chunk count
	for i := len(chunks); i < len(e.Msgs); i++ {
		if err := bs.Bot.Delete(&tele.Message{ID: e.Msgs[i], Chat: chat}); err != nil {
			helpers.RetryEdit(bs.Bot, &tele.Message{ID: e.Msgs[i], Chat: chat}, "<i>(obsolete)</i>", tele.ModeHTML)
		}
		logger.Info(fmt.Sprintf("Stream surplus removed: msg_id=%d message_id=%s chunk=%d", e.Msgs[i], e.MessageID, i))
	}
	if len(chunks) < len(e.Msgs) {
		e.Msgs = e.Msgs[:len(chunks)]
		if len(e.SentText) > len(chunks) {
			e.SentText = e.SentText[:len(chunks)]
		}
	}
}

// flushSession renders the session's stream. stop=true means a Stop boundary (relabel the last message);
// force=true is the 3s-deadline degradation that finalizes even an INCOMPLETE last message.
func flushSession(bs *BotState, sessionID string, stop, force bool) {
	ss := bs.Streams.Session(sessionID)
	ss.FlushMu.Lock()
	defer ss.FlushMu.Unlock()
	cfg, _ := config.LoadAppConfig()
	// Snapshot order + dirty-clear under DataMu (no I/O here). Abort if the session was reset.
	ss.DataMu.Lock()
	if ss.Closed {
		ss.DataMu.Unlock()
		return
	}
	order := append([]string(nil), ss.Order...)
	lastIdx := len(order) - 1
	type job struct {
		e        *stores.StreamEntry
		text     string
		complete bool
	}
	var jobs []job
	for i, mid := range order {
		e := ss.Msgs[mid]
		// Skip already-finalized messages — EXCEPT the turn's last one at Stop, which still needs the 💬→✅
		// header relabel even though its content is sealed.
		needRelabel := stop && i == lastIdx && !e.Relabeled
		if e.Sealed && !needRelabel {
			continue
		}
		text, complete := e.AssembledText()
		e.Dirty = false
		e.LastFlush = time.Now()
		jobs = append(jobs, job{e, text, complete})
	}
	ss.DataMu.Unlock()
	// Telegram I/O happens OUTSIDE DataMu. Msgs/SentText are flush-only (FlushMu-serialized).
	for i, j := range jobs {
		isLast := i == len(jobs)-1
		if strings.TrimSpace(j.text) == "" {
			continue
		}
		// Finalize (seal + table + ✅) ONLY when the message is actually complete, or on a forced deadline close.
		// A non-forced Stop on an incomplete last message just best-effort flushes the partial 💬 — no seal/relabel,
		// so a slightly-late final delta is still accepted on the next loop.
		finalize := j.complete || (force && isLast)
		relabel := stop && isLast && finalize
		renderStreamChunks(bs, sessionID, j.e, j.text, cfg, relabel)
		if finalize && !j.e.TablesSent {
			imgs := renderTableImages(j.text, cfg.TableMode)
			// sendTableImages logs "Stream table image sent: ... photo_msg_id=..." per photo
			sendTableImages(bs, &tele.Chat{ID: j.e.ChatID}, j.e.TopicID, imgs, "stream session="+sessionID+" message_id="+j.e.MessageID)
			// TablesSent is read by AppendDelta under DataMu, so write it under DataMu.
			ss.DataMu.Lock()
			j.e.TablesSent = true
			ss.DataMu.Unlock()
		}
		if finalize {
			ss.DataMu.Lock()
			j.e.Sealed = true
			if relabel {
				j.e.Relabeled = true
			}
			ss.DataMu.Unlock()
		}
	}
}

// streamFlush is the Callbacks.StreamFlush implementation: drain then flush; Stop drains then closes.
func streamFlush(bs *types.BotState, sessionID string, stop bool) {
	if !stop {
		drainUntilComplete(bs, sessionID, 1500*time.Millisecond)
		flushSession(bs, sessionID, false, false)
		return
	}
	// Stop: loop drain→flush→TryClose until quiescent. A non-forced stop flush never seals an incomplete
	// last message, so a slightly-late final delta is still picked up next loop.
	bs.Streams.MarkStopRequested(sessionID)
	deadline := time.Now().Add(3 * time.Second)
	for {
		drainUntilComplete(bs, sessionID, 1500*time.Millisecond)
		if time.Now().After(deadline) {
			logger.Info(fmt.Sprintf("Stream force close: session=%s (final delta did not arrive within deadline; sealing partial)", sessionID))
			flushSession(bs, sessionID, true, true) // degraded: finalize+relabel the partial last message
			bs.Streams.MarkStopped(sessionID)
			return
		}
		flushSession(bs, sessionID, true, false)
		if bs.Streams.TryClose(sessionID) {
			return
		}
	}
}

// streamFlushAwaitNewText is the Callbacks.StreamFlushAwaitNewText implementation: the AskUserQuestion
// boundary flush. It waits for a NEW completed text bubble (drainForNewFinal) so the text preceding the
// question is sent before the question, then flushes. Scoped to AskQ; tool/Stop boundaries use streamFlush.
func streamFlushAwaitNewText(bs *types.BotState, sessionID string) {
	drainForNewFinal(bs, sessionID, 1000*time.Millisecond)
	flushSession(bs, sessionID, false, false)
}

// streamFlushAwaitToolBoundary is the Callbacks.StreamFlushAwaitToolBoundary implementation: the PreToolUse
// (non-AskUserQuestion) boundary flush under async hooks. It waits up to a 1.5s budget for the FIRST of:
//
//	B1 — a MessageDisplay-final for the same prompt_id becomes complete-but-unsealed (the pre-tool text),
//	B2 — the PostToolUse for this tool_use_id has already arrived (fast tool; off-FIFO signal), or
//	B3 — the budget elapses.
//
// In EVERY branch it flushes BEFORE returning, so the caller sends the tool notification AFTER any pre-tool
// text (flush-before-notify). Returns the resolving branch label for observability. No transcript read.
func streamFlushAwaitToolBoundary(bs *types.BotState, sessionID, promptID, toolUseID string) string {
	const budget = 1500 * time.Millisecond
	// b2Floor delays the B2 (PostToolUse-already-arrived) resolution until 500ms in, so a slow-to-arrive
	// pre-tool text bubble has time to land and win via B1 (flush-before-notify) instead of the tool
	// notification firing first. B1 is NOT floored — it still wins at any time, including inside the floor.
	const b2Floor = 500 * time.Millisecond
	start := time.Now()
	qd := bs.SessionEvents.QueueDepth(sessionID)
	branch := "b3_timeout"
	for {
		// B1 — the pre-tool text for this prompt is complete-but-unsealed. Wins at ANY time (incl. inside the
		// 500ms floor), so text is always flushed before the tool notification.
		if promptID != "" && bs.Streams.HasCompleteUnsealedForPrompt(sessionID, promptID) {
			branch = "b1_prompt_text"
			break
		}
		// B2 — the PostToolUse for this tool_use_id already arrived (fast/textless tool). Gated by the 500ms
		// floor AND the in-flight rule: do NOT fire while pre-tool text is still streaming for this prompt
		// (deltas without a final) — keep waiting for its final (B1) up to the full budget instead.
		if toolUseID != "" && time.Since(start) >= b2Floor {
			if _, ok := bs.Streams.GetPostToolArrived(sessionID, toolUseID); ok && !bs.Streams.HasInflightForPrompt(sessionID, promptID) {
				branch = "b2_posttool"
				break
			}
		}
		// B3 — budget elapsed.
		if time.Since(start) >= budget {
			branch = "b3_timeout"
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	logger.Info(fmt.Sprintf("tool-boundary wait done: session=%s prompt=%s tool_use_id=%s branch=%s elapsed=%dms queue_depth=%d",
		sessionID, promptID, toolUseID, branch, time.Since(start).Milliseconds(), qd))
	flushSession(bs, sessionID, false, false)
	return branch
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
				// Serialize the ticker flush onto the per-session SessionEvents worker so it is FIFO-ordered
				// with hook-event sends (e.g. PreToolUse tool notifications). DispatchAsync (guaranteed enqueue)
				// not TryDispatchAsync — an inline fallback would reintroduce the text-overtakes-tool race.
				bs.SessionEvents.DispatchAsync(sid, "flush:stream-ticker", func() error {
					flushSession(bs, sid, false, false)
					return nil
				})
			}
			bs.Streams.SweepEnded(60 * time.Second)           // drop ended-session tombstones older than TTL
			bs.Streams.SweepPostToolArrived(30 * time.Second) // drop never-consumed PostToolUse (B2) signals
		}
	}
}

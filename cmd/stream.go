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
			return
		}
		elapsed := time.Since(start)
		if !has && elapsed >= grace {
			return // no streamed text before this boundary
		}
		if elapsed >= timeout {
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
				flushSession(bs, sid, false, false)
			}
			bs.Streams.SweepEnded(60 * time.Second) // drop ended-session tombstones older than TTL
		}
	}
}

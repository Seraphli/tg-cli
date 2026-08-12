package cmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// busyDisplayName returns the session name, or the tmux pane-id (the part of TmuxTarget before
// "@") when the name is empty, so an unnamed session still renders a meaningful busy label.
func busyDisplayName(name, tmuxTarget string) string {
	if name != "" {
		return name
	}
	if t, err := injector.ParseTarget(tmuxTarget); err == nil && t.PaneID != "" {
		return t.PaneID
	}
	return tmuxTarget
}

// busyAction is the per-key action decided by decideBusyAction each 1s tick.
type busyAction int

const (
	busyNone        busyAction = iota
	busyCreate                 // no entry, session running → create placeholder + send
	busyRefloat                // running, message sent after SentAt → delete+resend to re-bottom
	busyEdit                   // running, 15s since last edit → update elapsed
	busyGraceStart             // just became idle → record IdleSince
	busyGraceDelete            // idle >=2s → delete the status message
)

// isErrNotFoundToDelete reports whether err is the telebot "message to delete not found" error.
func isErrNotFoundToDelete(err error) bool {
	return errors.Is(err, tele.ErrNotFoundToDelete)
}

// isErrNotFoundToEdit reports whether err is a Telegram "message to edit not found" 400.
// Telebot does not define this as a typed sentinel, so match by description string.
func isErrNotFoundToEdit(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "message to edit not found")
}

// statusText builds the busy-status message body.
// elapsed==0 renders as "(0s)"; elapsed>0 renders minutes+seconds like "2m15s" or "15s".
func statusText(label string, elapsed time.Duration) string {
	var dStr string
	if elapsed <= 0 {
		dStr = "0s"
	} else {
		totalSec := int(elapsed.Seconds())
		m := totalSec / 60
		s := totalSec % 60
		if m > 0 {
			dStr = fmt.Sprintf("%dm%ds", m, s)
		} else {
			dStr = fmt.Sprintf("%ds", s)
		}
	}
	return fmt.Sprintf("⏳ [%s] Working… (%s)", label, dStr)
}

// makeBusyMsg constructs a minimal tele.Message pointer for bot.Delete / bot.Edit.
func makeBusyMsg(chatID int64, topicID, msgID int) *tele.Message {
	return &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}, ThreadID: topicID}
}

// splitKey parses a "chatID:topicID" key into its components. Returns 0,0 on parse error.
func splitKey(key string) (int64, int) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	chatID, _ := strconv.ParseInt(parts[0], 10, 64)
	topicID, _ := strconv.Atoi(parts[1])
	return chatID, topicID
}

// routeKey returns the canonical key for a (chatID, topicID) pair.
func routeKey(chatID int64, topicID int) string {
	return fmt.Sprintf("%d:%d", chatID, topicID)
}

// routeSnapshot holds the aggregated per-key routing state for one 1s tick.
type routeSnapshot struct {
	running bool
	names   []string
}

// decideBusyAction decides what the busy manager should do for a given key this tick.
// e is nil when no persisted entry exists. running reflects whether ANY session is currently running
// on this route. lastMark is the most recent FloatMarker timestamp. inBackoff means Telegram is
// rate-limiting this chat. now is the current time.
func decideBusyAction(e *stores.BusyStatusEntry, running bool, lastMark time.Time, inBackoff bool, now time.Time) busyAction {
	// No persisted entry.
	if e == nil {
		if running {
			return busyCreate
		}
		return busyNone
	}
	// Entry exists, session running.
	if running {
		// In-flight placeholder (MsgID==0): wait for the create goroutine to complete.
		if e.MsgID == 0 {
			return busyNone
		}
		// Re-float: a real outbound message arrived after the status was last floated, and
		// enough time has passed since the previous float, and no flood backoff is active.
		if lastMark.After(e.SentAt) && now.Sub(e.LastFloatAt) >= 2*time.Second && !inBackoff {
			return busyRefloat
		}
		// Edit elapsed: 15s since last edit, no flood backoff.
		if now.Sub(e.LastEditAt) >= 15*time.Second && !inBackoff {
			return busyEdit
		}
		return busyNone
	}
	// Entry exists, session idle.
	if e.MsgID == 0 {
		return busyNone
	}
	if e.IdleSince.IsZero() {
		return busyGraceStart
	}
	if now.Sub(e.IdleSince) >= 2*time.Second {
		return busyGraceDelete
	}
	return busyNone
}

// startBusyIndicatorLoop runs the dedicated 1s busy-status management loop. It is NOT folded
// into the 3s typing loop (a 3s tick gives 3-6s grace; the spec requires 2-3s → 1s tick).
func startBusyIndicatorLoop(ctx context.Context, bs *BotState) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runBusyTick(bs)
		}
	}
}

// oauth401Signatures are the exact CC strings printed on an OAuth 401 (token revoked), observed in
// production 2026-07-19. They are Anthropic-CC-specific, so they self-scope to CC panes (a codex pane
// never matches) — no backend gating is needed.
var oauth401Signatures = []string{"Please run /login", "401 OAuth access token has been revoked"}

// tail401Lines bounds the pane region matched for a 401 signature to the last N lines, so a historical
// 401 scrolled up in the full capture (CapturePane uses -S -) is not re-matched after the session
// re-logs in and rearms.
const tail401Lines = 30

// matches401 returns the pane-tail line(s) that contain a 401 signature (verbatim, trimmed, de-duplicated,
// in order). An empty result means no 401 in the bounded tail.
func matches401(paneTail string) []string {
	lines := strings.Split(paneTail, "\n")
	if len(lines) > tail401Lines {
		lines = lines[len(lines)-tail401Lines:]
	}
	var matched []string
	seen := make(map[string]bool)
	for _, ln := range lines {
		for _, sig := range oauth401Signatures {
			if strings.Contains(ln, sig) {
				trimmed := strings.TrimSpace(ln)
				if trimmed != "" && !seen[trimmed] {
					seen[trimmed] = true
					matched = append(matched, trimmed)
				}
				break
			}
		}
	}
	return matched
}

// shouldWarn401OnTransition reports whether a target's running-state change warrants a 401 pane check: it
// fires ONCE on a true→false (busy→idle) transition and never otherwise. This doubles as the once-per-stall
// dedup — staying idle is false→false (no re-warn) and resuming is false→true (rearm for the next stall).
func shouldWarn401OnTransition(prevRunning, running bool) bool {
	return prevRunning && !running
}

// detect401AndWarn captures the pane for target and, if its bounded tail shows a 401 signature, sends ONE
// TG warning to the session's chat/topic carrying the verbatim matched line(s) + the pane id. Called from
// runBusyTick on a busy→idle transition. The send is plain text (the bot has no default ParseMode, so no
// HTML escaping), routed async on the Message FIFO so it never blocks the 1s tick.
func detect401AndWarn(bs *BotState, target string) {
	t, err := injector.ParseTarget(target)
	if err != nil {
		return
	}
	pane, err := injector.CapturePane(t)
	if err != nil {
		return
	}
	matched := matches401(pane)
	if len(matched) == 0 {
		return
	}
	chat, _, topicID := resolveChat(bs, target)
	if chat == nil {
		return
	}
	sid, _ := bs.SessionState.FindByTarget(target)
	// ⚠️ leads the verbatim CC 401 line(s) (kept as a contiguous block); the 📟 line identifies the session
	// by its FULL target form including the @socket path (e.g. "%3@/tmp/tmux-1000/default"). LONG form is
	// intentional (boss ruling): distinct tmux servers are distinguished ONLY by their socket path, so the
	// short pane id ("%3") is ambiguous across servers — the 📟 line must carry the full target EVERYWHERE.
	// This supersedes the earlier short-form (t.PaneID) change. Verbatim, no invented phrasing (boss ruling).
	notifyText := "⚠️ " + strings.Join(matched, "\n") + "\n📟 " + target
	logger.Info(fmt.Sprintf("401 detected on busy→idle: target=%s sid=%s matched=%q", target, sid, matched))
	c, txt, tid := chat, notifyText, topicID
	bs.MessageQueue.DispatchAsync(sid, "msg:401-warning", func() error {
		var opts []interface{}
		if tid > 0 {
			opts = append(opts, &tele.SendOptions{ThreadID: tid})
		}
		helpers.RetrySend(bs.Bot, c, txt, opts...)
		return nil
	})
}

// runBusyTick executes one 1s tick of the busy indicator manager.
func runBusyTick(bs *BotState) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		logger.Warn("busy indicator: LoadAppConfig failed: " + err.Error())
		return
	}
	now := time.Now()

	// f29 D: lazily init the per-target busy→idle transition map (owned by this serial loop).
	if bs.BusyPrevRunning == nil {
		bs.BusyPrevRunning = make(map[string]bool)
	}
	seenTargets := make(map[string]bool)

	// Build per-key route snapshot from live sessions.
	snapshot := make(map[string]*routeSnapshot)
	for _, info := range bs.SessionState.All() {
		target := info.TmuxTarget
		running := helpers.IsSessionRunning(bs.HookRunning, target)
		seenTargets[target] = true

		// f29 D: on a busy→idle transition, capture the pane tail and warn ONCE if it shows a 401
		// signature. Runs independent of the BusyStatus/route action logic below (and of the per-key
		// config-off return), per-target so a route with another running pane cannot mask this one.
		if shouldWarn401OnTransition(bs.BusyPrevRunning[target], running) {
			detect401AndWarn(bs, target)
		}
		bs.BusyPrevRunning[target] = running

		chat, _, topic := resolveChat(bs, target)
		if chat == nil {
			continue
		}
		key := routeKey(chat.ID, topic)
		rs := snapshot[key]
		if rs == nil {
			rs = &routeSnapshot{}
			snapshot[key] = rs
		}
		if running {
			rs.running = true
			rs.names = append(rs.names, busyDisplayName(info.Name, target))
		}
	}
	// f29 D: drop transition-map entries for targets no longer in SessionState (dead sessions).
	for target := range bs.BusyPrevRunning {
		if !seenTargets[target] {
			delete(bs.BusyPrevRunning, target)
		}
	}

	// Key set = UNION of snapshot keys and persisted keys so removed sessions still drive
	// their old status to idle-delete.
	allKeys := make(map[string]bool)
	for k := range snapshot {
		allKeys[k] = true
	}
	for _, e := range bs.BusyStatus.GetAll() {
		allKeys[routeKey(e.ChatID, e.TopicID)] = true
	}

	for key := range allKeys {
		key := key // capture for goroutine
		go func() {
			if !bs.BusyStatus.TryBeginAction(key) {
				return
			}
			defer bs.BusyStatus.EndAction(key)

			chatID, topicID := splitKey(key)
			if chatID == 0 {
				return
			}

			var ep *stores.BusyStatusEntry
			if entry, ok := bs.BusyStatus.Get(chatID, topicID); ok {
				e := entry
				ep = &e
			}

			rs := snapshot[key]
			running := rs != nil && rs.running
			label := ""
			if rs != nil && len(rs.names) > 0 {
				sorted := make([]string, len(rs.names))
				copy(sorted, rs.names)
				sort.Strings(sorted)
				label = strings.Join(sorted, ", ")
			}

			// Config off: drive all persisted statuses to deletion, never create new ones.
			if !cfg.BusyIndicatorEnabled() {
				if ep != nil && ep.MsgID != 0 {
					busyMsg := makeBusyMsg(chatID, topicID, ep.MsgID)
					if err := bs.Bot.Delete(busyMsg); err == nil || isErrNotFoundToDelete(err) {
						bs.BusyStatus.Delete(chatID, topicID)
						logger.Info(fmt.Sprintf("busy indicator disabled: deleted status chat=%d topic=%d msg_id=%d", chatID, topicID, ep.MsgID))
					} else {
						logger.Warn(fmt.Sprintf("busy indicator disabled: delete failed (retry next tick) chat=%d topic=%d msg_id=%d err=%v", chatID, topicID, ep.MsgID, err))
					}
				} else if ep != nil && ep.MsgID == 0 {
					bs.BusyStatus.Delete(chatID, topicID)
				}
				return
			}

			// Clear IdleSince on the running path to avoid grace-delete flicker when busy resumes.
			if ep != nil && running && !ep.IdleSince.IsZero() {
				ep.IdleSince = time.Time{}
				tmp := *ep
				bs.BusyStatus.Update(tmp)
				if epUpdated, ok := bs.BusyStatus.Get(chatID, topicID); ok {
					ep = &epUpdated
				}
			}

			lastMark := time.Time{}
			if helpers.FloatMarker != nil {
				lastMark = helpers.FloatMarker.LastMark(chatID, topicID)
			}
			inBackoff := false
			if helpers.FloodBackoff != nil {
				inBackoff = helpers.FloodBackoff.InBackoff(chatID, now)
			}

			action := decideBusyAction(ep, running, lastMark, inBackoff, now)

			switch action {
			case busyCreate:
				_, created := bs.BusyStatus.Reserve(chatID, topicID)
				if !created {
					// Race: another goroutine already reserved it; skip this tick.
					return
				}
				sent, sendErr := helpers.RetrySendNoFloat(bs.Bot, &tele.Chat{ID: chatID},
					statusText(label, 0),
					&tele.SendOptions{DisableNotification: true, ThreadID: topicID})
				if sendErr != nil || sent == nil {
					// Non-retryable or nil: remove the placeholder so the next tick can retry.
					bs.BusyStatus.Delete(chatID, topicID)
					if sendErr != nil {
						logger.Warn(fmt.Sprintf("busy status send failed (removed placeholder) chat=%d topic=%d err=%v", chatID, topicID, sendErr))
					}
					return
				}
				// f29 E: stamp SentAt/LastEditAt/LastFloatAt with a FRESH time.Now() taken AFTER the send
				// completes. The tick `now` (captured at the top of runBusyTick BEFORE the network I/O) is
				// stale, so a FloatMarker mark created DURING this send would never be consumed by SentAt and
				// would spuriously re-float one tick later. StartedAt keeps the tick `now` (elapsed anchor).
				sentNow := time.Now()
				e := stores.BusyStatusEntry{
					ChatID:      chatID,
					TopicID:     topicID,
					MsgID:       sent.ID,
					StartedAt:   now,
					SentAt:      sentNow,
					LastEditAt:  sentNow,
					LastFloatAt: sentNow,
				}
				bs.BusyStatus.Update(e)
				logger.Info(fmt.Sprintf("busy status sent chat=%d topic=%d msg_id=%d label=%q", chatID, topicID, sent.ID, label))

			case busyRefloat:
				busyMsg := makeBusyMsg(chatID, topicID, ep.MsgID)
				delErr := bs.Bot.Delete(busyMsg)
				if delErr != nil && !isErrNotFoundToDelete(delErr) {
					// Non-not-found error: skip resend this round to avoid duplicate status messages.
					logger.Warn(fmt.Sprintf("busy refloat: delete failed (skip resend) chat=%d topic=%d msg_id=%d err=%v", chatID, topicID, ep.MsgID, delErr))
					return
				}
				// Proceed: either deleted ok or already-gone (both are fine for resend).
				elapsed := now.Sub(ep.StartedAt)
				sent, sendErr := helpers.RetrySendNoFloat(bs.Bot, &tele.Chat{ID: chatID},
					statusText(label, elapsed),
					&tele.SendOptions{DisableNotification: true, ThreadID: topicID})
				if sendErr != nil || sent == nil {
					// Resend failed: remove the entry so next tick starts fresh.
					bs.BusyStatus.Delete(chatID, topicID)
					if sendErr != nil {
						logger.Warn(fmt.Sprintf("busy refloat: resend failed (removed entry) chat=%d topic=%d err=%v", chatID, topicID, sendErr))
					}
					return
				}
				// f29 E: fresh post-send stamp (see busyCreate) so the mark that triggered THIS re-float — or
				// a mark created during the delete+resend window — is consumed by SentAt and does not re-fire
				// one tick (~2s) later. StartedAt is untouched so the elapsed display stays anchored.
				sentNow := time.Now()
				ep.MsgID = sent.ID
				ep.SentAt = sentNow
				ep.LastFloatAt = sentNow
				ep.LastEditAt = sentNow
				bs.BusyStatus.Update(*ep)
				logger.Info(fmt.Sprintf("busy status re-floated chat=%d topic=%d new_msg_id=%d label=%q", chatID, topicID, sent.ID, label))

			case busyEdit:
				elapsed := now.Sub(ep.StartedAt)
				busyMsg := makeBusyMsg(chatID, topicID, ep.MsgID)
				_, editErr := helpers.RetryEdit(bs.Bot, busyMsg, statusText(label, elapsed))
				if editErr != nil {
					if isErrNotFoundToEdit(editErr) {
						// Message was externally deleted; recreate next tick.
						bs.BusyStatus.Delete(chatID, topicID)
						logger.Info(fmt.Sprintf("busy status edit: not-found, deleted entry chat=%d topic=%d msg_id=%d", chatID, topicID, ep.MsgID))
					} else {
						logger.Warn(fmt.Sprintf("busy status edit failed (retry next tick) chat=%d topic=%d msg_id=%d err=%v", chatID, topicID, ep.MsgID, editErr))
					}
					return
				}
				ep.LastEditAt = now
				bs.BusyStatus.Update(*ep)
				logger.Info(fmt.Sprintf("busy status edited chat=%d topic=%d msg_id=%d elapsed=%v", chatID, topicID, ep.MsgID, elapsed))

			case busyGraceStart:
				ep.IdleSince = now
				bs.BusyStatus.Update(*ep)
				logger.Info(fmt.Sprintf("busy status grace start chat=%d topic=%d msg_id=%d", chatID, topicID, ep.MsgID))

			case busyGraceDelete:
				busyMsg := makeBusyMsg(chatID, topicID, ep.MsgID)
				delErr := bs.Bot.Delete(busyMsg)
				if delErr == nil || isErrNotFoundToDelete(delErr) {
					bs.BusyStatus.Delete(chatID, topicID)
					logger.Info(fmt.Sprintf("busy status deleted chat=%d topic=%d msg_id=%d (idle grace expired)", chatID, topicID, ep.MsgID))
				} else {
					// Retain entry; the 1s loop retries because the key remains in the persisted set.
					logger.Warn(fmt.Sprintf("busy status grace delete failed (retained, retry next tick) chat=%d topic=%d msg_id=%d err=%v", chatID, topicID, ep.MsgID, delErr))
				}

			case busyNone:
				// Nothing to do this tick.
			}
		}()
	}
}

package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/hooks"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

// liveBusyState classifies a session's pane; a variable so a test can answer without a live pane.
var liveBusyState = helpers.LiveBusyState

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
	busyNone    busyAction = iota
	busyCreate             // no entry, session running → create placeholder + send
	busyRefloat            // running, message sent after SentAt → delete+resend to re-bottom
	busyEdit               // running, 15s since last edit → update elapsed
	busyDelete             // idle → delete the status message at the first idle tick
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
// elapsed==0 renders as "(0s)"; elapsed>0 renders minutes+seconds like "2m15s" or "15s". Waiting renders the
// cc stall banner text with the retry countdown copied from the pane; compacting renders its own text.
func statusText(label string, elapsed time.Duration, state stores.BusyState, countdown string) string {
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
	switch state {
	case stores.BusyWaiting:
		return fmt.Sprintf("⚠️ [%s] Waiting for API response · retry in %s (%s)", label, countdown, dStr)
	case stores.BusyCompacting:
		return fmt.Sprintf("🗜️ [%s] Compacting conversation (%s)", label, dStr)
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

// routeSnapshot holds the per-key routing state for one 1s tick: how many registered sessions resolve to the
// route, and the state, stall countdown and display name of the last one seen (the only one when sessions == 1).
type routeSnapshot struct {
	sessions  int
	state     stores.BusyState
	countdown string
	name      string
}

// decideBusyAction decides what the busy manager should do for a given key this tick.
// e is nil when no persisted entry exists. state is what the message must show now: idle when the one session of
// this route is not running (a route shared by two or more sessions never runs: it gets no message, like the
// private chat), else working, waiting or compacting.
// lastMark is the most recent FloatMarker timestamp. inBackoff means Telegram is rate-limiting this
// chat. now is the current time.
func decideBusyAction(e *stores.BusyStatusEntry, state stores.BusyState, lastMark time.Time, inBackoff bool, now time.Time) busyAction {
	running := state != stores.BusyIdle
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
		// Edit: the shown state changed (working, waiting, compacting) this tick, or 15s since the last edit
		// (refreshes the elapsed time and the countdown); no flood backoff.
		if (e.State != state || now.Sub(e.LastEditAt) >= 15*time.Second) && !inBackoff {
			return busyEdit
		}
		return busyNone
	}
	// Entry exists, session idle.
	if e.MsgID == 0 {
		return busyNone
	}
	return busyDelete
}

// startBusyIndicatorLoop runs the dedicated 1s busy-status management loop. It is NOT folded
// into the 3s typing loop: a 3s tick would delete the status message up to 3 s after the session goes idle; the
// 1s tick deletes it within 1 s.
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

// notifyCodexTurnError sends ONE Run Error notification when the codex turn that just ended carries an error in
// its rollout file: codex has no hook at an error turn end, so the file is the only source. Called from
// runBusyTick on the busy→idle change of a codex session (once per turn); the read and the send run on the
// Message FIFO so a slow disk never delays the 1s tick. A rollout that cannot be read sends nothing and writes
// one warn line.
func notifyCodexTurnError(bs *BotState, sid string, info stores.SessionInfo) {
	chat, chatID, topicID := helpers.ResolveChat(bs.SessionState, info.TmuxTarget)
	if chat == nil {
		return
	}
	bs.MessageQueue.DispatchAsync(sid, "msg:agent-error-notify", func() error {
		msg, err := helpers.CodexTurnError(info.TranscriptPath)
		if err != nil {
			logger.Warn(fmt.Sprintf("codex run error check: session %s: %v", sid, err))
		}
		if msg != "" {
			// The turn ended on an error and no Stop follows: the held queue no longer holds past it, and the pending
			// lines of an open compact tool message are edited in before the notice goes out below it.
			bs.InjectQueue.ClearHeld(info.TmuxTarget)
			if pending := bs.CompactTools.TakePending(sid); pending != nil {
				hooks.EditCompact(bs, sid, *pending, true)
			}
			sendEventNotification(bs, chat, chatID, sid, "AgentError", filepath.Base(info.CWD), info.CWD, info.TmuxTarget, "⚠️ codex run error\n\n"+msg, "", info.Name, topicID)
		}
		return nil
	})
}

// runBusyTick executes one 1s tick of the busy indicator manager.
func runBusyTick(bs *BotState) {
	now := time.Now()

	seenTargets := make(map[string]bool)

	// Build per-key route snapshot from live sessions. This loop classifies every session once per tick and
	// stores the state (the UserPromptSubmit hook also stores working at once); every other place reads the
	// stored value.
	privateChat := pairing.GetDefaultChatID()
	snapshot := make(map[string]*routeSnapshot)
	for sid, info := range bs.SessionState.All() {
		target := info.TmuxTarget
		prev, _ := bs.HookRunning.GetBusyState(target)
		state, countdown := liveBusyState(bs.HookRunning, target)
		bs.HookRunning.SetBusyState(target, state, countdown)
		seenTargets[target] = true

		// A codex turn that ended on an API error leaves no hook: its rollout file holds the error.
		if prev != stores.BusyIdle && state == stores.BusyIdle && info.Backend == "codex" {
			notifyCodexTurnError(bs, sid, info)
		}

		chat, _, topic := helpers.ResolveChat(bs.SessionState, target)
		// The private chat (the paired user's own chat) gets no busy status message: a stored one is deleted at the
		// first idle tick like any route with nothing running.
		if chat == nil || strconv.FormatInt(chat.ID, 10) == privateChat {
			continue
		}
		key := routeKey(chat.ID, topic)
		rs := snapshot[key]
		if rs == nil {
			rs = &routeSnapshot{}
			snapshot[key] = rs
		}
		rs.sessions++
		rs.state, rs.countdown, rs.name = state, countdown, busyDisplayName(info.Name, target)
	}
	// Drop the stored state of targets no longer in SessionState (dead sessions).
	bs.HookRunning.PruneBusyStates(seenTargets)

	cfg, err := config.LoadAppConfig()
	if err != nil {
		logger.Warn("busy indicator: LoadAppConfig failed: " + err.Error())
		return
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
			// A route shared by two or more registered sessions gets no busy status message (like the private
			// chat): it never runs, so a stored message is deleted at the first idle tick.
			shown := stores.BusyIdle
			if rs != nil && rs.sessions == 1 {
				shown = rs.state
			}
			running := shown != stores.BusyIdle
			label, countdown := "", ""
			if running {
				label, countdown = rs.name, rs.countdown
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

			lastMark := time.Time{}
			if helpers.FloatMarker != nil {
				lastMark = helpers.FloatMarker.LastMark(chatID, topicID)
			}
			inBackoff := false
			if helpers.FloodBackoff != nil {
				inBackoff = helpers.FloodBackoff.InBackoff(chatID, now)
			}

			action := decideBusyAction(ep, shown, lastMark, inBackoff, now)

			switch action {
			case busyCreate:
				_, created := bs.BusyStatus.Reserve(chatID, topicID)
				if !created {
					// Race: another goroutine already reserved it; skip this tick.
					return
				}
				sent, sendErr := helpers.RetrySendNoFloat(bs.Bot, &tele.Chat{ID: chatID},
					statusText(label, 0, shown, countdown),
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
					State:       shown,
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
					statusText(label, elapsed, shown, countdown),
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
				ep.State = shown
				bs.BusyStatus.Update(*ep)
				logger.Info(fmt.Sprintf("busy status re-floated chat=%d topic=%d new_msg_id=%d label=%q", chatID, topicID, sent.ID, label))

			case busyEdit:
				elapsed := now.Sub(ep.StartedAt)
				busyMsg := makeBusyMsg(chatID, topicID, ep.MsgID)
				_, editErr := helpers.RetryEdit(bs.Bot, busyMsg, statusText(label, elapsed, shown, countdown))
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
				ep.State = shown
				bs.BusyStatus.Update(*ep)
				logger.Info(fmt.Sprintf("busy status edited chat=%d topic=%d msg_id=%d elapsed=%v", chatID, topicID, ep.MsgID, elapsed))

			case busyDelete:
				busyMsg := makeBusyMsg(chatID, topicID, ep.MsgID)
				delErr := bs.Bot.Delete(busyMsg)
				if delErr == nil || isErrNotFoundToDelete(delErr) {
					bs.BusyStatus.Delete(chatID, topicID)
					logger.Info(fmt.Sprintf("busy status deleted chat=%d topic=%d msg_id=%d (session idle)", chatID, topicID, ep.MsgID))
				} else {
					// Retain entry; the 1s loop retries because the key remains in the persisted set.
					logger.Warn(fmt.Sprintf("busy status delete failed (retained, retry next tick) chat=%d topic=%d msg_id=%d err=%v", chatID, topicID, ep.MsgID, delErr))
				}

			case busyNone:
				// Nothing to do this tick.
			}
		}()
	}
}

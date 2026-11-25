package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

// ResolveChat finds the TG chat and topic for a tmux target.
// Priority: name route > session ID route > default chat. The "Route resolved" line is written when the session has
// no such line stored yet or its route differs from the stored one; an unchanged route writes nothing.
func ResolveChat(sessionState *stores.SessionStateStore, tmuxTarget string) (*tele.Chat, string, int) {
	routes, err := config.LoadRoutes()
	if err == nil && tmuxTarget != "" {
		sid, found := sessionState.FindByTarget(tmuxTarget)
		if found {
			info := sessionState.FindInfoByTarget(tmuxTarget)
			if info != nil && info.Name != "" {
				if route, ok := routes.NameRouteMap[info.Name]; ok {
					logRouteOnce(sessionState, sid, fmt.Sprintf("Route resolved: name=%s → chat=%d topic=%d (name route)", info.Name, route.ChatID, route.TopicID))
					return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
				}
			}
			if route, ok := routes.NameRouteMap[sid]; ok {
				logRouteOnce(sessionState, sid, fmt.Sprintf("Route resolved: sessionID=%s → chat=%d topic=%d (session route)", sid[:8], route.ChatID, route.TopicID))
				return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
			}
			sessionState.NoteRouteLine(sid, "")
		}
	}
	chatIDStr := pairing.GetDefaultChatID()
	if chatIDStr == "" {
		return nil, "", 0
	}
	chatIDInt, _ := strconv.ParseInt(chatIDStr, 10, 64)
	return &tele.Chat{ID: chatIDInt}, chatIDStr, 0
}

// logRouteOnce writes line unless it is the line last written for the session.
func logRouteOnce(sessionState *stores.SessionStateStore, sid, line string) {
	if sessionState.NoteRouteLine(sid, line) {
		logger.Info(line)
	}
}

// CleanDeadSession cleans up state for a dead tmux session.
func CleanDeadSession(
	sessionState *stores.SessionStateStore,
	pages *stores.PageCacheStore,
	sessionCounts *stores.SessionCountStore,
	injectQueue *stores.InjectQueueStore,
	hookRunning *stores.HookRunningStateStore,
	tmuxTarget string,
) {
	injectQueue.ClearTarget(tmuxTarget)
	hookRunning.ClearCCActivity(tmuxTarget)
	if sid, found := sessionState.FindByTarget(tmuxTarget); found {
		sessionState.Remove(sid)
		pages.CleanupSession(sid)
		sessionCounts.Cleanup(sid)
		// File-based pending cleanup removed; wait-store cleanup handled by CancelPendingWaitBySession
	}
}

// CleanupPendingState cleans up bot memory state and freezes TG buttons.
// Uses GetSnapshot(uuid) when uuid is available to look up entry data.
// EditFunc uses PendingMsgStore-provided msgID and chatID parameters.
func CleanupPendingState(
	bot *tele.Bot,
	pendingWait *stores.PendingWaitStore,
	pendingMsgStore *stores.PendingMsgStore,
	msgID int,
	uuid string,
	reason string,
) {
	if uuid != "" {
		snap, ok := pendingWait.GetSnapshot(uuid)
		if !ok {
			logger.Info(fmt.Sprintf("Stale pending cleanup: msg_id=%d uuid=%s reason=%s (entry not found)", msgID, uuid, reason))
			return
		}
		// Pre-build frozen markup before CAS
		var frozenMarkup *tele.ReplyMarkup
		var msgText string
		if snap.ToolName == "AskUserQuestion" {
			frozenMarkup = BuildFrozenMarkup(snap.Questions, "❌ Cancelled")
			msgText = snap.MsgText
		} else {
			msgText = snap.MsgText
			sugLabel, _ := ParseSuggestionLabel(snap.PermSuggestions)
			frozenMarkup = BuildFrozenPermMarkup("❌ Cancelled", sugLabel)
		}
		won, _, _ := pendingWait.ResolveIfUnresolved(uuid, stores.WaitEvent{Type: "cancel"})
		if won && frozenMarkup != nil {
			if pendingMsgStore != nil {
				capturedMarkup := frozenMarkup
				capturedRich := snap.Rich
				pendingMsgStore.EditOrDefer(uuid, func(eID int, eChatID int64, editMsgText string, topicID int) {
					editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: eChatID}}
					_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
					if err != nil {
						logger.Error(fmt.Sprintf("CleanupPendingState: EDIT failed msg_id=%d err=%v", eID, err))
					} else {
						logger.Info(fmt.Sprintf("CleanupPendingState: EDIT completed msg_id=%d", eID))
					}
				})
			} else {
				editMsg := &tele.Message{ID: snap.MsgID, Chat: &tele.Chat{ID: snap.ChatID}}
				RetryFreezeEditAuto(bot, editMsg, snap.Rich, msgText, frozenMarkup)
			}
		}
	}
	logger.Info(fmt.Sprintf("Stale pending cleanup: msg_id=%d uuid=%s reason=%s", msgID, uuid, reason))
}

// IsSessionRunning reports whether the stored busy state of the session is anything but idle: working, waiting
// or compacting. The 1 s busy loop (runBusyTick) stores it from the pane and the UserPromptSubmit hook stores
// working at once; this makes no tmux call.
func IsSessionRunning(hookRunning *stores.HookRunningStateStore, tmuxTarget string) bool {
	state, _ := hookRunning.GetBusyState(tmuxTarget)
	return state != stores.BusyIdle
}

// LiveBusyState classifies the session's pane now (title, backend, and the viewport of a cc, a pi or a busy
// codex pane) and stores nothing. Only the 1 s busy loop (which stores the result) and the unregistered-target
// branch of /session/idle (no stored entry can exist yet) call it. A closed pane (empty title) is idle before
// DetectBackend, so it costs no further tmux call.
func LiveBusyState(hookRunning *stores.HookRunningStateStore, tmuxTarget string) (stores.BusyState, string) {
	title := GetPaneTitle(tmuxTarget)
	if title == "" {
		return stores.BusyIdle, ""
	}
	return busyStateFor(context.Background(), hookRunning, DetectBackend(tmuxTarget), tmuxTarget, title)
}

// busyStateFor is the per-backend classification, unit-testable without a live pane: cc by the pane rules
// (ccBusyFromContent), pi by the hook store, codex by the pane title; a compaction is read from the pane for codex
// (only while its title spinner shows, so an idle pane costs no capture) and pi (every tick), and wins.
func busyStateFor(ctx context.Context, hookRunning *stores.HookRunningStateStore, backend, tmuxTarget, title string) (stores.BusyState, string) {
	switch backend {
	case "cc":
		return ccBusyFromContent(ctx, hookRunning, tmuxTarget)
	case "codex":
		if TitleIsBusy(backend, title) {
			if viewport, _ := capturePane(ctx, tmuxTarget); codexCompacting(viewport) {
				return stores.BusyCompacting, ""
			}
		}
	case "pi":
		if viewport, _ := capturePane(ctx, tmuxTarget); piCompacting(viewport) {
			return stores.BusyCompacting, ""
		}
	}
	if storeOrTitleBusy(hookRunning, backend, tmuxTarget, title) {
		return stores.BusyWorking, ""
	}
	return stores.BusyIdle, ""
}

// storeOrTitleBusy is pure and unit-testable without a live tmux pane: the store is
// passed in, never a package var. For a pi target it consults the run-state store
// first (return running when known); every other backend uses the title path.
//
// NORMATIVE (do not reorder): the pi branch is gated on THIS-TICK backend detection — the LIVE
// DetectBackend(target) == "pi" (the LiveBusyState path), NOT any stored/remembered backend — and the
// classifier sits AFTER the title == "" guard in LiveBusyState. When the pi process dies,
// pane_current_command is no longer "pi" -> detection returns "" -> the store is bypassed -> the pane
// reads idle (the dead-pi self-heal). A remembered backend would keep it busy forever. The title == ""
// guard first means a closed/dead pane returns idle regardless of the store. No timeout/reaper. No package var, no nil fallback — a required param fails loud if misused rather
// than silently degrading pi to the title path.
func storeOrTitleBusy(hookRunning *stores.HookRunningStateStore, backend, target, title string) bool {
	if backend == "pi" {
		if running, known := hookRunning.IsRunning(target); known {
			return running
		}
	}
	return TitleIsBusy(backend, title)
}

// TitleIsBusy is the pane-title busy classifier. cc no longer routes through here (LiveBusyState delegates
// cc classification to the content classifier ccBusyFromContent: viewport box + spinner scan). The cc arm below is RETAINED solely for the
// /session/idle start_codex readiness contract and the unit tests; codex (braille) and the pi run-state
// store path are unchanged. It trims first: tmux #{pane_title} can carry surrounding whitespace and BOTH
// rules are PREFIX rules, so leading whitespace would silently defeat them. cc is busy unless the title
// starts with the "✳" idle marker; codex is busy when the title starts with a braille spinner frame
// (U+2800–U+28FF, e.g. "⠙ project") and idle when it shows just the (possibly truncated) directory name —
// the braille-prefix test is robust to title truncation, unlike comparing against basename(cwd).
func TitleIsBusy(backend, title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return false
	}
	switch backend {
	case "cc":
		return !strings.HasPrefix(title, "✳")
	case "codex":
		r, _ := utf8.DecodeRuneInString(title)
		return r >= 0x2800 && r <= 0x28FF
	}
	return false
}

// CancelPermBySnapshot cancels a PermissionRequest using a snapshot (target-based path).
// Sends ESC to the tmux pane, resolves the entry, and defers an EDIT to freeze TG buttons.
// EditFunc uses PendingMsgStore-provided msgID and chatID parameters.
func CancelPermBySnapshot(bot *tele.Bot, pendingWait *stores.PendingWaitStore, pendingMsgStore *stores.PendingMsgStore, extractTarget func(string) string, snap stores.EntrySnapshot) {
	// Parse snap.TmuxTarget → send ESC
	target := extractTarget(snap.TmuxTarget)
	if target != "" {
		parsed, err := injector.ParseTarget(target)
		if err == nil {
			injector.SendKeys(parsed, "Escape")
		}
	}
	// ResolveIfUnresolved — if !won, return
	won, _, _ := pendingWait.ResolveIfUnresolved(snap.UUID, stores.WaitEvent{Type: "cancel"})
	if !won {
		return
	}
	// Build frozen perm markup from snap.PermSuggestions
	sugLabel, _ := ParseSuggestionLabel(snap.PermSuggestions)
	frozenMarkup := BuildFrozenPermMarkup("❌ Cancelled", sugLabel)
	capturedMarkup := frozenMarkup
	capturedRich := snap.Rich
	// EditOrDefer EDIT — editFunc uses PendingMsgStore-provided msgID/chatID
	pendingMsgStore.EditOrDefer(snap.UUID, func(msgID int, chatID int64, editMsgText string, topicID int) {
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
		if err != nil {
			logger.Error(fmt.Sprintf("CancelPermBySnapshot: EDIT failed uuid=%s err=%v", snap.UUID, err))
		} else {
			logger.Info(fmt.Sprintf("CancelPermBySnapshot: EDIT completed uuid=%s", snap.UUID))
		}
	})
}

// CancelAskBySnapshot cancels an AskUserQuestion using a snapshot (session-exit path).
// Sends ESC to the tmux pane, resolves the entry, and defers an EDIT to freeze TG buttons.
// Mirrors CancelPermBySnapshot but for AskUserQuestion entries.
func CancelAskBySnapshot(bot *tele.Bot, pendingWait *stores.PendingWaitStore, pendingMsgStore *stores.PendingMsgStore, extractTarget func(string) string, snap stores.EntrySnapshot) {
	// Parse snap.TmuxTarget → send ESC
	target := extractTarget(snap.TmuxTarget)
	if target != "" {
		parsed, err := injector.ParseTarget(target)
		if err == nil {
			injector.SendKeys(parsed, "Escape")
		}
	}
	// ResolveIfUnresolved — if !won, return
	won, _, _ := pendingWait.ResolveIfUnresolved(snap.UUID, stores.WaitEvent{Type: "cancel"})
	if !won {
		return
	}
	// Build frozen AskQ markup from snap.Questions
	if len(snap.Questions) == 0 {
		return
	}
	frozenMarkup := BuildFrozenMarkup(snap.Questions, "❌ Cancelled")
	capturedMarkup := frozenMarkup
	capturedRich := snap.Rich
	// EditOrDefer EDIT — editFunc uses PendingMsgStore-provided msgID/chatID
	pendingMsgStore.EditOrDefer(snap.UUID, func(msgID int, chatID int64, editMsgText string, topicID int) {
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
		if err != nil {
			logger.Error(fmt.Sprintf("CancelAskBySnapshot: EDIT failed uuid=%s err=%v", snap.UUID, err))
		} else {
			logger.Info(fmt.Sprintf("CancelAskBySnapshot: EDIT completed uuid=%s", snap.UUID))
		}
	})
}

// InterruptIfBusy is the busy-only interrupt of /t and /stop. It reports whether it sent an Escape. A pane that no
// longer exists is an error. An unresolved permission or AskUserQuestion dialog on the target counts as busy and is
// dismissed through CancelPermBySnapshot or CancelAskBySnapshot (Escape, entry resolved, buttons frozen); otherwise
// the live pane decides (LiveBusyState) and an idle pane gets nothing.
func InterruptIfBusy(bot *tele.Bot, pendingWait *stores.PendingWaitStore, pendingMsgStore *stores.PendingMsgStore, hookRunning *stores.HookRunningStateStore, extractTarget func(string) string, target injector.TmuxTarget) (bool, error) {
	if !injector.SessionExists(target) {
		return false, fmt.Errorf("tmux pane %s no longer exists", target.PaneID)
	}
	tmuxTarget := injector.FormatTarget(target)
	if snap, ok := pendingWait.FindByTmuxTarget(tmuxTarget); ok {
		if snap.ToolName == "AskUserQuestion" {
			CancelAskBySnapshot(bot, pendingWait, pendingMsgStore, extractTarget, *snap)
		} else {
			CancelPermBySnapshot(bot, pendingWait, pendingMsgStore, extractTarget, *snap)
		}
		return true, nil
	}
	if state, _ := LiveBusyState(hookRunning, tmuxTarget); state == stores.BusyIdle {
		return false, nil
	}
	return true, injector.SendKeys(target, "Escape")
}

// DoCancelPerm cancels a PermissionRequest via TG-button path (real msgID from callback).
// Uses FindByMsgIDSnapshot — safe because msgID is real Telegram callback ID.
// EditFunc uses PendingMsgStore-provided msgID and chatID parameters.
func DoCancelPerm(
	bot *tele.Bot,
	pendingWait *stores.PendingWaitStore,
	pendingMsgStore *stores.PendingMsgStore,
	extractTarget func(string) (*injector.TmuxTarget, error),
	msgID int,
) string {
	snap, ok := pendingWait.FindByMsgIDSnapshot(msgID)
	if !ok {
		return ""
	}
	uuid := snap.UUID
	// Pre-build frozen markup before CAS
	sugLabel, _ := ParseSuggestionLabel(snap.PermSuggestions)
	frozenMarkup := BuildFrozenPermMarkup("❌ Cancelled", sugLabel)
	targetPtr, err := extractTarget(snap.MsgText)
	if err == nil && targetPtr != nil {
		injector.SendKeys(*targetPtr, "Escape")
	}
	won, _, _ := pendingWait.ResolveIfUnresolved(uuid, stores.WaitEvent{Type: "cancel"})
	if won && pendingMsgStore != nil {
		capturedMarkup := frozenMarkup
		capturedRich := snap.Rich
		pendingMsgStore.EditOrDefer(uuid, func(eID int, eChatID int64, editMsgText string, topicID int) {
			editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: eChatID}}
			_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
			if err != nil {
				logger.Error(fmt.Sprintf("DoCancelPerm: EDIT failed msg_id=%d err=%v", eID, err))
			} else {
				logger.Info(fmt.Sprintf("DoCancelPerm: EDIT completed msg_id=%d", eID))
			}
		})
	}
	logger.Info(fmt.Sprintf("Permission cancelled: msg_id=%d uuid=%s", msgID, uuid))
	return uuid
}

// IsRouteToolNotifyOff checks if the resolved route for a tmux target has ToolNotifyOff=true.
func IsRouteToolNotifyOff(ss *stores.SessionStateStore, tmuxTarget string) bool {
	routes, err := config.LoadRoutes()
	if err != nil {
		return false
	}
	info := ss.FindInfoByTarget(tmuxTarget)
	if info == nil {
		return false
	}
	if info.Name != "" {
		if route, ok := routes.NameRouteMap[info.Name]; ok {
			return route.ToolNotifyOff
		}
	}
	sid, found := ss.FindByTarget(tmuxTarget)
	if found {
		if route, ok := routes.NameRouteMap[sid]; ok {
			return route.ToolNotifyOff
		}
	}
	return false
}

// GetPrivateChat returns the default private chat.
func GetPrivateChat() (*tele.Chat, string, int) {
	chatIDStr := pairing.GetDefaultChatID()
	if chatIDStr == "" {
		return nil, "", 0
	}
	chatIDInt, _ := strconv.ParseInt(chatIDStr, 10, 64)
	return &tele.Chat{ID: chatIDInt}, chatIDStr, 0
}

// DoDecidePerm resolves a PermissionRequest: ResolveIfUnresolved + EditOrDefer.
// Uses FindByMsgIDSnapshot (TG-button path — safe because msgID is real Telegram callback ID).
// EditFunc uses PendingMsgStore-provided msgID and chatID parameters.
func DoDecidePerm(
	bot *tele.Bot,
	pendingWait *stores.PendingWaitStore,
	reactionTracker *stores.ReactionTrackerStore,
	pendingMsgStore *stores.PendingMsgStore,
	checkAlive func(string) bool,
	extractTarget func(string) (*injector.TmuxTarget, error),
	msgID int,
	decision string,
) (*PermDecision, error) {
	snap, ok := pendingWait.FindByMsgIDSnapshot(msgID)
	if !ok {
		return nil, fmt.Errorf("no pending permission for msg_id %d", msgID)
	}
	if snap.TmuxTarget != "" && !checkAlive(snap.TmuxTarget) {
		return nil, fmt.Errorf("session disconnected")
	}
	uuid := snap.UUID
	// Pre-build frozen markup before CAS
	sugLabel, _ := ParseSuggestionLabel(snap.PermSuggestions)
	frozenMarkup := BuildFrozenPermMarkup(decision, sugLabel)
	d, err := resolvePermissionFromSnap(snap, decision)
	if err != nil {
		return nil, err
	}
	var updatedPerms []interface{}
	if d.UpdatedPermissions != nil {
		var perms []interface{}
		json.Unmarshal(d.UpdatedPermissions, &perms)
		updatedPerms = perms
	}
	ccOutput := BuildPermCCOutput(d.Behavior, d.Message, updatedPerms)
	won, _, _ := pendingWait.ResolveIfUnresolved(uuid, stores.WaitEvent{
		Type:   "answer",
		Output: ccOutput,
	})
	if won && pendingMsgStore != nil {
		capturedMarkup := frozenMarkup
		capturedRich := snap.Rich
		pendingMsgStore.EditOrDefer(uuid, func(eID int, eChatID int64, editMsgText string, topicID int) {
			editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: eChatID}}
			_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgText, capturedMarkup)
			if err != nil {
				logger.Error(fmt.Sprintf("DoDecidePerm: EDIT failed msg_id=%d decision=%s err=%v", eID, decision, err))
			} else {
				logger.Info(fmt.Sprintf("DoDecidePerm: EDIT completed msg_id=%d decision=%s", eID, decision))
			}
		})
	}
	logger.Info(fmt.Sprintf("Permission resolved: msg_id=%d decision=%s uuid=%s", msgID, decision, uuid))
	targetPtr, err2 := extractTarget(snap.MsgText)
	if err2 == nil && targetPtr != nil {
		reactionTracker.RecordPending(bot, injector.FormatTarget(*targetPtr), snap.ChatID, msgID)
	}
	return &d, nil
}

package helpers

import (
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
// Priority: name route > session ID route > default chat.
func ResolveChat(sessionState *stores.SessionStateStore, tmuxTarget string) (*tele.Chat, string, int) {
	creds, err := config.LoadCredentials()
	if err == nil && tmuxTarget != "" {
		sid, found := sessionState.FindByTarget(tmuxTarget)
		if found {
			info := sessionState.FindInfoByTarget(tmuxTarget)
			if info != nil && info.Name != "" {
				if route, ok := creds.NameRouteMap[info.Name]; ok {
					logger.Info(fmt.Sprintf("Route resolved: name=%s → chat=%d topic=%d (name route)", info.Name, route.ChatID, route.TopicID))
					return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
				}
			}
			if route, ok := creds.NameRouteMap[sid]; ok {
				logger.Info(fmt.Sprintf("Route resolved: sessionID=%s → chat=%d topic=%d (session route)", sid[:8], route.ChatID, route.TopicID))
				return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
			}
		}
	}
	chatIDStr := pairing.GetDefaultChatID()
	if chatIDStr == "" {
		return nil, "", 0
	}
	chatIDInt, _ := strconv.ParseInt(chatIDStr, 10, 64)
	return &tele.Chat{ID: chatIDInt}, chatIDStr, 0
}

// CleanDeadSession cleans up state for a dead tmux session.
func CleanDeadSession(
	sessionState *stores.SessionStateStore,
	pages *stores.PageCacheStore,
	sessionCounts *stores.SessionCountStore,
	injectQueue *stores.InjectQueueStore,
	tmuxTarget string,
) {
	injectQueue.ClearTarget(tmuxTarget)
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

// IsSessionRunning checks if a CLI session is actively running by examining the tmux pane.
func IsSessionRunning(hookRunning *stores.HookRunningStateStore, tmuxTarget string) bool {
	title := GetPaneTitle(tmuxTarget)
	if title == "" {
		return false // dead/closed pane -> idle, BEFORE the store
	}
	backend := DetectBackend(tmuxTarget) // LIVE detection, at call time
	return storeOrTitleBusy(hookRunning, backend, tmuxTarget, title)
}

// storeOrTitleBusy is pure and unit-testable without a live tmux pane: the store is
// passed in, never a package var. For a pi target it consults the run-state store
// first (return running when known); every other backend uses the title path.
//
// NORMATIVE (do not reorder): the pi branch is gated on THIS-TICK backend detection — either the LIVE
// DetectBackend(target) == "pi" (the IsSessionRunning path) OR the batched detectBackend(info.Command,
// children) snapshot computed on the same tick (the PaneState typing-tick path). Both are current-tick
// (NOT any stored/remembered backend), and the classifier sits AFTER the title == "" guard in each caller.
// When the pi process dies, pane_current_command is no longer "pi" -> detection returns "" -> the store is
// bypassed -> the pane reads idle (the dead-pi self-heal holds on BOTH paths). A remembered backend would
// keep it busy forever. The title == "" guard first means a closed/dead pane returns idle regardless of the
// store. No timeout/reaper. No package var, no nil fallback — a required param fails loud if misused rather
// than silently degrading pi to the title path.
func storeOrTitleBusy(hookRunning *stores.HookRunningStateStore, backend, target, title string) bool {
	if backend == "pi" {
		if running, known := hookRunning.IsRunning(target); known {
			return running
		}
	}
	return TitleIsBusy(backend, title)
}

// TitleIsBusy is the single busy classifier for every pane-title decision — the live path
// (IsSessionRunning), the batched path (PaneState), and the /session/idle start_codex readiness gate all
// route through here. It trims first: tmux #{pane_title} can carry surrounding whitespace and BOTH rules
// are PREFIX rules, so leading whitespace would silently defeat them. cc is busy unless the title starts
// with the "✳" idle marker; codex is busy when the title starts with a braille spinner frame
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
	creds, err := config.LoadCredentials()
	if err != nil {
		return false
	}
	info := ss.FindInfoByTarget(tmuxTarget)
	if info == nil {
		return false
	}
	if info.Name != "" {
		if route, ok := creds.NameRouteMap[info.Name]; ok {
			return route.ToolNotifyOff
		}
	}
	sid, found := ss.FindByTarget(tmuxTarget)
	if found {
		if route, ok := creds.NameRouteMap[sid]; ok {
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

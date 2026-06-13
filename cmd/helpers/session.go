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
	tmuxTarget string,
) {
	if sid, found := sessionState.FindByTarget(tmuxTarget); found {
		sessionState.Remove(sid)
		pages.CleanupSession(sid)
		sessionCounts.Cleanup(sid)
		// File-based pending cleanup removed; wait-store cleanup handled by CancelPendingWaitBySession
	}
}

// CleanupPendingState cleans up bot memory state and freezes TG buttons.
func CleanupPendingState(
	bot *tele.Bot,
	toolNotifs *stores.ToolNotifyStore,
	pendingPerms *stores.PendingPermStore,
	pendingFiles *stores.PendingFileStore,
	pendingWait *stores.PendingWaitStore,
	msgID int,
	uuid string,
	reason string,
) {
	if entry, ok := toolNotifs.Get(msgID); ok && !entry.Resolved {
		toolNotifs.MarkResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(bot, editMsg, entry.MsgText, BuildFrozenMarkup(entry, "❌ Cancelled"), tele.ModeHTML)
	}
	if _, ok := pendingPerms.GetTarget(msgID); ok {
		pendingPerms.Resolve(msgID, stores.PermDecision{Behavior: "deny", Message: "Cancelled (hook dead)"})
	}
	if uuid != "" {
		pendingWait.Push(uuid, stores.WaitEvent{Type: "cancel"})
	}
	pendingFiles.Remove(msgID)
	logger.Info(fmt.Sprintf("Stale pending cleanup: msg_id=%d uuid=%s reason=%s", msgID, uuid, reason))
}

// IsSessionRunning checks if a CLI session is actively running by examining the tmux pane.
func IsSessionRunning(tmuxTarget string) bool {
	title := GetPaneTitle(tmuxTarget)
	if title == "" {
		return false
	}
	switch DetectBackend(tmuxTarget) {
	case "cc":
		return !strings.HasPrefix(title, "✳")
	case "codex":
		return IsCodexBusyTitle(title)
	default:
		return false
	}
}

// IsCodexBusyTitle reports whether a codex pane title indicates the CLI is busy.
// Codex prefixes its pane title with a braille spinner frame (U+2800–U+28FF,
// e.g. "⠙ project") while working, and shows just the (possibly truncated)
// directory name when idle. Detecting the braille-block prefix is robust to
// title truncation, unlike comparing against basename(cwd).
func IsCodexBusyTitle(title string) bool {
	r, _ := utf8.DecodeRuneInString(title)
	return r >= 0x2800 && r <= 0x28FF
}

// RecordPending records a message for later ✍ reaction when UserPromptSubmit fires.
func RecordPending(reactionTracker *stores.ReactionTrackerStore, tmuxTarget string, chatID int64, msgID int) {
	reactionTracker.RecordPending(tmuxTarget, chatID, msgID)
}

// DoCancelPerm cancels a PermissionRequest: push cancel to wait store + ESC + resolve + edit TG msg.
func DoCancelPerm(
	bot *tele.Bot,
	pendingPerms *stores.PendingPermStore,
	pendingFiles *stores.PendingFileStore,
	pendingWait *stores.PendingWaitStore,
	extractTarget func(string) (*injector.TmuxTarget, error),
	msgID int,
) string {
	sugLabel, _ := ParseSuggestionLabel(pendingPerms.GetSuggestions(msgID))
	uuid, _ := pendingFiles.Get(msgID)
	if uuid != "" {
		// Push cancel to wait store (no Remove — live handler removes on delivery)
		pendingWait.Push(uuid, stores.WaitEvent{Type: "cancel"})
	}
	msgText := pendingPerms.GetMsgText(msgID)
	chatID := pendingPerms.GetChatID(msgID)
	targetPtr, err := extractTarget(msgText)
	if err == nil && targetPtr != nil {
		injector.SendKeys(*targetPtr, "Escape")
	}
	pendingPerms.Resolve(msgID, stores.PermDecision{Behavior: "deny", Message: "Cancelled by user (Esc)"})
	if chatID != 0 && msgText != "" {
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		RetryEdit(bot, editMsg, msgText, BuildFrozenPermMarkup("❌ Cancelled", sugLabel), tele.ModeHTML)
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

// DoDecidePerm resolves a PermissionRequest: resolve + push answer to wait store + edit + recordPending.
func DoDecidePerm(
	bot *tele.Bot,
	pendingPerms *stores.PendingPermStore,
	pendingFiles *stores.PendingFileStore,
	pendingWait *stores.PendingWaitStore,
	reactionTracker *stores.ReactionTrackerStore,
	checkAlive func(string) bool,
	extractTarget func(string) (*injector.TmuxTarget, error),
	msgID int,
	decision string,
) (*stores.PermDecision, error) {
	if permTarget, ok := pendingPerms.GetTarget(msgID); ok && permTarget != "" && !checkAlive(permTarget) {
		return nil, fmt.Errorf("session disconnected")
	}
	uuid, uuidOk := pendingPerms.GetUUID(msgID)
	if !uuidOk {
		uuid, uuidOk = pendingFiles.Get(msgID)
	}
	sugLabel, _ := ParseSuggestionLabel(pendingPerms.GetSuggestions(msgID))
	msgText := pendingPerms.GetMsgText(msgID)
	chatID := pendingPerms.GetChatID(msgID)
	d, err := ResolvePermission(pendingPerms, msgID, decision, nil)
	if err != nil {
		return nil, err
	}
	if uuidOk {
		var updatedPerms []interface{}
		if d.UpdatedPermissions != nil {
			var perms []interface{}
			json.Unmarshal(d.UpdatedPermissions, &perms)
			updatedPerms = perms
		}
		ccOutput := BuildPermCCOutput(d.Behavior, d.Message, updatedPerms)
		WritePendingAnswer(pendingWait, uuid, ccOutput)
	}
	logger.Info(fmt.Sprintf("Permission resolved: msg_id=%d decision=%s uuid=%s", msgID, decision, uuid))
	if chatID != 0 && msgText != "" {
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		RetryEdit(bot, editMsg, msgText, BuildFrozenPermMarkup(decision, sugLabel), tele.ModeHTML)
	}
	targetPtr, err2 := extractTarget(msgText)
	if err2 == nil && targetPtr != nil {
		reactionTracker.RecordPending(injector.FormatTarget(*targetPtr), chatID, msgID)
	}
	return &d, nil
}

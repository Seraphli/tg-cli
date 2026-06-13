package helpers

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// CanonicalToolInput returns a stable sorted-key JSON string for correlation.
// Unmarshals to map[string]any (encoding/json sorts map keys on marshal) then re-marshals.
// Non-object inputs (arrays, scalars) are returned as compact raw JSON.
func CanonicalToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		b, _ := json.Marshal(obj)
		return string(b)
	}
	// Non-object: return compacted raw
	b, err := json.Marshal(json.RawMessage(raw))
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// FormatAnswers joins answer values into a short single-line string for display.
func FormatAnswers(answers map[string]string) string {
	var parts []string
	for _, v := range answers {
		parts = append(parts, v)
	}
	return strings.Join(parts, " / ")
}

// FreezeWaitEntryOnDesktop edits the stored TG message for a PendingWaitEntry using the bot.
func FreezeWaitEntryOnDesktop(bot *tele.Bot, toolNotifs *stores.ToolNotifyStore, pendingPerms *stores.PendingPermStore, entry *stores.PendingWaitEntry, label string) {
	if entry.ToolName == "AskUserQuestion" {
		if notifEntry, ok := toolNotifs.Get(entry.MsgID); ok {
			editMsg := &tele.Message{ID: entry.MsgID, Chat: &tele.Chat{ID: entry.ChatID}}
			RetryEdit(bot, editMsg, notifEntry.MsgText, BuildFrozenMarkup(notifEntry, label), tele.ModeHTML)
			logger.Info(fmt.Sprintf("FreezeWaitEntry(AskQ): msg_id=%d label=%s", entry.MsgID, label))
		}
		return
	}
	// PermissionRequest
	msgText := pendingPerms.GetMsgText(entry.MsgID)
	sugLabel, _ := ParseSuggestionLabel(pendingPerms.GetSuggestions(entry.MsgID))
	if msgText != "" {
		editMsg := &tele.Message{ID: entry.MsgID, Chat: &tele.Chat{ID: entry.ChatID}}
		RetryEdit(bot, editMsg, msgText, BuildFrozenPermMarkup(label, sugLabel), tele.ModeHTML)
		logger.Info(fmt.Sprintf("FreezeWaitEntry(Perm): msg_id=%d label=%s", entry.MsgID, label))
	}
}

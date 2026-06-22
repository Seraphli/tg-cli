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

// FreezeWaitEntryOnDesktop edits the stored TG message for a pending entry using the bot.
// Takes an EntrySnapshot to avoid data races. Uses ResolveIfUnresolved for atomic CAS,
// then TryEnqueue EDIT to avoid data race B3. EditFunc uses worker-provided msgID/chatID.
func FreezeWaitEntryOnDesktop(bot *tele.Bot, pendingWait *stores.PendingWaitStore, opQueue *stores.NotifOpQueue, snap stores.EntrySnapshot, label string) {
	// Pre-build frozen markup BEFORE CAS (data race B3 prevention)
	var frozenMarkup *tele.ReplyMarkup
	if snap.ToolName == "AskUserQuestion" {
		frozenMarkup = BuildFrozenMarkup(snap.Questions, label)
	} else {
		sugLabel, _ := ParseSuggestionLabel(snap.PermSuggestions)
		frozenMarkup = BuildFrozenPermMarkup(label, sugLabel)
	}
	if frozenMarkup == nil {
		return
	}
	won, _, _ := pendingWait.ResolveIfUnresolved(snap.UUID, stores.WaitEvent{Type: "cancel"})
	if !won {
		return
	}
	capturedLabel := label
	capturedMarkup := frozenMarkup
	opQueue.TryEnqueue(stores.NotifOp{
		Type:         stores.OpEDIT,
		UUID:         snap.UUID,
		FreezeLabel:  capturedLabel,
		FrozenMarkup: capturedMarkup,
		EditFunc: func(msgID int, chatID int64, editMsgText string) {
			editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
			RetryEdit(bot, editMsg, editMsgText, capturedMarkup, tele.ModeHTML)
			logger.Info(fmt.Sprintf("FreezeWaitEntry: msg_id=%d label=%s", msgID, capturedLabel))
		},
	})
}

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
// Takes an EntrySnapshot to avoid data races. B.8: the STATE (BuildFrozenMarkup + ResolveIfUnresolved)
// stays on the Hook FIFO; the EditOrDefer callback ONLY captures the coords and enqueues a msg:freeze-edit
// op onto the Message FIFO that performs the actual freeze edit I/O (INV2) — for BOTH the immediate
// (coords already known) AND the deferred (SetAndDrain) paths. The freeze SEMANTICS are unchanged: still
// exactly-once (ResolveIfUnresolved CAS) and still both PendingMsgStore paths.
func FreezeWaitEntryOnDesktop(bot *tele.Bot, messageQueue *stores.SessionEventStore, pendingWait *stores.PendingWaitStore, pendingMsgStore *stores.PendingMsgStore, snap stores.EntrySnapshot, label string) {
	// Pre-build frozen markup BEFORE CAS (data race B3 prevention) — STATE on the Hook FIFO.
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
	capturedRich := snap.Rich
	capturedSession := snap.SessionID
	pendingMsgStore.EditOrDefer(snap.UUID, func(msgID int, chatID int64, editMsgText string, topicID int) {
		// I/O-only callback: capture the resolved coords and enqueue msg:freeze-edit onto the Message FIFO
		// (INV2). Fire-and-forget — no returned msg_id is reused, so DispatchAsync in Hook-FIFO order.
		editMsgTextCaptured := editMsgText
		messageQueue.DispatchAsync(capturedSession, "msg:freeze-edit", func() error {
			editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
			_, err := RetryFreezeEditAuto(bot, editMsg, capturedRich, editMsgTextCaptured, capturedMarkup)
			if err != nil {
				logger.Error(fmt.Sprintf("FreezeWaitEntry: EDIT failed msg_id=%d label=%s err=%v", msgID, capturedLabel, err))
			} else {
				logger.Info(fmt.Sprintf("FreezeWaitEntry: EDIT completed msg_id=%d label=%s", msgID, capturedLabel))
			}
			return nil
		})
	})
}

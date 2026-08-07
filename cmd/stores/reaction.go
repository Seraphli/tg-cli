package stores

import (
	"fmt"
	"sync"

	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// ReactionEntry tracks a Telegram message that is showing the ✍ receipt-acknowledgement reaction.
type ReactionEntry struct {
	ChatID int64
	MsgID  int
}

// ReactionTrackerStore tracks messages currently showing the ✍ receipt-acknowledgement reaction,
// keyed by tmux target. The ✍ is set the moment a message is received/injected (RecordPending) and
// cleared once CC has started processing the prompt (ClearReactions, driven by UserPromptSubmit).
type ReactionTrackerStore struct {
	mu      sync.Mutex
	showing map[string][]ReactionEntry
}

// NewReactionTrackerStore creates an empty ReactionTrackerStore.
func NewReactionTrackerStore() *ReactionTrackerStore {
	return &ReactionTrackerStore{
		showing: make(map[string][]ReactionEntry),
	}
}

// RecordPending records a received/injected message for the given tmux target and immediately sets
// the ✍ reaction to acknowledge receipt. The reaction is cleared once CC starts processing the
// prompt (ClearReactions).
func (rt *ReactionTrackerStore) RecordPending(bot *tele.Bot, tmuxTarget string, chatID int64, msgID int) {
	rt.mu.Lock()
	rt.showing[tmuxTarget] = append(rt.showing[tmuxTarget], ReactionEntry{ChatID: chatID, MsgID: msgID})
	rt.mu.Unlock()
	setReaction(bot, chatID, msgID, "✍")
	logger.Debug(fmt.Sprintf("Reaction set (receipt ack): target=%s msg_id=%d emoji=✍", tmuxTarget, msgID))
}

// TakeReactions removes and returns every tracked reaction entry for the target — the STATE half of the
// clear (B.7). It mutates only the in-memory map (no TG I/O), so it stays on the Hook FIFO; the caller
// enqueues ApplyClear onto the Message FIFO (INV2).
func (rt *ReactionTrackerStore) TakeReactions(tmuxTarget string) []ReactionEntry {
	rt.mu.Lock()
	entries := rt.showing[tmuxTarget]
	delete(rt.showing, tmuxTarget)
	rt.mu.Unlock()
	return entries
}

// ApplyClear removes the ✍ reaction from each captured entry — the pure I/O half of the clear (B.7).
// Runs inside a Message-FIFO op (INV2).
func (rt *ReactionTrackerStore) ApplyClear(bot *tele.Bot, entries []ReactionEntry) {
	for _, e := range entries {
		setReaction(bot, e.ChatID, e.MsgID)
	}
	if len(entries) > 0 {
		logger.Debug(fmt.Sprintf("Reactions cleared: cleared=%d", len(entries)))
	}
}

// ClearReactions removes the ✍ reaction from every message recorded for the target and drops the
// records. Called when CC has started processing the prompt (via UserPromptSubmit), so the receipt
// acknowledgement is no longer needed. Retained for the OFF-Hook-FIFO callers (SafeInjectText background
// injects) — the Hook-FIFO PreToolUse path uses the TakeReactions/ApplyClear split instead (B.7).
func (rt *ReactionTrackerStore) ClearReactions(bot *tele.Bot, tmuxTarget string) {
	rt.ApplyClear(bot, rt.TakeReactions(tmuxTarget))
}

// buildReactionPayload builds the setMessageReaction payload. With no emojis the reaction list is an
// empty (non-nil) slice, which serializes to [] and Telegram interprets as "remove all reactions".
func buildReactionPayload(chatID int64, msgID int, emojis ...string) map[string]interface{} {
	reaction := make([]interface{}, 0, len(emojis))
	for _, e := range emojis {
		reaction = append(reaction, map[string]interface{}{"type": "emoji", "emoji": e})
	}
	return map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
		"reaction":   reaction,
	}
}

// setReaction sets the given emoji reactions on a message; passing no emojis clears all reactions.
func setReaction(bot *tele.Bot, chatID int64, msgID int, emojis ...string) {
	if bot == nil {
		return
	}
	bot.Raw("setMessageReaction", buildReactionPayload(chatID, msgID, emojis...))
}

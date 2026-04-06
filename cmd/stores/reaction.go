package stores

import (
	"fmt"
	"sync"

	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// ReactionEntry tracks a Telegram message that should receive an emoji reaction.
type ReactionEntry struct {
	ChatID int64
	MsgID  int
}

// ReactionTrackerStore tracks pending and active emoji reactions per tmux target.
type ReactionTrackerStore struct {
	mu      sync.Mutex
	pending map[string][]ReactionEntry // injected, waiting for UserPromptSubmit
	active  map[string][]ReactionEntry // confirmed by UserPromptSubmit, showing ✍
}

// NewReactionTrackerStore creates an empty ReactionTrackerStore.
func NewReactionTrackerStore() *ReactionTrackerStore {
	return &ReactionTrackerStore{
		pending: make(map[string][]ReactionEntry),
		active:  make(map[string][]ReactionEntry),
	}
}

// RecordPending adds an entry to the pending set for the given tmux target.
func (rt *ReactionTrackerStore) RecordPending(tmuxTarget string, chatID int64, msgID int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.pending[tmuxTarget] = append(rt.pending[tmuxTarget], ReactionEntry{ChatID: chatID, MsgID: msgID})
	logger.Debug(fmt.Sprintf("Reaction pending recorded: target=%s msg_id=%d", tmuxTarget, msgID))
}

// PromotePending moves pending entries to active and sets the ✍ reaction via the bot.
func (rt *ReactionTrackerStore) PromotePending(bot *tele.Bot, tmuxTarget string) {
	rt.mu.Lock()
	newEntries := rt.pending[tmuxTarget]
	delete(rt.pending, tmuxTarget)
	if len(newEntries) > 0 {
		rt.active[tmuxTarget] = append(rt.active[tmuxTarget], newEntries...)
	}
	rt.mu.Unlock()

	// Set ✍ on newly promoted entries (confirmed by UserPromptSubmit)
	for _, e := range newEntries {
		bot.Raw("setMessageReaction", map[string]interface{}{
			"chat_id":    e.ChatID,
			"message_id": e.MsgID,
			"reaction":   []interface{}{map[string]interface{}{"type": "emoji", "emoji": "✍"}},
		})
	}
	if len(newEntries) > 0 {
		logger.Debug(fmt.Sprintf("Reactions promoted: target=%s promoted=%d", tmuxTarget, len(newEntries)))
	}
}

package helpers

import (
	"errors"
	"fmt"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// RetrySend sends a Telegram message with retries.
// On FloodError it waits the RetryAfter duration; on GroupError it auto-migrates chat ID;
// on other errors it retries up to 3 times with backoff.
func RetrySend(b *tele.Bot, to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	var msg *tele.Message
	var err error
	attempt := 0
	for {
		msg, err = b.Send(to, what, opts...)
		if err == nil {
			return msg, nil
		}
		attempt++
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			wait := time.Duration(floodErr.RetryAfter) * time.Second
			logger.Info(fmt.Sprintf("FloodError, waiting %v (attempt %d)", wait, attempt))
			time.Sleep(wait)
			continue
		}
		var groupErr tele.GroupError
		if errors.As(err, &groupErr) {
			newID := groupErr.MigratedTo
			if chat, ok := to.(*tele.Chat); ok && newID != 0 {
				logger.Info(fmt.Sprintf("GroupError: migrating chat %d → %d", chat.ID, newID))
				if merr := config.MigrateChat(chat.ID, newID); merr != nil {
					logger.Error(fmt.Sprintf("Auto-migrate failed: %v", merr))
				}
				chat.ID = newID
				continue
			}
		}
		if attempt >= 3 {
			logger.Error(fmt.Sprintf("Send failed after %d attempts: %v", attempt, err))
			return nil, err
		}
		wait := time.Duration(attempt) * time.Second
		logger.Error(fmt.Sprintf("Send failed (attempt %d): %v, retrying in %v", attempt, err, wait))
		time.Sleep(wait)
	}
}

// RetryEdit edits a Telegram message with retries.
// On FloodError it waits the RetryAfter duration; "message not modified" is treated as success;
// on other errors it retries up to 3 times with backoff.
func RetryEdit(b *tele.Bot, msg tele.Editable, what interface{}, opts ...interface{}) (*tele.Message, error) {
	var result *tele.Message
	var err error
	attempt := 0
	for {
		result, err = b.Edit(msg, what, opts...)
		if err == nil {
			return result, nil
		}
		attempt++
		// Treat "message not modified" as success (coalesced EDIT hit identical content)
		var teleErr *tele.Error
		if errors.As(err, &teleErr) {
			if teleErr == tele.ErrSameMessageContent || teleErr == tele.ErrMessageNotModified {
				return result, nil
			}
		}
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			wait := time.Duration(floodErr.RetryAfter) * time.Second
			logger.Info(fmt.Sprintf("FloodError on edit, waiting %v (attempt %d)", wait, attempt))
			time.Sleep(wait)
			continue
		}
		if attempt >= 3 {
			logger.Error(fmt.Sprintf("Edit failed after %d attempts: %v", attempt, err))
			return nil, err
		}
		wait := time.Duration(attempt) * time.Second
		logger.Error(fmt.Sprintf("Edit failed (attempt %d): %v, retrying in %v", attempt, err, wait))
		time.Sleep(wait)
	}
}

// freezeEditArgs builds the what/opts arguments for a freeze edit.
// When text is empty, only the markup is updated (avoids "message text is empty" Telegram error).
// When text is non-empty, both text and markup are updated with HTML parse mode.
func freezeEditArgs(text string, markup *tele.ReplyMarkup) (what interface{}, opts []interface{}) {
	if text == "" {
		return markup, nil
	}
	return text, []interface{}{markup, tele.ModeHTML}
}

// RetryFreezeEdit edits a message's reply markup (and optionally its text) with retries.
// It delegates argument building to freezeEditArgs so that an empty text never causes a 400 error.
func RetryFreezeEdit(b *tele.Bot, msg tele.Editable, text string, markup *tele.ReplyMarkup) (*tele.Message, error) {
	what, opts := freezeEditArgs(text, markup)
	return RetryEdit(b, msg, what, opts...)
}

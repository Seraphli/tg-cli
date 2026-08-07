package handlers

import (
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	tele "gopkg.in/telebot.v3"
)

// markIncoming bumps the FloatMarker for (chatID, topic) on ENTRY only. An incoming message that
// lands below a pre-existing status is a legitimate re-float trigger, so the entry Mark stays.
// The old deferred Mark (fired unconditionally at handler return) postdated the status SentAt even
// when the handler sent nothing below the status — causing a spurious re-float ~3s after every
// message. It is removed here; a real send-below is now marked by markingContext / markedBotReply
// on send SUCCESS only (see below).
func markIncoming(m *helpers.FloatMarkerStore, chatID int64, topic int, now func() time.Time, next func() error) error {
	m.Mark(chatID, topic, now())
	return next()
}

// markingContext decorates a tele.Context so Reply/Send bump the FloatMarker only when the send
// SUCCEEDS (err == nil). This centrally covers every c.Reply / c.Send site in message-triggered
// handlers with zero call-site churn: a handler that actually sends a message below the status
// marks the route (busy manager re-floats), while a handler that returns without sending does not.
type markingContext struct {
	tele.Context
	marker *helpers.FloatMarkerStore
	now    func() time.Time
}

func (mc *markingContext) Reply(what interface{}, opts ...interface{}) error {
	return markOnSuccess(mc.marker, mc.Context, mc.now, mc.Context.Reply(what, opts...))
}

func (mc *markingContext) Send(what interface{}, opts ...interface{}) error {
	return markOnSuccess(mc.marker, mc.Context, mc.now, mc.Context.Send(what, opts...))
}

// markOnSuccess bumps the marker for the context's route iff the send succeeded. Nil-guards the
// marker, chat, and message so a context lacking them never panics.
func markOnSuccess(m *helpers.FloatMarkerStore, c tele.Context, now func() time.Time, err error) error {
	if err != nil || m == nil {
		return err
	}
	ch, msg := c.Chat(), c.Message()
	if ch == nil || msg == nil {
		return err
	}
	m.Mark(ch.ID, msg.ThreadID, now())
	return err
}

// markedBotReply is the bot.Reply variant for sites that need the returned *tele.Message (e.g. to
// store the sent message ID). It marks the route on send SUCCESS, mirroring markingContext.Reply.
func markedBotReply(bot *tele.Bot, m *helpers.FloatMarkerStore, now func() time.Time, to *tele.Message, what interface{}, opts ...interface{}) (*tele.Message, error) {
	sent, err := bot.Reply(to, what, opts...)
	return sent, markToOnSuccess(m, now, to, err)
}

// markToOnSuccess bumps the marker for a bot.Reply target on send SUCCESS. Split from markedBotReply
// so the nil-guarded marking decision is unit-testable without a live *tele.Bot (bot.Reply itself
// dereferences to.Chat and performs network I/O). Nil-guards marker, to, and to.Chat.
func markToOnSuccess(m *helpers.FloatMarkerStore, now func() time.Time, to *tele.Message, err error) error {
	if err != nil || m == nil || to == nil || to.Chat == nil {
		return err
	}
	m.Mark(to.Chat.ID, to.ThreadID, now())
	return err
}

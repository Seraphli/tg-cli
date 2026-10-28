package handlers

import (
	"fmt"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// statusNowFn is an A3-style seam so tests can inject a controllable clock. Used by the FIFO queue
// (to timestamp becameHeadAt) and by audioStatusText computing elapsed time.
var statusNowFn = time.Now

// statusTickerFn is an A3-style seam so tests can inject a controllable ticker instead of waiting on a
// real 10-second interval. Returns a tick channel and a stop function.
var statusTickerFn = func() (tick <-chan time.Time, stop func()) {
	t := time.NewTicker(10 * time.Second)
	return t.C, t.Stop
}

// audioQueueEntry is one registered transcription slot in the FIFO queue.
type audioQueueEntry struct {
	id           int64
	becameHeadAt time.Time // zero until this entry becomes the head
}

var (
	audioQueueMu   sync.Mutex
	audioQueueCond = sync.NewCond(&audioQueueMu)
	audioQueueSeq  int64
	audioQueueList []*audioQueueEntry
)

// audioQueueEnter registers a new ticket at the tail of the FIFO queue and returns its id and the
// number of tickets ahead of it (entries already present and not yet left). If ahead == 0, this ticket
// is already the head, so becameHeadAt is recorded immediately, in this same call.
func audioQueueEnter() (id int64, ahead int) {
	audioQueueMu.Lock()
	defer audioQueueMu.Unlock()
	audioQueueSeq++
	id = audioQueueSeq
	ahead = len(audioQueueList)
	e := &audioQueueEntry{id: id}
	if ahead == 0 {
		e.becameHeadAt = statusNowFn()
	}
	audioQueueList = append(audioQueueList, e)
	return id, ahead
}

// audioQueueAhead returns the number of tickets with a lower id still registered.
func audioQueueAhead(id int64) int {
	audioQueueMu.Lock()
	defer audioQueueMu.Unlock()
	n := 0
	for _, e := range audioQueueList {
		if e.id < id {
			n++
		}
	}
	return n
}

// audioQueueBecameHeadAt returns the time id became (or will become) the head, and whether id is still
// registered at all. present=false (id already left) tells the caller there is nothing to show for
// this ticket anymore — see audioStatusText.
func audioQueueBecameHeadAt(id int64) (becameHeadAt time.Time, present bool) {
	audioQueueMu.Lock()
	defer audioQueueMu.Unlock()
	for _, e := range audioQueueList {
		if e.id == id {
			return e.becameHeadAt, true
		}
	}
	return time.Time{}, false
}

// audioQueueWaitTurn blocks until id is at the head of the queue (index 0), then returns the time it
// became head. Returns immediately if id is already head (including the ahead==0-at-entry case, where
// becameHeadAt was already set by audioQueueEnter).
func audioQueueWaitTurn(id int64) time.Time {
	audioQueueMu.Lock()
	defer audioQueueMu.Unlock()
	for {
		if len(audioQueueList) > 0 && audioQueueList[0].id == id {
			return audioQueueList[0].becameHeadAt
		}
		audioQueueCond.Wait()
	}
}

// audioQueueLeave removes id wherever it is in the queue. Idempotent: calling it again for an
// already-removed id is a safe no-op, so every early-return path in handleAudioTranscription can call
// it unconditionally. If the removed entry was the head, the new head (if any) gets becameHeadAt
// recorded right now, and every blocked waiter is woken (Cond.Broadcast) to recheck its own position.
func audioQueueLeave(id int64) {
	audioQueueMu.Lock()
	defer audioQueueMu.Unlock()
	for i, e := range audioQueueList {
		if e.id == id {
			audioQueueList = append(audioQueueList[:i], audioQueueList[i+1:]...)
			if i == 0 && len(audioQueueList) > 0 {
				audioQueueList[0].becameHeadAt = statusNowFn()
			}
			audioQueueCond.Broadcast()
			return
		}
	}
}

// audioStatusText computes the display text for ticket given verb ("Transcribing"/"Retrying") and
// engine, by polling the FIFO queue's live state. Returns ok=false when ticket is no longer registered
// (it already left the queue) — the caller (the ticker) MUST skip the tick entirely in that case: a
// tick firing between audioQueueLeave (the instant the engine call returns) and the ticker actually
// being stopped must never fall back to computing "Queued (0 ahead)", which SPEC.md says must never be
// shown. A pure function of immutable inputs (ticket/verb/engine, each set once at entry and never
// reassigned) plus the queue's own mutex-protected state — safe to call concurrently from both the
// ticker goroutine and the handler goroutine with no additional locking.
func audioStatusText(ticket int64, verb, engine string) (text string, ok bool) {
	headAt, present := audioQueueBecameHeadAt(ticket)
	if !present {
		return "", false
	}
	if headAt.IsZero() {
		return fmt.Sprintf("🕒 Queued (%d ahead)", audioQueueAhead(ticket)), true
	}
	elapsed := int(statusNowFn().Sub(headAt).Round(time.Second).Seconds())
	return fmt.Sprintf("⏳ %s with %s… %ds", verb, engineLabel(engine), elapsed), true
}

// audioStatus owns the single Telegram message that carries a voice/audio transcription's lifecycle end
// to end, editing it in place. update is a no-op (no Bot API call) when text is unchanged from the last
// call — this single check backs both the periodic ticker and the synchronous flip in
// handleAudioTranscription (SPEC SS1 step 4 / C11). The mutex serializes update calls from the ticker
// goroutine and the handler goroutine, which both call it concurrently (started at step 3, stopped at
// step 6). Logging the semantic state-transition line (state=queued/transcribing/retrying/done/failed,
// with whatever extra fields that state needs) is the CALLER's job, right after a call to update
// returns true — the right fields differ per state, so audioStatus itself only logs send/edit errors
// (fixed markers, never the transcript text).
type audioStatus struct {
	mu   sync.Mutex
	bot  *tele.Bot
	msg  *tele.Message
	text string
}

// newAudioStatus sends text as a brand-new reply to c's message — NO reply_markup argument (nothing to
// clear on a message that doesn't exist yet). On a send failure, logs send_error and returns a status
// with no backing message; subsequent update calls become safe no-ops.
func newAudioStatus(bot *tele.Bot, c tele.Context, text string) *audioStatus {
	msg, err := bot.Reply(c.Message(), text)
	if err != nil {
		logger.Error(fmt.Sprintf("Voice status: send_error=%v", err))
		return &audioStatus{bot: bot, text: text}
	}
	return &audioStatus{bot: bot, msg: msg, text: text}
}

// reuseAudioStatus edits existing into text, with an explicit empty markup in that same edit — the
// vretry retry path: old failure buttons are cleared in the same edit that starts the retry. Returns
// whether that edit actually succeeded: msgID is never 0 on the reuse path (unlike the brand-new-send
// path), so the caller cannot use msgID alone to tell a successful edit from a failed one ([P.Step 2]
// round-1 D2 fix) — it must use this return value instead.
func reuseAudioStatus(bot *tele.Bot, existing *tele.Message, text string) (*audioStatus, bool) {
	s := &audioStatus{bot: bot, msg: existing}
	ok := s.update(text, &tele.ReplyMarkup{})
	return s, ok
}

// msgID returns the backing message's id, or 0 if there is none (the initial send failed).
func (s *audioStatus) msgID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.msg == nil {
		return 0
	}
	return s.msg.ID
}

// update edits the status message to text with markup, which MUST be explicit (either real retry
// buttons or an empty &tele.ReplyMarkup{} — SPEC SS2; never nil, never omitted) on every call. Returns
// true only when an edit actually happened (text changed AND the edit succeeded) — false when the text
// was unchanged (no-op, no Bot API call), when there is no backing message, or when the edit failed
// (edit_error already logged here). The whole method runs under s.mu so concurrent callers (the ticker
// goroutine and the handler goroutine) are serialized and each compares against the text actually sent
// last.
func (s *audioStatus) update(text string, markup *tele.ReplyMarkup) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == s.text {
		return false
	}
	s.text = text
	if s.msg == nil {
		return false
	}
	if _, err := helpers.RetryEdit(s.bot, s.msg, text, markup); err != nil {
		logger.Error(fmt.Sprintf("Voice status: msg_id=%d edit_error=%v", s.msg.ID, err))
		return false
	}
	return true
}

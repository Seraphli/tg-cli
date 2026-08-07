package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/archive"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	tele "gopkg.in/telebot.v3"
)

// parseChatID parses a Telegram recipient string (numeric chat id) into int64.
// Returns 0 when the string is non-numeric (inline/nonnumeric recipients).
func parseChatID(s string) int64 {
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

// Archive is the process-wide SQLite message archive, set at bot startup (nil when disabled or on a
// load error). All Archive methods are nil-safe, so the send paths need only this single indirection.
var Archive *archive.Archive

// RichSendOpts holds optional parameters for rich send/edit operations.
type RichSendOpts struct {
	TopicID             int
	Markup              *tele.ReplyMarkup
	SkipEntityDetection bool
	// LegacyHTML is the pre-rich (HTML parse mode) fallback body for G2 burn-in fallback.
	// If empty, the rich html string itself is used as fallback text.
	LegacyHTML string
	// CCMessageID is the CC assistant-message UUID for a stream send/edit; recorded as messages.cc_message_id
	// so hook_events can be joined to the TG message. Empty for non-assistant sends.
	CCMessageID string
}

// rawEnvelope is the JSON envelope returned by bot.Raw for methods returning a Message.
type rawEnvelope struct {
	Ok     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
}

// editableTextLen returns the rune length of a send/edit payload when it is a string, else 0
// (markup-only edits carry no text). Used for the Fix 16 send/edit logging.
func editableTextLen(what interface{}) int {
	if s, ok := what.(string); ok {
		return len([]rune(s))
	}
	return 0
}

// buildRichMessage builds the InputRichMessage map for a rich send/edit payload.
func buildRichMessage(html string, skipEntityDetection bool) map[string]interface{} {
	rm := map[string]interface{}{"html": html}
	if skipEntityDetection {
		rm["skip_entity_detection"] = true
	}
	return rm
}

// encodeInlineButtons applies telebot's callback-data encoding ("\f<unique>|<data>") to a rich
// markup's inline buttons. bot.Send/bot.Edit do this via the unexported processButtons, but the
// rich wrappers send the markup through bot.Raw, which skips it — so without this the serialized
// callback_data omits the "\f<unique>" prefix and button clicks never route to their handler
// (bot.go cbackRx requires the prefix). Idempotent: already-encoded buttons are left untouched.
func encodeInlineButtons(markup *tele.ReplyMarkup) {
	if markup == nil {
		return
	}
	for i := range markup.InlineKeyboard {
		for j := range markup.InlineKeyboard[i] {
			btn := &markup.InlineKeyboard[i][j]
			if btn.Unique == "" || strings.HasPrefix(btn.Data, "\f") {
				continue
			}
			if btn.Data == "" {
				btn.Data = "\f" + btn.Unique
			} else {
				btn.Data = "\f" + btn.Unique + "|" + btn.Data
			}
		}
	}
}

// decodeRichResponse decodes a bot.Raw response into a *tele.Message.
func decodeRichResponse(data []byte) (*tele.Message, error) {
	var env rawEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("rich response decode: %w", err)
	}
	var msg tele.Message
	if err := json.Unmarshal(env.Result, &msg); err != nil {
		return nil, fmt.Errorf("rich result decode: %w", err)
	}
	return &msg, nil
}

// isTelegram400 reports whether err is a Telegram Bad Request (HTTP 400) error.
func isTelegram400(err error) bool {
	if err == nil {
		return false
	}
	var teleErr *tele.Error
	if errors.As(err, &teleErr) {
		return teleErr.Code == 400
	}
	// Unknown error descriptions are wrapped by telebot as fmt.Errorf("telegram: %s (%d)"),
	// which is not a *tele.Error. A rejected rich_message payload typically yields such a
	// novel description, so match the "(400)" code suffix to still trigger the G2 fallback.
	return strings.HasSuffix(err.Error(), "(400)")
}

// nonRetryableCodeSuffixes are the "telegram: <desc> (<code>)" suffixes for Telegram business
// errors (4xx) that telebot wraps as a plain fmt error rather than a typed *tele.Error.
var nonRetryableCodeSuffixes = []string{"(400)", "(401)", "(403)", "(404)", "(409)", "(413)"}

// isNonRetryable reports whether err is a business error that will never succeed on retry — a
// Telegram 4xx (other than 429 rate limit, which is retryable). Everything else (5xx, transport
// failures like connection refused/timeout, and undecodable gateway bodies) is treated as a
// transient network error and retried. Fix 21: network errors must retry indefinitely; only genuine
// business errors fail fast.
func isNonRetryable(err error) bool {
	if err == nil {
		return false
	}
	var teleErr *tele.Error
	if errors.As(err, &teleErr) {
		return teleErr.Code >= 400 && teleErr.Code < 500 && teleErr.Code != 429
	}
	msg := err.Error()
	for _, suffix := range nonRetryableCodeSuffixes {
		if strings.HasSuffix(msg, suffix) {
			return true
		}
	}
	return false
}

// floodLogMsg returns the log message for a FloodError retry. The send form (msg=="") omits msg_id;
// the edit form (msg!="") includes it.
func floodLogMsg(method, chat, msg string, wait time.Duration, attempt int) string {
	if msg == "" {
		return fmt.Sprintf("FloodError on %s, chat=%s waiting %v (attempt %d)", method, chat, wait, attempt)
	}
	return fmt.Sprintf("FloodError on %s, chat=%s msg_id=%s waiting %v (attempt %d)", method, chat, msg, wait, attempt)
}

// maxFloodSleep caps each individual FloodError backoff sleep (boss instruction D4). This bounds a SINGLE
// sleep, not the total retry loop — the loop remains unbounded, so a retry may fire back into an open flood
// window and back off again for up to another 5s.
const maxFloodSleep = 5 * time.Second

// capFloodSleep clamps a Telegram-advertised FloodError RetryAfter to maxFloodSleep (boss instruction D4).
func capFloodSleep(d time.Duration) time.Duration {
	if d > maxFloodSleep {
		return maxFloodSleep
	}
	return d
}

// networkRetryBackoff returns the exponential backoff (1,2,4,8,16s) for the given 1-based attempt,
// capped at 30s. Fix 21: there is no attempt limit for network errors, so the backoff plateaus.
func networkRetryBackoff(attempt int) time.Duration {
	if attempt >= 6 {
		return 30 * time.Second
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// responseOK reports whether a bot.Raw response body is a genuine Telegram success ({"ok":true}).
// telebot's extractOk swallows non-JSON gateway bodies (e.g. an HTML 502 page during an outage) as a
// nil error, so a nil error from Raw does NOT by itself mean success — the body must carry ok:true.
func responseOK(data []byte) bool {
	var probe struct {
		Ok bool `json:"ok"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.Ok
}

// FinalizeRichHTML applies the send-time rich transforms — collapse the 📂/📟/🖥 header metadata into a
// default-collapsed <details> block, then convert body newlines to <br> (rich_message.html collapses a
// bare "\n" to a space) while preserving <pre>/<tg-math-block> — yielding the exact HTML that Telegram
// receives. The rich senders apply it before sending; callers that log the sent body call it too, so
// the debug log always matches what was actually sent (Fix 22). NOTE: distinct from markdown's
// RenderRichHTML (markdown → rich-HTML dialect), which runs earlier when the body is built.
func FinalizeRichHTML(html string) string {
	return FinalizeRichHTMLWithID(html, 0)
}

// ArchiveIDPlaceholder is a sentinel a caller can embed in rich HTML where the reserved archive message
// ID should appear (e.g. a mailbox header that is not a session header, so CollapseSessionMetaWithID does
// not inject the ID for it). FinalizeRichHTMLWithID replaces it with "🆔 #<id>" at send time, or removes
// it (with a leading newline) when archiving is disabled / no id. Uses private-use codepoints so it never
// collides with real content.
const ArchiveIDPlaceholder = "tgcli-archive-id"

// injectArchiveID replaces ArchiveIDPlaceholder with the reserved archive id line (or removes it).
func injectArchiveID(html string, msgID int64) string {
	if !strings.Contains(html, ArchiveIDPlaceholder) {
		return html
	}
	if msgID > 0 {
		return strings.ReplaceAll(html, ArchiveIDPlaceholder, "🆔 #"+strconv.FormatInt(msgID, 10))
	}
	// No id: drop the placeholder AND a single leading newline so no blank line is left behind.
	html = strings.ReplaceAll(html, "\n"+ArchiveIDPlaceholder, "")
	return strings.ReplaceAll(html, ArchiveIDPlaceholder, "")
}

// FinalizeRichHTMLWithID is FinalizeRichHTML with an optional tg-cli message ID injected into the
// collapsed <details> header (Feature 2): via CollapseSessionMetaWithID for session headers, and via
// ArchiveIDPlaceholder for non-session headers (mailbox). msgID == 0 omits the ID line — the behaviour
// the logger call sites rely on (they log via FinalizeRichHTML), so only the rich senders pass a real id.
func FinalizeRichHTMLWithID(html string, msgID int64) string {
	html = injectArchiveID(html, msgID)
	return markdown.RichifyNewlines(markdown.CollapseSessionMetaWithID(html, msgID))
}

// editableString returns a send/edit payload as a string when it is one, else "" (markup-only edits).
func editableString(what interface{}) string {
	if s, ok := what.(string); ok {
		return s
	}
	return ""
}

// resolveReservation returns the tg-cli message ID to use for a SEND. A non-zero reservedID (an
// already-reserved ID threaded from a rich→legacy fallback) is returned unchanged and NEVER
// re-reserved (M1). Otherwise, when archiving applies (Archive set, text payload, real numeric chat),
// a fresh ID is reserved; any reserve error is logged and 0 returned — degraded mode never blocks the
// send (numeric-recipient guard: chatID == 0 for inline/nonnumeric recipients skips archiving).
func resolveReservation(reservedID, chatID int64, isText bool) int64 {
	if reservedID != 0 {
		return reservedID
	}
	if Archive == nil || !isText || chatID == 0 {
		return 0
	}
	id, err := Archive.ReserveSend(chatID)
	if err != nil {
		logger.Warn("message archive: ReserveSend failed: " + err.Error())
		return 0
	}
	return id
}

// editArchiveID resolves (or creates) the tg-cli message ID for an EDIT, or 0 when archiving does not
// apply: Archive unset, a non-text (markup-only) payload, an inline/nonnumeric message id, a nonpositive
// tg msg id, or a zero chat id (numeric-recipient guard). Errors are logged and 0 returned (degraded).
func editArchiveID(msgIDStr string, chatID int64, isText bool) int64 {
	if Archive == nil || !isText || chatID == 0 {
		return 0
	}
	tgMsgID, err := strconv.ParseInt(msgIDStr, 10, 64)
	if err != nil || tgMsgID <= 0 {
		return 0
	}
	id, lerr := Archive.LookupOrCreate(chatID, tgMsgID)
	if lerr != nil {
		logger.Warn("message archive: LookupOrCreate failed: " + lerr.Error())
		return 0
	}
	return id
}

// RetrySendRich sends a rich Telegram message via bot.Raw("sendRichMessage").
// Reuses the same FloodError/GroupError retry loop as RetrySend: network/transient errors (5xx,
// transport failures, undecodable gateway bodies) retry indefinitely with capped backoff; 4xx
// business errors fail fast (Fix 21). On a Telegram 400 (G2 fallback): retries ONCE via the legacy
// HTML path and logs RICH_FALLBACK.
// TODO(rich-burnin): remove fallback after burn-in.
func RetrySendRich(b *tele.Bot, chat tele.Recipient, html string, o RichSendOpts) (*tele.Message, error) {
	// Preserve the pre-transform html for the empty-LegacyHTML G2 fallback (E1b): the transformed html
	// carries <details>/<br> which are invalid under parse_mode=HTML and would 400 the fallback too.
	originalHTML := html
	// Reserve the tg-cli message ID up front (a rich send is always a text/html payload) so the id can be
	// injected into the header BEFORE the send; 0 when archiving is disabled/not-applicable.
	chatID, _ := strconv.ParseInt(chat.Recipient(), 10, 64)
	reservedID := resolveReservation(0, chatID, true)
	// Apply the send-time transforms (collapse header → <br> newlines) at this single choke point for
	// all rich sends, injecting the reserved id. The legacy fallback (o.LegacyHTML) is left untouched (G3).
	html = FinalizeRichHTMLWithID(html, reservedID)
	payload := map[string]interface{}{
		"chat_id":      chat.Recipient(),
		"rich_message": buildRichMessage(html, o.SkipEntityDetection),
	}
	if o.TopicID > 0 {
		payload["message_thread_id"] = o.TopicID
	}
	if o.Markup != nil {
		encodeInlineButtons(o.Markup)
		payload["reply_markup"] = o.Markup
	}
	attempt := 0
	for {
		data, err := b.Raw("sendRichMessage", payload)
		if err == nil && responseOK(data) {
			sent, derr := decodeRichResponse(data)
			mid := 0
			if sent != nil {
				mid = sent.ID
			}
			logger.Info(fmt.Sprintf("TG send: chat=%s msg_id=%d text_len=%d rich=true", chat.Recipient(), mid, len([]rune(html))))
			// Archive on success (M2: re-read the recipient so a GroupError migration is reflected).
			if reservedID != 0 && sent != nil {
				finalChatID, _ := strconv.ParseInt(chat.Recipient(), 10, 64)
				if cerr := Archive.CompleteSend(reservedID, finalChatID, int64(sent.ID), true, html, o.CCMessageID); cerr != nil {
					logger.Warn("message archive: CompleteSend failed: " + cerr.Error())
				}
			}
			// Bump float marker on send success (M2 pattern: re-read current recipient after any GroupError).
			if FloatMarker != nil {
				currentChatID := parseChatID(chat.Recipient())
				FloatMarker.Mark(currentChatID, o.TopicID, time.Now())
			}
			return sent, derr
		}
		attempt++
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			// Boss instruction (D4): cap each FloodError backoff sleep at 5s. This caps the INDIVIDUAL
			// sleep, not the total retry loop (the loop is still unbounded; a retry may fire into an open
			// flood window and back off again).
			wait := capFloodSleep(time.Duration(floodErr.RetryAfter) * time.Second)
			logger.Info(floodLogMsg("sendRichMessage", chat.Recipient(), "", wait, attempt))
			// B1: re-read the current recipient (post-GroupError migration) for accurate backoff.
			if FloodBackoff != nil {
				FloodBackoff.Set(parseChatID(chat.Recipient()), time.Now().Add(wait))
			}
			time.Sleep(wait)
			continue
		}
		var groupErr tele.GroupError
		if errors.As(err, &groupErr) {
			newID := groupErr.MigratedTo
			if c, ok := chat.(*tele.Chat); ok && newID != 0 {
				logger.Info(fmt.Sprintf("GroupError: migrating chat %d → %d", c.ID, newID))
				if merr := config.MigrateChat(c.ID, newID); merr != nil {
					logger.Error(fmt.Sprintf("Auto-migrate failed: %v", merr))
				}
				c.ID = newID
				payload["chat_id"] = c.Recipient()
				continue
			}
		}
		// G2: on Telegram 400 retry ONCE via legacy HTML path
		if isTelegram400(err) {
			legacyHTML := o.LegacyHTML
			if legacyHTML == "" {
				legacyHTML = originalHTML // E1b: pre-transform html (no <details>/<br>), not the rich-mutated html
			}
			logger.Warn(fmt.Sprintf("RICH_FALLBACK method=sendRichMessage chat=%s err=%v", chat.Recipient(), err))
			// Thread the reserved id (M1): the legacy fallback completes ID A, not a new ID B.
			return sendReserved(b, chat, legacyHTML, reservedID, false, tele.ModeHTML)
		}
		// Fix 21: business errors (4xx) fail immediately; network/transient errors retry indefinitely.
		if isNonRetryable(err) {
			logger.Error(fmt.Sprintf("sendRichMessage failed (non-retryable): %v", err))
			return nil, err
		}
		wait := networkRetryBackoff(attempt)
		if err == nil {
			// err==nil but no ok:true → undecodable gateway body (e.g. 502 HTML during an outage).
			logger.Warn(fmt.Sprintf("sendRichMessage: undecodable response, retrying in %v (attempt %d)", wait, attempt))
		} else {
			logger.Warn(fmt.Sprintf("sendRichMessage failed (network), retrying in %v (attempt %d): %v", wait, attempt, err))
		}
		time.Sleep(wait)
	}
}

// RetryEditRich edits a Telegram message with rich_message via bot.Raw("editMessageText").
// Reuses the same retry/backoff loop as RetryEdit.
// On a Telegram 400 (G2 fallback): retries ONCE via the legacy HTML edit path.
// TODO(rich-burnin): remove fallback after burn-in.
func RetryEditRich(b *tele.Bot, msg tele.Editable, html string, o RichSendOpts) (*tele.Message, error) {
	// Preserve pre-transform html for the empty-LegacyHTML G2 fallback (E1b) — reachable via
	// RetryFreezeEditRich (AskUserQuestion freeze-edit passes no LegacyHTML).
	originalHTML := html
	msgID, chatID := msg.MessageSig()
	// Resolve the tg-cli id up front (both keys are known before the edit) so it can be injected into the
	// header; 0 when archiving is disabled/not-applicable (numeric-recipient guard).
	archiveID := editArchiveID(msgID, chatID, true)
	// Apply the send-time transforms (collapse header → <br> newlines), injecting the id; RetryFreezeEditRich
	// delegates here for non-empty html, so all rich wrappers are covered. The legacy fallback is untouched (G3).
	html = FinalizeRichHTMLWithID(html, archiveID)
	payload := map[string]interface{}{
		"chat_id":      chatID,
		"message_id":   msgID,
		"rich_message": buildRichMessage(html, o.SkipEntityDetection),
	}
	if o.Markup != nil {
		encodeInlineButtons(o.Markup)
		payload["reply_markup"] = o.Markup
	}
	attempt := 0
	for {
		data, err := b.Raw("editMessageText", payload)
		if err == nil && responseOK(data) {
			logger.Info(fmt.Sprintf("TG edit: chat=%v msg_id=%v text_len=%d rich=true", chatID, msgID, len([]rune(html))))
			edited, _ := decodeRichResponse(data) // result may be `true` on inline edits — message optional
			if archiveID != 0 {
				if rerr := Archive.RecordEdit(archiveID, true, html, o.CCMessageID); rerr != nil {
					logger.Warn("message archive: RecordEdit failed: " + rerr.Error())
				}
			}
			return edited, nil
		}
		attempt++
		// Treat "message not modified" as success
		var teleErr *tele.Error
		if errors.As(err, &teleErr) {
			if teleErr == tele.ErrSameMessageContent || teleErr == tele.ErrMessageNotModified {
				return nil, nil
			}
		}
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			// Boss instruction (D4): cap each FloodError backoff sleep at 5s (caps the individual sleep, not
			// the total retry loop).
			wait := capFloodSleep(time.Duration(floodErr.RetryAfter) * time.Second)
			logger.Info(floodLogMsg("editMessageText(rich)", fmt.Sprint(chatID), fmt.Sprint(msgID), wait, attempt))
			time.Sleep(wait)
			continue
		}
		// G2: on Telegram 400 retry ONCE via legacy HTML path
		if isTelegram400(err) {
			legacyHTML := o.LegacyHTML
			if legacyHTML == "" {
				legacyHTML = originalHTML // E1b: pre-transform html (no <details>/<br>), not the rich-mutated html
			}
			logger.Warn(fmt.Sprintf("RICH_FALLBACK method=editMessageText chat=%v err=%v", chatID, err))
			return RetryEdit(b, msg, legacyHTML, tele.ModeHTML)
		}
		// Fix 21: business errors (4xx) fail immediately; network/transient errors retry indefinitely.
		if isNonRetryable(err) {
			logger.Error(fmt.Sprintf("editMessageText(rich) failed (non-retryable): %v", err))
			return nil, err
		}
		wait := networkRetryBackoff(attempt)
		if err == nil {
			// err==nil but no ok:true → undecodable gateway body (e.g. 502 HTML during an outage).
			logger.Warn(fmt.Sprintf("editMessageText(rich): undecodable response, retrying in %v (attempt %d)", wait, attempt))
		} else {
			logger.Warn(fmt.Sprintf("editMessageText(rich) failed (network), retrying in %v (attempt %d): %v", wait, attempt, err))
		}
		time.Sleep(wait)
	}
}

// RetryFreezeEditRich edits a message's reply markup (and optionally its rich text) with retries.
// When html is empty, only the markup is updated (avoids "message text is empty" Telegram error).
// TODO(rich-burnin): remove fallback after burn-in.
func RetryFreezeEditRich(b *tele.Bot, msg tele.Editable, html string, markup *tele.ReplyMarkup) (*tele.Message, error) {
	if html == "" {
		return RetryEdit(b, msg, markup)
	}
	return RetryEditRich(b, msg, html, RichSendOpts{Markup: markup})
}

// RetrySend sends a Telegram message with retries.
// On FloodError it waits the RetryAfter duration; on GroupError it auto-migrates chat ID.
// Fix 21: network/transient errors retry indefinitely with capped exponential backoff; only 4xx
// business errors fail immediately.
// On success bumps the FloatMarker (marks an outbound message for re-float detection).
func RetrySend(b *tele.Bot, to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	return sendReserved(b, to, what, 0, false, opts...)
}

// RetrySendNoFloat is like RetrySend but does NOT bump the FloatMarker on success.
// Used exclusively by the busy manager to send the status message — prevents the status send
// from re-triggering its own re-float (A2 recursion guard).
func RetrySendNoFloat(b *tele.Bot, to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	return sendReserved(b, to, what, 0, true, opts...)
}

// sendReserved is the shared SEND implementation for RetrySend, RetrySendNoFloat, and the
// rich→legacy fallback. It runs the same FloodError/GroupError/backoff retry loop and, on
// success, archives the send under reservedID: a non-zero reservedID (threaded from a rich
// fallback) completes the SAME id (M1); reservedID==0 reserves a fresh id here when archiving
// applies. finalChatID is re-read after the loop so a GroupError migration is reflected in the
// archived chat_id (M2). noFloat suppresses the FloatMarker bump on success.
func sendReserved(b *tele.Bot, to tele.Recipient, what interface{}, reservedID int64, noFloat bool, opts ...interface{}) (*tele.Message, error) {
	_, isText := what.(string)
	chatID, _ := strconv.ParseInt(to.Recipient(), 10, 64)
	reservedID = resolveReservation(reservedID, chatID, isText)
	var msg *tele.Message
	var err error
	attempt := 0
	for {
		msg, err = b.Send(to, what, opts...)
		if err == nil {
			mid := 0
			if msg != nil {
				mid = msg.ID
			}
			logger.Info(fmt.Sprintf("TG send: chat=%s msg_id=%d text_len=%d rich=false", to.Recipient(), mid, editableTextLen(what)))
			if reservedID != 0 && msg != nil {
				finalChatID, _ := strconv.ParseInt(to.Recipient(), 10, 64)
				if cerr := Archive.CompleteSend(reservedID, finalChatID, int64(msg.ID), false, editableString(what), ""); cerr != nil {
					logger.Warn("message archive: CompleteSend failed: " + cerr.Error())
				}
			}
			// Bump float marker on success (M2: re-read recipient after any GroupError migration).
			if !noFloat && FloatMarker != nil {
				finalChatID, _ := strconv.ParseInt(to.Recipient(), 10, 64)
				FloatMarker.Mark(finalChatID, topicIDFromOpts(opts), time.Now())
			}
			return msg, nil
		}
		attempt++
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			// Boss instruction (D4): cap each FloodError backoff sleep at 5s (caps the individual sleep, not
			// the total retry loop).
			wait := capFloodSleep(time.Duration(floodErr.RetryAfter) * time.Second)
			logger.Info(floodLogMsg("send", to.Recipient(), "", wait, attempt))
			// B1: re-read the current recipient (post-GroupError migration) for accurate backoff.
			if FloodBackoff != nil {
				FloodBackoff.Set(parseChatID(to.Recipient()), time.Now().Add(wait))
			}
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
		// Fix 21: business errors (4xx) fail immediately; network/transient errors retry indefinitely.
		if isNonRetryable(err) {
			logger.Error(fmt.Sprintf("Send failed (non-retryable): %v", err))
			return nil, err
		}
		wait := networkRetryBackoff(attempt)
		logger.Warn(fmt.Sprintf("Send failed (network), retrying in %v (attempt %d): %v", wait, attempt, err))
		time.Sleep(wait)
	}
}

// RetryEdit edits a Telegram message with retries.
// On FloodError it waits the RetryAfter duration; "message not modified" is treated as success.
// Fix 21: network/transient errors retry indefinitely with capped exponential backoff; only 4xx
// business errors fail immediately.
func RetryEdit(b *tele.Bot, msg tele.Editable, what interface{}, opts ...interface{}) (*tele.Message, error) {
	msgIDStr, chatID := msg.MessageSig()
	_, isText := what.(string)
	// Resolve the tg-cli id up front so a successful edit records under it; 0 skips archiving
	// (markup-only edit, inline/nonnumeric id, or archiving disabled).
	archiveID := editArchiveID(msgIDStr, chatID, isText)
	var result *tele.Message
	var err error
	attempt := 0
	for {
		result, err = b.Edit(msg, what, opts...)
		if err == nil {
			logger.Info(fmt.Sprintf("TG edit: chat=%d msg_id=%s text_len=%d rich=false", chatID, msgIDStr, editableTextLen(what)))
			if archiveID != 0 {
				if rerr := Archive.RecordEdit(archiveID, false, editableString(what), ""); rerr != nil {
					logger.Warn("message archive: RecordEdit failed: " + rerr.Error())
				}
			}
			return result, nil
		}
		attempt++
		// Treat "message not modified" as success (coalesced EDIT hit identical content) — no content
		// change, so NO edit operation is recorded.
		var teleErr *tele.Error
		if errors.As(err, &teleErr) {
			if teleErr == tele.ErrSameMessageContent || teleErr == tele.ErrMessageNotModified {
				return result, nil
			}
		}
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			// Boss instruction (D4): cap each FloodError backoff sleep at 5s (caps the individual sleep, not
			// the total retry loop).
			wait := capFloodSleep(time.Duration(floodErr.RetryAfter) * time.Second)
			logger.Info(floodLogMsg("edit", fmt.Sprint(chatID), msgIDStr, wait, attempt))
			time.Sleep(wait)
			continue
		}
		// Fix 21: business errors (4xx) fail immediately; network/transient errors retry indefinitely.
		if isNonRetryable(err) {
			logger.Error(fmt.Sprintf("Edit failed (non-retryable): %v", err))
			return nil, err
		}
		wait := networkRetryBackoff(attempt)
		logger.Warn(fmt.Sprintf("Edit failed (network), retrying in %v (attempt %d): %v", wait, attempt, err))
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

// RetryFreezeEditAuto freezes a message's markup (and optionally re-sends its text), choosing the
// rich or legacy edit path from the message's persisted format (G1 mixed-era). A pre-upgrade
// (legacy) message stays legacy HTML; a rich message is edited via editMessageText rich_message.
func RetryFreezeEditAuto(b *tele.Bot, msg tele.Editable, rich bool, text string, markup *tele.ReplyMarkup) (*tele.Message, error) {
	if rich {
		return RetryFreezeEditRich(b, msg, text, markup)
	}
	return RetryFreezeEdit(b, msg, text, markup)
}

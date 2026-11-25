package handlers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// rewindPageSize is the number of messages on a list page, rewindButtonCols the display columns of label text on an entry
// button and rewindBodyRunes the characters of a label in the list body.
const (
	rewindPageSize   = 10
	rewindButtonCols = 20
	rewindBodyRunes  = 100
)

// rewindActionIcon is the icon in front of an action button. It is display only: the driver is called with the plain
// action name (RewindActions) and the result texts keep it.
func rewindActionIcon(action string) string {
	switch action {
	case "Restore code and conversation", "Rewind conversation to here", "No summary":
		return "↩️"
	case "Restore conversation":
		return "💬"
	case "Restore code":
		return "📝"
	case "Summarize from here", "Summarize up to here", "Summarize":
		return "🗜"
	}
	return ""
}

// rewindMenu is the in-memory state of one /rewind Telegram message, kept in bs.RewindMenus by chat id and message id.
type rewindMenu struct {
	Target  injector.TmuxTarget
	Backend string
	Entries []helpers.RewindEntry // newest first, everything read so far
	More    bool                  // older entries exist beyond Entries
	Page    int                   // index of the first entry on the list page
	Picked  int                   // entry whose actions are shown, -1 on the list page
	mu      sync.Mutex            // held by every callback while it works on the menu; a tap that finds it held is answered and dropped
}

// rewindKey identifies a /rewind message: Telegram message ids are unique only within one chat.
type rewindKey struct {
	Chat int64
	Msg  int
}

func rewindKeyOf(c tele.Context) rewindKey { return rewindKey{c.Chat().ID, c.Message().ID} }

// rewindPageEnd is the index after the last entry of the list page that starts at menu.Page.
func rewindPageEnd(menu *rewindMenu) int { return min(menu.Page+rewindPageSize, len(menu.Entries)) }

// rewindNumber is the number of entry idx on the list page and on the action page: the newest message is 1 and the numbers rise toward older messages, continuous across pages.
func rewindNumber(idx int) int { return idx + 1 }

// rewindBackendRefusal is the reply for a backend /rewind does not drive ("" for cc, codex and pi). It reads no pane.
func rewindBackendRefusal(backend string) string {
	switch backend {
	case "cc", "codex", "pi":
		return ""
	}
	return "❌ Rewind from Telegram works with Claude Code, codex and pi sessions."
}

// rewindCheck is the refusal check RewindList and RewindDrive run first inside the per-target injection mutex, before
// the box clear: pane gone, dialog pending, agent busy ("" = go on). It reads the pane and the stores and takes no
// injection mutex, so it cannot deadlock inside the lock.
func rewindCheck(bs *types.BotState, tmuxStr string, target injector.TmuxTarget) func() string {
	return func() string {
		if !injector.SessionExists(target) {
			return "❌ The tmux pane no longer exists."
		}
		if _, ok := bs.PendingWait.FindByTmuxTarget(tmuxStr); ok {
			return "❓ A question or permission is waiting. Answer it or press /t, then run /rewind again."
		}
		if state, _ := helpers.LiveBusyState(bs.HookRunning, tmuxStr); state != stores.BusyIdle {
			return "⏳ The agent is busy. Press /t to interrupt it, then run /rewind again."
		}
		return ""
	}
}

// rewindErrText is the reply for a failed RewindList or RewindDrive; pane is true when the pane capture follows the text.
func rewindErrText(err error) (text string, pane bool) {
	var refused *helpers.RewindRefusedError
	switch {
	case errors.As(err, &refused):
		return refused.Msg, false
	case errors.Is(err, helpers.ErrRewindBoxNotEmpty):
		return "❌ The input box still holds text after 3 tries. Clear it in the terminal and run /rewind again.", false
	case errors.Is(err, helpers.ErrRewindNoMenu):
		return "❌ Could not open the rewind menu. The pane is shown below, nothing was changed.", true
	case errors.Is(err, helpers.ErrRewindListChanged):
		return "⚠ The list changed. Run /rewind again.", false
	case errors.Is(err, helpers.ErrRewindNoAction):
		return "⚠ The menu does not offer that action. Run /rewind again.", false
	case errors.Is(err, helpers.ErrRewindSettle):
		return "⚠ Still working after 60 s. The pane is shown below.", true
	case errors.Is(err, helpers.ErrRewindStillSummarizing):
		return "⏳ Still summarizing after 10 min.", true
	}
	return fmt.Sprintf("❌ Rewind failed: %v", err), false
}

// handleRewindCommand handles /rewind: read the CLI's own list of earlier messages and post it as buttons. The status
// message goes out before the read (RewindList waits for the injection mutex and the menu takes seconds); that same
// message is then edited into the list, the refusal or the error text, and the pane capture of an error stays a separate
// message.
func handleRewindCommand(bs *types.BotState, c tele.Context) error {
	tmuxStr, target, msg := resolveSessionTarget(bs, c)
	if msg != "" {
		return c.Reply(msg)
	}
	backend := helpers.DetectBackend(tmuxStr)
	if msg := rewindBackendRefusal(backend); msg != "" {
		return c.Reply(msg)
	}
	status, err := helpers.RetrySend(bs.Bot, c.Chat(), "⏳ Reading the rewind menu…", inTopic(topicOf(c.Message()))...)
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to send: %v", err))
	}
	entries, more, err := helpers.RewindList(target, backend, rewindPageSize, rewindCheck(bs, tmuxStr, target))
	if err != nil {
		logger.Info(fmt.Sprintf("Rewind list not read: target=%s err=%v", tmuxStr, err))
		text, pane := rewindErrText(err)
		helpers.RetryEdit(bs.Bot, status, text)
		if !pane {
			return nil
		}
		return handleCaptureCommand(bs, c, target, true)
	}
	if len(entries) == 0 {
		helpers.RetryEdit(bs.Bot, status, "No earlier message to go back to.")
		return nil
	}
	menu := &rewindMenu{Target: target, Backend: backend, Entries: entries, More: more, Picked: -1}
	text, kb := rewindListView(menu, tmuxStr)
	if _, err := helpers.RetryEdit(bs.Bot, status, text, kb, tele.ModeHTML); err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to send: %v", err))
	}
	bs.RewindMenus.Store(rewindKey{c.Chat().ID, status.ID}, menu)
	logger.Info(fmt.Sprintf("Rewind list sent: target=%s backend=%s entries=%d more=%v msg_id=%d", tmuxStr, backend, len(entries), more, status.ID))
	return nil
}

// rewindBodyLabel is a label as the list body shows it: cut after rewindBodyRunes characters, with "…" when it was cut.
func rewindBodyLabel(label string) string {
	if r := []rune(label); len(r) > rewindBodyRunes {
		return string(r[:rewindBodyRunes]) + "…"
	}
	return label
}

// rewindButtonLabel is the text of an entry button: the number, then the start of the label (at most rewindButtonCols
// display columns, a CJK character counts 2) and "…" when the label was cut.
func rewindButtonLabel(n int, label string) string {
	start, cut := markdown.TruncateWidth(label, rewindButtonCols)
	if cut {
		start += "…"
	}
	return fmt.Sprintf("%d. %s", n, start)
}

// rewindListView renders the page of entries starting at menu.Page. The body lists the page oldest first and newest
// last, numbered from the newest message of the whole list (1) and continuing across pages, with cc's annotation
// indented under its entry; the entry buttons follow the body order, one to a row, so the newest entry of the page is
// directly above the bottom rows. "⬆️ Older" is the top row when older entries exist; "⬇️ Newer" (older pages only) and
// "❌ Cancel" each take a row of their own below the entry buttons.
func rewindListView(menu *rewindMenu, tmuxStr string) (string, *tele.ReplyMarkup) {
	kb := &tele.ReplyMarkup{}
	var rows []tele.Row
	if menu.Page+rewindPageSize < len(menu.Entries) || menu.More {
		rows = append(rows, kb.Row(kb.Data("⬆️ Older", "rw_more")))
	}
	text := fmt.Sprintf("⏪ <b>Rewind</b> - %s · 📟 %s\nNewest at the bottom\n", markdown.EscapeHTML(backendDisplayName(menu.Backend)), markdown.EscapeHTML(notify.FormatPaneID(tmuxStr)))
	for i := rewindPageEnd(menu) - 1; i >= menu.Page; i-- {
		e := menu.Entries[i]
		first, _, _ := strings.Cut(e.Label, "\n")
		n := rewindNumber(i)
		text += fmt.Sprintf("\n%d. %s", n, markdown.EscapeHTML(rewindBodyLabel(first)))
		if e.Annotation != "" {
			text += "\n   " + markdown.EscapeHTML(e.Annotation)
		}
		rows = append(rows, kb.Row(kb.Data(rewindButtonLabel(n, first), "rw_pick", strconv.Itoa(i))))
	}
	if menu.Page > 0 {
		rows = append(rows, kb.Row(kb.Data("⬇️ Newer", "rw_newer")))
	}
	rows = append(rows, kb.Row(kb.Data("❌ Cancel", "rw_cancel")))
	kb.Inline(rows...)
	return text, kb
}

// rewindActionView renders the picked entry (its number in the list, label and cc's annotation indented by three spaces)
// and the actions the CLI offers for it, one per row with the action's icon in front, then "◀️ Back" and "❌ Cancel",
// each on a row of its own.
func rewindActionView(menu *rewindMenu) (string, *tele.ReplyMarkup) {
	entry := menu.Entries[menu.Picked]
	kb := &tele.ReplyMarkup{}
	var rows []tele.Row
	for i, action := range helpers.RewindActions(menu.Backend, entry) {
		rows = append(rows, kb.Row(kb.Data(rewindActionIcon(action)+" "+action, "rw_act", strconv.Itoa(i))))
	}
	rows = append(rows, kb.Row(kb.Data("◀️ Back", "rw_back")), kb.Row(kb.Data("❌ Cancel", "rw_cancel")))
	kb.Inline(rows...)
	first, _, _ := strings.Cut(entry.Label, "\n")
	text := fmt.Sprintf("%d. %s", rewindNumber(menu.Picked), markdown.EscapeHTML(helpers.TruncateStr(first, 500)))
	if entry.Annotation != "" {
		text += "\n   " + markdown.EscapeHTML(entry.Annotation)
	}
	return text, kb
}

// rewindFail edits the tapped message to the error text, posts the pane as a collapsed capture when the error asks for it and drops the menu.
func rewindFail(bs *types.BotState, c tele.Context, menu *rewindMenu, err error) error {
	logger.Info(fmt.Sprintf("Rewind not done: target=%s err=%v", injector.FormatTarget(menu.Target), err))
	text, pane := rewindErrText(err)
	bs.RewindMenus.Delete(rewindKeyOf(c))
	helpers.RetryEdit(bs.Bot, c.Message(), text)
	if !pane {
		return nil
	}
	if content, cerr := injector.CapturePane(menu.Target); cerr == nil && content != "" {
		SendCaptureReply(bs.Bot, c.Chat(), topicOf(c.Message()), bs.Pages, content, true)
	}
	return nil
}

// rewindNotConfirmed is the reply when the action key was pressed but its effect never reached the screen (cc, and pi's Summarize; the wait is 60 s, 10 min for a Summarize): the box is not read or cleared, the tapped message becomes the warning in one edit and the pane follows as a collapsed capture.
func rewindNotConfirmed(bs *types.BotState, c tele.Context, menu *rewindMenu, action string) error {
	logger.Info(fmt.Sprintf("Rewind not confirmed: target=%s action=%q", injector.FormatTarget(menu.Target), action))
	text := "⚠ Not confirmed: " + action + "\nThe conversation did not go back."
	if strings.HasPrefix(action, "Summarize") {
		text = "⚠ Not confirmed: " + action + "\nNo summary appeared."
	}
	helpers.RetryEdit(bs.Bot, c.Message(), text)
	if content, err := injector.CapturePane(menu.Target); err == nil && content != "" {
		SendCaptureReply(bs.Bot, c.Chat(), topicOf(c.Message()), bs.Pages, content, true)
	}
	return nil
}

// rewindPostRunes is the size of one piece of a restored message: Telegram caps a message at 4096 characters and the first piece shares the edited message of the buttons with the done line. The box was cleared, so that message and the further posts are the only copy.
const rewindPostRunes = 3500

// rewindParts cuts text into pieces of at most rewindPostRunes runes (none for "").
func rewindParts(text string) []string {
	r := []rune(text)
	var parts []string
	for len(r) > rewindPostRunes {
		parts = append(parts, string(r[:rewindPostRunes]))
		r = r[rewindPostRunes:]
	}
	if len(r) > 0 {
		parts = append(parts, string(r))
	}
	return parts
}

// rewindDoneText is the tapped message after a rewind that went through (HTML): the done line, then each line that applies (cc's Restore code cannot be checked by the bot; the message is still in the input box), then the restored message in a <pre> block (the first piece of rewindParts). rest holds the further pieces; the caller posts each on its own.
func rewindDoneText(action string, res helpers.RewindResult) (text string, rest []string) {
	text = "✅ Rewound: " + action
	if action == "Restore code" {
		text += "\nFiles are not checked by the bot."
	}
	if res.InBox {
		text += "\n⚠ Your message is still in the input box."
	}
	parts := rewindParts(res.Restored)
	if len(parts) == 0 {
		return text, nil
	}
	return text + "\n↩️ Your message (tap to copy):\n<pre>" + markdown.EscapeHTML(parts[0]) + "</pre>", parts[1:]
}

// registerRewindCallbacks registers the buttons of the /rewind message; the state between taps is bs.RewindMenus.
func registerRewindCallbacks(bs *types.BotState) {
	bot := bs.Bot
	expired := func(c tele.Context) {
		helpers.RetryEdit(bot, c.Message(), "Menu expired. Send /rewind again.")
	}
	// withMenu runs fn on the stored menu of the tapped message; a missing menu (bot restart) answers "expired".
	withMenu := func(name string, fn func(c tele.Context, menu *rewindMenu) error) {
		bot.Handle(&tele.InlineButton{Unique: name}, func(c tele.Context) error {
			val, ok := bs.RewindMenus.Load(rewindKeyOf(c))
			if !ok {
				expired(c)
				return c.Respond()
			}
			menu := val.(*rewindMenu)
			if !menu.mu.TryLock() {
				return c.Respond()
			}
			defer menu.mu.Unlock()
			return fn(c, menu)
		})
	}
	withMenu("rw_pick", func(c tele.Context, menu *rewindMenu) error {
		i, err := strconv.Atoi(c.Data())
		if err != nil || i < 0 || i >= len(menu.Entries) {
			return c.Respond()
		}
		menu.Picked = i
		text, kb := rewindActionView(menu)
		helpers.RetryEdit(bot, c.Message(), text, kb, tele.ModeHTML)
		return c.Respond()
	})
	withMenu("rw_back", func(c tele.Context, menu *rewindMenu) error {
		menu.Picked = -1
		text, kb := rewindListView(menu, injector.FormatTarget(menu.Target))
		helpers.RetryEdit(bot, c.Message(), text, kb, tele.ModeHTML)
		return c.Respond()
	})
	withMenu("rw_newer", func(c tele.Context, menu *rewindMenu) error {
		menu.Page = max(menu.Page-rewindPageSize, 0)
		text, kb := rewindListView(menu, injector.FormatTarget(menu.Target))
		helpers.RetryEdit(bot, c.Message(), text, kb, tele.ModeHTML)
		return c.Respond()
	})
	withMenu("rw_more", func(c tele.Context, menu *rewindMenu) error {
		// Answer first: reading the list opens and closes the CLI's menu.
		c.Respond()
		tmuxStr := injector.FormatTarget(menu.Target)
		if menu.Page+rewindPageSize >= len(menu.Entries) {
			helpers.RetryEdit(bot, c.Message(), "⏳ Loading older messages…")
			entries, more, err := helpers.RewindList(menu.Target, menu.Backend, len(menu.Entries)+rewindPageSize, rewindCheck(bs, tmuxStr, menu.Target))
			if err != nil {
				return rewindFail(bs, c, menu, err)
			}
			menu.Entries, menu.More = entries, more
		}
		if menu.Page+rewindPageSize < len(menu.Entries) {
			menu.Page += rewindPageSize
		}
		text, kb := rewindListView(menu, tmuxStr)
		helpers.RetryEdit(bot, c.Message(), text, kb, tele.ModeHTML)
		return nil
	})
	withMenu("rw_cancel", func(c tele.Context, menu *rewindMenu) error {
		bs.RewindMenus.Delete(rewindKeyOf(c))
		helpers.RetryEdit(bot, c.Message(), "Rewind cancelled")
		return c.Respond()
	})
	bot.Handle(&tele.InlineButton{Unique: "rw_act"}, func(c tele.Context) error {
		val, ok := bs.RewindMenus.Load(rewindKeyOf(c))
		if !ok {
			expired(c)
			return c.Respond()
		}
		menu := val.(*rewindMenu)
		if !menu.mu.TryLock() {
			return c.Respond()
		}
		defer menu.mu.Unlock()
		// The menu stays stored while the drive runs: a tap during it finds the lock held and is answered with c.Respond() only. The delete is deferred after the Unlock defer, so it runs first, still under the lock: a tap after the handler finds nothing.
		defer bs.RewindMenus.Delete(rewindKeyOf(c))
		c.Respond(&tele.CallbackResponse{Text: "⏳ Rewinding"})
		var actions []string
		if menu.Picked >= 0 {
			actions = helpers.RewindActions(menu.Backend, menu.Entries[menu.Picked])
		}
		i, err := strconv.Atoi(c.Data())
		if err != nil || i < 0 || i >= len(actions) {
			expired(c)
			return nil
		}
		action := actions[i]
		tmuxStr := injector.FormatTarget(menu.Target)
		// A summarize is a model call that can run for minutes: say so in the tapped message (no buttons) before the drive starts. The result below edits the same message.
		if strings.HasPrefix(action, "Summarize") {
			helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("⏳ Summarizing: %s. This can take up to 10 minutes.", action))
		}
		res, err := helpers.RewindDrive(menu.Target, menu.Backend, menu.Picked, menu.Entries[menu.Picked], action, rewindCheck(bs, tmuxStr, menu.Target))
		if errors.Is(err, helpers.ErrRewindNotConfirmed) {
			return rewindNotConfirmed(bs, c, menu, action)
		}
		if err != nil {
			return rewindFail(bs, c, menu, err)
		}
		logger.Info(fmt.Sprintf("Rewind done: target=%s backend=%s action=%q restored_len=%d", tmuxStr, menu.Backend, action, len(res.Restored)))
		text, rest := rewindDoneText(action, res)
		helpers.RetryEdit(bot, c.Message(), text, tele.ModeHTML)
		for _, part := range rest {
			helpers.RetrySend(bot, c.Chat(), "<pre>"+markdown.EscapeHTML(part)+"</pre>", inTopic(topicOf(c.Message()), tele.ModeHTML)...)
		}
		if res.Pane != "" {
			SendCaptureReply(bot, c.Chat(), topicOf(c.Message()), bs.Pages, res.Pane, true)
		}
		return nil
	})
}

package handlers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// BuildMergeNotifyText builds the merge notification text with title, status line, and collected items.
func BuildMergeNotifyText(status string, items []string) string {
	var b strings.Builder
	b.WriteString("📎 <b>Merge mode</b>\n")
	b.WriteString(status)
	if len(items) > 0 {
		b.WriteString("\n──────\n")
		for _, item := range items {
			b.WriteString(markdown.EscapeHTML(item))
			b.WriteString("\n")
		}
		b.WriteString("──────")
	}
	return b.String()
}

// ParseMergeItems extracts collected message items from existing merge notification text.
// Parses content between "──────" delimiters.
func ParseMergeItems(text string) []string {
	parts := strings.SplitN(text, "──────", 3)
	if len(parts) < 2 {
		return nil
	}
	content := strings.TrimSpace(parts[1])
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	var items []string
	for _, line := range lines {
		if line != "" {
			items = append(items, line)
		}
	}
	return items
}

// CollapseEntry collapses a collapsible PageEntry; returns the text to display (header first line).
func CollapseEntry(entry *stores.PageEntry) string {
	entry.Collapsed = true
	return strings.SplitN(entry.Header, "\n", 2)[0]
}

// collapsibleBody returns the display text for one chunk of a collapsible capture/@forward entry.
// A pane-capture rich entry (CaptureHeader) is rendered WITHOUT a <pre> block — its chunks are already
// HTML-escaped at send time, so they display as rich text. An @forward rich entry wraps its raw chunk
// in an escaped <pre> block. A legacy (RawMode) entry shows the chunk as plain text. The header is
// stored already-safe for the entry's format.
func collapsibleBody(entry *stores.PageEntry, chunk string) string {
	if entry.Rich {
		if entry.Header == CaptureHeader {
			return entry.Header + chunk
		}
		return entry.Header + helpers.WrapRichPre(chunk)
	}
	return entry.Header + chunk
}

// ExpandEntry expands a collapsible PageEntry to its last-viewed page (CurrentPage, bounds-checked);
// returns the text to display and the page number used.
func ExpandEntry(entry *stores.PageEntry) (string, int) {
	entry.Collapsed = false
	page := entry.CurrentPage
	if page < 1 || page > len(entry.Chunks) {
		page = 1
	}
	return collapsibleBody(entry, entry.Chunks[page-1]), page
}

// NavigateEntry records page navigation on a collapsible PageEntry (sets CurrentPage)
// and returns the text to display for that page. Caller must validate pageNum range first.
func NavigateEntry(entry *stores.PageEntry, pageNum int) string {
	entry.CurrentPage = pageNum
	return collapsibleBody(entry, entry.Chunks[pageNum-1])
}

// askMarkupLabels flattens an inline keyboard's button labels (row-major). Used to inspect the ✅
// prefix on selected AskUserQuestion options from tests.
func askMarkupLabels(m *tele.ReplyMarkup) []string {
	var labels []string
	for _, row := range m.InlineKeyboard {
		for _, b := range row {
			labels = append(labels, b.Text)
		}
	}
	return labels
}

// ToggleAskAndReedit toggles a multiSelect AskUserQuestion option in the store, rebuilds the inline
// markup (with a ✅ on selected options), and re-edits the message via the format-aware freeze helper.
// Fix 15: a rich AskUserQuestion has an empty .Text, so a plain RetryEdit with empty text fails to
// update the buttons — RetryFreezeEditAuto uses the stored MsgText/Rich instead. Returns the resulting
// button labels (nil only when the store toggle itself failed, i.e. the entry expired) and the re-edit
// error (nil on success). Shared by the "tool" callback and the /test/callback endpoint so tests
// exercise the exact production re-edit path.
func ToggleAskAndReedit(bs *types.BotState, editMsg tele.Editable, snap stores.EntrySnapshot, qIdx, optIdx int) ([]string, error) {
	questions, err := bs.PendingWait.ToggleQuestionOption(snap.UUID, qIdx, optIdx)
	if err != nil {
		return nil, err
	}
	newMarkup := helpers.RebuildAskMarkup(questions)
	_, editErr := helpers.RetryFreezeEditAuto(bs.Bot, editMsg, snap.Rich, snap.MsgText, newMarkup)
	return askMarkupLabels(newMarkup), editErr
}

// RegisterCallbackHandlers registers all Telegram inline button callback handlers.
func RegisterCallbackHandlers(bs *types.BotState) {
	bot := bs.Bot
	bot.Handle(&tele.InlineButton{Unique: "p"}, func(c tele.Context) error {
		pageNum, err := strconv.Atoi(c.Data())
		if err != nil {
			return c.Respond()
		}
		entry, ok := bs.Pages.Get(c.Message().ID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Page expired"})
		}
		if pageNum < 1 || pageNum > len(entry.Chunks) {
			return c.Respond()
		}
		logger.Info(fmt.Sprintf("p callback: page=%d msgID=%d hasHeader=%v", pageNum, c.Message().ID, entry.Header != ""))
		var text string
		var kb *tele.ReplyMarkup
		if entry.Header != "" {
			// @ forward message or pane capture: show header + chunk with collapse button
			text = NavigateEntry(entry, pageNum)
			kb = helpers.BuildPageKeyboardWithExtra(pageNum, len(entry.Chunks), []tele.Row{CaptureExtraRow(entry.Header == CaptureHeader, true)})
		} else if entry.PermRows != nil {
			text = entry.Chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.Chunks))
			kb = helpers.BuildPageKeyboardWithExtra(pageNum, len(entry.Chunks), entry.PermRows)
		} else {
			// S12b: Chunks/LegacyChunks are BODY chunks — re-wrap via BuildNotificationText. The rich
			// payload gets the <hr/> boundary for Cron/SessionSend; the legacy payload never does.
			nd := notify.NotificationData{
				Event:          entry.Event,
				Project:        entry.Project,
				CWD:            entry.CWD,
				Body:           entry.Chunks[pageNum-1],
				TmuxTarget:     entry.TmuxTarget,
				Page:           pageNum,
				TotalPages:     len(entry.Chunks),
				CronJobID:      entry.CronJobID,
				CronName:       entry.CronName,
				CronNoHeader:   entry.CronNoHeader,
				SendFrom:       entry.SendFrom,
				SendNoHeader:   entry.SendNoHeader,
				DeliveryStatus: entry.DeliveryStatus,
			}
			text = notify.BuildNotificationText(nd)
			if entry.Event == "Cron" || entry.Event == "SessionSend" {
				text = helpers.InsertRichHr(text)
			}
			kb = helpers.BuildPageKeyboardWithExtra(pageNum, len(entry.Chunks), entry.PermRows)
		}
		// Legacy chunk paired 1:1 with Chunks; fall back to the rich text when absent (backward compat).
		legacyText := text
		if entry.Header == "" && entry.PermRows == nil && len(entry.Chunks) == len(entry.LegacyChunks) && pageNum-1 < len(entry.LegacyChunks) {
			ndLegacy := notify.NotificationData{
				Event:          entry.Event,
				Project:        entry.Project,
				CWD:            entry.CWD,
				Body:           entry.LegacyChunks[pageNum-1],
				TmuxTarget:     entry.TmuxTarget,
				Page:           pageNum,
				TotalPages:     len(entry.Chunks),
				CronJobID:      entry.CronJobID,
				CronName:       entry.CronName,
				CronNoHeader:   entry.CronNoHeader,
				SendFrom:       entry.SendFrom,
				SendNoHeader:   entry.SendNoHeader,
				DeliveryStatus: entry.DeliveryStatus,
			}
			legacyText = notify.BuildNotificationText(ndLegacy)
		}
		if entry.RawMode {
			_, err = helpers.RetryEdit(bot, c.Message(), text, kb)
		} else if entry.Rich {
			// G1 mixed-era: a rich-sent message is re-rendered rich. Permission/capture/forward
			// content is code/raw → skip entity detection; standard notifications are prose (C3).
			_, err = helpers.RetryEditRich(bot, c.Message(), text, helpers.RichSendOpts{
				Markup:              kb,
				SkipEntityDetection: entry.PermRows != nil || entry.Header != "",
				LegacyHTML:          legacyText,
			})
		} else {
			_, err = helpers.RetryEdit(bot, c.Message(), text, kb, tele.ModeHTML)
		}
		if err != nil {
			logger.Debug(fmt.Sprintf("edit page error: %v", err))
		}
		return c.Respond()
	})

	// "ce" callback: collapse/expand collapsible messages (@ forward, pane capture, etc.)
	bot.Handle(&tele.Btn{Unique: "ce"}, func(c tele.Context) error {
		entry, ok := bs.Pages.Get(c.Message().ID)
		logger.Info(fmt.Sprintf("ce callback: data=%s msgID=%d hasEntry=%v", c.Data(), c.Message().ID, entry != nil))
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Expired"})
		}
		var text string
		var kb *tele.ReplyMarkup
		if c.Data() == "c" {
			// Collapse: show only the first line of the header (the link line)
			text = CollapseEntry(entry)
			kb = &tele.ReplyMarkup{}
			kb.Inline(CaptureExtraRow(entry.Header == CaptureHeader, false))
		} else {
			var page int
			text, page = ExpandEntry(entry)
			extraRow := CaptureExtraRow(entry.Header == CaptureHeader, true)
			if len(entry.Chunks) > 1 {
				kb = helpers.BuildPageKeyboardWithExtra(page, len(entry.Chunks), []tele.Row{extraRow})
			} else {
				kb = &tele.ReplyMarkup{}
				kb.Inline(extraRow)
			}
		}
		var err error
		if entry.Rich {
			// Rich collapsible (capture/@forward): edit via editMessageText rich_message; the raw
			// pane/forward content is code-like → skip entity detection (C3).
			_, err = helpers.RetryEditRich(bot, c.Message(), text, helpers.RichSendOpts{
				Markup:              kb,
				SkipEntityDetection: entry.Header != "",
				LegacyHTML:          text,
			})
		} else if entry.RawMode {
			_, err = helpers.RetryEdit(bot, c.Message(), text, kb)
		} else {
			_, err = helpers.RetryEdit(bot, c.Message(), text, kb, tele.ModeHTML)
		}
		logger.Info(fmt.Sprintf("ce edit: collapsed=%v err=%v", entry.Collapsed, err))
		if err != nil {
			logger.Debug(fmt.Sprintf("ce edit error: %v", err))
		}
		return c.Respond()
	})

	// "del" callback: delete the transient message the button sits on
	bot.Handle(&tele.InlineButton{Unique: "del"}, func(c tele.Context) error {
		msgID := c.Message().ID
		if err := c.Delete(); err != nil {
			logger.Info(fmt.Sprintf("del callback: delete failed msg_id=%d err=%v", msgID, err))
			return c.Respond(&tele.CallbackResponse{Text: "⚠️ Can't delete (>48h)"})
		}
		logger.Info(fmt.Sprintf("del callback: deleted msg_id=%d", msgID))
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "perm"}, func(c tele.Context) error {
		decision := c.Data()
		if decision == "cancel" {
			doCancelPerm(bs, c.Message().ID)
			return c.Respond(&tele.CallbackResponse{Text: "❌ Cancelled"})
		}
		// Use FindByMsgIDSnapshot — safe because msgID comes from real TG callback
		d, err := doDecidePerm(bs, c.Message().ID, decision)
		if err != nil {
			if err.Error() == "session disconnected" {
				return c.Respond(&tele.CallbackResponse{Text: "⚠️ Session disconnected"})
			}
			return c.Respond(&tele.CallbackResponse{Text: "Expired or invalid"})
		}
		displayText := decision
		if decision == "sAll" || strings.HasPrefix(decision, "s") {
			displayText = "Always Allow"
		}
		_ = d
		return c.Respond(&tele.CallbackResponse{Text: "✅ " + displayText})
	})

	bot.Handle(&tele.InlineButton{Unique: "tool"}, func(c tele.Context) error {
		parts := strings.SplitN(c.Data(), "|", 2)
		if len(parts) < 2 {
			return c.Respond(&tele.CallbackResponse{Text: "Invalid data"})
		}
		toolName := parts[0]
		switch toolName {
		case "AskUserQuestion":
			// Use FindByMsgIDSnapshot — safe because msgID comes from real TG callback
			snap, ok := bs.PendingWait.FindByMsgIDSnapshot(c.Message().ID)
			if !ok {
				return c.Respond(&tele.CallbackResponse{Text: "Expired"})
			}
			// Only handle AskUserQuestion entries here
			if snap.ToolName != "AskUserQuestion" {
				return c.Respond(&tele.CallbackResponse{Text: "Expired"})
			}
			// Check session alive before processing tool response
			if snap.TmuxTarget != "" && !checkSessionAlive(bs, snap.TmuxTarget) {
				return c.Respond(&tele.CallbackResponse{Text: "⚠️ Session disconnected"})
			}
			if snap.Resolved {
				return c.Respond(&tele.CallbackResponse{Text: "Already answered"})
			}
			if parts[1] == "cancel" {
				doCancelAsk(bs, c.Message().ID)
				return c.Respond(&tele.CallbackResponse{Text: "❌ Cancelled"})
			} else if parts[1] == "chat" {
				if err := doChatAsk(bs, c.Message().ID); err != nil {
					return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
				}
				return c.Respond(&tele.CallbackResponse{Text: "Chat mode"})
			} else if parts[1] == "submit" {
				// Get current questions from store for building answers
				questions, hasQ := bs.PendingWait.GetQuestions(snap.UUID)
				if !hasQ {
					return c.Respond(&tele.CallbackResponse{Text: "Expired"})
				}
				if err := doRespondAsk(bs, c.Message().ID, helpers.BuildAnswers(questions), ""); err != nil {
					return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
				}
				return c.Respond(&tele.CallbackResponse{Text: "✅ Submitted"})
			} else {
				split := strings.SplitN(parts[1], ":", 2)
				qIdx, _ := strconv.Atoi(split[0])
				optIdx, _ := strconv.Atoi(split[1])
				if qIdx >= len(snap.Questions) {
					return c.Respond(&tele.CallbackResponse{Text: "Invalid question"})
				}
				if snap.Questions[qIdx].MultiSelect {
					// Toggle option + rich-aware re-edit (Fix 15). Shared with /test/callback via
					// ToggleAskAndReedit so the ✅ checkmark path is E2E-tested.
					logger.Info(fmt.Sprintf("AskUserQuestion multiSelect toggle: msg_id=%d q=%d opt=%d label=%s", c.Message().ID, qIdx, optIdx, snap.Questions[qIdx].OptionLabels[optIdx]))
					// labels==nil only when the store toggle failed (entry expired); a transient re-edit error
					// is non-fatal (matches the pre-refactor behavior of ignoring the edit error).
					if labels, _ := ToggleAskAndReedit(bs, c.Message(), snap, qIdx, optIdx); labels == nil {
						return c.Respond(&tele.CallbackResponse{Text: "Expired"})
					}
					return c.Respond(&tele.CallbackResponse{Text: "Toggled"})
				} else {
					// Select option — use store method for atomic update
					questions, err := bs.PendingWait.SelectQuestionOption(snap.UUID, qIdx, optIdx)
					if err != nil {
						return c.Respond(&tele.CallbackResponse{Text: "Expired"})
					}
					hasSubmit := len(questions) > 1
					for _, q := range questions {
						if q.MultiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						if err := doRespondAsk(bs, c.Message().ID, helpers.BuildAnswers(questions), ""); err != nil {
							return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
						}
						return c.Respond(&tele.CallbackResponse{Text: "✅ Selected"})
					}
					logger.Info(fmt.Sprintf("AskUserQuestion option selected: msg_id=%d q=%d opt=%d label=%s", c.Message().ID, qIdx, optIdx, snap.Questions[qIdx].OptionLabels[optIdx]))
					newMarkup := helpers.RebuildAskMarkup(questions)
					// Fix 15: the AskUserQuestion message may have been sent rich, in which case c.Message().Text
					// is empty and a plain RetryEdit with empty text fails to update the button markup (the ✅
					// never appears). Re-edit via the format-aware freeze helper using the stored MsgText/Rich.
					helpers.RetryFreezeEditAuto(bot, c.Message(), snap.Rich, snap.MsgText, newMarkup)
					return c.Respond(&tele.CallbackResponse{Text: "Selected"})
				}
			}
		}
		return c.Respond()
	})

	// namesPendingSession tracks session name inputs (msgID -> sessionID)
	var namesPendingSession sync.Map
	bot.Handle(&tele.InlineButton{Unique: "names"}, func(c tele.Context) error {
		sessionID := c.Data()
		info, ok := bs.SessionState.All()[sessionID]
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Session not found"})
		}
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("Session: %s\nReply with the new name for this session.", notify.FormatPaneID(info.TmuxTarget)), tele.ModeHTML)
		namesPendingSession.Store(c.Message().ID, sessionID)
		return c.Respond(&tele.CallbackResponse{Text: "Reply with name"})
	})

	// "cwd" callback: update CWDSource setting
	bot.Handle(&tele.InlineButton{Unique: "cwd"}, func(c tele.Context) error {
		source := c.Data() // "tmux" or "payload"
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		cfg.CWDSource = source
		if err := config.SaveAppConfig(cfg); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
		}
		logger.Info(fmt.Sprintf("CWDSource updated to: %s", source))
		if IsSettingsMenu(bs, c.Message().ID) {
			sel := &tele.ReplyMarkup{}
			btnTmux := sel.Data("📟 tmux", "cwd", "tmux")
			btnPayload := sel.Data("📦 payload", "cwd", "payload")
			sel.Inline(sel.Row(btnTmux, btnPayload))
			appendBackButton(sel)
			helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("🔧 CWD source: %s\n\n✅ Saved. Select source:", source), sel, tele.ModeHTML)
		} else {
			helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("✅ CWD source set to: %s", source), tele.ModeHTML)
		}
		return c.Respond(&tele.CallbackResponse{Text: "✅ Saved: " + source})
	})

	// "usage_src" callback: update UsageSource setting and fetch usage
	bot.Handle(&tele.Btn{Unique: "usage_src"}, func(c tele.Context) error {
		source := c.Data()
		cfg, _ := config.LoadAppConfig()
		cfg.UsageSource = source
		config.SaveAppConfig(cfg)
		fetchKb := &tele.ReplyMarkup{}
		appendDeleteButton(fetchKb)
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("📊 Usage source → <b>%s</b>\n⏳ Fetching...", source), fetchKb, tele.ModeHTML)
		if source == "api" {
			return handleUsageCommandAPI(bs, c, c.Message())
		}
		return handleUsageCommandTmux(bs, c, c.Message())
	})

	bot.Handle(&tele.InlineButton{Unique: "resume"}, func(c tele.Context) error {
		sessionID := c.Data()
		targetPtr, err := helpers.ExtractTmuxTargetFromText(c.Message().Text)
		if err != nil || targetPtr == nil {
			return c.Respond(&tele.CallbackResponse{Text: "No tmux target found"})
		}
		if !checkSessionAlive(bs, injector.FormatTarget(*targetPtr)) {
			return c.Respond(&tele.CallbackResponse{Text: "⚠️ Session disconnected"})
		}
		if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, *targetPtr, "/resume "+sessionID); err != nil {
			logger.Error(fmt.Sprintf("resume inject failed: target=%s session=%s err=%v", injector.FormatTarget(*targetPtr), sessionID, err))
			return c.Respond(&tele.CallbackResponse{Text: "❌ Injection failed"})
		}
		logger.Info(fmt.Sprintf("Resume injected: target=%s session=%s", injector.FormatTarget(*targetPtr), sessionID))
		// Rebuild keyboard with ✅ on selected button
		markup := &tele.ReplyMarkup{}
		var rows []tele.Row
		for _, row := range c.Message().ReplyMarkup.InlineKeyboard {
			var btns []tele.Btn
			for _, btn := range row {
				label := btn.Text
				// Extract per-button session ID from callback data (\f<unique>|<sessionID>)
				btnSessionID := sessionID
				if idx := strings.Index(btn.Data, "|"); idx != -1 {
					btnSessionID = btn.Data[idx+1:]
				}
				if btnSessionID == sessionID && !strings.HasPrefix(label, "✅") {
					label = "✅ " + label
				}
				btns = append(btns, markup.Data(label, "resume", btnSessionID))
			}
			rows = append(rows, markup.Row(btns...))
		}
		markup.Inline(rows...)
		if _, err := helpers.RetryEdit(bot, c.Message(), c.Message().Text, markup, tele.ModeHTML); err != nil {
			logger.Debug(fmt.Sprintf("resume edit markup error: %v", err))
		}
		recordPending(bs, injector.FormatTarget(*targetPtr), c.Message().Chat.ID, c.Message().ID)
		return c.Respond(&tele.CallbackResponse{Text: "✅ Resuming"})
	})

	bot.Handle(&tele.InlineButton{Unique: "bind_select"}, func(c tele.Context) error {
		data := strings.TrimSpace(c.Data())
		num, err := strconv.Atoi(data)
		if err != nil {
			helpers.RetryEdit(bot, c.Message(), "❌ Invalid selection.", tele.ModeHTML)
			return c.Respond()
		}
		val, ok := bs.BindMenuItems.LoadAndDelete(c.Message().ID)
		if !ok {
			helpers.RetryEdit(bot, c.Message(), "❌ Menu expired. Send /bot_bind again.", tele.ModeHTML)
			return c.Respond()
		}
		ctx := val.(BindMenuContext)
		idx := num - 1
		if idx < 0 || idx >= len(ctx.Items) {
			helpers.RetryEdit(bot, c.Message(), "❌ Selection out of range.", tele.ModeHTML)
			return c.Respond()
		}
		item := ctx.Items[idx]
		creds, err := config.LoadCredentials()
		if err != nil {
			helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to load config: %s", markdown.EscapeHTML(err.Error())), tele.ModeHTML)
			return c.Respond()
		}
		creds.NameRouteMap[item.Key] = config.NameRoute{ChatID: ctx.ChatID, TopicID: ctx.TopicID}
		config.SaveCredentials(creds)
		topicStr := ""
		if ctx.TopicID != 0 {
			topicStr = fmt.Sprintf(", topic=%d", ctx.TopicID)
		}
		logger.Info(fmt.Sprintf("Route bound (menu): key=%s → chat=%d topic=%d", item.Key, ctx.ChatID, ctx.TopicID))
		if IsSettingsMenu(bs, c.Message().ID) {
			showSettingsRoutes(bot, bs, c.Message())
			return c.Respond()
		}
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("✅ Bound to this chat.\n🏷 %s → %d%s", markdown.EscapeHTML(item.Label), ctx.ChatID, topicStr), tele.ModeHTML)
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "unbind_select"}, func(c tele.Context) error {
		data := strings.TrimSpace(c.Data())
		num, err := strconv.Atoi(data)
		if err != nil {
			helpers.RetryEdit(bot, c.Message(), "❌ Invalid selection.", tele.ModeHTML)
			return c.Respond()
		}
		val, ok := bs.UnbindMenuItems.LoadAndDelete(c.Message().ID)
		if !ok {
			helpers.RetryEdit(bot, c.Message(), "❌ Menu expired. Send /bot_unbind again.", tele.ModeHTML)
			return c.Respond()
		}
		keys := val.([]string)
		idx := num - 1
		if idx < 0 || idx >= len(keys) {
			helpers.RetryEdit(bot, c.Message(), "❌ Selection out of range.", tele.ModeHTML)
			return c.Respond()
		}
		name := keys[idx]
		creds, err := config.LoadCredentials()
		if err != nil {
			helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to load config: %s", markdown.EscapeHTML(err.Error())), tele.ModeHTML)
			return c.Respond()
		}
		delete(creds.NameRouteMap, name)
		config.SaveCredentials(creds)
		logger.Info(fmt.Sprintf("Route unbound (menu/name): name=%s", name))
		if IsSettingsMenu(bs, c.Message().ID) {
			showSettingsRoutes(bot, bs, c.Message())
			return c.Respond()
		}
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("✅ Unbound agent name route: %s", markdown.EscapeHTML(name)), tele.ModeHTML)
		return c.Respond()
	})

	btnVerbose := tele.InlineButton{Unique: "verbose"}
	bot.Handle(&btnVerbose, func(c tele.Context) error {
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		action := c.Data()
		if action == "toggle" {
			enabled := cfg.ToolNotifyEnabled == nil || *cfg.ToolNotifyEnabled
			newEnabled := !enabled
			cfg.ToolNotifyEnabled = &newEnabled
		} else {
			enabled := action == "on"
			cfg.ToolNotifyEnabled = &enabled
		}
		if err := config.SaveAppConfig(cfg); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save config"})
		}
		resultEnabled := cfg.ToolNotifyEnabled != nil && *cfg.ToolNotifyEnabled
		var statusText string
		if resultEnabled {
			statusText = "✅ ON"
		} else {
			statusText = "❌ OFF"
		}
		if IsSettingsMenu(bs, c.Message().ID) {
			showSettingsToolNotify(bot, bs, c.Message())
			return c.Respond(&tele.CallbackResponse{Text: "Saved: " + statusText})
		}
		menu := &tele.ReplyMarkup{}
		btnOn := menu.Data("✅ ON", "verbose", "on")
		btnOff := menu.Data("❌ OFF", "verbose", "off")
		menu.Inline(menu.Row(btnOn, btnOff))
		c.Edit(fmt.Sprintf("🔧 Tool Notifications: %s\n\nSelect to toggle:", statusText), menu)
		return c.Respond(&tele.CallbackResponse{Text: "Saved: " + statusText})
	})

	btnToolCompact := tele.InlineButton{Unique: "tool_compact"}
	bot.Handle(&btnToolCompact, func(c tele.Context) error {
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		cfg.ToolNotifyCompact = !cfg.ToolNotifyCompact
		if err := config.SaveAppConfig(cfg); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save config"})
		}
		statusText := "OFF"
		if cfg.ToolNotifyCompact {
			statusText = "ON"
		}
		if IsSettingsMenu(bs, c.Message().ID) {
			showSettingsToolNotify(bot, bs, c.Message())
			return c.Respond(&tele.CallbackResponse{Text: "Compact: " + statusText})
		}
		return c.Respond(&tele.CallbackResponse{Text: "Compact: " + statusText})
	})

	btnToolsToggle := tele.InlineButton{Unique: "tools_toggle"}
	bot.Handle(&btnToolsToggle, func(c tele.Context) error {
		toolName := c.Data()
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		// Toggle tool in list
		found := false
		var newList []string
		for _, t := range cfg.ToolNotifyList {
			if t == toolName {
				found = true
				continue // remove
			}
			newList = append(newList, t)
		}
		if !found {
			newList = append(newList, toolName)
		}
		cfg.ToolNotifyList = newList
		if err := config.SaveAppConfig(cfg); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
		}
		menu := buildToolsMenu(cfg.ToolNotifyList)
		if IsSettingsMenu(bs, c.Message().ID) {
			appendBackButton(menu)
		}
		c.Edit("🔧 Select tools for notifications:\n(Click to toggle)", menu)
		var action string
		if found {
			action = "OFF"
		} else {
			action = "ON"
		}
		return c.Respond(&tele.CallbackResponse{Text: toolName + ": " + action})
	})

	bot.Handle(&tele.InlineButton{Unique: "tools_toggle_all"}, func(c tele.Context) error {
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		// Toggle: if all selected, clear; otherwise select all
		allTools := builtinTools
		allSelected := len(cfg.ToolNotifyList) >= len(allTools)
		if allSelected {
			toolSet := make(map[string]bool, len(cfg.ToolNotifyList))
			for _, t := range cfg.ToolNotifyList {
				toolSet[t] = true
			}
			for _, t := range allTools {
				if !toolSet[t] {
					allSelected = false
					break
				}
			}
		}
		if allSelected {
			cfg.ToolNotifyList = nil
		} else {
			cfg.ToolNotifyList = make([]string, len(allTools))
			copy(cfg.ToolNotifyList, allTools)
		}
		if err := config.SaveAppConfig(cfg); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
		}
		menu := buildToolsMenu(cfg.ToolNotifyList)
		if IsSettingsMenu(bs, c.Message().ID) {
			appendBackButton(menu)
		}
		c.Edit("🔧 Select tools for notifications:\n(Click to toggle)", menu)
		action := "All ON"
		if len(cfg.ToolNotifyList) == 0 {
			action = "All OFF"
		}
		return c.Respond(&tele.CallbackResponse{Text: action})
	})

	bot.Handle(&tele.Btn{Unique: "tools_route_toggle"}, func(c tele.Context) error {
		key := c.Callback().Data
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		route, ok := creds.NameRouteMap[key]
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Route not found"})
		}
		route.ToolNotifyOff = !route.ToolNotifyOff
		creds.NameRouteMap[key] = route
		if err := config.SaveCredentials(creds); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
		}
		status := "ON"
		if route.ToolNotifyOff {
			status = "OFF"
		}
		if IsSettingsMenu(bs, c.Message().ID) {
			showSettingsToolNotify(bot, bs, c.Message())
			return c.Respond(&tele.CallbackResponse{Text: "Tool notify: " + status})
		}
		label := "✅ Tool Notify: ON"
		if route.ToolNotifyOff {
			label = "⬜ Tool Notify: OFF"
		}
		menu := &tele.ReplyMarkup{}
		btn := menu.Data(label, "tools_route_toggle", key)
		menu.Inline(menu.Row(btn))
		c.Bot().Edit(c.Message(), "🔧 Tool notification for this group:", menu)
		return c.Respond(&tele.CallbackResponse{Text: "Tool notify: " + status})
	})

	bot.Handle(&tele.InlineButton{Unique: "merge_submit"}, func(c tele.Context) error {
		chatID := c.Message().Chat.ID
		key := stores.MergeKey(chatID)
		buf, ok := bs.MergeBuffers.Finish(key)
		itemCount := 0
		if buf != nil {
			itemCount = len(buf.Items)
		}
		logger.Info(fmt.Sprintf("Merge submit: key=%s found=%v items=%d", key, ok, itemCount))
		if !ok {
			// Buffer expired — parse items from existing message text
			items := ParseMergeItems(c.Message().Text)
			c.Respond(&tele.CallbackResponse{Text: "⚠️ Buffer expired"})
			return c.Edit(BuildMergeNotifyText("⚠️ Buffer expired", items), tele.ModeHTML)
		}
		if len(buf.Items) == 0 {
			c.Respond(&tele.CallbackResponse{Text: "⚠️ No messages collected"})
			return c.Edit(BuildMergeNotifyText("⚠️ No messages collected", nil), tele.ModeHTML)
		}
		merged := strings.Join(buf.Items, "\n")
		if err := safeInjectText(bs, buf.TmuxTarget, merged); err != nil {
			c.Respond(&tele.CallbackResponse{Text: "❌ Injection failed"})
			return c.Edit(BuildMergeNotifyText(fmt.Sprintf("❌ Injection failed: %v", err), buf.Items), tele.ModeHTML)
		}
		logger.Info(fmt.Sprintf("Merge submitted: target=%s items=%d text=%s", buf.TmuxTarget, len(buf.Items), merged))
		recordPending(bs, buf.TmuxTarget, chatID, c.Message().ID)
		c.Respond(&tele.CallbackResponse{Text: fmt.Sprintf("✅ Submitted %d messages", len(buf.Items))})
		return c.Edit(BuildMergeNotifyText(fmt.Sprintf("✅ Submitted (%d messages)", len(buf.Items)), buf.Items), tele.ModeHTML)
	})

	bot.Handle(&tele.InlineButton{Unique: "merge_cancel"}, func(c tele.Context) error {
		chatID := c.Message().Chat.ID
		key := stores.MergeKey(chatID)
		logger.Info(fmt.Sprintf("Merge cancel: key=%s", key))
		buf, ok := bs.MergeBuffers.Finish(key)
		var items []string
		if ok && buf != nil {
			items = buf.Items
		} else {
			items = ParseMergeItems(c.Message().Text)
		}
		c.Respond(&tele.CallbackResponse{Text: "Cancelled"})
		return c.Edit(BuildMergeNotifyText("❌ Cancelled", items), tele.ModeHTML)
	})

	bot.Handle(&tele.InlineButton{Unique: "bot_new"}, func(c tele.Context) error {
		data := c.Data()
		msgID := c.Message().ID
		val, ok := bs.LaunchPending.Load(msgID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Expired"})
		}
		state := val.(*LaunchState)
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		switch {
		case data == "session_default":
			state.SessionName = cfg.DefaultSessionName
			c.Bot().Edit(c.Message(), fmt.Sprintf("📦 Session name\n✅ %s", state.SessionName))
			bs.LaunchPending.Delete(msgID)
			if state.WorkDir == "" {
				AskWorkDir(bs, c.Bot(), state.ChatID, state)
			} else {
				go ExecuteLaunch(bs, c.Bot(), state.ChatID, state)
			}
		case data == "dir_select":
			state.WorkDir = state.BrowsePath
			c.Bot().Edit(c.Message(), fmt.Sprintf("📂 Working directory\n✅ %s", state.WorkDir))
			bs.LaunchPending.Delete(msgID)
			go ExecuteLaunch(bs, c.Bot(), state.ChatID, state)
		case data == "cd_up":
			parent := filepath.Dir(state.BrowsePath)
			if parent != state.BrowsePath {
				state.BrowsePath = parent
				state.DirPage = 0
			}
			RefreshDirBrowser(c.Bot(), c.Message(), state)
		case strings.HasPrefix(data, "cd:"):
			idx, err := strconv.Atoi(strings.TrimPrefix(data, "cd:"))
			if err == nil {
				dirs, _ := ListSubDirs(state.BrowsePath, state.ShowHidden)
				if idx >= 0 && idx < len(dirs) {
					state.BrowsePath = filepath.Join(state.BrowsePath, dirs[idx])
					state.DirPage = 0
				}
			}
			RefreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "toggle_hidden":
			state.ShowHidden = !state.ShowHidden
			state.DirPage = 0
			RefreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "page_prev":
			if state.DirPage > 0 {
				state.DirPage--
			}
			RefreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "page_next":
			state.DirPage++
			RefreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "page_noop":
			// no-op
		case data == "cancel":
			c.Bot().Edit(c.Message(), "❌ Launch cancelled.")
			bs.LaunchPending.Delete(msgID)
			DeleteLaunchState(state.UUID)
			logger.Info(fmt.Sprintf("bot_new: cancel pressed msg_id=%d uuid=%s", msgID, state.UUID))
		}
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "voice"}, func(c tele.Context) error {
		data := c.Data()
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		if strings.HasPrefix(data, "engine:") {
			engine := strings.TrimPrefix(data, "engine:")
			if engine == "sensevoice" {
				helpers.RetryEdit(bot, c.Message(), "⏳ Checking sensevoice dependencies...", tele.ModeHTML)
				sherpaPath, err := helpers.EnsureSherpaOnnx()
				if err != nil {
					helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ sherpa-onnx: %s", markdown.EscapeHTML(err.Error())), tele.ModeHTML)
					return c.Respond()
				}
				cfg.SherpaOnnxPath = sherpaPath
				modelPath, err := helpers.EnsureSenseVoiceModel()
				if err != nil {
					helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ SenseVoice model: %s", markdown.EscapeHTML(err.Error())), tele.ModeHTML)
					return c.Respond()
				}
				cfg.SenseVoiceModelPath = modelPath
			}
			if engine == "whisper" {
				whisperPath := cfg.WhisperPath
				if whisperPath == "" {
					for _, name := range []string{"whisper-cli", "whisper-cpp", "whisper"} {
						if p, err := exec.LookPath(name); err == nil {
							whisperPath = p
							break
						}
					}
				}
				if whisperPath == "" {
					helpers.RetryEdit(bot, c.Message(), "❌ whisper.cpp not found. Install: `yay -S whisper.cpp-cuda`", tele.ModeHTML)
					return c.Respond()
				}
				cfg.WhisperPath = whisperPath
				helpers.RetryEdit(bot, c.Message(), "⏳ Checking whisper model...", tele.ModeHTML)
				modelPath, err := helpers.EnsureWhisperModel()
				if err != nil {
					helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ Whisper model: %s", markdown.EscapeHTML(err.Error())), tele.ModeHTML)
					return c.Respond()
				}
				cfg.ModelPath = modelPath
			}
			cfg.VoiceEngine = engine
			if err := config.SaveAppConfig(cfg); err != nil {
				return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
			}
			text := buildVoiceText(cfg)
			menu := buildVoiceMenu(engine)
			if IsSettingsMenu(bs, c.Message().ID) {
				appendBackButton(menu)
			}
			c.Edit(text, menu)
			logger.Info(fmt.Sprintf("Voice engine changed to: %s", engine))
			return c.Respond(&tele.CallbackResponse{Text: "✅ Engine: " + engine})
		}
		if strings.HasPrefix(data, "lang:") {
			lang := strings.TrimPrefix(data, "lang:")
			if lang == "auto" {
				cfg.Language = ""
			} else {
				cfg.Language = lang
			}
			if err := config.SaveAppConfig(cfg); err != nil {
				return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
			}
			engine := cfg.VoiceEngine
			if engine == "" {
				engine = "whisper"
			}
			text := buildVoiceText(cfg)
			menu := buildVoiceMenu(engine)
			if IsSettingsMenu(bs, c.Message().ID) {
				appendBackButton(menu)
			}
			c.Edit(text, menu)
			logger.Info(fmt.Sprintf("Voice language changed to: %s", lang))
			return c.Respond(&tele.CallbackResponse{Text: "✅ Language: " + lang})
		}
		if strings.HasPrefix(data, "model:") {
			modelName := strings.TrimPrefix(data, "model:")
			var selected helpers.VoiceModelInfo
			found := false
			for _, m := range helpers.WhisperModels {
				if m.Name == modelName {
					selected = m
					found = true
					break
				}
			}
			if !found {
				return c.Respond(&tele.CallbackResponse{Text: "❌ Unknown model"})
			}
			helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("⏳ Checking whisper model %s...", markdown.EscapeHTML(selected.Name)), tele.ModeHTML)
			modelsDir := filepath.Join(config.GetConfigDir(), "models")
			home, _ := os.UserHomeDir()
			systemModelsDir := filepath.Join(home, ".local", "share", "whisper.cpp", "models")
			var modelPath string
			if helpers.FileExists(filepath.Join(modelsDir, selected.Filename)) {
				modelPath = filepath.Join(modelsDir, selected.Filename)
			} else if helpers.FileExists(filepath.Join(systemModelsDir, selected.Filename)) {
				modelPath = filepath.Join(systemModelsDir, selected.Filename)
			} else {
				modelPath = filepath.Join(systemModelsDir, selected.Filename)
				modelURL := fmt.Sprintf("https://huggingface.co/ggerganov/whisper.cpp/resolve/main/%s", selected.Filename)
				if err := os.MkdirAll(systemModelsDir, 0755); err != nil {
					helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to create dir: %s", markdown.EscapeHTML(err.Error())), tele.ModeHTML)
					return c.Respond()
				}
				if err := helpers.DownloadFile(modelPath, modelURL); err != nil {
					helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ Download failed: %s", markdown.EscapeHTML(err.Error())), tele.ModeHTML)
					return c.Respond()
				}
			}
			cfg.ModelPath = modelPath
			if err := config.SaveAppConfig(cfg); err != nil {
				return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
			}
			engine := cfg.VoiceEngine
			if engine == "" {
				engine = "whisper"
			}
			text := buildVoiceText(cfg)
			menu := buildVoiceMenu(engine)
			if IsSettingsMenu(bs, c.Message().ID) {
				appendBackButton(menu)
			}
			c.Edit(text, menu)
			logger.Info(fmt.Sprintf("Whisper model changed to: %s path=%s", selected.Name, modelPath))
			return c.Respond(&tele.CallbackResponse{Text: "✅ Model: " + selected.Name})
		}
		return c.Respond()
	})

	bot.Handle(&tele.Btn{Unique: "cron_delete"}, func(c tele.Context) error {
		jobID := c.Data()
		if bs.CronJobs.Remove(jobID) {
			c.Respond(&tele.CallbackResponse{Text: "✅ Job deleted"})
			logger.Info(fmt.Sprintf("Cron job deleted via TG: id=%s", jobID[:8]))
			jobs := bs.CronJobs.All()
			if len(jobs) == 0 {
				if IsSettingsMenu(bs, c.Message().ID) {
					menu := &tele.ReplyMarkup{}
					appendBackButton(menu)
					return c.Edit("⏰ <b>Cron Jobs</b>\n\nNo cron jobs configured.", menu, tele.ModeHTML)
				}
				return c.Edit("📋 No cron jobs configured.")
			}
			var text strings.Builder
			text.WriteString("📋 <b>Cron Jobs</b>\n\n")
			markup := &tele.ReplyMarkup{}
			var rows []tele.Row
			for _, j := range jobs {
				modeIcon := "🖥️"
				if j.Mode == "inject" {
					modeIcon = "💉"
				}
				onceTag := ""
				if j.Once {
					onceTag = " [once]"
				}
				agentInfo := ""
				if j.AgentName != "" {
					agentInfo = fmt.Sprintf(" → %s", j.AgentName)
				}
				lastRunStr := "never"
				if !j.LastRun.IsZero() {
					lastRunStr = helpers.RelativeTime(j.LastRun)
				}
				nameStr := ""
				if j.Name != "" {
					nameStr = fmt.Sprintf(" <b>%s</b>", j.Name)
				}
				text.WriteString(fmt.Sprintf("%s <code>%s</code>%s%s%s\n📅 %s | Last: %s\n📝 %s\n\n",
					modeIcon, j.ID[:8], nameStr, onceTag, agentInfo, j.Schedule, lastRunStr, j.Prompt))
				btnLabel := "🗑 " + j.ID[:8]
				if j.Name != "" {
					btnLabel = "🗑 " + j.Name
				}
				rows = append(rows, markup.Row(markup.Data(btnLabel, "cron_delete", j.ID)))
			}
			fullText := text.String()
			chunks := helpers.SplitBody(fullText, 3900)
			if len(chunks) <= 1 {
				markup.Inline(rows...)
				if IsSettingsMenu(bs, c.Message().ID) {
					appendBackButton(markup)
				}
				return c.Edit(fullText, markup, tele.ModeHTML)
			}
			kb := helpers.BuildPageKeyboardWithExtra(1, len(chunks), rows)
			if IsSettingsMenu(bs, c.Message().ID) {
				appendBackButton(kb)
			}
			sent, err := helpers.RetrySend(bot, c.Chat(), chunks[0]+fmt.Sprintf("\n\n📄 1/%d", len(chunks)), kb, tele.ModeHTML)
			if err != nil {
				return err
			}
			bs.Pages.Store(sent.ID, "", &stores.PageEntry{Chunks: chunks, PermRows: rows, ChatID: c.Chat().ID})
			return nil
		}
		c.Respond(&tele.CallbackResponse{Text: "❌ Job not found"})
		return nil
	})

	bot.Handle(&tele.InlineButton{Unique: "mailbox_bind"}, func(c tele.Context) error {
		chatID := c.Message().Chat.ID
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		creds.MailboxChatID = chatID
		if err := config.SaveCredentials(creds); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
		}
		logger.Info(fmt.Sprintf("Mailbox group bound: chat=%d", chatID))
		if IsSettingsMenu(bs, c.Message().ID) {
			menu := &tele.ReplyMarkup{}
			menu.Inline(menu.Row(menu.Data("✅ Bound (click to unbind)", "mailbox_unbind")))
			appendBackButton(menu)
			c.Bot().Edit(c.Message(), fmt.Sprintf("📬 <b>Mailbox Group</b>\nStatus: ✅ Bound as mailbox group\nChat ID: %d", chatID), menu, tele.ModeHTML)
			return c.Respond(&tele.CallbackResponse{Text: "✅ Bound"})
		}
		c.Respond(&tele.CallbackResponse{Text: "✅ Bound"})
		menu := &tele.ReplyMarkup{}
		menu.Inline(menu.Row(menu.Data("✅ Bound (click to unbind)", "mailbox_unbind")))
		return c.Edit(fmt.Sprintf("📬 Mailbox Group\nStatus: ✅ Bound as mailbox group\nChat ID: %d", chatID), menu)
	})

	bot.Handle(&tele.InlineButton{Unique: "mailbox_unbind"}, func(c tele.Context) error {
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to load config"})
		}
		creds.MailboxChatID = 0
		if err := config.SaveCredentials(creds); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save"})
		}
		logger.Info("Mailbox group unbound")
		if IsSettingsMenu(bs, c.Message().ID) {
			chatID := c.Message().Chat.ID
			menu := &tele.ReplyMarkup{}
			menu.Inline(menu.Row(menu.Data("📬 Bind as mailbox group", "mailbox_bind")))
			appendBackButton(menu)
			c.Bot().Edit(c.Message(), fmt.Sprintf("📬 <b>Mailbox Group</b>\nStatus: Not bound\nChat ID: %d", chatID), menu, tele.ModeHTML)
			return c.Respond(&tele.CallbackResponse{Text: "Unbound"})
		}
		c.Respond(&tele.CallbackResponse{Text: "Unbound"})
		chatID := c.Message().Chat.ID
		menu := &tele.ReplyMarkup{}
		menu.Inline(menu.Row(menu.Data("📬 Bind as mailbox group", "mailbox_bind")))
		return c.Edit(fmt.Sprintf("📬 Mailbox Group\nStatus: Not bound\nChat ID: %d", chatID), menu)
	})

	bot.Handle(&tele.InlineButton{Unique: "settings"}, func(c tele.Context) error {
		RenderSettingsSubmenu(bs, c.Message(), c.Data())
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "settings_perm"}, func(c tele.Context) error {
		action := c.Data()
		val, ok := bs.SettingsMenuMsgs.Load(c.Message().ID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Session expired"})
		}
		tmuxStr, _ := val.(string)
		target, err := injector.ParseTarget(tmuxStr)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Invalid target"})
		}
		if action == "status" {
			mode, _, err := helpers.DetectPermMode(target)
			if err != nil {
				return c.Respond(&tele.CallbackResponse{Text: fmt.Sprintf("❌ %v", err)})
			}
			return c.Respond(&tele.CallbackResponse{Text: fmt.Sprintf("Current: %s", mode)})
		}
		finalMode, err := helpers.SwitchPermMode(target, action)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: fmt.Sprintf("❌ %v", err)})
		}
		menu := buildPermSubMenu(finalMode)
		bs.SettingsMenuMsgs.Store(c.Message().ID, tmuxStr)
		appendBackButton(menu)
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("🔒 <b>Permission Mode</b>\n📟 %s\nCurrent: %s", notify.FormatPaneID(tmuxStr), finalMode), menu, tele.ModeHTML)
		return c.Respond(&tele.CallbackResponse{Text: "✅ " + finalMode})
	})

	bot.Handle(&tele.InlineButton{Unique: "settings_displayname"}, func(c tele.Context) error {
		if c.Data() == "set" {
			helpers.RetryEdit(bot, c.Message(), "👤 <b>Display Name</b>\n\nReply to this message with your desired display name:", tele.ModeHTML)
			bs.SettingsMenuMsgs.Store(c.Message().ID, "displayname")
			return c.Respond(&tele.CallbackResponse{Text: "Reply with name"})
		}
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "settings_bind"}, func(c tele.Context) error {
		sessions := bs.SessionState.All()
		if len(sessions) == 0 {
			return c.Respond(&tele.CallbackResponse{Text: "No active sessions"})
		}
		chatID := c.Message().Chat.ID
		topicID := c.Message().ThreadID
		sel := &tele.ReplyMarkup{}
		var rows []tele.Row
		var items []BindMenuItem
		idx := 1
		for sid, info := range sessions {
			key := sid
			label := fmt.Sprintf("session:%s", sid[:8])
			if info.Name != "" {
				key = info.Name
				label = info.Name
			}
			cwdStr := ""
			if info.CWD != "" {
				cwdStr = " " + notify.CompressPath(info.CWD)
			}
			items = append(items, BindMenuItem{Key: key, Label: label})
			rows = append(rows, sel.Row(sel.Data(fmt.Sprintf("🔗 %d: %s%s", idx, label, cwdStr), "bind_select", fmt.Sprintf("%d", idx))))
			idx++
		}
		sel.Inline(rows...)
		appendBackButton(sel)
		bs.BindMenuItems.Store(c.Message().ID, BindMenuContext{Items: items, ChatID: chatID, TopicID: topicID})
		helpers.RetryEdit(bot, c.Message(), "Select a session to bind to this group:", sel, tele.ModeHTML)
		return c.Respond()
	})

	bot.Handle(&tele.Btn{Unique: "upgrade"}, func(c tele.Context) error {
		tmuxTarget := c.Data()
		logger.Info(fmt.Sprintf("Upgrade callback: target=%s", tmuxTarget))
		if helpers.IsSessionRunning(tmuxTarget) {
			return c.Respond(&tele.CallbackResponse{Text: "⚠️ Session is busy. Wait for idle before upgrading."})
		}
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("🔄 Upgrading CC...\n📟 %s", notify.FormatPaneID(tmuxTarget)), tele.ModeHTML)
		go func() {
			if err := doUpgradeSession(bs, tmuxTarget); err != nil {
				logger.Error(fmt.Sprintf("doUpgradeSession failed: %v", err))
				helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("❌ Upgrade failed: %s\n📟 %s", err.Error(), notify.FormatPaneID(tmuxTarget)), tele.ModeHTML)
				return
			}
			helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("✅ CC restarted\n📟 %s", notify.FormatPaneID(tmuxTarget)), tele.ModeHTML)
		}()
		return c.Respond()
	})
}

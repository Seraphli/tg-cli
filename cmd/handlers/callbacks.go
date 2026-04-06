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
		var text string
		if entry.PermRows != nil {
			text = entry.Chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.Chunks))
		} else {
			text = notify.BuildNotificationText(notify.NotificationData{
				Event:      entry.Event,
				Project:    entry.Project,
				CWD:        entry.CWD,
				Body:       entry.Chunks[pageNum-1],
				TmuxTarget: entry.TmuxTarget,
				Page:       pageNum,
				TotalPages: len(entry.Chunks),
			})
		}
		kb := helpers.BuildPageKeyboardWithExtra(pageNum, len(entry.Chunks), entry.PermRows)
		if entry.RawMode {
			_, err = helpers.RetryEdit(bot, c.Message(), text, kb)
		} else {
			_, err = helpers.RetryEdit(bot, c.Message(), text, kb, tele.ModeHTML)
		}
		if err != nil {
			logger.Debug(fmt.Sprintf("edit page error: %v", err))
		}
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "perm"}, func(c tele.Context) error {
		decision := c.Data()
		if decision == "cancel" {
			doCancelPerm(bs, c.Message().ID)
			return c.Respond(&tele.CallbackResponse{Text: "❌ Cancelled"})
		}
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
			entry, ok := bs.ToolNotifs.Get(c.Message().ID)
			if !ok {
				return c.Respond(&tele.CallbackResponse{Text: "Expired"})
			}
			// Check session alive before processing tool response
			if entry.TmuxTarget != "" && !checkSessionAlive(bs, entry.TmuxTarget) {
				return c.Respond(&tele.CallbackResponse{Text: "⚠️ Session disconnected"})
			}
			if entry.Resolved {
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
				if err := doRespondAsk(bs, c.Message().ID, helpers.BuildAnswers(entry), ""); err != nil {
					return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
				}
				return c.Respond(&tele.CallbackResponse{Text: "✅ Submitted"})
			} else {
				split := strings.SplitN(parts[1], ":", 2)
				qIdx, _ := strconv.Atoi(split[0])
				optIdx, _ := strconv.Atoi(split[1])
				if qIdx >= len(entry.Questions) {
					return c.Respond(&tele.CallbackResponse{Text: "Invalid question"})
				}
				qm := &entry.Questions[qIdx]
				if qm.MultiSelect {
					qm.SelectedOptions[optIdx] = !qm.SelectedOptions[optIdx]
					logger.Info(fmt.Sprintf("AskUserQuestion multiSelect toggle: msg_id=%d q=%d opt=%d state=%v label=%s", c.Message().ID, qIdx, optIdx, qm.SelectedOptions[optIdx], qm.OptionLabels[optIdx]))
					newMarkup := helpers.RebuildAskMarkup(entry)
					helpers.RetryEdit(bot, c.Message(), c.Message().Text, newMarkup, tele.ModeHTML)
					return c.Respond(&tele.CallbackResponse{Text: "Toggled"})
				} else {
					qm.SelectedOption = optIdx
					hasSubmit := len(entry.Questions) > 1
					for _, q := range entry.Questions {
						if q.MultiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						if err := doRespondAsk(bs, c.Message().ID, helpers.BuildAnswers(entry), ""); err != nil {
							return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
						}
						return c.Respond(&tele.CallbackResponse{Text: "✅ Selected"})
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected: msg_id=%d q=%d opt=%d label=%s", c.Message().ID, qIdx, optIdx, qm.OptionLabels[optIdx]))
						newMarkup := helpers.RebuildAskMarkup(entry)
						helpers.RetryEdit(bot, c.Message(), c.Message().Text, newMarkup, tele.ModeHTML)
						return c.Respond(&tele.CallbackResponse{Text: "Selected"})
					}
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
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("✅ CWD source set to: %s", source), tele.ModeHTML)
		logger.Info(fmt.Sprintf("CWDSource updated to: %s", source))
		return c.Respond(&tele.CallbackResponse{Text: "✅ Saved: " + source})
	})

	// "usage_src" callback: update UsageSource setting and fetch usage
	bot.Handle(&tele.Btn{Unique: "usage_src"}, func(c tele.Context) error {
		source := c.Data()
		cfg, _ := config.LoadAppConfig()
		cfg.UsageSource = source
		config.SaveAppConfig(cfg)
		helpers.RetryEdit(bot, c.Message(), fmt.Sprintf("📊 Usage source → <b>%s</b>\n⏳ Fetching...", source), tele.ModeHTML)
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
		if err := injector.InjectText(*targetPtr, "/resume "+sessionID); err != nil {
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
		enabled := action == "on"
		cfg.ToolNotifyEnabled = &enabled
		if err := config.SaveAppConfig(cfg); err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "❌ Failed to save config"})
		}
		var statusText string
		if enabled {
			statusText = "✅ ON"
		} else {
			statusText = "❌ OFF"
		}
		menu := &tele.ReplyMarkup{}
		btnOn := menu.Data("✅ ON", "verbose", "on")
		btnOff := menu.Data("❌ OFF", "verbose", "off")
		menu.Inline(menu.Row(btnOn, btnOff))
		c.Edit(fmt.Sprintf("🔧 Tool Notifications: %s\n\nSelect to toggle:", statusText), menu)
		return c.Respond(&tele.CallbackResponse{Text: "Saved: " + statusText})
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
		c.Edit("🔧 Select tools for notifications:\n(Click to toggle)", menu)
		action := "All ON"
		if len(cfg.ToolNotifyList) == 0 {
			action = "All OFF"
		}
		return c.Respond(&tele.CallbackResponse{Text: action})
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
		logger.Info(fmt.Sprintf("Merge submitted: target=%s items=%d text=%s", buf.TmuxTarget, len(buf.Items), helpers.TruncateStr(merged, 200)))
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
				return c.Edit(fullText, markup, tele.ModeHTML)
			}
			kb := helpers.BuildPageKeyboardWithExtra(1, len(chunks), rows)
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
		c.Respond(&tele.CallbackResponse{Text: "Unbound"})
		chatID := c.Message().Chat.ID
		menu := &tele.ReplyMarkup{}
		menu.Inline(menu.Row(menu.Data("📬 Bind as mailbox group", "mailbox_bind")))
		return c.Edit(fmt.Sprintf("📬 Mailbox Group\nStatus: Not bound\nChat ID: %d", chatID), menu)
	})

	bot.Handle(&tele.Btn{Unique: "upgrade"}, func(c tele.Context) error {
		tmuxTarget := c.Data()
		logger.Info(fmt.Sprintf("Upgrade callback: target=%s", tmuxTarget))
		if isSessionRunning(bs, tmuxTarget) {
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

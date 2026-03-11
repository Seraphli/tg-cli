package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

func registerCallbackHandlers(bot *tele.Bot) {
	bot.Handle(&tele.InlineButton{Unique: "p"}, func(c tele.Context) error {
		pageNum, err := strconv.Atoi(c.Data())
		if err != nil {
			return c.Respond()
		}
		entry, ok := pages.get(c.Message().ID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Page expired"})
		}
		if pageNum < 1 || pageNum > len(entry.chunks) {
			return c.Respond()
		}
		var text string
		if entry.permRows != nil {
			text = entry.chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.chunks))
		} else {
			text = notify.BuildNotificationText(notify.NotificationData{
				Event:      entry.event,
				Project:    entry.project,
				CWD:        entry.cwd,
				Body:       entry.chunks[pageNum-1],
				TmuxTarget: entry.tmuxTarget,
				Page:       pageNum,
				TotalPages: len(entry.chunks),
			})
		}
		kb := buildPageKeyboardWithExtra(pageNum, len(entry.chunks), entry.permRows)
		_, err = retryEdit(bot, c.Message(), text, kb)
		if err != nil {
			logger.Debug(fmt.Sprintf("edit page error: %v", err))
		}
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "perm"}, func(c tele.Context) error {
		decision := c.Data()
		if decision == "cancel" {
			doCancelPerm(bot, c.Message().ID)
			return c.Respond(&tele.CallbackResponse{Text: "❌ Cancelled"})
		}
		d, err := doDecidePerm(bot, c.Message().ID, decision)
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
			entry, ok := toolNotifs.get(c.Message().ID)
			if !ok {
				return c.Respond(&tele.CallbackResponse{Text: "Expired"})
			}
			// Check session alive before processing tool response
			if entry.tmuxTarget != "" && !checkSessionAlive(entry.tmuxTarget, bot) {
				return c.Respond(&tele.CallbackResponse{Text: "⚠️ Session disconnected"})
			}
			if entry.resolved {
				return c.Respond(&tele.CallbackResponse{Text: "Already answered"})
			}
			if parts[1] == "cancel" {
				doCancelAsk(bot, c.Message().ID)
				return c.Respond(&tele.CallbackResponse{Text: "❌ Cancelled"})
			} else if parts[1] == "chat" {
				if err := doChatAsk(bot, c.Message().ID); err != nil {
					return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
				}
				return c.Respond(&tele.CallbackResponse{Text: "Chat mode"})
			} else if parts[1] == "submit" {
				if err := doRespondAsk(bot, c.Message().ID, buildAnswers(entry), ""); err != nil {
					return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
				}
				return c.Respond(&tele.CallbackResponse{Text: "✅ Submitted"})
			} else {
				split := strings.SplitN(parts[1], ":", 2)
				qIdx, _ := strconv.Atoi(split[0])
				optIdx, _ := strconv.Atoi(split[1])
				if qIdx >= len(entry.questions) {
					return c.Respond(&tele.CallbackResponse{Text: "Invalid question"})
				}
				qm := &entry.questions[qIdx]
				if qm.multiSelect {
					qm.selectedOptions[optIdx] = !qm.selectedOptions[optIdx]
					logger.Info(fmt.Sprintf("AskUserQuestion multiSelect toggle: msg_id=%d q=%d opt=%d state=%v label=%s", c.Message().ID, qIdx, optIdx, qm.selectedOptions[optIdx], qm.optionLabels[optIdx]))
					newMarkup := rebuildAskMarkup(entry)
					retryEdit(bot, c.Message(), c.Message().Text, newMarkup)
					return c.Respond(&tele.CallbackResponse{Text: "Toggled"})
				} else {
					qm.selectedOption = optIdx
					hasSubmit := len(entry.questions) > 1
					for _, q := range entry.questions {
						if q.multiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						if err := doRespondAsk(bot, c.Message().ID, buildAnswers(entry), ""); err != nil {
							return c.Respond(&tele.CallbackResponse{Text: "❌ " + err.Error()})
						}
						return c.Respond(&tele.CallbackResponse{Text: "✅ Selected"})
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected: msg_id=%d q=%d opt=%d label=%s", c.Message().ID, qIdx, optIdx, qm.optionLabels[optIdx]))
						newMarkup := rebuildAskMarkup(entry)
						retryEdit(bot, c.Message(), c.Message().Text, newMarkup)
						return c.Respond(&tele.CallbackResponse{Text: "Selected"})
					}
				}
			}
		}
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "bind"}, func(c tele.Context) error {
		val, ok := bindPending.Load(c.Message().ID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Expired"})
		}
		bp := val.(bindPendingInfo)
		bindType := c.Data() // "tmux" or "project"
		bindPending.Delete(c.Message().ID)

		creds, err := config.LoadCredentials()
		if err != nil {
			retryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to load config: %v", err))
			return c.Respond()
		}
		var resultMsg string
		if bindType == "tmux" {
			creds.RouteMap[bp.tmuxTarget] = bp.chatID
			resultMsg = fmt.Sprintf("✅ Bound tmux session to this chat.\n📟 %s", bp.tmuxTarget)
			logger.Info(fmt.Sprintf("Route bound (tmux): tmux=%s → chat=%d", bp.tmuxTarget, bp.chatID))
		} else {
			creds.ProjectRouteMap[bp.cwd] = bp.chatID
			resultMsg = fmt.Sprintf("✅ Bound project to this chat.\n📂 %s", notify.CompressPath(bp.cwd))
			logger.Info(fmt.Sprintf("Route bound (project): cwd=%s → chat=%d", bp.cwd, bp.chatID))
		}
		if err := config.SaveCredentials(creds); err != nil {
			retryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to save: %v", err))
			return c.Respond()
		}
		retryEdit(bot, c.Message(), resultMsg)
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "resume"}, func(c tele.Context) error {
		sessionID := c.Data()
		targetPtr, err := extractTmuxTarget(c.Message().Text)
		if err != nil || targetPtr == nil {
			return c.Respond(&tele.CallbackResponse{Text: "No tmux target found"})
		}
		if !checkSessionAlive(injector.FormatTarget(*targetPtr), bot) {
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
		if _, err := retryEdit(bot, c.Message(), c.Message().Text, markup); err != nil {
			logger.Debug(fmt.Sprintf("resume edit markup error: %v", err))
		}
		recordPending(injector.FormatTarget(*targetPtr), c.Message().Chat.ID, c.Message().ID)
		return c.Respond(&tele.CallbackResponse{Text: "✅ Resuming"})
	})

	bot.Handle(&tele.InlineButton{Unique: "unbind_select"}, func(c tele.Context) error {
		data := strings.TrimSpace(c.Data())
		num, err := strconv.Atoi(data)
		if err != nil {
			retryEdit(bot, c.Message(), "❌ Invalid selection.")
			return c.Respond()
		}
		val, ok := unbindMenuItems.LoadAndDelete(c.Message().ID)
		if !ok {
			retryEdit(bot, c.Message(), "❌ Menu expired. Send /bot_unbind again.")
			return c.Respond()
		}
		items := val.([]unbindItem)
		idx := num - 1
		if idx < 0 || idx >= len(items) {
			retryEdit(bot, c.Message(), "❌ Selection out of range.")
			return c.Respond()
		}
		item := items[idx]
		switch item.kind {
		case "tmux":
			creds, err := config.LoadCredentials()
			if err != nil {
				retryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to load config: %v", err))
				return c.Respond()
			}
			delete(creds.RouteMap, item.key)
			config.SaveCredentials(creds)
			logger.Info(fmt.Sprintf("Route unbound (menu/tmux): tmux=%s", item.key))
			retryEdit(bot, c.Message(), fmt.Sprintf("✅ Unbound tmux route.\n📟 %s", getPaneLabel(item.key)))
		case "project":
			creds, err := config.LoadCredentials()
			if err != nil {
				retryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to load config: %v", err))
				return c.Respond()
			}
			delete(creds.ProjectRouteMap, item.key)
			config.SaveCredentials(creds)
			logger.Info(fmt.Sprintf("Route unbound (menu/project): cwd=%s", item.key))
			retryEdit(bot, c.Message(), fmt.Sprintf("✅ Unbound project route.\n📂 %s", notify.CompressPath(item.key)))
		case "session":
			sessionState.remove(item.key)
			logger.Info(fmt.Sprintf("Route unbound (menu/session): sid=%s", item.key))
			retryEdit(bot, c.Message(), fmt.Sprintf("✅ Unbound session.\n🔑 %s", item.key[:8]))
		}
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
		if len(cfg.ToolNotifyList) == len(availTools) {
			cfg.ToolNotifyList = nil
		} else {
			cfg.ToolNotifyList = make([]string, len(availTools))
			copy(cfg.ToolNotifyList, availTools)
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

	bot.Handle(&tele.InlineButton{Unique: "bot_new"}, func(c tele.Context) error {
		data := c.Data()
		msgID := c.Message().ID
		val, ok := launchPending.Load(msgID)
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
			launchPending.Delete(msgID)
			if state.WorkDir == "" {
				askWorkDir(c.Bot(), state.ChatID, state)
			} else {
				go executeLaunch(c.Bot(), state.ChatID, state)
			}
		case data == "dir_select":
			state.WorkDir = state.BrowsePath
			c.Bot().Edit(c.Message(), fmt.Sprintf("📂 Working directory\n✅ %s", state.WorkDir))
			launchPending.Delete(msgID)
			go executeLaunch(c.Bot(), state.ChatID, state)
		case data == "cd_up":
			parent := filepath.Dir(state.BrowsePath)
			if parent != state.BrowsePath {
				state.BrowsePath = parent
				state.DirPage = 0
			}
			refreshDirBrowser(c.Bot(), c.Message(), state)
		case strings.HasPrefix(data, "cd:"):
			idx, err := strconv.Atoi(strings.TrimPrefix(data, "cd:"))
			if err == nil {
				dirs, _ := listSubDirs(state.BrowsePath, state.ShowHidden)
				if idx >= 0 && idx < len(dirs) {
					state.BrowsePath = filepath.Join(state.BrowsePath, dirs[idx])
					state.DirPage = 0
				}
			}
			refreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "toggle_hidden":
			state.ShowHidden = !state.ShowHidden
			state.DirPage = 0
			refreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "page_prev":
			if state.DirPage > 0 {
				state.DirPage--
			}
			refreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "page_next":
			state.DirPage++
			refreshDirBrowser(c.Bot(), c.Message(), state)
		case data == "page_noop":
			// no-op
		case data == "cancel":
			c.Bot().Edit(c.Message(), "❌ Launch cancelled.")
			launchPending.Delete(msgID)
			deleteLaunchState(state.UUID)
			logger.Info(fmt.Sprintf("bot_new: cancel pressed msg_id=%d uuid=%s", msgID, state.UUID))
		}
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "unbind_confirm"}, func(c tele.Context) error {
		action := c.Data() // "yes" or "no"
		val, ok := unbindPending.Load(c.Message().ID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Expired"})
		}
		up := val.(unbindPendingInfo)
		unbindPending.Delete(c.Message().ID)

		if action != "yes" {
			retryEdit(bot, c.Message(), "❌ Unbind cancelled.")
			return c.Respond()
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			retryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to load config: %v", err))
			return c.Respond()
		}
		delete(creds.ProjectRouteMap, up.cwd)
		if err := config.SaveCredentials(creds); err != nil {
			retryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to save: %v", err))
			return c.Respond()
		}
		logger.Info(fmt.Sprintf("Route unbound (project): cwd=%s", up.cwd))
		retryEdit(bot, c.Message(), fmt.Sprintf("✅ Unbound project route.\n📂 %s", notify.CompressPath(up.cwd)))
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
				retryEdit(bot, c.Message(), "⏳ Checking sensevoice dependencies...")
				sherpaPath, err := ensureSherpaOnnx()
				if err != nil {
					retryEdit(bot, c.Message(), fmt.Sprintf("❌ sherpa-onnx: %v", err))
					return c.Respond()
				}
				cfg.SherpaOnnxPath = sherpaPath
				modelPath, err := ensureSenseVoiceModel()
				if err != nil {
					retryEdit(bot, c.Message(), fmt.Sprintf("❌ SenseVoice model: %v", err))
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
					retryEdit(bot, c.Message(), "❌ whisper.cpp not found. Install: `yay -S whisper.cpp-cuda`")
					return c.Respond()
				}
				cfg.WhisperPath = whisperPath
				retryEdit(bot, c.Message(), "⏳ Checking whisper model...")
				modelPath, err := ensureWhisperModel()
				if err != nil {
					retryEdit(bot, c.Message(), fmt.Sprintf("❌ Whisper model: %v", err))
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
			var selected modelInfo
			found := false
			for _, m := range whisperModels {
				if m.name == modelName {
					selected = m
					found = true
					break
				}
			}
			if !found {
				return c.Respond(&tele.CallbackResponse{Text: "❌ Unknown model"})
			}
			retryEdit(bot, c.Message(), fmt.Sprintf("⏳ Checking whisper model %s...", selected.name))
			modelsDir := filepath.Join(config.GetConfigDir(), "models")
			home, _ := os.UserHomeDir()
			systemModelsDir := filepath.Join(home, ".local", "share", "whisper.cpp", "models")
			var modelPath string
			if fileExists(filepath.Join(modelsDir, selected.filename)) {
				modelPath = filepath.Join(modelsDir, selected.filename)
			} else if fileExists(filepath.Join(systemModelsDir, selected.filename)) {
				modelPath = filepath.Join(systemModelsDir, selected.filename)
			} else {
				modelPath = filepath.Join(systemModelsDir, selected.filename)
				modelURL := fmt.Sprintf("https://huggingface.co/ggerganov/whisper.cpp/resolve/main/%s", selected.filename)
				if err := os.MkdirAll(systemModelsDir, 0755); err != nil {
					retryEdit(bot, c.Message(), fmt.Sprintf("❌ Failed to create dir: %v", err))
					return c.Respond()
				}
				if err := downloadFile(modelPath, modelURL); err != nil {
					retryEdit(bot, c.Message(), fmt.Sprintf("❌ Download failed: %v", err))
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
			logger.Info(fmt.Sprintf("Whisper model changed to: %s path=%s", selected.name, modelPath))
			return c.Respond(&tele.CallbackResponse{Text: "✅ Model: " + selected.name})
		}
		return c.Respond()
	})
}

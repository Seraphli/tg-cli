package cmd

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

var bindPending sync.Map // msgID (int) -> bindPendingInfo

type bindPendingInfo struct {
	tmuxTarget string
	cwd        string
	chatID     int64
}

var unbindPending sync.Map // msgID (int) -> unbindPendingInfo
var unbindMenuItems sync.Map // msgID (int) -> []unbindItem

type unbindPendingInfo struct {
	cwd string
}

type unbindItem struct {
	kind string // "tmux", "project", "session"
	key  string
}

// registerTGHandlers registers all Telegram bot handlers
func registerTGHandlers(bot *tele.Bot, creds *config.Credentials) {
	// Log every incoming Telegram message and callback for debugging
	bot.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			if cb := c.Callback(); cb != nil {
				logger.Info(fmt.Sprintf("TG recv callback: chat=%d sender=%d msg_id=%d data=%s",
					c.Chat().ID, c.Sender().ID, c.Message().ID, cb.Data))
			} else if msg := c.Message(); msg != nil {
				msgType := "text"
				if msg.Voice != nil {
					msgType = "voice"
				}
				preview := truncateStr(c.Text(), 50)
				logger.Info(fmt.Sprintf("TG recv %s: chat=%d sender=%d msg_id=%d text=%s",
					msgType, c.Chat().ID, c.Sender().ID, msg.ID, preview))
			}
			return next(c)
		}
	})
	// Build TG→CC name mapping
	ccCommandMap := make(map[string]string)
	for tgName := range ccBuiltinCommands {
		ccName := tgName
		if tgName == "terminal_setup" {
			ccName = "terminal-setup"
		}
		ccCommandMap[tgName] = ccName
	}
	customCmds := scanCustomCommands()
	for tgName, cmd := range customCmds {
		ccCommandMap[tgName] = cmd.ccName
	}

	// Register CC command handlers
	for tgName, ccName := range ccCommandMap {
		tg, cc := tgName, ccName
		bot.Handle("/"+tg, func(c tele.Context) error {
			if c.Message().ReplyTo == nil {
				if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
					tmuxStr, target, err := resolveGroupTarget(c.Chat().ID)
					if err != nil {
						if err.Error() == "no targets bound" {
							return c.Send("💡 Please reply to a notification message to target a session.")
						}
						if err.Error() == "multiple sessions bound" {
							return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
						}
						return c.Reply("❌ tmux session not found.")
					}
					text := "/" + cc
					if payload := strings.TrimSpace(c.Message().Payload); payload != "" {
						text += " " + payload
					}
					// Check for pending AskUserQuestion — resolve with command text instead of injecting
					if msgID, entry, ok := toolNotifs.findByTmuxTarget(tmuxStr); ok {
						uuid, uuidOk := pendingFiles.get(msgID)
						if uuidOk && !handleStalePending(msgID, uuid, bot) {
							path := filepath.Join(pendingDir(), uuid+".json")
							pf, err := readPendingFile(path)
							if err == nil {
								answers := make(map[string]string)
								if len(entry.questions) > 0 {
									answers[entry.questions[0].questionText] = text
								}
								ccOutput := buildAskCCOutput(pf.Payload, answers)
								if werr := writePendingAnswer(uuid, ccOutput); werr != nil {
									logger.Error(fmt.Sprintf("Failed to write pending answer for CC command: %v", werr))
								} else {
									toolNotifs.markResolved(msgID)
									logger.Info(fmt.Sprintf("AskUserQuestion resolved via CC command (group): msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(text, 200)))
									editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
									retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"))
								}
							}
						}
						recordPending(tmuxStr, c.Message().Chat.ID, c.Message().ID)
						return nil
					}
					if err := injector.InjectText(target, text); err != nil {
						return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
					}
					logger.Info(fmt.Sprintf("Group quick reply (command): target=%s text=%s", tmuxStr, truncateStr(text, 200)))
					recordPending(tmuxStr, c.Message().Chat.ID, c.Message().ID)
					return nil
				}
				return c.Send("💡 Please reply to a notification message to target a session.")
			}
			target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
			if err != nil {
				if err.Error() == "no target found" {
					return c.Send("❌ No tmux session info found in the original message.")
				}
				return c.Send("❌ tmux session not found. The Claude Code session may have ended.")
			}
			text := "/" + cc
			if payload := strings.TrimSpace(c.Message().Payload); payload != "" {
				text += " " + payload
			}
			tmuxStr := injector.FormatTarget(target)
			// Check for pending AskUserQuestion — resolve with command text instead of injecting
			if msgID, entry, ok := toolNotifs.findByTmuxTarget(tmuxStr); ok {
				uuid, uuidOk := pendingFiles.get(msgID)
				if uuidOk && !handleStalePending(msgID, uuid, bot) {
					path := filepath.Join(pendingDir(), uuid+".json")
					pf, err := readPendingFile(path)
					if err == nil {
						answers := make(map[string]string)
						if len(entry.questions) > 0 {
							answers[entry.questions[0].questionText] = text
						}
						ccOutput := buildAskCCOutput(pf.Payload, answers)
						if werr := writePendingAnswer(uuid, ccOutput); werr != nil {
							logger.Error(fmt.Sprintf("Failed to write pending answer for CC command: %v", werr))
						} else {
							toolNotifs.markResolved(msgID)
							logger.Info(fmt.Sprintf("AskUserQuestion resolved via CC command (reply): msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(text, 200)))
							editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
							retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"))
						}
					}
				}
				recordPending(tmuxStr, c.Message().Chat.ID, c.Message().ID)
				return nil
			}
			if err := injector.InjectText(target, text); err != nil {
				return c.Send(fmt.Sprintf("❌ Injection failed: %v", err))
			}
			recordPending(tmuxStr, c.Message().Chat.ID, c.Message().ID)
			return nil
		})
	}

	bot.Handle("/resume", func(c tele.Context) error {
		payload := strings.TrimSpace(c.Message().Payload)
		// Resolve target: reply-to or group
		var target injector.TmuxTarget
		var tmuxStr string
		if c.Message().ReplyTo != nil {
			t, err := resolveReplyTarget(c.Message().ReplyTo.Text)
			if err != nil {
				if err.Error() == "no target found" {
					return c.Send("❌ No tmux session info found in the original message.")
				}
				return c.Send("❌ tmux session not found. The Claude Code session may have ended.")
			}
			target = t
			tmuxStr = injector.FormatTarget(t)
		} else if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
			ts, t, err := resolveGroupTarget(c.Chat().ID)
			if err != nil {
				if err.Error() == "no targets bound" {
					return c.Send("💡 Please reply to a notification message to target a session.")
				}
				if err.Error() == "multiple sessions bound" {
					return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
				}
				return c.Reply("❌ tmux session not found.")
			}
			target = t
			tmuxStr = ts
			logger.Debug(fmt.Sprintf("/resume: resolved tmuxStr=%s", tmuxStr))
		} else {
			return c.Send("💡 Please reply to a notification message to target a session.")
		}
		// With payload: inject /resume <payload> directly
		if payload != "" {
			if err := injector.InjectText(target, "/resume "+payload); err != nil {
				return c.Send(fmt.Sprintf("❌ Injection failed: %v", err))
			}
			recordPending(tmuxStr, c.Message().Chat.ID, c.Message().ID)
			return nil
		}
		// Without payload: show session picker
		var cwd string
		info := sessionState.findInfoByTarget(tmuxStr)
		logger.Debug(fmt.Sprintf("/resume: findInfoByTarget tmuxStr=%s found=%v", tmuxStr, info != nil))
		if info != nil && info.cwd != "" {
			cwd = info.cwd
		} else {
			// Fallback: get CWD directly from tmux pane
			out, err := exec.Command("tmux", "display-message", "-p", "-t", tmuxStr, "#{pane_current_path}").Output()
			if err == nil {
				cwd = strings.TrimSpace(string(out))
			}
			logger.Debug(fmt.Sprintf("/resume: tmux fallback cwd=%s", cwd))
		}
		if cwd == "" {
			return c.Send("❌ No working directory info available for this session.")
		}
		currentSID, _ := sessionState.findByTarget(tmuxStr)
		var sessions []sessionListEntry
		var err error
		if info != nil && info.projectDir != "" {
			sessions, err = listProjectSessionsByDir(info.projectDir, 8, currentSID)
		} else {
			sessions, err = listProjectSessions(cwd, 8, currentSID)
		}
		if err != nil || len(sessions) == 0 {
			return c.Send("📂 No previous sessions found for this project.")
		}
		if len(sessions) == 0 {
			return c.Send("📂 No other sessions found for this project.")
		}
		kb := buildResumeKeyboard(sessions)
		var lines []string
		lines = append(lines, "📟 "+notify.FormatPaneID(tmuxStr))
		lines = append(lines, "")
		for i, s := range sessions {
			prefix := "🤖"
			if s.SummarySource == "user" {
				prefix = "👤"
			}
			lines = append(lines, fmt.Sprintf("%d. %s %s — %s", i+1, prefix, truncateStr(s.Summary, 500), relativeTime(s.Modified)))
		}
		text := strings.Join(lines, "\n")
		_, err = retrySend(bot, c.Chat(), text, kb)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Failed to send: %v", err))
		}
		return nil
	})

	bot.Handle("/start", func(c tele.Context) error {
		return c.Send("tg-cli bot is running. Use /bot_pair to pair this chat.")
	})

	bot.Handle("/bot_pair", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if pairing.IsAllowed(userID) || pairing.IsAllowed(chatID) {
			return c.Send("Already paired.")
		}
		code := pairing.CreatePairingRequest(userID, chatID)
		return c.Send(fmt.Sprintf("Pairing code: %s\n\nEnter this code in the bot terminal to approve.\n\nCode expires in 10 minutes.", code))
	})

	bot.Handle("/status", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		return c.Send("Bot is running and paired.")
	})

	bot.Handle("/bot_routes", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("❌ Not paired. Use /bot_pair first.")
		}
		creds, _ := config.LoadCredentials()
		sessions := sessionState.all()
		if len(creds.RouteMap) == 0 && len(creds.ProjectRouteMap) == 0 && len(sessions) == 0 {
			return c.Send("No active route bindings.")
		}
		var sections []string
		// Section 1: Tmux routes
		if len(creds.RouteMap) > 0 {
			var lines []string
			lines = append(lines, "📟 Tmux routes:")
			for tmux, cid := range creds.RouteMap {
				chatName := fmt.Sprintf("%d", cid)
				if chat, err := bot.ChatByID(cid); err == nil && chat.Title != "" {
					chatName = chat.Title
				}
				lines = append(lines, fmt.Sprintf("  %s → %s", getPaneLabel(tmux), chatName))
			}
			sections = append(sections, strings.Join(lines, "\n"))
		}
		// Section 2: Project routes
		if len(creds.ProjectRouteMap) > 0 {
			var lines []string
			lines = append(lines, "📂 Project routes:")
			for cwd, cid := range creds.ProjectRouteMap {
				chatName := fmt.Sprintf("%d", cid)
				if chat, err := bot.ChatByID(cid); err == nil && chat.Title != "" {
					chatName = chat.Title
				}
				lines = append(lines, fmt.Sprintf("  %s → %s", notify.CompressPath(cwd), chatName))
			}
			sections = append(sections, strings.Join(lines, "\n"))
		}
		// Section 3: Active sessions
		var sessLines []string
		for _, info := range sessions {
			chatName := ""
			if cid, ok := creds.ProjectRouteMap[info.cwd]; ok {
				chatName = fmt.Sprintf("%d", cid)
				if chat, err := bot.ChatByID(cid); err == nil && chat.Title != "" {
					chatName = chat.Title
				}
			}
			label := notify.FormatPaneID(info.tmuxTarget)
			line := fmt.Sprintf("  %s → %s", label, notify.CompressPath(info.cwd))
			if chatName != "" {
				line += fmt.Sprintf(" → %s", chatName)
			}
			sessLines = append(sessLines, line)
		}
		if len(sessLines) > 0 {
			sections = append(sections, "📟 Active sessions:\n"+strings.Join(sessLines, "\n"))
		}
		return c.Send("🗺 Route bindings:\n" + strings.Join(sections, "\n\n"))
	})

	bot.Handle("/bot_bind", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			return c.Reply("❌ Reply to a notification message with /bot_bind to bind that session to this chat.")
		}
		target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info (📟) found in the replied message.")
		}
		tmuxStr := injector.FormatTarget(*target)
		if tmuxStr == "" {
			return c.Reply("❌ Empty tmux target, cannot bind.")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
		}
		info := sessionState.findInfoByTarget(target.PaneID)
		if info != nil && info.cwd != "" {
			// Show choice buttons
			sel := &tele.ReplyMarkup{}
			btnTmux := sel.Data("📟 Tmux", "bind", "tmux")
			btnProject := sel.Data("📂 Project", "bind", "project")
			sel.Inline(sel.Row(btnTmux, btnProject))
			sent, err := retrySend(bot, c.Chat(), fmt.Sprintf("Choose binding type:\n📟 %s\n📂 %s", tmuxStr, notify.CompressPath(info.cwd)), sel)
			if err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to send: %v", err))
			}
			bindPending.Store(sent.ID, bindPendingInfo{tmuxTarget: tmuxStr, cwd: info.cwd, chatID: c.Chat().ID})
			return nil
		}
		// No CWD available — bind tmux directly
		creds.RouteMap[tmuxStr] = c.Chat().ID
		if err := config.SaveCredentials(creds); err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to save binding: %v", err))
		}
		logger.Info(fmt.Sprintf("Route bound: tmux=%s → chat=%d by user=%s", tmuxStr, c.Chat().ID, userID))
		return c.Reply(fmt.Sprintf("✅ Bound tmux session to this chat.\n📟 %s", tmuxStr))
	})

	bot.Handle("/bot_unbind", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		if c.Message().ReplyTo == nil {
			creds, err := config.LoadCredentials()
			if err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
			}
			sessions := sessionState.all()
			if len(creds.RouteMap) == 0 && len(creds.ProjectRouteMap) == 0 && len(sessions) == 0 {
				return c.Reply("No active route bindings to unbind.")
			}
			// Build grouped list: RouteMap -> ProjectRouteMap -> sessions
			sel := &tele.ReplyMarkup{}
			var rows []tele.Row
			var lines []string
			var items []unbindItem
			idx := 1
			// Tmux routes group
			var tmuxBtns []tele.Btn
			for tmux := range creds.RouteMap {
				label := getPaneLabel(tmux)
				lines = append(lines, fmt.Sprintf("  %d. %s", idx, label))
				items = append(items, unbindItem{"tmux", tmux})
				tmuxBtns = append(tmuxBtns, sel.Data(fmt.Sprintf("📟 %d", idx), "unbind_select", fmt.Sprintf("%d", idx)))
				idx++
			}
			if len(tmuxBtns) > 0 {
				lines = append([]string{"📟 Tmux routes:"}, lines...)
				for i := 0; i < len(tmuxBtns); i += 2 {
					if i+1 < len(tmuxBtns) {
						rows = append(rows, sel.Row(tmuxBtns[i], tmuxBtns[i+1]))
					} else {
						rows = append(rows, sel.Row(tmuxBtns[i]))
					}
				}
			}
			// Project routes group
			var projLines []string
			var projBtns []tele.Btn
			for cwd := range creds.ProjectRouteMap {
				projLines = append(projLines, fmt.Sprintf("  %d. %s", idx, notify.CompressPath(cwd)))
				items = append(items, unbindItem{"project", cwd})
				projBtns = append(projBtns, sel.Data(fmt.Sprintf("📂 %d", idx), "unbind_select", fmt.Sprintf("%d", idx)))
				idx++
			}
			if len(projBtns) > 0 {
				lines = append(lines, "", "📂 Project routes:")
				lines = append(lines, projLines...)
				for i := 0; i < len(projBtns); i += 2 {
					if i+1 < len(projBtns) {
						rows = append(rows, sel.Row(projBtns[i], projBtns[i+1]))
					} else {
						rows = append(rows, sel.Row(projBtns[i]))
					}
				}
			}
			// Active sessions group
			var sesLines []string
			var sesBtns []tele.Btn
			for sid, info := range sessions {
				label := notify.FormatPaneID(info.tmuxTarget)
				sesLines = append(sesLines, fmt.Sprintf("  %d. %s → %s", idx, label, notify.CompressPath(info.cwd)))
				items = append(items, unbindItem{"session", sid})
				sesBtns = append(sesBtns, sel.Data(fmt.Sprintf("📟 %d", idx), "unbind_select", fmt.Sprintf("%d", idx)))
				idx++
			}
			if len(sesBtns) > 0 {
				lines = append(lines, "", "📟 Active sessions:")
				lines = append(lines, sesLines...)
				for i := 0; i < len(sesBtns); i += 2 {
					if i+1 < len(sesBtns) {
						rows = append(rows, sel.Row(sesBtns[i], sesBtns[i+1]))
					} else {
						rows = append(rows, sel.Row(sesBtns[i]))
					}
				}
			}
			sel.Inline(rows...)
			sent, err := bot.Reply(c.Message(), "Select a route to unbind:\n"+strings.Join(lines, "\n"), sel)
			if err == nil {
				unbindMenuItems.Store(sent.ID, items)
			}
			return err
		}
		target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info (📟) found in the replied message.")
		}
		tmuxStr := injector.FormatTarget(*target)
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
		}
		// Check tmux route first — direct unbind
		if _, ok := creds.RouteMap[tmuxStr]; ok {
			delete(creds.RouteMap, tmuxStr)
			if err := config.SaveCredentials(creds); err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to save: %v", err))
			}
			logger.Info(fmt.Sprintf("Route unbound (tmux): tmux=%s by user=%s", tmuxStr, userID))
			return c.Reply(fmt.Sprintf("✅ Unbound tmux session.\n📟 %s", tmuxStr))
		}
		// Check project route — needs confirmation
		if info := sessionState.findInfoByTarget(target.PaneID); info != nil && info.cwd != "" {
			if _, ok := creds.ProjectRouteMap[info.cwd]; ok {
				sel := &tele.ReplyMarkup{}
				btnYes := sel.Data("✅ Yes, unbind", "unbind_confirm", "yes")
				btnNo := sel.Data("❌ Cancel", "unbind_confirm", "no")
				sel.Inline(sel.Row(btnYes, btnNo))
				sent, err := retrySend(bot, c.Chat(), fmt.Sprintf("Unbind project route?\n📂 %s\n⚠️ This affects all sessions in this project.", notify.CompressPath(info.cwd)), sel)
				if err != nil {
					return c.Reply(fmt.Sprintf("❌ Failed to send: %v", err))
				}
				unbindPending.Store(sent.ID, unbindPendingInfo{cwd: info.cwd})
				return nil
			}
		}
		return c.Reply("❌ No binding found for this session.")
	})
	bot.Handle("/bot_verbose", func(c tele.Context) error {
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Reply("❌ Failed to load config")
		}
		enabled := cfg.ToolNotifyEnabled == nil || *cfg.ToolNotifyEnabled
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
		return c.Reply(fmt.Sprintf("🔧 Tool Notifications: %s\n\nSelect to toggle:", statusText), menu)
	})

	bot.Handle("/bot_tools", func(c tele.Context) error {
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Reply("❌ Failed to load config")
		}
		menu := buildToolsMenu(cfg.ToolNotifyList)
		return c.Reply("🔧 Select tools for notifications:\n(Click to toggle)", menu)
	})

	bot.Handle("/bot_new", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		args := strings.TrimSpace(c.Message().Payload)
		sessionName, workDir, command := parseBotNewArgs(args)
		return startLaunchFlow(c, sessionName, workDir, command)
	})

	bot.Handle("/bot_usage", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		return handleUsageCommand(c, bot)
	})

	bot.Handle("/bot_voice", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		return handleVoiceCommand(c)
	})

	registerMessageHandlers(bot)
	registerCallbackHandlers(bot)
}

var availTools = []string{"Edit", "Write", "Bash", "Read", "Glob", "Grep", "Agent", "WebFetch", "WebSearch"}

// buildToolsMenu builds an inline keyboard for tool notification selection.
func buildToolsMenu(selected []string) *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	selectedSet := make(map[string]bool)
	for _, t := range selected {
		selectedSet[t] = true
	}
	var rows []tele.Row
	var pending []tele.Btn
	for _, tool := range availTools {
		label := "⬜ " + tool
		if selectedSet[tool] {
			label = "✅ " + tool
		}
		pending = append(pending, menu.Data(label, "tools_toggle", tool))
		if len(pending) == 2 {
			rows = append(rows, menu.Row(pending...))
			pending = nil
		}
	}
	if len(pending) > 0 {
		rows = append(rows, menu.Row(pending...))
	}
	// Add All toggle button on the last row
	allLabel := "☑️ All"
	if len(selected) == len(availTools) {
		allLabel = "✅ All"
	}
	rows = append(rows, menu.Row(menu.Data(allLabel, "tools_toggle_all")))
	menu.Inline(rows...)
	return menu
}

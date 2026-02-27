package cmd

import (
	"fmt"
	"os/exec"
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

type unbindPendingInfo struct {
	cwd string
}

// registerTGHandlers registers all Telegram bot handlers
func registerTGHandlers(bot *tele.Bot, creds *config.Credentials) {
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
					if err := injector.InjectText(target, text); err != nil {
						return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
					}
					logger.Info(fmt.Sprintf("Group quick reply (command): target=%s text=%s", tmuxStr, truncateStr(text, 200)))
					reactAndTrack(bot, c.Message().Chat, c.Message(), tmuxStr)
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
			if err := injector.InjectText(target, text); err != nil {
				return c.Send(fmt.Sprintf("❌ Injection failed: %v", err))
			}
			tmuxStr := injector.FormatTarget(target)
			reactAndTrack(bot, c.Message().Chat, c.Message(), tmuxStr)
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
			reactAndTrack(bot, c.Message().Chat, c.Message(), tmuxStr)
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
		sessions, err := listProjectSessions(cwd, 8, currentSID)
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
		_, err = bot.Send(c.Chat(), text, kb)
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
		if !pairing.IsAllowed(userID) {
			return c.Send("❌ Not paired. Use /bot_pair first.")
		}
		creds, _ := config.LoadCredentials()
		if len(creds.RouteMap) == 0 && len(creds.ProjectRouteMap) == 0 {
			return c.Send("No active route bindings.")
		}
		var lines []string
		for tmux, chatID := range creds.RouteMap {
			chatName := fmt.Sprintf("%d", chatID)
			if chat, err := bot.ChatByID(chatID); err == nil && chat.Title != "" {
				chatName = chat.Title
			}
			paneID := tmux
			if idx := strings.Index(paneID, "@"); idx != -1 {
				paneID = paneID[:idx]
			}
			lines = append(lines, fmt.Sprintf("📟 %s → %s", paneID, chatName))
		}
		for cwd, chatID := range creds.ProjectRouteMap {
			chatName := fmt.Sprintf("%d", chatID)
			if chat, err := bot.ChatByID(chatID); err == nil && chat.Title != "" {
				chatName = chat.Title
			}
			lines = append(lines, fmt.Sprintf("📂 %s → %s", notify.CompressPath(cwd), chatName))
		}
		return c.Send("🗺 Route bindings:\n" + strings.Join(lines, "\n"))
	})

	bot.Handle("/bot_bind", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		if !pairing.IsAllowed(userID) {
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
			sent, err := bot.Send(c.Chat(), fmt.Sprintf("Choose binding type:\n📟 %s\n📂 %s", tmuxStr, notify.CompressPath(info.cwd)), sel)
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
		if !pairing.IsAllowed(userID) {
			return c.Reply("❌ Not paired.")
		}
		if c.Message().ReplyTo == nil {
			return c.Reply("❌ Reply to a notification message with /bot_unbind to unbind that session.")
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
				sent, err := bot.Send(c.Chat(), fmt.Sprintf("Unbind project route?\n📂 %s\n⚠️ This affects all sessions in this project.", notify.CompressPath(info.cwd)), sel)
				if err != nil {
					return c.Reply(fmt.Sprintf("❌ Failed to send: %v", err))
				}
				unbindPending.Store(sent.ID, unbindPendingInfo{cwd: info.cwd})
				return nil
			}
		}
		return c.Reply("❌ No binding found for this session.")
	})
	registerMessageHandlers(bot)
	registerCallbackHandlers(bot)
}

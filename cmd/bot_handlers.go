package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

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
		if len(creds.RouteMap) == 0 {
			return c.Send("No active route bindings.")
		}
		var lines []string
		for tmux, chatID := range creds.RouteMap {
			chatName := fmt.Sprintf("%d", chatID)
			if chat, err := bot.ChatByID(chatID); err == nil && chat.Title != "" {
				chatName = chat.Title
			}
			lines = append(lines, fmt.Sprintf("📟 %s → %s", tmux, chatName))
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
		creds.RouteMap[tmuxStr] = c.Chat().ID
		if err := config.SaveCredentials(creds); err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to save binding: %v", err))
		}
		logger.Info(fmt.Sprintf("Route bound: tmux=%s → chat=%d by user=%s", tmuxStr, c.Chat().ID, userID))
		return c.Reply(fmt.Sprintf("✅ Bound session to this chat.\n📟 %s", tmuxStr))
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
		if _, ok := creds.RouteMap[tmuxStr]; !ok {
			return c.Reply("❌ This session is not bound to any chat.")
		}
		delete(creds.RouteMap, tmuxStr)
		if err := config.SaveCredentials(creds); err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to save: %v", err))
		}
		logger.Info(fmt.Sprintf("Route unbound: tmux=%s by user=%s", tmuxStr, userID))
		return c.Reply(fmt.Sprintf("✅ Unbound session. Messages will go to default chat.\n📟 %s", tmuxStr))
	})
	registerMessageHandlers(bot)
	registerCallbackHandlers(bot)
}

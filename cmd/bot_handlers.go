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
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

// unbindMenuItems maps msgID to list of name-route keys for /bot_unbind inline menu
var unbindMenuItems sync.Map // msgID (int) -> []string (name keys)

// bindMenuItems maps msgID to bind menu context for /bot_bind inline menu
var bindMenuItems sync.Map // msgID (int) -> bindMenuContext

type bindMenuItem struct {
	key   string // name or sessionID
	label string
}

type bindMenuContext struct {
	items   []bindMenuItem
	chatID  int64
	topicID int
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
				replyInfo := ""
				if msg.ReplyTo != nil {
					replyInfo = fmt.Sprintf(" reply_to=%d", msg.ReplyTo.ID)
				}
				threadInfo := ""
				if msg.ThreadID != 0 {
					threadInfo = fmt.Sprintf(" thread_id=%d", msg.ThreadID)
				}
				logger.Info(fmt.Sprintf("TG recv %s: chat=%d sender=%d msg_id=%d%s%s text=%s",
					msgType, c.Chat().ID, c.Sender().ID, msg.ID, replyInfo, threadInfo, preview))
			}
			return next(c)
		}
	})
	bot.Handle(tele.OnMigration, func(c tele.Context) error {
		from, to := c.Migration()
		logger.Info(fmt.Sprintf("Chat migration detected: %d → %d", from, to))
		if err := config.MigrateChat(from, to); err != nil {
			logger.Error(fmt.Sprintf("Failed to migrate chat: %v", err))
			return nil
		}
		logger.Info(fmt.Sprintf("Chat migration completed: %d → %d", from, to))
		retrySend(bot, &tele.Chat{ID: to}, fmt.Sprintf("✅ Chat migrated: %d → %d\nAll route bindings updated.", from, to), tele.ModeHTML)
		return nil
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
			isTopicAnchor := c.Message().ReplyTo != nil && c.Message().ReplyTo.ID == c.Message().ThreadID
			if c.Message().ReplyTo == nil || isTopicAnchor {
				if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
					tmuxStr, target, err := resolveGroupTarget(c.Chat().ID, c.Message().ThreadID)
					if err != nil {
						if err.Error() == "no targets bound" {
							return c.Reply("💡 Please reply to a notification message to target a session.")
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
									retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"), tele.ModeHTML)
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
				return c.Reply("💡 Please reply to a notification message to target a session.")
			}
			target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
			if err != nil {
				if err.Error() == "no target found" {
					return c.Reply("❌ No tmux session info found in the original message.")
				}
				return c.Reply("❌ tmux session not found. The Claude Code session may have ended.")
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
							retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"), tele.ModeHTML)
						}
					}
				}
				recordPending(tmuxStr, c.Message().Chat.ID, c.Message().ID)
				return nil
			}
			if err := injector.InjectText(target, text); err != nil {
				return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
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
		isTopicAnchor2 := c.Message().ReplyTo != nil && c.Message().ReplyTo.ID == c.Message().ThreadID
		if c.Message().ReplyTo != nil && !isTopicAnchor2 {
			t, err := resolveReplyTarget(c.Message().ReplyTo.Text)
			if err != nil {
				if err.Error() == "no target found" {
					return c.Reply("❌ No tmux session info found in the original message.")
				}
				return c.Reply("❌ tmux session not found. The Claude Code session may have ended.")
			}
			target = t
			tmuxStr = injector.FormatTarget(t)
		} else if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
			ts, t, err := resolveGroupTarget(c.Chat().ID, c.Message().ThreadID)
			if err != nil {
				if err.Error() == "no targets bound" {
					return c.Reply("💡 Please reply to a notification message to target a session.")
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
			return c.Reply("💡 Please reply to a notification message to target a session.")
		}
		// With payload: inject /resume <payload> directly
		if payload != "" {
			if err := injector.InjectText(target, "/resume "+payload); err != nil {
				return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
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
			return c.Reply("❌ No working directory info available for this session.")
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
			return c.Reply("📂 No previous sessions found for this project.")
		}
		if len(sessions) == 0 {
			return c.Reply("📂 No other sessions found for this project.")
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
			lines = append(lines, fmt.Sprintf("%d. %s %s — %s", i+1, prefix, markdown.EscapeHTML(truncateStr(s.Summary, 500)), relativeTime(s.Modified)))
		}
		text := strings.Join(lines, "\n")
		_, err = retrySend(bot, c.Chat(), text, kb, tele.ModeHTML)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to send: %v", err))
		}
		return nil
	})

	bot.Handle("/start", func(c tele.Context) error {
		return c.Reply("tg-cli bot is running. Use /bot_pair to pair this chat.")
	})

	bot.Handle("/bot_pair", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if pairing.IsAllowed(userID) || pairing.IsAllowed(chatID) {
			return c.Reply("Already paired.")
		}
		code := pairing.CreatePairingRequest(userID, chatID)
		return c.Reply(fmt.Sprintf("Pairing code: %s\n\nEnter this code in the bot terminal to approve.\n\nCode expires in 10 minutes.", code))
	})

	bot.Handle("/status", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("Not paired. Use /bot_pair first.")
		}
		return c.Reply("Bot is running and paired.")
	})

	bot.Handle("/bot_routes", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired. Use /bot_pair first.")
		}
		creds, _ := config.LoadCredentials()
		sessions := sessionState.all()
		var sections []string
		// Section 1: Agent routes (NameRouteMap)
		if len(creds.NameRouteMap) > 0 {
			var lines []string
			lines = append(lines, "📋 Agent routes:")
			for name, route := range creds.NameRouteMap {
				chatName := fmt.Sprintf("%d", route.ChatID)
				topicStr := ""
				if ch, err := bot.ChatByID(route.ChatID); err == nil {
					if ch.Title != "" {
						chatName = fmt.Sprintf("%s (%d)", ch.Title, route.ChatID)
					}
					if ch.Type == tele.ChatSuperGroup {
						if route.TopicID == 0 {
							topicStr = ", topic=General"
						} else {
							topicStr = fmt.Sprintf(", topic=#%d", route.TopicID)
						}
					}
				}
				paneID := ""
				if info := sessionState.findByName(name); info != nil {
					paneID = " (" + notify.FormatPaneID(info.tmuxTarget) + ")"
				}
				lines = append(lines, fmt.Sprintf("  %s%s → %s%s", name, paneID, chatName, topicStr))
			}
			sections = append(sections, strings.Join(lines, "\n"))
		}
		// Section 2: Unnamed sessions
		var unnamedLines []string
		for _, info := range sessions {
			if info.name != "" {
				continue
			}
			label := notify.FormatPaneID(info.tmuxTarget)
			unnamedLines = append(unnamedLines, fmt.Sprintf("  %s → %s", label, notify.CompressPath(info.cwd)))
		}
		if len(unnamedLines) > 0 {
			sections = append(sections, "📟 Unnamed sessions:\n"+strings.Join(unnamedLines, "\n"))
		}
		if len(sections) == 0 {
			return c.Reply("No active route bindings or sessions.")
		}
		return c.Reply("🗺 Route bindings:\n" + strings.Join(sections, "\n\n"))
	})

	bot.Handle("/bot_bind", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatIDStr := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatIDStr) {
			return c.Reply("❌ Not paired. Use /bot_pair first.")
		}
		chatID := c.Chat().ID
		topicID := c.Message().ThreadID

		// Mode 1: /bot_bind <name> — bind by name
		name := strings.TrimSpace(c.Message().Payload)
		if name != "" {
			creds, err := config.LoadCredentials()
			if err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
			}
			creds.NameRouteMap[name] = config.NameRoute{ChatID: chatID, TopicID: topicID}
			if err := config.SaveCredentials(creds); err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to save binding: %v", err))
			}
			topicStr := ""
			if topicID != 0 {
				topicStr = fmt.Sprintf(", topic=%d", topicID)
			}
			logger.Info(fmt.Sprintf("Route bound: name=%s → chat=%d topic=%d by user=%s", name, chatID, topicID, userID))
			return c.Reply(fmt.Sprintf("✅ Bound to this chat.\n🏷 %s → %d%s", name, chatID, topicStr))
		}

		// Mode 2: reply to notification — bind session from replied message
		isTopicAnchorBind := c.Message().ReplyTo != nil && c.Message().ReplyTo.ID == c.Message().ThreadID
		if c.Message().ReplyTo != nil && !isTopicAnchorBind {
			target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info (📟) found in the replied message.")
			}
			tmuxStr := injector.FormatTarget(*target)
			sid, found := sessionState.findByTarget(tmuxStr)
			if !found {
				return c.Reply("❌ Session not found for that pane.")
			}
			info := sessionState.findInfoByTarget(tmuxStr)
			// Use name if available, otherwise session ID
			key := sid
			keyLabel := fmt.Sprintf("session:%s", sid[:8])
			if info != nil && info.name != "" {
				key = info.name
				keyLabel = info.name
			}
			creds, err := config.LoadCredentials()
			if err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
			}
			creds.NameRouteMap[key] = config.NameRoute{ChatID: chatID, TopicID: topicID}
			if err := config.SaveCredentials(creds); err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to save binding: %v", err))
			}
			topicStr := ""
			if topicID != 0 {
				topicStr = fmt.Sprintf(", topic=%d", topicID)
			}
			logger.Info(fmt.Sprintf("Route bound: key=%s → chat=%d topic=%d by user=%s", key, chatID, topicID, userID))
			return c.Reply(fmt.Sprintf("✅ Bound to this chat.\n🏷 %s → %d%s", keyLabel, chatID, topicStr))
		}

		// Mode 3: no args, no reply — show session list buttons
		sessions := sessionState.all()
		if len(sessions) == 0 {
			return c.Reply("No active sessions to bind.")
		}
		sel := &tele.ReplyMarkup{}
		var rows []tele.Row
		var lines []string
		var items []bindMenuItem
		idx := 1
		for sid, info := range sessions {
			key := sid
			label := fmt.Sprintf("session:%s", sid[:8])
			if info.name != "" {
				key = info.name
				label = info.name
			}
			cwdStr := ""
			if info.cwd != "" {
				cwdStr = " " + notify.CompressPath(info.cwd)
			}
			lines = append(lines, fmt.Sprintf("  %d. %s%s", idx, label, cwdStr))
			items = append(items, bindMenuItem{key: key, label: label})
			rows = append(rows, sel.Row(sel.Data(fmt.Sprintf("🔗 %d: %s", idx, label), "bind_select", fmt.Sprintf("%d", idx))))
			idx++
		}
		sel.Inline(rows...)
		sent, err := bot.Reply(c.Message(), fmt.Sprintf("Select a session to bind to this chat:\n%s", strings.Join(lines, "\n")), sel)
		if err == nil {
			bindMenuItems.Store(sent.ID, bindMenuContext{items: items, chatID: chatID, topicID: topicID})
		}
		return err
	})

	bot.Handle("/bot_unbind", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatIDStr := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatIDStr) {
			return c.Reply("❌ Not paired.")
		}
		// Mode 1: reply to notification — unbind session from replied message
		isTopicAnchorUnbind := c.Message().ReplyTo != nil && c.Message().ReplyTo.ID == c.Message().ThreadID
		if c.Message().ReplyTo != nil && !isTopicAnchorUnbind {
			target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info (📟) found in the replied message.")
			}
			tmuxStr := injector.FormatTarget(*target)
			sid, found := sessionState.findByTarget(tmuxStr)
			if !found {
				return c.Reply("❌ Session not found for that pane.")
			}
			info := sessionState.findInfoByTarget(tmuxStr)
			creds, err := config.LoadCredentials()
			if err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
			}
			// Try unbind by name first, then by session ID
			var unboundKey string
			if info != nil && info.name != "" {
				if _, ok := creds.NameRouteMap[info.name]; ok {
					delete(creds.NameRouteMap, info.name)
					unboundKey = info.name
				}
			}
			if unboundKey == "" {
				if _, ok := creds.NameRouteMap[sid]; ok {
					delete(creds.NameRouteMap, sid)
					unboundKey = fmt.Sprintf("session:%s", sid[:8])
				}
			}
			if unboundKey == "" {
				return c.Reply("❌ No route binding found for this session.")
			}
			if err := config.SaveCredentials(creds); err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to save: %v", err))
			}
			logger.Info(fmt.Sprintf("Route unbound (reply): key=%s by user=%s", unboundKey, userID))
			return c.Reply(fmt.Sprintf("✅ Unbound: %s", unboundKey))
		}
		// With arg: direct delete
		name := strings.TrimSpace(c.Message().Payload)
		if name != "" {
			creds, err := config.LoadCredentials()
			if err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
			}
			if _, ok := creds.NameRouteMap[name]; !ok {
				return c.Reply(fmt.Sprintf("❌ No route binding found for name: %s", name))
			}
			delete(creds.NameRouteMap, name)
			if err := config.SaveCredentials(creds); err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to save: %v", err))
			}
			logger.Info(fmt.Sprintf("Route unbound (name): name=%s by user=%s", name, userID))
			return c.Reply(fmt.Sprintf("✅ Unbound agent name: %s", name))
		}
		// No arg: show inline buttons listing NameRouteMap entries
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
		}
		if len(creds.NameRouteMap) == 0 {
			return c.Reply("No agent name route bindings to unbind.")
		}
		sel := &tele.ReplyMarkup{}
		var rows []tele.Row
		var lines []string
		var keys []string
		idx := 1
		for n, route := range creds.NameRouteMap {
			topicStr := ""
			if route.TopicID != 0 {
				topicStr = fmt.Sprintf(" topic=%d", route.TopicID)
			}
			lines = append(lines, fmt.Sprintf("  %d. %s → %d%s", idx, n, route.ChatID, topicStr))
			keys = append(keys, n)
			rows = append(rows, sel.Row(sel.Data(fmt.Sprintf("🗑 %d: %s", idx, n), "unbind_select", fmt.Sprintf("%d", idx))))
			idx++
		}
		sel.Inline(rows...)
		sent, err := bot.Reply(c.Message(), "Select a route to unbind:\n"+strings.Join(lines, "\n"), sel)
		if err == nil {
			unbindMenuItems.Store(sent.ID, keys)
		}
		return err
	})
	bot.Handle("/bot_name", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatIDStr := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatIDStr) {
			return c.Reply("❌ Not paired.")
		}
		name := strings.TrimSpace(c.Message().Payload)
		if name == "" {
			return c.Reply("❌ Usage: /bot_name <name>")
		}
		// Resolve session: from replied message or single active session
		var sessionID string
		isTopicAnchorName := c.Message().ReplyTo != nil && c.Message().ReplyTo.ID == c.Message().ThreadID
		if c.Message().ReplyTo != nil && !isTopicAnchorName {
			target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info (📟) found in the replied message.")
			}
			sid, found := sessionState.findByTarget(injector.FormatTarget(*target))
			if !found {
				return c.Reply("❌ Session not found for that pane.")
			}
			sessionID = sid
		} else {
			sessions := sessionState.all()
			if len(sessions) == 0 {
				return c.Reply("❌ No active sessions found.")
			}
			if len(sessions) > 1 {
				return c.Reply("❌ Multiple active sessions. Reply to a notification to target a specific session.")
			}
			for sid := range sessions {
				sessionID = sid
			}
		}
		ok, errMsg := sessionState.setName(sessionID, name)
		if !ok {
			return c.Reply(fmt.Sprintf("❌ Failed to set name: %s", errMsg))
		}
		logger.Info(fmt.Sprintf("Session name set: session=%s name=%s by user=%s", sessionID, name, userID))
		return c.Reply(fmt.Sprintf("✅ Session named: %s", name))
	})

	bot.Handle("/bot_names", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatIDStr := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatIDStr) {
			return c.Reply("❌ Not paired.")
		}
		sessions := sessionState.all()
		if len(sessions) == 0 {
			return c.Reply("No active sessions.")
		}
		sel := &tele.ReplyMarkup{}
		var rows []tele.Row
		var lines []string
		for sid, info := range sessions {
			label := notify.FormatPaneID(info.tmuxTarget)
			rawNameTag := ""
			escapedNameTag := ""
			if info.name != "" {
				rawNameTag = " [" + info.name + "]"
				escapedNameTag = " [" + markdown.EscapeHTML(info.name) + "]"
			}
			lines = append(lines, fmt.Sprintf("  %s%s → %s", markdown.EscapeHTML(label), escapedNameTag, markdown.EscapeHTML(notify.CompressPath(info.cwd))))
			rows = append(rows, sel.Row(sel.Data(label+rawNameTag, "names", sid)))
		}
		sel.Inline(rows...)
		_, err := retrySend(bot, c.Chat(), "Select a session to name:\n"+strings.Join(lines, "\n"), sel, tele.ModeHTML)
		return err
	})

	bot.Handle("/bot_cwd", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatIDStr := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatIDStr) {
			return c.Reply("❌ Not paired.")
		}
		cfg, err := config.LoadAppConfig()
		if err != nil {
			return c.Reply("❌ Failed to load config")
		}
		current := cfg.CWDSource
		if current == "" {
			current = "tmux"
		}
		sel := &tele.ReplyMarkup{}
		btnTmux := sel.Data("📟 tmux CWD (current)", "cwd", "tmux")
		btnPayload := sel.Data("📦 payload CWD", "cwd", "payload")
		sel.Inline(sel.Row(btnTmux, btnPayload))
		return c.Reply(fmt.Sprintf("🔧 CWD source: %s\n\nSelect source for working directory:", current), sel)
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

var builtinTools = []string{"Edit", "Write", "Bash", "Read", "Glob", "Grep", "Agent", "WebFetch", "WebSearch", "MCP"}

// buildToolsMenu builds an inline keyboard for tool notification selection.
func buildToolsMenu(selected []string) *tele.ReplyMarkup {
	availTools := builtinTools
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

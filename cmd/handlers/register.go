package handlers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

// BindMenuItem describes a session entry for the bind selection menu.
type BindMenuItem struct {
	Key   string // name or sessionID
	Label string
}

// BindMenuContext holds the context for an active bind selection menu.
type BindMenuContext struct {
	Items   []BindMenuItem
	ChatID  int64
	TopicID int
}

var builtinTools = []string{
	"Edit", "Write", "Bash", "Read", "Glob", "Grep",
	"Agent", "WebFetch", "WebSearch", "MCP", "Skill",
	"TaskCreate", "TaskUpdate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
	"NotebookEdit", "EnterPlanMode", "ExitPlanMode",
	"EnterWorktree", "ExitWorktree", "Other",
}

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
		if len(pending) == 3 {
			rows = append(rows, menu.Row(pending...))
			pending = nil
		}
	}
	if len(pending) > 0 {
		rows = append(rows, menu.Row(pending...))
	}
	// Add All toggle button on the last row
	allSelected := len(selected) >= len(availTools)
	if allSelected {
		for _, tool := range availTools {
			if !selectedSet[tool] {
				allSelected = false
				break
			}
		}
	}
	allLabel := "⬜ All"
	if allSelected {
		allLabel = "✅ All"
	}
	rows = append(rows, menu.Row(menu.Data(allLabel, "tools_toggle_all")))
	menu.Inline(rows...)
	return menu
}

// ScanCustomCommands scans ~/.claude/commands/ for custom slash commands.
func ScanCustomCommands() map[string]stores.CustomCmd {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	commandsDir := filepath.Join(home, ".claude", "commands")
	result := make(map[string]stores.CustomCmd)
	filepath.Walk(commandsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(commandsDir, path)
		name := strings.TrimSuffix(rel, ".md")
		// Build CC command name: dir/file → dir:file
		parts := strings.Split(name, string(filepath.Separator))
		ccName := strings.Join(parts, ":")
		// Build TG command name: replace : and - with _
		tgName := strings.ReplaceAll(ccName, ":", "_")
		tgName = strings.ReplaceAll(tgName, "-", "_")
		// Read description: parse YAML frontmatter if present, else use first line
		desc := "Custom command: /" + ccName
		f, err := os.Open(path)
		if err == nil {
			scanner := bufio.NewScanner(f)
			if scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "---" {
					// YAML frontmatter: scan until closing --- and extract description
					for scanner.Scan() {
						fmLine := strings.TrimSpace(scanner.Text())
						if fmLine == "---" {
							break
						}
						if strings.HasPrefix(fmLine, "description:") {
							desc = helpers.TruncateStr(strings.TrimSpace(strings.TrimPrefix(fmLine, "description:")), 200)
						}
					}
				} else {
					line = strings.TrimLeft(line, "# ")
					if len(line) > 0 {
						desc = helpers.TruncateStr(line, 200)
					}
				}
			}
			f.Close()
		}
		result[tgName] = stores.CustomCmd{Desc: desc, CCName: ccName}
		return nil
	})
	return result
}

// resolveReplyTargetFromText resolves a tmux target from reply-to message text.
func resolveReplyTargetFromText(bs *types.BotState, text string) (injector.TmuxTarget, error) {
	targetPtr, err := helpers.ExtractTmuxTargetFromText(text)
	if err != nil || targetPtr == nil {
		return injector.TmuxTarget{}, fmt.Errorf("no target found")
	}
	tmuxStr := injector.FormatTarget(*targetPtr)
	if !checkSessionAlive(bs, tmuxStr) {
		return injector.TmuxTarget{}, fmt.Errorf("session not found")
	}
	return *targetPtr, nil
}

// Register registers all Telegram bot handlers.
func Register(bs *types.BotState) {
	bot := bs.Bot
	_ = bs.Creds
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
				preview := helpers.TruncateStr(c.Text(), 50)
				replyInfo := ""
				if msg.ReplyTo != nil {
					replyInfo = fmt.Sprintf(" reply_to=%d", msg.ReplyTo.ID)
				}
				threadInfo := ""
				if msg.ThreadID != 0 {
					threadInfo = fmt.Sprintf(" thread_id=%d", msg.ThreadID)
				}
				chatType := c.Chat().Type
				topicInfo := ""
				if msg.TopicMessage {
					topicInfo = " is_topic=true"
				}
				logger.Info(fmt.Sprintf("TG recv %s: chat=%d type=%s sender=%d msg_id=%d%s%s%s text=%s",
					msgType, c.Chat().ID, chatType, c.Sender().ID, msg.ID, replyInfo, threadInfo, topicInfo, preview))
				if raw, err := json.Marshal(msg); err == nil {
					logger.Debug(fmt.Sprintf("TG recv raw: %s", string(raw)))
				}
			}
			return next(c)
		}
	})
	// Track command usage for menu auto-sorting (runs after logging middleware)
	bot.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			if msg := c.Message(); msg != nil && strings.HasPrefix(msg.Text, "/") {
				cmdPart := strings.SplitN(msg.Text, " ", 2)[0]
				cmdPart = strings.SplitN(cmdPart, "@", 2)[0]
				cmdName := strings.TrimPrefix(cmdPart, "/")
				if cmdName != "" {
					bs.CommandStats.Record(cmdName)
				}
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
		helpers.RetrySend(bot, &tele.Chat{ID: to}, fmt.Sprintf("✅ Chat migrated: %d → %d\nAll route bindings updated.", from, to), tele.ModeHTML)
		return nil
	})
	// Build TG→CC name mapping
	ccCommandMap := make(map[string]string)
	for tgName := range stores.CCBuiltinCommands {
		ccName := tgName
		if tgName == "terminal_setup" {
			ccName = "terminal-setup"
		}
		ccCommandMap[tgName] = ccName
	}
	customCmds := ScanCustomCommands()
	for tgName, cmd := range customCmds {
		ccCommandMap[tgName] = cmd.CCName
	}

	// Register CC command handlers
	for tgName, ccName := range ccCommandMap {
		tg, cc := tgName, ccName
		bot.Handle("/"+tg, func(c tele.Context) error {
			// Try reply path first when ReplyTo exists and contains target info
			if c.Message().ReplyTo != nil {
				target, err := resolveReplyTargetFromText(bs, c.Message().ReplyTo.Text)
				if err == nil {
					text := "/" + cc
					if payload := strings.TrimSpace(c.Message().Payload); payload != "" {
						text += " " + payload
					}
					tmuxStr := injector.FormatTarget(target)
					// Check for pending tool wait via PendingWait (target-based, snap.UUID path)
					if pwSnap, hasPW := bs.PendingWait.FindByTmuxTarget(tmuxStr); hasPW {
						switch pwSnap.ToolName {
						default:
							// Cancel perm via snapshot then delayed inject (any non-AskQ tool is a PermReq)
							helpers.CancelPermBySnapshot(bot, bs.PendingWait, bs.NotifOpQueue, extractTarget, *pwSnap)
							go func() {
								time.Sleep(3 * time.Second)
								helpers.QueuedInject(bs.SessionEvents, bs.SessionState, target, text)
							}()
							logger.Info(fmt.Sprintf("Permission cancelled via CC command (reply) + delayed inject: target=%s text=%s", tmuxStr, helpers.TruncateStr(text, 200)))
							recordPending(bs, tmuxStr, c.Message().Chat.ID, c.Message().ID)
							return nil
						case "AskUserQuestion":
							// Resolve AskQ — build answer from snap.Questions
							if waitEntry, wok := bs.PendingWait.Get(pwSnap.UUID); wok {
								answers := make(map[string]string)
								if len(pwSnap.Questions) > 0 {
									answers[pwSnap.Questions[0].QuestionText] = text
								} else {
									answers["question"] = text
								}
								ccOutput := helpers.BuildAskCCOutput(waitEntry.Payload, answers)
								frozenMarkup := helpers.BuildFrozenMarkup(pwSnap.Questions, "✅ Text answer")
								capturedUUID := pwSnap.UUID
								won, _, _ := bs.PendingWait.ResolveIfUnresolved(pwSnap.UUID, stores.WaitEvent{
									Type:   "answer",
									Output: ccOutput,
								})
								if won {
									bs.NotifOpQueue.TryEnqueue(stores.NotifOp{
										Type:         stores.OpEDIT,
										UUID:         capturedUUID,
										FreezeLabel:  "✅ Text answer",
										FrozenMarkup: frozenMarkup,
										EditFunc: func(eID int, eChatID int64, editMsgText string) {
											editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: eChatID}}
											helpers.RetryEdit(bot, editMsg, editMsgText, frozenMarkup, tele.ModeHTML)
											logger.Info(fmt.Sprintf("CC command (reply): AskQ EDIT completed msg_id=%d", eID))
										},
									})
									logger.Info(fmt.Sprintf("AskUserQuestion resolved via CC command (reply): uuid=%s text=%s", pwSnap.UUID, helpers.TruncateStr(text, 200)))
								}
							}
							recordPending(bs, tmuxStr, c.Message().Chat.ID, c.Message().ID)
							return nil
						}
					}
					if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, target, text); err != nil {
						return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
					}
					recordPending(bs, tmuxStr, c.Message().Chat.ID, c.Message().ID)
					return nil
				}
				// ReplyTo exists but no target found (e.g. topic anchor) — fall through to group path
			}
			// Group path
			if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
				tmuxStr, target, err := resolveGroupTarget(bs, c.Chat().ID, c.Message().ThreadID)
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
				// Check for pending tool wait via PendingWait (target-based, snap.UUID path)
				if pwSnap, hasPW := bs.PendingWait.FindByTmuxTarget(tmuxStr); hasPW {
					switch pwSnap.ToolName {
					default:
						// Cancel perm via snapshot then delayed inject (any non-AskQ tool is a PermReq)
						helpers.CancelPermBySnapshot(bot, bs.PendingWait, bs.NotifOpQueue, extractTarget, *pwSnap)
						go func() {
							time.Sleep(3 * time.Second)
							helpers.QueuedInject(bs.SessionEvents, bs.SessionState, target, text)
						}()
						logger.Info(fmt.Sprintf("Permission cancelled via CC command (group) + delayed inject: target=%s text=%s", tmuxStr, helpers.TruncateStr(text, 200)))
						recordPending(bs, tmuxStr, c.Message().Chat.ID, c.Message().ID)
						return nil
					case "AskUserQuestion":
						// Resolve AskQ — build answer from snap.Questions
						if waitEntry, wok := bs.PendingWait.Get(pwSnap.UUID); wok {
							answers := make(map[string]string)
							if len(pwSnap.Questions) > 0 {
								answers[pwSnap.Questions[0].QuestionText] = text
							} else {
								answers["question"] = text
							}
							ccOutput := helpers.BuildAskCCOutput(waitEntry.Payload, answers)
							frozenMarkup := helpers.BuildFrozenMarkup(pwSnap.Questions, "✅ Text answer")
							capturedUUID := pwSnap.UUID
							won, _, _ := bs.PendingWait.ResolveIfUnresolved(pwSnap.UUID, stores.WaitEvent{
								Type:   "answer",
								Output: ccOutput,
							})
							if won {
								bs.NotifOpQueue.TryEnqueue(stores.NotifOp{
									Type:         stores.OpEDIT,
									UUID:         capturedUUID,
									FreezeLabel:  "✅ Text answer",
									FrozenMarkup: frozenMarkup,
									EditFunc: func(eID int, eChatID int64, editMsgText string) {
										editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: eChatID}}
										helpers.RetryEdit(bot, editMsg, editMsgText, frozenMarkup, tele.ModeHTML)
										logger.Info(fmt.Sprintf("CC command (group): AskQ EDIT completed msg_id=%d", eID))
									},
								})
								logger.Info(fmt.Sprintf("AskUserQuestion resolved via CC command (group): uuid=%s text=%s", pwSnap.UUID, helpers.TruncateStr(text, 200)))
							}
						}
						recordPending(bs, tmuxStr, c.Message().Chat.ID, c.Message().ID)
						return nil
					}
				}
				if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, target, text); err != nil {
					return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
				}
				logger.Info(fmt.Sprintf("Group quick reply (command): target=%s text=%s", tmuxStr, helpers.TruncateStr(text, 200)))
				recordPending(bs, tmuxStr, c.Message().Chat.ID, c.Message().ID)
				return nil
			}
			return c.Reply("💡 Please reply to a notification message to target a session.")
		})
	}

	bot.Handle("/resume", func(c tele.Context) error {
		payload := strings.TrimSpace(c.Message().Payload)
		// Resolve target: reply-to or group
		var target injector.TmuxTarget
		var tmuxStr string
		if c.Message().ReplyTo != nil {
			t, err := resolveReplyTargetFromText(bs, c.Message().ReplyTo.Text)
			if err != nil {
				if err.Error() == "no target found" {
					return c.Reply("❌ No tmux session info found in the original message.")
				}
				return c.Reply("❌ tmux session not found. The Claude Code session may have ended.")
			}
			target = t
			tmuxStr = injector.FormatTarget(t)
		} else if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
			ts, t, err := resolveGroupTarget(bs, c.Chat().ID, c.Message().ThreadID)
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
			if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, target, "/resume "+payload); err != nil {
				return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
			}
			recordPending(bs, tmuxStr, c.Message().Chat.ID, c.Message().ID)
			return nil
		}
		// Without payload: show session picker
		var cwd string
		info := bs.SessionState.FindInfoByTarget(tmuxStr)
		logger.Debug(fmt.Sprintf("/resume: findInfoByTarget tmuxStr=%s found=%v", tmuxStr, info != nil))
		if info != nil && info.CWD != "" {
			cwd = info.CWD
		} else {
			// Fallback: get CWD directly from tmux pane
			if target, parseErr := injector.ParseTarget(tmuxStr); parseErr == nil {
				out, err := injector.TmuxCmd(target, "display-message", "-p", "-t", target.PaneID, "#{pane_current_path}").Output()
				if err == nil {
					cwd = strings.TrimSpace(string(out))
				}
			}
			logger.Debug(fmt.Sprintf("/resume: tmux fallback cwd=%s", cwd))
		}
		if cwd == "" {
			return c.Reply("❌ No working directory info available for this session.")
		}
		currentSID, _ := bs.SessionState.FindByTarget(tmuxStr)
		var sessions []helpers.SessionListEntry
		var err error
		if info != nil && info.ProjectDir != "" {
			sessions, err = helpers.ListProjectSessionsByDir(info.ProjectDir, 8, currentSID)
		} else {
			sessions, err = helpers.ListProjectSessions(cwd, 8, currentSID)
		}
		if err != nil || len(sessions) == 0 {
			return c.Reply("📂 No previous sessions found for this project.")
		}
		if len(sessions) == 0 {
			return c.Reply("📂 No other sessions found for this project.")
		}
		kb := helpers.BuildResumeKeyboard(sessions)
		var lines []string
		lines = append(lines, "📟 "+notify.FormatPaneID(tmuxStr))
		lines = append(lines, "")
		for i, s := range sessions {
			prefix := "🤖"
			if s.SummarySource == "user" {
				prefix = "👤"
			}
			lines = append(lines, fmt.Sprintf("%d. %s %s — %s", i+1, prefix, markdown.EscapeHTML(helpers.TruncateStr(s.Summary, 500)), helpers.RelativeTime(s.Modified)))
		}
		text := strings.Join(lines, "\n")
		_, err = helpers.RetrySend(bot, c.Chat(), text, kb, tele.ModeHTML)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to send: %v", err))
		}
		return nil
	})

	bot.Handle("/start", func(c tele.Context) error {
		kb := &tele.ReplyMarkup{}
		appendDeleteButton(kb)
		return c.Reply("tg-cli bot is running. Use /bot_pair to pair this chat.", kb)
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
		if c.Message().ReplyTo != nil {
			targetPtr, err := helpers.ExtractTmuxTargetFromText(c.Message().ReplyTo.Text)
			if err != nil || targetPtr == nil {
				return c.Reply("❌ No tmux session info (📟) found in the replied message.")
			}
			tmuxStr := injector.FormatTarget(*targetPtr)
			sid, found := bs.SessionState.FindByTarget(tmuxStr)
			if !found {
				return c.Reply("❌ Session not found for that pane.")
			}
			info := bs.SessionState.FindInfoByTarget(tmuxStr)
			// Use name if available, otherwise session ID
			key := sid
			keyLabel := fmt.Sprintf("session:%s", sid[:8])
			if info != nil && info.Name != "" {
				key = info.Name
				keyLabel = info.Name
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
		sessions := bs.SessionState.All()
		if len(sessions) == 0 {
			return c.Reply("No active sessions to bind.")
		}
		sel := &tele.ReplyMarkup{}
		var rows []tele.Row
		var lines []string
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
			lines = append(lines, fmt.Sprintf("  %d. %s%s", idx, label, cwdStr))
			items = append(items, BindMenuItem{Key: key, Label: label})
			rows = append(rows, sel.Row(sel.Data(fmt.Sprintf("🔗 %d: %s", idx, label), "bind_select", fmt.Sprintf("%d", idx))))
			idx++
		}
		sel.Inline(rows...)
		sent, err := bot.Reply(c.Message(), fmt.Sprintf("Select a session to bind to this chat:\n%s", strings.Join(lines, "\n")), sel)
		if err == nil {
			bs.BindMenuItems.Store(sent.ID, BindMenuContext{Items: items, ChatID: chatID, TopicID: topicID})
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
		if c.Message().ReplyTo != nil {
			targetPtr, err := helpers.ExtractTmuxTargetFromText(c.Message().ReplyTo.Text)
			if err != nil || targetPtr == nil {
				return c.Reply("❌ No tmux session info (📟) found in the replied message.")
			}
			tmuxStr := injector.FormatTarget(*targetPtr)
			sid, found := bs.SessionState.FindByTarget(tmuxStr)
			if !found {
				return c.Reply("❌ Session not found for that pane.")
			}
			info := bs.SessionState.FindInfoByTarget(tmuxStr)
			creds, err := config.LoadCredentials()
			if err != nil {
				return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
			}
			// Try unbind by name first, then by session ID
			var unboundKey string
			if info != nil && info.Name != "" {
				if _, ok := creds.NameRouteMap[info.Name]; ok {
					delete(creds.NameRouteMap, info.Name)
					unboundKey = info.Name
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
			bs.UnbindMenuItems.Store(sent.ID, keys)
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
		if c.Message().ReplyTo != nil {
			targetPtr, err := helpers.ExtractTmuxTargetFromText(c.Message().ReplyTo.Text)
			if err != nil || targetPtr == nil {
				return c.Reply("❌ No tmux session info (📟) found in the replied message.")
			}
			sid, found := bs.SessionState.FindByTarget(injector.FormatTarget(*targetPtr))
			if !found {
				return c.Reply("❌ Session not found for that pane.")
			}
			sessionID = sid
		} else {
			sessions := bs.SessionState.All()
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
		ok, errMsg := bs.SessionState.SetName(sessionID, name)
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
		sessions := bs.SessionState.All()
		if len(sessions) == 0 {
			return c.Reply("No active sessions.")
		}
		sel := &tele.ReplyMarkup{}
		var rows []tele.Row
		var lines []string
		for sid, info := range sessions {
			label := notify.FormatPaneID(info.TmuxTarget)
			rawNameTag := ""
			escapedNameTag := ""
			if info.Name != "" {
				rawNameTag = " [" + info.Name + "]"
				escapedNameTag = " [" + markdown.EscapeHTML(info.Name) + "]"
			}
			lines = append(lines, fmt.Sprintf("  %s%s → %s", markdown.EscapeHTML(label), escapedNameTag, markdown.EscapeHTML(notify.CompressPath(info.CWD))))
			rows = append(rows, sel.Row(sel.Data(label+rawNameTag, "names", sid)))
		}
		sel.Inline(rows...)
		appendDeleteButton(sel)
		_, err := helpers.RetrySend(bot, c.Chat(), "Select a session to name:\n"+strings.Join(lines, "\n"), sel, tele.ModeHTML)
		return err
	})

	bot.Handle("/bot_new", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		args := strings.TrimSpace(c.Message().Payload)
		sessionName, workDir, command := ParseBotNewArgs(args)
		return StartLaunchFlow(bs, c, sessionName, workDir, command)
	})

	usageHandler := func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		return handleUsageCommand(bs, c)
	}
	bot.Handle("/bot_usage", usageHandler)
	bot.Handle("/u", usageHandler)

	checkUpdateHandler := func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		if c.Chat().Type != tele.ChatPrivate {
			chatIDInt := c.Chat().ID
			found := false
			for _, info := range bs.SessionState.All() {
				if info.Name == "" {
					continue
				}
				creds, _ := config.LoadCredentials()
				if route, ok := creds.NameRouteMap[info.Name]; ok && route.ChatID == chatIDInt {
					if checkSessionVersionByTarget(bs, info.TmuxTarget) {
						found = true
					}
				}
			}
			if !found {
				return c.Reply("✅ All sessions in this group are up to date.")
			}
			return nil
		}
		count := checkAllSessionVersions(bs)
		if count == 0 {
			return c.Reply("✅ All sessions are up to date.")
		}
		return nil
	}
	bot.Handle("/cu", checkUpdateHandler)
	bot.Handle("/check_update", checkUpdateHandler)

	mergeHandler := func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired.")
		}
		if c.Chat().Type != "group" && c.Chat().Type != "supergroup" {
			return c.Reply("❌ Merge mode is only available in groups.")
		}
		key := stores.MergeKey(c.Chat().ID)
		// Force cancel: /bot_merge cancel
		if strings.TrimSpace(c.Message().Payload) == "cancel" {
			buf, _ := bs.MergeBuffers.Finish(key)
			logger.Info(fmt.Sprintf("Merge force cancelled: chat=%d key=%s", c.Chat().ID, key))
			if buf != nil && buf.NotifyMsgID != 0 {
				editMsg := &tele.Message{ID: buf.NotifyMsgID, Chat: &tele.Chat{ID: buf.ChatID}}
				helpers.RetryEdit(bot, editMsg, BuildMergeNotifyText("❌ Cancelled", buf.Items), tele.ModeHTML)
			}
			return c.Reply("📎 Merge mode cancelled.")
		}
		if bs.MergeBuffers.Get(key) != nil {
			return c.Reply("⚠️ Merge mode already active. Send messages, then click Submit.")
		}
		tmuxStr, _, err := resolveGroupTarget(bs, c.Chat().ID, c.Message().ThreadID)
		if err != nil {
			if err.Error() == "multiple sessions bound" {
				return c.Reply("❌ Multiple sessions bound. Reply to a specific notification.")
			}
			return c.Reply("❌ No tmux session bound to this chat.")
		}
		menu := &tele.ReplyMarkup{}
		btnSubmit := menu.Data("📤 Submit", "merge_submit")
		btnCancel := menu.Data("❌ Cancel", "merge_cancel")
		menu.Inline(menu.Row(btnSubmit, btnCancel))
		sent, err := bot.Reply(c.Message(), BuildMergeNotifyText("📝 Collecting (0 messages)", nil), menu, tele.ModeHTML)
		if err != nil {
			return err
		}
		bs.MergeBuffers.Start(key, c.Chat().ID, tmuxStr, sent.ID)
		logger.Info(fmt.Sprintf("Merge started: chat=%d target=%s key=%s notify_msg=%d", c.Chat().ID, tmuxStr, key, sent.ID))
		return nil
	}
	bot.Handle("/bot_merge", mergeHandler)
	bot.Handle("/m", mergeHandler)

	bot.Handle("/bot_settings", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired. Use /bot_pair first.")
		}
		_, err := SendSettingsMenu(bs, c.Chat())
		return err
	})

	bot.Handle("/bot_at", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("❌ Not paired. Use /bot_pair first.")
		}
		payload := strings.TrimSpace(c.Message().Payload)
		if payload == "" {
			return c.Reply("❌ Usage: /bot_at <Name> [message]\n/bot_at end <Name>")
		}
		parts := strings.SplitN(payload, " ", 2)
		targetName := parts[0]
		msg := ""
		if len(parts) > 1 {
			msg = parts[1]
		}
		if targetName == "end" {
			if msg == "" {
				return c.Reply("❌ Usage: /bot_at end <Name>")
			}
			endTarget := strings.TrimSpace(msg)
			tmuxStr, _, err := resolveGroupTarget(bs, c.Chat().ID, c.Message().ThreadID)
			if err != nil {
				return c.Reply("❌ No session bound to this group.")
			}
			initiatorInfo := bs.SessionState.FindInfoByTarget(tmuxStr)
			if initiatorInfo == nil || initiatorInfo.Name == "" {
				return c.Reply("❌ Session has no name.")
			}
			// Call /at/close API to reuse the full close logic (TG notifications + pane inject)
			creds, _ := config.LoadCredentials()
			port := creds.Port
			if port == 0 {
				port = 12500
			}
			closeBody, _ := json.Marshal(map[string]string{"initiator": initiatorInfo.Name, "target": endTarget})
			closeResp, closeErr := http.Post(fmt.Sprintf("http://127.0.0.1:%d/at/close", port), "application/json", bytes.NewReader(closeBody))
			if closeErr != nil {
				return c.Reply(fmt.Sprintf("❌ Close failed: %v", closeErr))
			}
			defer closeResp.Body.Close()
			if closeResp.StatusCode != 200 {
				respBody, _ := io.ReadAll(closeResp.Body)
				return c.Reply(fmt.Sprintf("❌ %s", string(respBody)))
			}
			return c.Reply(fmt.Sprintf("@ channel closed: %s ↔ %s", initiatorInfo.Name, endTarget))
		}
		tmuxStr, _, err := resolveGroupTarget(bs, c.Chat().ID, c.Message().ThreadID)
		if err != nil {
			return c.Reply("❌ No session bound to this group.")
		}
		initiatorInfo := bs.SessionState.FindInfoByTarget(tmuxStr)
		if initiatorInfo == nil || initiatorInfo.Name == "" {
			return c.Reply("❌ Session has no name. Use /bot_name to set one first.")
		}
		go openAtChannel(bs, initiatorInfo.Name, targetName, 3, 0, msg)
		return c.Reply(fmt.Sprintf("📨 Opening @ channel: %s → %s", initiatorInfo.Name, targetName))
	})

	RegisterMessageHandlers(bs)
	RegisterCallbackHandlers(bs)
}

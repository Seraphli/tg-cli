package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	"github.com/Seraphli/tg-cli/internal/voice"
	tele "gopkg.in/telebot.v3"
)

// resolveGroupTarget finds the unique bound tmux target for a group chat.
// Checks both direct tmux routes and project routes with active sessions.
func resolveGroupTarget(chatID int64) (string, injector.TmuxTarget, error) {
	creds, _ := config.LoadCredentials()
	var targets []string
	// Direct tmux routes — validate pane still exists
	for t, cid := range creds.RouteMap {
		if cid == chatID {
			target, err := injector.ParseTarget(t)
			if err != nil || !injector.SessionExists(target) {
				continue
			}
			targets = append(targets, t)
		}
	}
	// Project routes: find active sessions with matching CWD
	for cwd, cid := range creds.ProjectRouteMap {
		if cid == chatID {
			addedFromState := false
			if info := sessionState.findByCWD(cwd); info != nil {
				target, err := injector.ParseTarget(info.tmuxTarget)
				if err == nil && injector.SessionExists(target) {
					normalized := notify.FormatPaneID(info.tmuxTarget)
					found := false
					for _, t := range targets {
						if notify.FormatPaneID(t) == normalized {
							found = true
							break
						}
					}
					if !found {
						targets = append(targets, info.tmuxTarget)
					}
					addedFromState = true
				}
			}
			// Fall through to tmux scan if sessionState had no live pane
			if !addedFromState {
				out, scanErr := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_path}").Output()
				if scanErr == nil {
					for _, pl := range strings.Split(strings.TrimSpace(string(out)), "\n") {
						parts := strings.SplitN(pl, "\t", 2)
						if len(parts) >= 2 && parts[1] == cwd {
							normalized := notify.FormatPaneID(parts[0])
							found := false
							for _, t := range targets {
								if notify.FormatPaneID(t) == normalized {
									found = true
									break
								}
							}
							if !found {
								targets = append(targets, parts[0])
							}
						}
					}
				}
			}
		}
	}
	if len(targets) == 0 {
		return "", injector.TmuxTarget{}, fmt.Errorf("no targets bound")
	}
	if len(targets) > 1 {
		return "", injector.TmuxTarget{}, fmt.Errorf("multiple sessions bound")
	}
	target, err := injector.ParseTarget(targets[0])
	if err != nil || !injector.SessionExists(target) {
		return "", injector.TmuxTarget{}, fmt.Errorf("session not found")
	}
	return targets[0], target, nil
}

// transcribeVoice downloads and transcribes a voice message
func transcribeVoice(bot *tele.Bot, fileID string) (string, error) {
	file, err := bot.FileByID(fileID)
	if err != nil {
		logger.Error(fmt.Sprintf("Voice file lookup failed: %v", err))
		return "", fmt.Errorf("failed to get voice file: %w", err)
	}
	tmpFile := filepath.Join(os.TempDir(), "tg-cli-voice-"+fileID+".ogg")
	defer os.Remove(tmpFile)
	if err := bot.Download(&file, tmpFile); err != nil {
		logger.Error(fmt.Sprintf("Voice download failed: %v", err))
		return "", fmt.Errorf("failed to download voice: %w", err)
	}
	text, engine, err := voice.Transcribe(tmpFile)
	if err != nil {
		logger.Error(fmt.Sprintf("Voice transcription failed: %v", err))
		return "", fmt.Errorf("transcription failed: %w", err)
	}
	logger.Info(fmt.Sprintf("Voice transcribed: engine=%s text=%s", engine, truncateStr(text, 200)))
	return text, nil
}

// restoreSpoilers wraps spoiler entities back into ||spoiler|| markdown syntax.
func restoreSpoilers(text string, entities []tele.MessageEntity) string {
	if len(entities) == 0 {
		return text
	}
	type spoilerRange struct{ offset, length int }
	var spoilers []spoilerRange
	for _, e := range entities {
		if e.Type == tele.EntitySpoiler {
			spoilers = append(spoilers, spoilerRange{e.Offset, e.Length})
		}
	}
	if len(spoilers) == 0 {
		return text
	}
	sort.Slice(spoilers, func(i, j int) bool { return spoilers[i].offset > spoilers[j].offset })
	runes := []rune(text)
	for _, sp := range spoilers {
		end := sp.offset + sp.length
		if end > len(runes) {
			end = len(runes)
		}
		runes = append(runes[:end], append([]rune("||"), runes[end:]...)...)
		runes = append(runes[:sp.offset], append([]rune("||"), runes[sp.offset:]...)...)
	}
	return string(runes)
}

// resolveReplyTarget extracts and validates tmux target from reply message.
// If the pane is alive but not in sessionState, recovers it.
func resolveReplyTarget(replyText string) (injector.TmuxTarget, error) {
	if replyText == "" {
		logger.Debug("resolveReplyTarget: empty replyText")
		return injector.TmuxTarget{}, fmt.Errorf("no target found")
	}
	targetPtr, err := extractTmuxTarget(replyText)
	if err != nil {
		logger.Debug(fmt.Sprintf("resolveReplyTarget: extractTmuxTarget failed: %v text=%s", err, truncateStr(replyText, 100)))
		return injector.TmuxTarget{}, fmt.Errorf("no target found")
	}
	target := *targetPtr
	if !injector.SessionExists(target) {
		return injector.TmuxTarget{}, fmt.Errorf("session not found")
	}
	tmuxStr := injector.FormatTarget(target)
	if _, found := sessionState.findByTarget(tmuxStr); !found {
		cwd := getPaneCWD(target.PaneID)
		if cwd != "" {
			sessionState.add("recovered-"+target.PaneID, tmuxStr, cwd, "")
			logger.Info(fmt.Sprintf("Session recovered from reply: tmux=%s cwd=%s", tmuxStr, cwd))
		}
	}
	return target, nil
}

// processUserInput handles the shared logic for OnText and OnVoice after routing.
// text is the raw transcribed or typed text; isVoice indicates input method.
// voicePrefix is prepended to injected text when isVoice is true.
func processUserInput(c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string) error {
	answerLabel := "✅ Text answer"
	if isVoice {
		answerLabel = "✅ Voice answer"
	}
	text = restoreSpoilers(text, c.Message().Entities)
	injectionText := text
	if isVoice {
		injectionText = voicePrefix + " " + text
	}
	reactSeen(bot, c.Message().Chat, c.Message())
	// sendFeedback sends the appropriate feedback message for a group or reply context
	sendFeedback := func(tmuxTarget string) {
		recordPending(tmuxTarget, c.Message().Chat.ID, c.Message().ID)
		if isVoice {
			bot.Reply(c.Message(), voicePrefix+" "+text)
		}
	}

	// Group path: no reply, group/supergroup chat
	if c.Message().ReplyTo == nil {
		if c.Chat().Type != "group" && c.Chat().Type != "supergroup" {
			return nil
		}
		// Skip forwarded messages (used for /bot_bind, not injection)
		if c.Message().OriginalUnixtime != 0 {
			return nil
		}
		tmuxStr, target, err := resolveGroupTarget(c.Chat().ID)
		if err != nil {
			if err.Error() == "no targets bound" {
				return nil
			}
			if err.Error() == "multiple sessions bound" {
				return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
			}
			return c.Reply("❌ tmux session not found.")
		}
		for {
			msgID, entry, ok := toolNotifs.findByTmuxTarget(tmuxStr)
			if !ok {
				break
			}
			uuid, uuidOk := pendingFiles.get(msgID)
			if !uuidOk {
				toolNotifs.markResolved(msgID)
				continue
			}
			if handleStalePending(msgID, uuid, bot) {
				continue
			}
			path := filepath.Join(pendingDir(), uuid+".json")
			pf, err := readPendingFile(path)
			if err != nil {
				toolNotifs.markResolved(msgID)
				continue
			}
			answers := make(map[string]string)
			if len(entry.questions) > 0 {
				answers[entry.questions[0].questionText] = text
			}
			ccOutput := buildAskCCOutput(pf.Payload, answers)
			if err := writePendingAnswer(uuid, ccOutput); err != nil {
				logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
			} else {
				toolNotifs.markResolved(msgID)
				logger.Info(fmt.Sprintf("AskUserQuestion custom text via group direct msg: msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(text, 200)))
				editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
				retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, answerLabel))
			}
			sendFeedback(tmuxStr)
			return nil
		}
		if msgID, ok := pendingPerms.findByTmuxTarget(tmuxStr); ok {
			doCancelPerm(bot, msgID)
			go func() {
				time.Sleep(3 * time.Second)
				injector.InjectText(target, injectionText)
			}()
			logger.Info(fmt.Sprintf("Permission cancelled via group msg, ESC+delayed inject: perm_msg=%d target=%s voice=%v text=%s", msgID, tmuxStr, isVoice, truncateStr(text, 200)))
			sendFeedback(tmuxStr)
			return nil
		}
		if !checkSessionAlive(tmuxStr, bot) {
			return c.Reply("⚠️ Session is no longer running. Tmux route has been unbound.")
		}
		sendFeedback(tmuxStr)
		if err := injector.InjectText(target, injectionText); err != nil {
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Group quick reply: target=%s voice=%v text=%s", tmuxStr, isVoice, truncateStr(text, 200)))
		return nil
	}

	// Reply path: ReplyTo != nil
	replyTo := c.Message().ReplyTo

	// Check if this is a reply to a bot_new confirmation message
	if val, ok := launchPending.Load(replyTo.ID); ok {
		state := val.(*LaunchState)
		customValue := strings.TrimSpace(c.Text())
		launchPending.Delete(replyTo.ID)
		switch state.Step {
		case "session":
			state.SessionName = customValue
			c.Bot().Edit(c.Message().ReplyTo, fmt.Sprintf("📦 Session name\n✅ %s", state.SessionName))
			if state.WorkDir == "" {
				askWorkDir(c.Bot(), state.ChatID, state)
			} else {
				go executeLaunch(c.Bot(), state.ChatID, state)
			}
		case "workdir":
			if strings.HasPrefix(customValue, "~") {
				home, _ := os.UserHomeDir()
				customValue = home + customValue[1:]
			}
			state.WorkDir = customValue
			c.Bot().Edit(c.Message().ReplyTo, fmt.Sprintf("📂 Working directory\n✅ %s", state.WorkDir))
			go executeLaunch(c.Bot(), state.ChatID, state)
		}
		return nil
	}

	if _, ok := pendingPerms.getTarget(replyTo.ID); ok {
		doCancelPerm(bot, replyTo.ID)
		targetPtr, err := extractTmuxTarget(replyTo.Text)
		if err == nil && targetPtr != nil {
			target := *targetPtr
			if injector.SessionExists(target) {
				go func() {
					time.Sleep(3 * time.Second)
					injector.InjectText(target, injectionText)
				}()
				logger.Info(fmt.Sprintf("Permission cancelled via reply, text will inject after ESC: msg_id=%d target=%s voice=%v text=%s", replyTo.ID, injector.FormatTarget(target), isVoice, truncateStr(text, 200)))
				sendFeedback(injector.FormatTarget(target))
			}
		}
		return nil
	}

	if entry, ok := toolNotifs.get(replyTo.ID); ok {
		target, err := injector.ParseTarget(entry.tmuxTarget)
		if err != nil || !injector.SessionExists(target) {
			return c.Reply("❌ tmux session not found.")
		}
		switch entry.toolName {
		case "AskUserQuestion":
			if entry.resolved {
				toolNotifs.markResolved(replyTo.ID)
				injector.InjectText(target, injectionText)
				return nil
			}
			uuid, uuidOk := pendingFiles.get(replyTo.ID)
			if !uuidOk {
				// No pending file mapping, treat as stale
				toolNotifs.markResolved(replyTo.ID)
				injector.InjectText(target, injectionText)
				return nil
			}
			if handleStalePending(replyTo.ID, uuid, bot) {
				// Stale: hook dead or file missing, inject text
				injector.InjectText(target, injectionText)
				return nil
			}
			// Cancel pending file and send ESC, then inject after delay
			path := filepath.Join(pendingDir(), uuid+".json")
			pf, err := readPendingFile(path)
			if err == nil {
				pf.Status = "cancelled"
				writePendingFile(path, pf)
			}
			toolNotifs.markResolved(replyTo.ID)
			editMsg := &tele.Message{ID: replyTo.ID, Chat: &tele.Chat{ID: entry.chatID}}
			retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, answerLabel))
			injector.SendKeys(target, "Escape")
			go func() {
				time.Sleep(3 * time.Second)
				injector.InjectText(target, injectionText)
			}()
			logger.Info(fmt.Sprintf("AskUserQuestion text reply: ESC+delayed inject: msg_id=%d uuid=%s voice=%v text=%s", replyTo.ID, uuid, isVoice, truncateStr(text, 200)))
			sendFeedback(entry.tmuxTarget)
			return nil
		}
		logger.Info(fmt.Sprintf("Tool reply: tool=%s msg_id=%d target=%s voice=%v text=%s", entry.toolName, replyTo.ID, entry.tmuxTarget, isVoice, truncateStr(text, 200)))
		recordPending(entry.tmuxTarget, c.Message().Chat.ID, c.Message().ID)
		return nil
	}

	// General reply path
	target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
	if err != nil {
		return c.Reply("❌ No tmux session info found in the original message.")
	}
	if !checkSessionAlive(injector.FormatTarget(target), bot) {
		return c.Reply("⚠️ Session is no longer running. Tmux route has been unbound.")
	}
	if err := injector.InjectText(target, injectionText); err != nil {
		logger.Error(fmt.Sprintf("Injection failed: %v", err))
		return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
	}
	logger.Info(fmt.Sprintf("Injected reply to %s voice=%v text=%s", injector.FormatTarget(target), isVoice, truncateStr(text, 200)))
	recordPending(injector.FormatTarget(target), c.Message().Chat.ID, c.Message().ID)
	if isVoice {
		bot.Reply(c.Message(), voicePrefix+" "+text)
	}
	return nil
}

// registerMessageHandlers registers OnText and OnVoice handlers
func registerMessageHandlers(bot *tele.Bot) {
	cfg, _ := config.LoadAppConfig()
	voicePrefix := cfg.VoicePrefix

	bot.Handle(tele.OnText, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
				isCmd := strings.HasPrefix(c.Message().Text, "/bot_perm_") ||
					c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") ||
					c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@")
				if isCmd {
					_, target, err := resolveGroupTarget(c.Chat().ID)
					if err != nil {
						if err.Error() == "multiple sessions bound" {
							return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
						}
						return c.Reply("❌ tmux session not found.")
					}
					if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
						return handlePermCommand(c, target)
					}
					if c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") {
						return handleCaptureCommand(c, target)
					}
					return handleEscapeCommand(c, target)
				}
			}
		} else {
			if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
				target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handlePermCommand(c, target)
			}
			if c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") {
				target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleCaptureCommand(c, target)
			}
			if c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@") {
				target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleEscapeCommand(c, target)
			}
		}
		return processUserInput(c, bot, c.Message().Text, false, voicePrefix)
	})

	bot.Handle(tele.OnVoice, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			if c.Chat().Type != "group" && c.Chat().Type != "supergroup" {
				return nil
			}
			text, err := transcribeVoice(bot, c.Message().Voice.FileID)
			if err != nil {
				logger.Error(fmt.Sprintf("Group voice transcription failed: err=%v", err))
				return c.Reply(fmt.Sprintf("❌ %v", err))
			}
			if text == "" {
				logger.Error("Group voice transcription failed: empty text")
				return c.Reply("❌ Transcription produced empty text.")
			}
			return processUserInput(c, bot, text, true, voicePrefix)
		}
		text, err := transcribeVoice(bot, c.Message().Voice.FileID)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ %v", err))
		}
		if text == "" {
			return c.Reply("❌ Transcription produced empty text.")
		}
		return processUserInput(c, bot, text, true, voicePrefix)
	})
}

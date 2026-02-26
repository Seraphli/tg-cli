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
	"github.com/Seraphli/tg-cli/internal/pairing"
	"github.com/Seraphli/tg-cli/internal/voice"
	tele "gopkg.in/telebot.v3"
)

// resolveGroupTarget finds the unique bound tmux target for a group chat.
// Checks both direct tmux routes and project routes with active sessions.
func resolveGroupTarget(chatID int64) (string, injector.TmuxTarget, error) {
	creds, _ := config.LoadCredentials()
	var targets []string
	// Direct tmux routes
	for t, cid := range creds.RouteMap {
		if cid == chatID {
			targets = append(targets, t)
		}
	}
	// Project routes: find active sessions with matching CWD
	for cwd, cid := range creds.ProjectRouteMap {
		if cid == chatID {
			if info := sessionState.findByCWD(cwd); info != nil {
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
			} else {
				out, scanErr := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_path}\t#{pane_current_command}").Output()
				if scanErr == nil {
					for _, pl := range strings.Split(strings.TrimSpace(string(out)), "\n") {
						parts := strings.SplitN(pl, "\t", 3)
						if len(parts) == 3 && parts[1] == cwd && parts[2] == "claude" {
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
		return "", fmt.Errorf("failed to get voice file: %w", err)
	}
	tmpFile := filepath.Join(os.TempDir(), "tg-cli-voice-"+fileID+".ogg")
	defer os.Remove(tmpFile)
	if err := bot.Download(&file, tmpFile); err != nil {
		return "", fmt.Errorf("failed to download voice: %w", err)
	}
	text, err := voice.Transcribe(tmpFile)
	if err != nil {
		return "", fmt.Errorf("transcription failed: %w", err)
	}
	return text, nil
}

// reactAndTrack adds a reaction emoji and records it in the tracker
func reactAndTrack(bot *tele.Bot, chat *tele.Chat, msg *tele.Message, tmuxTarget string) {
	if err := bot.React(chat, msg, tele.ReactionOptions{
		Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
	}); err == nil {
		reactionTracker.record(tmuxTarget, chat.ID, msg.ID)
	}
}

// resolveReplyTarget extracts and validates tmux target from reply message
func resolveReplyTarget(replyText string) (injector.TmuxTarget, error) {
	targetPtr, err := extractTmuxTarget(replyText)
	if err != nil {
		return injector.TmuxTarget{}, fmt.Errorf("no target found")
	}
	target := *targetPtr
	if !injector.SessionExists(target) {
		return injector.TmuxTarget{}, fmt.Errorf("session not found")
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
	injectionText := text
	if isVoice {
		injectionText = voicePrefix + " " + text
	}
	// sendFeedback sends the appropriate feedback message for a group or reply context
	sendFeedback := func(tmuxTarget string) {
		if isVoice {
			sentMsg, _ := bot.Reply(c.Message(), voicePrefix+" "+text)
			if sentMsg != nil {
				reactAndTrack(bot, c.Message().Chat, sentMsg, tmuxTarget)
			}
		} else {
			reactAndTrack(bot, c.Message().Chat, c.Message(), tmuxTarget)
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
		if msgID, entry, ok := toolNotifs.findByTmuxTarget(tmuxStr); ok {
			uuid, uuidOk := pendingFiles.get(msgID)
			if uuidOk {
				if handleStalePending(msgID, uuid, bot) {
					// Stale: hook dead or file missing, fall through to InjectText
				} else {
					path := filepath.Join(pendingDir(), uuid+".json")
					pf, err := readPendingFile(path)
					if err == nil {
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
							bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, answerLabel))
						}
						sendFeedback(tmuxStr)
						return nil
					}
				}
			}
		}
		if !checkSessionAlive(tmuxStr, bot) {
			return c.Reply("⚠️ Session is no longer running. Tmux route has been unbound.")
		}
		if err := injector.InjectText(target, injectionText); err != nil {
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Group quick reply: target=%s voice=%v text=%s", tmuxStr, isVoice, truncateStr(text, 200)))
		sendFeedback(tmuxStr)
		return nil
	}

	// Reply path: ReplyTo != nil
	replyTo := c.Message().ReplyTo
	if _, ok := pendingPerms.getTarget(replyTo.ID); ok {
		uuid, uuidOk := pendingPerms.getUUID(replyTo.ID)
		if !uuidOk {
			uuid, uuidOk = pendingFiles.get(replyTo.ID)
		}
		sugLabels := parseSuggestionLabels(pendingPerms.getSuggestions(replyTo.ID))
		denyMsg := "User provided custom input: " + text
		if isVoice {
			denyMsg = "User provided voice input: " + text
		}
		d := permDecision{
			Behavior: "deny",
			Message:  denyMsg,
		}
		pendingPerms.resolve(replyTo.ID, d)
		if uuidOk {
			ccOutput := buildPermCCOutput(d.Behavior, d.Message, nil)
			if err := writePendingAnswer(uuid, ccOutput); err != nil {
				logger.Error(fmt.Sprintf("Failed to write pending answer for perm: %v", err))
			}
		}
		editMsg := &tele.Message{ID: replyTo.ID, Chat: &tele.Chat{ID: c.Chat().ID}}
		bot.Edit(editMsg, replyTo.Text, buildFrozenPermMarkup("deny", sugLabels))
		targetPtr, err := extractTmuxTarget(replyTo.Text)
		if err == nil && targetPtr != nil {
			target := *targetPtr
			if injector.SessionExists(target) {
				injector.InjectText(target, injectionText)
			}
			logger.Info(fmt.Sprintf("Permission denied via reply, text injected: msg_id=%d target=%s uuid=%s voice=%v text=%s", replyTo.ID, injector.FormatTarget(target), uuid, isVoice, truncateStr(text, 200)))
			sendFeedback(injector.FormatTarget(target))
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
			uuid, ok := pendingFiles.get(replyTo.ID)
			if !ok {
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
			path := filepath.Join(pendingDir(), uuid+".json")
			pf, err := readPendingFile(path)
			if err != nil {
				if !isVoice {
					// For text: fall through to log+react below
					break
				}
				// For voice: unexpected read error after stale check
				return c.Reply("❌ Failed to read pending file.")
			}
			answers := make(map[string]string)
			if len(entry.questions) > 0 {
				answers[entry.questions[0].questionText] = text
			}
			ccOutput := buildAskCCOutput(pf.Payload, answers)
			if err := writePendingAnswer(uuid, ccOutput); err != nil {
				logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
			} else {
				toolNotifs.markResolved(replyTo.ID)
				logger.Info(fmt.Sprintf("AskUserQuestion custom reply: msg_id=%d uuid=%s voice=%v text=%s", replyTo.ID, uuid, isVoice, truncateStr(text, 200)))
				editMsg := &tele.Message{ID: replyTo.ID, Chat: &tele.Chat{ID: entry.chatID}}
				bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, answerLabel))
				sendFeedback(entry.tmuxTarget)
				return nil
			}
		}
		logger.Info(fmt.Sprintf("Tool reply: tool=%s msg_id=%d target=%s voice=%v text=%s", entry.toolName, replyTo.ID, entry.tmuxTarget, isVoice, truncateStr(text, 200)))
		reactAndTrack(bot, c.Message().Chat, c.Message(), entry.tmuxTarget)
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
	if isVoice {
		tmuxStr := injector.FormatTarget(target)
		sentMsg, _ := bot.Reply(c.Message(), voicePrefix+" "+text)
		if sentMsg != nil {
			reactAndTrack(bot, c.Message().Chat, sentMsg, tmuxStr)
		}
	} else {
		if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		}); err != nil {
			logger.Debug(fmt.Sprintf("React failed: %v, falling back to reply", err))
			return c.Reply("✅")
		} else {
			tmuxStr := injector.FormatTarget(target)
			reactionTracker.record(tmuxStr, c.Chat().ID, c.Message().ID)
		}
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
			if err != nil || text == "" {
				return c.Reply("❌ Transcription failed or empty.")
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

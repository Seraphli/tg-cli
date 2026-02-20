package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/pairing"
	"github.com/Seraphli/tg-cli/internal/voice"
	tele "gopkg.in/telebot.v3"
)

// resolveGroupTarget finds the unique bound tmux target for a group chat
func resolveGroupTarget(chatID int64) (string, injector.TmuxTarget, error) {
	creds, _ := config.LoadCredentials()
	var targets []string
	for t, cid := range creds.RouteMap {
		if cid == chatID {
			targets = append(targets, t)
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

// registerMessageHandlers registers OnText and OnVoice handlers
func registerMessageHandlers(bot *tele.Bot) {
	bot.Handle(tele.OnText, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
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
				if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
					return handlePermCommand(c, target)
				}
				if c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") {
					return handleCaptureCommand(c, target)
				}
				if c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@") {
					return handleEscapeCommand(c, target)
				}
				if msgID, entry, ok := toolNotifs.findByTmuxTarget(tmuxStr); ok {
					uuid, uuidOk := pendingFiles.get(msgID)
					if uuidOk {
						path := filepath.Join(pendingDir(), uuid+".json")
						pf, err := readPendingFile(path)
						if err == nil {
							answers := make(map[string]string)
							if len(entry.questions) > 0 {
								answers[entry.questions[0].questionText] = c.Message().Text
							}
							ccOutput := buildAskCCOutput(pf.Payload, answers)
							if err := writePendingAnswer(uuid, ccOutput); err != nil {
								logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
							} else {
								toolNotifs.markResolved(msgID)
								logger.Info(fmt.Sprintf("AskUserQuestion custom text via group direct msg: msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(c.Message().Text, 200)))
								editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
								bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"))
							}
							reactAndTrack(bot, c.Message().Chat, c.Message(), tmuxStr)
							return nil
						}
					}
				}
				if err := injector.InjectText(target, c.Message().Text); err != nil {
					return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
				}
				logger.Info(fmt.Sprintf("Group quick reply: target=%s text=%s", tmuxStr, truncateStr(c.Message().Text, 200)))
				bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
					Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
				})
				reactionTracker.record(tmuxStr, c.Chat().ID, c.Message().ID)
				return nil
			}
			return nil
		}
		if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
			target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info found.")
			}
			return handlePermCommand(c, target)
		}
		if (c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@")) && c.Message().ReplyTo != nil {
			target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info found.")
			}
			return handleCaptureCommand(c, target)
		}
		if (c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@")) && c.Message().ReplyTo != nil {
			target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info found.")
			}
			return handleEscapeCommand(c, target)
		}
		if replyTo := c.Message().ReplyTo; replyTo != nil {
			if _, ok := pendingPerms.getTarget(replyTo.ID); ok {
				uuid, uuidOk := pendingPerms.getUUID(replyTo.ID)
				if !uuidOk {
					uuid, uuidOk = pendingFiles.get(replyTo.ID)
				}
				sugLabels := parseSuggestionLabels(pendingPerms.getSuggestions(replyTo.ID))
				d := permDecision{
					Behavior: "deny",
					Message:  "User provided custom input: " + c.Message().Text,
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
						injector.InjectText(target, c.Message().Text)
					}
					logger.Info(fmt.Sprintf("Permission denied via text reply, text injected: msg_id=%d target=%s uuid=%s text=%s", replyTo.ID, injector.FormatTarget(target), uuid, truncateStr(c.Message().Text, 200)))
					reactAndTrack(bot, c.Message().Chat, c.Message(), injector.FormatTarget(target))
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
					if !entry.resolved {
						uuid, ok := pendingFiles.get(replyTo.ID)
						if ok {
							path := filepath.Join(pendingDir(), uuid+".json")
							pf, err := readPendingFile(path)
							if err == nil {
								answers := make(map[string]string)
								if len(entry.questions) > 0 {
									answers[entry.questions[0].questionText] = c.Message().Text
								}
								ccOutput := buildAskCCOutput(pf.Payload, answers)
								if err := writePendingAnswer(uuid, ccOutput); err != nil {
									logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
								} else {
									toolNotifs.markResolved(replyTo.ID)
									logger.Info(fmt.Sprintf("AskUserQuestion custom text via reply: msg_id=%d uuid=%s text=%s", replyTo.ID, uuid, truncateStr(c.Message().Text, 200)))
									editMsg := &tele.Message{ID: replyTo.ID, Chat: &tele.Chat{ID: entry.chatID}}
									bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"))
								}
							}
						}
					} else {
						injector.InjectText(target, c.Message().Text)
					}
				}
				logger.Info(fmt.Sprintf("Tool text reply: tool=%s msg_id=%d target=%s text=%s", entry.toolName, replyTo.ID, entry.tmuxTarget, truncateStr(c.Message().Text, 200)))
				reactAndTrack(bot, c.Message().Chat, c.Message(), entry.tmuxTarget)
				return nil
			}
		}
		target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info found in the original message.")
		}
		if err := injector.InjectText(target, c.Message().Text); err != nil {
			logger.Error(fmt.Sprintf("Injection failed: %v", err))
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Injected text to %s text=%s", injector.FormatTarget(target), truncateStr(c.Message().Text, 200)))
		if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		}); err != nil {
			logger.Debug(fmt.Sprintf("React failed: %v, falling back to reply", err))
			return c.Reply("✅")
		} else {
			tmuxStr := injector.FormatTarget(target)
			reactionTracker.record(tmuxStr, c.Chat().ID, c.Message().ID)
		}
		return nil
	})

	bot.Handle(tele.OnVoice, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
				tmuxStr, target, err := resolveGroupTarget(c.Chat().ID)
				if err != nil {
					if err.Error() == "no targets bound" {
						return nil
					}
					if err.Error() == "multiple sessions bound" {
						return c.Reply("❌ Multiple sessions bound. Reply to a specific notification.")
					}
					return c.Reply("❌ tmux session not found.")
				}
				text, err := transcribeVoice(bot, c.Message().Voice.FileID)
				if err != nil || text == "" {
					return c.Reply("❌ Transcription failed or empty.")
				}
				if msgID, entry, ok := toolNotifs.findByTmuxTarget(tmuxStr); ok {
					uuid, uuidOk := pendingFiles.get(msgID)
					if uuidOk {
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
								logger.Info(fmt.Sprintf("AskUserQuestion custom voice via group direct msg: msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(text, 200)))
								editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
								bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Voice answer"))
							}
							sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
							if sentMsg != nil {
								reactAndTrack(bot, c.Message().Chat, sentMsg, tmuxStr)
							}
							return nil
						}
					}
				}
				if err := injector.InjectText(target, text); err != nil {
					return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
				}
				logger.Info(fmt.Sprintf("Group voice quick reply: target=%s text=%s", tmuxStr, truncateStr(text, 200)))
				sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
				if sentMsg != nil {
					bot.React(c.Message().Chat, sentMsg, tele.ReactionOptions{
						Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
					})
					reactionTracker.record(tmuxStr, c.Chat().ID, sentMsg.ID)
				}
				return nil
			}
			return nil
		}
		text, err := transcribeVoice(bot, c.Message().Voice.FileID)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ %v", err))
		}
		if text == "" {
			return c.Reply("❌ Transcription produced empty text.")
		}
		if replyTo := c.Message().ReplyTo; replyTo != nil {
			if _, ok := pendingPerms.getTarget(replyTo.ID); ok {
				uuid, uuidOk := pendingPerms.getUUID(replyTo.ID)
				if !uuidOk {
					uuid, uuidOk = pendingFiles.get(replyTo.ID)
				}
				sugLabels := parseSuggestionLabels(pendingPerms.getSuggestions(replyTo.ID))
				d := permDecision{
					Behavior: "deny",
					Message:  "User provided voice input: " + text,
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
						injector.InjectText(target, text)
					}
					logger.Info(fmt.Sprintf("Permission denied via voice reply, text injected: msg_id=%d target=%s uuid=%s text=%s", replyTo.ID, injector.FormatTarget(target), uuid, truncateStr(text, 200)))
					sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
					if sentMsg != nil {
						reactAndTrack(bot, c.Message().Chat, sentMsg, injector.FormatTarget(target))
					}
				}
				return nil
			}
			if entry, ok := toolNotifs.get(replyTo.ID); ok {
				switch entry.toolName {
				case "AskUserQuestion":
					if entry.resolved {
						logger.Info(fmt.Sprintf("AskUserQuestion voice reply: already resolved: msg_id=%d", replyTo.ID))
						return c.Reply("❌ Question already answered.")
					}
					uuid, ok := pendingFiles.get(replyTo.ID)
					if !ok {
						logger.Info(fmt.Sprintf("AskUserQuestion voice reply: pending file not found: msg_id=%d", replyTo.ID))
						return c.Reply("❌ Question expired.")
					}
					path := filepath.Join(pendingDir(), uuid+".json")
					pf, err := readPendingFile(path)
					if err != nil {
						logger.Error(fmt.Sprintf("Failed to read pending file: %v", err))
						return c.Reply("❌ Failed to read pending file.")
					}
					answers := make(map[string]string)
					if len(entry.questions) > 0 {
						answers[entry.questions[0].questionText] = text
					}
					ccOutput := buildAskCCOutput(pf.Payload, answers)
					if err := writePendingAnswer(uuid, ccOutput); err != nil {
						logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
						return c.Reply("❌ Failed to save answer.")
					}
					toolNotifs.markResolved(replyTo.ID)
					logger.Info(fmt.Sprintf("AskUserQuestion custom voice via reply: msg_id=%d uuid=%s text=%s", replyTo.ID, uuid, truncateStr(text, 200)))
					editChat := &tele.Chat{ID: entry.chatID}
					editMsg := &tele.Message{ID: replyTo.ID, Chat: editChat}
					bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Voice answer"))
					sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
					if sentMsg != nil {
						reactAndTrack(bot, c.Message().Chat, sentMsg, entry.tmuxTarget)
					}
					return nil
				}
			}
		}
		target, err := resolveReplyTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info found in the original message.")
		}
		if err := injector.InjectText(target, text); err != nil {
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Injected voice transcription to %s text=%s", injector.FormatTarget(target), truncateStr(text, 200)))
		sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
		if sentMsg != nil {
			if err := bot.React(c.Message().Chat, sentMsg, tele.ReactionOptions{
				Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
			}); err != nil {
				logger.Debug(fmt.Sprintf("React failed: %v", err))
			} else {
				tmuxStr := injector.FormatTarget(target)
				reactionTracker.record(tmuxStr, c.Chat().ID, sentMsg.ID)
			}
		}
		return nil
	})
}

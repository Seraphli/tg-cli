package cmd

import (
	"fmt"
	"os"
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

// resolveGroupTarget finds the unique bound tmux target for a group/topic chat.
// Uses NameRouteMap: finds name routes matching (chatID, topicID), then finds active session by name.
// Pass topicID=0 for non-topic group chats.
func resolveGroupTarget(chatID int64, topicID int) (string, injector.TmuxTarget, error) {
	creds, _ := config.LoadCredentials()
	var targets []string
	for key, route := range creds.NameRouteMap {
		if route.ChatID != chatID {
			continue
		}
		if route.TopicID != topicID {
			continue
		}
		// Try as name first
		var tmuxTarget string
		info := sessionState.findByName(key)
		if info != nil {
			tmuxTarget = info.tmuxTarget
		} else {
			// Try as session ID
			sessionInfo := sessionState.findInfoByID(key)
			if sessionInfo != nil {
				tmuxTarget = sessionInfo.tmuxTarget
			}
		}
		if tmuxTarget == "" {
			continue
		}
		target, err := injector.ParseTarget(tmuxTarget)
		if err != nil || !injector.SessionExists(target) {
			continue
		}
		normalized := notify.FormatPaneID(tmuxTarget)
		found := false
		for _, t := range targets {
			if notify.FormatPaneID(t) == normalized {
				found = true
				break
			}
		}
		if !found {
			targets = append(targets, tmuxTarget)
		}
	}
	if len(targets) == 0 && topicID != 0 {
		// Fallback: retry with topicID=0 for reply threads in non-topic supergroups
		for key, route := range creds.NameRouteMap {
			if route.ChatID != chatID || route.TopicID != 0 {
				continue
			}
			var tmuxTarget string
			info := sessionState.findByName(key)
			if info != nil {
				tmuxTarget = info.tmuxTarget
			} else {
				sessionInfo := sessionState.findInfoByID(key)
				if sessionInfo != nil {
					tmuxTarget = sessionInfo.tmuxTarget
				}
			}
			if tmuxTarget == "" {
				continue
			}
			target, err := injector.ParseTarget(tmuxTarget)
			if err != nil || !injector.SessionExists(target) {
				continue
			}
			normalized := notify.FormatPaneID(tmuxTarget)
			found := false
			for _, t := range targets {
				if notify.FormatPaneID(t) == normalized {
					found = true
					break
				}
			}
			if !found {
				targets = append(targets, tmuxTarget)
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

// downloadTGFile downloads a Telegram file to /tmp/tg-cli/uploads/
func downloadTGFile(bot *tele.Bot, fileID, fileName string) (string, error) {
	dir := "/tmp/tg-cli/uploads"
	os.MkdirAll(dir, 0755)
	file, err := bot.FileByID(fileID)
	if err != nil {
		return "", fmt.Errorf("file lookup failed: %w", err)
	}
	destPath := filepath.Join(dir, fileName)
	if err := bot.Download(&file, destPath); err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	logger.Info(fmt.Sprintf("File downloaded: %s -> %s", fileID, destPath))
	return destPath, nil
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
	// Merge mode: buffer content and return
	key := mergeKey(c.Chat().ID)
	if buf := mergeBuffers.get(key); buf != nil {
		content := text
		if isVoice {
			content = voicePrefix + " " + text
		}
		items, notifyMsgID, chatID, _ := mergeBuffers.addAndGetInfo(key, content)
		logger.Info(fmt.Sprintf("Merge add: key=%s items=%d text=%s", key, len(items), truncateStr(content, 200)))
		if isVoice {
			bot.Reply(c.Message(), voicePrefix+" "+text)
		}
		// Update notification with content preview
		if notifyMsgID != 0 {
			preview := buildMergeNotifyText(fmt.Sprintf("📝 Collecting (%d messages)", len(items)), items)
			menu := &tele.ReplyMarkup{}
			btnSubmit := menu.Data("📤 Submit", "merge_submit")
			btnCancel := menu.Data("❌ Cancel", "merge_cancel")
			menu.Inline(menu.Row(btnSubmit, btnCancel))
			editMsg := &tele.Message{ID: notifyMsgID, Chat: &tele.Chat{ID: chatID}}
			retryEdit(bot, editMsg, preview, menu, tele.ModeHTML)
		}
		return nil
	}
	text = restoreSpoilers(text, c.Message().Entities)
	injectionText := text
	if isVoice {
		injectionText = voicePrefix + " " + text
	}
	// Skip forwarded messages globally (not just in group path)
	if c.Message().OriginalUnixtime != 0 {
		return nil
	}
	// sendFeedback sends the appropriate feedback message for a group or reply context
	sendFeedback := func(tmuxTarget string) {
		recordPending(tmuxTarget, c.Message().Chat.ID, c.Message().ID)
		if isVoice {
			bot.Reply(c.Message(), voicePrefix+" "+text)
		}
	}

	// Group path: no reply (or reply is just the topic anchor), group/supergroup chat
	// isTopicAnchor: ReplyTo exists but points to the topic anchor message (same ID as ThreadID),
	// meaning the message was sent directly in a topic, not as a real reply.
	isTopicAnchor := c.Message().ReplyTo != nil && c.Message().ReplyTo.ID == c.Message().ThreadID
	if c.Message().ReplyTo == nil || isTopicAnchor {
		if c.Chat().Type != "group" && c.Chat().Type != "supergroup" {
			return nil
		}
		tmuxStr, _, err := resolveGroupTarget(c.Chat().ID, c.Message().ThreadID)
		if err != nil {
			// Fallback: try extracting target from ReplyTo when in reply thread
			if isTopicAnchor {
				fallbackTarget, fbErr := resolveReplyTarget(c.Message().ReplyTo.Text)
				if fbErr == nil {
					fbTmuxStr := injector.FormatTarget(fallbackTarget)
					if !checkSessionAlive(fbTmuxStr, bot) {
						return c.Reply("⚠️ Session is no longer running. Tmux route has been unbound.")
					}
					sendFeedback(fbTmuxStr)
					if err := safeInjectText(bot, fbTmuxStr, injectionText); err != nil {
						return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
					}
					logger.Info(fmt.Sprintf("Group quick reply (fallback): target=%s voice=%v text=%s", fbTmuxStr, isVoice, truncateStr(text, 200)))
					return nil
				}
			}
			if err.Error() == "no targets bound" {
				return nil
			}
			if err.Error() == "multiple sessions bound" {
				return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
			}
			return c.Reply("❌ tmux session not found.")
		}
		if !checkSessionAlive(tmuxStr, bot) {
			return c.Reply("⚠️ Session is no longer running. Tmux route has been unbound.")
		}
		sendFeedback(tmuxStr)
		if err := safeInjectText(bot, tmuxStr, injectionText); err != nil {
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
		targetPtr, err := extractTmuxTarget(replyTo.Text)
		if err == nil && targetPtr != nil {
			tmuxStr := injector.FormatTarget(*targetPtr)
			if injector.SessionExists(*targetPtr) {
				// Cancel PermissionRequest explicitly, then inject
				if permMsgID, found := pendingPerms.findByTmuxTarget(tmuxStr); found {
					doCancelPerm(bot, permMsgID)
					injector.SendKeys(*targetPtr, "Escape")
					for i := 0; i < 20; i++ {
						time.Sleep(500 * time.Millisecond)
						if !isSessionBusy(tmuxStr) {
							break
						}
					}
				}
				sendFeedback(tmuxStr)
				injector.InjectText(*targetPtr, injectionText)
				logger.Info(fmt.Sprintf("Permission cancelled via reply + inject: msg_id=%d target=%s voice=%v text=%s", replyTo.ID, tmuxStr, isVoice, truncateStr(text, 200)))
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
			sendFeedback(entry.tmuxTarget)
			if err := safeInjectText(bot, entry.tmuxTarget, injectionText); err != nil {
				logger.Error(fmt.Sprintf("AskUserQuestion safeInject failed: %v", err))
			}
			logger.Info(fmt.Sprintf("AskUserQuestion reply via safeInject: msg_id=%d target=%s voice=%v text=%s", replyTo.ID, entry.tmuxTarget, isVoice, truncateStr(text, 200)))
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
	sendFeedback(injector.FormatTarget(target))
	// Prepend quoted original message for context
	if replyTo.Text != "" {
		var lines []string
		for _, line := range strings.Split(replyTo.Text, "\n") {
			lines = append(lines, "> "+line)
		}
		injectionText = strings.Join(lines, "\n") + "\n\n" + injectionText
	}
	if err := safeInjectText(bot, injector.FormatTarget(target), injectionText); err != nil {
		return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
	}
	logger.Info(fmt.Sprintf("Injected reply to %s voice=%v text=%s", injector.FormatTarget(target), isVoice, truncateStr(text, 200)))
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
			return c.Reply("Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
				isCmd := strings.HasPrefix(c.Message().Text, "/bot_perm_") ||
					c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") ||
					c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@") ||
					c.Message().Text == "/stop" || strings.HasPrefix(c.Message().Text, "/stop@")
				if isCmd {
					_, target, err := resolveGroupTarget(c.Chat().ID, c.Message().ThreadID)
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
			if c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@") ||
			c.Message().Text == "/stop" || strings.HasPrefix(c.Message().Text, "/stop@") {
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
			return c.Reply("Not paired. Use /bot_pair first.")
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

	bot.Handle(tele.OnDocument, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("Not paired. Use /bot_pair first.")
		}
		doc := c.Message().Document
		localPath, err := downloadTGFile(bot, doc.FileID, doc.FileName)
		if err != nil {
			logger.Error(fmt.Sprintf("Document download failed: %v", err))
			return c.Reply(fmt.Sprintf("❌ Download failed: %v", err))
		}
		content := localPath
		if c.Message().Caption != "" {
			content = c.Message().Caption + "\n" + localPath
		}
		return processUserInput(c, bot, content, false, voicePrefix)
	})

	bot.Handle(tele.OnPhoto, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Reply("Not paired. Use /bot_pair first.")
		}
		photo := c.Message().Photo
		fileName := fmt.Sprintf("photo_%d.jpg", time.Now().UnixNano())
		localPath, err := downloadTGFile(bot, photo.FileID, fileName)
		if err != nil {
			logger.Error(fmt.Sprintf("Photo download failed: %v", err))
			return c.Reply(fmt.Sprintf("❌ Download failed: %v", err))
		}
		content := localPath
		if c.Message().Caption != "" {
			content = c.Message().Caption + "\n" + localPath
		}
		return processUserInput(c, bot, content, false, voicePrefix)
	})
}

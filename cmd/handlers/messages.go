package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	"github.com/Seraphli/tg-cli/internal/voice"
	tele "gopkg.in/telebot.v3"
)

// resolveGroupTarget finds the unique bound tmux target for a group/topic chat.
func resolveGroupTarget(bs *types.BotState, chatID int64, topicID int) (string, injector.TmuxTarget, error) {
	creds, _ := config.LoadCredentials()
	var targets []string
	for key, route := range creds.NameRouteMap {
		if route.ChatID != chatID {
			continue
		}
		if route.TopicID != topicID {
			continue
		}
		var tmuxTarget string
		info := bs.SessionState.FindByName(key)
		if info != nil {
			tmuxTarget = info.TmuxTarget
		} else {
			sessionInfo := bs.SessionState.FindInfoByID(key)
			if sessionInfo != nil {
				tmuxTarget = sessionInfo.TmuxTarget
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
		for key, route := range creds.NameRouteMap {
			if route.ChatID != chatID || route.TopicID != 0 {
				continue
			}
			var tmuxTarget string
			info := bs.SessionState.FindByName(key)
			if info != nil {
				tmuxTarget = info.TmuxTarget
			} else {
				sessionInfo := bs.SessionState.FindInfoByID(key)
				if sessionInfo != nil {
					tmuxTarget = sessionInfo.TmuxTarget
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

// transcribeVoice downloads and transcribes a voice message.
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
	logger.Info(fmt.Sprintf("Voice transcribed: engine=%s text=%s", engine, helpers.TruncateStr(text, 200)))
	return text, nil
}

// downloadTGFile downloads a Telegram file to /tmp/tg-cli/uploads/.
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
func resolveReplyTarget(bs *types.BotState, replyText string) (injector.TmuxTarget, error) {
	if replyText == "" {
		logger.Debug("resolveReplyTarget: empty replyText")
		return injector.TmuxTarget{}, fmt.Errorf("no target found")
	}
	targetPtr, err := helpers.ExtractTmuxTargetFromText(replyText)
	if err != nil {
		logger.Debug(fmt.Sprintf("resolveReplyTarget: ExtractTmuxTargetFromText failed: %v text=%s", err, helpers.TruncateStr(replyText, 100)))
		return injector.TmuxTarget{}, fmt.Errorf("no target found")
	}
	target := *targetPtr
	if !injector.SessionExists(target) {
		return injector.TmuxTarget{}, fmt.Errorf("session not found")
	}
	tmuxStr := injector.FormatTarget(target)
	if _, found := bs.SessionState.FindByTarget(tmuxStr); !found {
		cwd := helpers.GetPaneCWD(target.PaneID)
		if cwd != "" {
			bs.SessionState.Add("recovered-"+target.PaneID, tmuxStr, cwd, "")
			logger.Info(fmt.Sprintf("Session recovered from reply: tmux=%s cwd=%s", tmuxStr, cwd))
		}
	}
	return target, nil
}

// InjectMessage handles image + text injection to a target tmux pane.
func InjectMessage(bs *types.BotState, tmuxTarget string, text string, imagePath string) error {
	if imagePath != "" && text == "" {
		return safeInjectImageText(bs, tmuxTarget, imagePath)
	}
	if imagePath != "" {
		if err := safeInjectImageText(bs, tmuxTarget, imagePath, false); err != nil {
			return fmt.Errorf("image inject failed: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
		target, parseErr := injector.ParseTarget(tmuxTarget)
		if parseErr != nil {
			return fmt.Errorf("parse target: %w", parseErr)
		}
		return injector.InjectTextAppend(target, "\n"+text)
	}
	return safeInjectText(bs, tmuxTarget, text)
}

// safeInjectImageText wraps SafeInjectText for image paths, using "[Image" as alt snippet.
func safeInjectImageText(bs *types.BotState, tmuxTarget string, text string, submit ...bool) error {
	p := helpers.SafeInjectTextParams{
		Bot:              bs.Bot,
		ToolNotifs:       bs.ToolNotifs,
		PendingFiles:     bs.PendingFiles,
		PendingPerms:     bs.PendingPerms,
		InjectQueue:      bs.InjectQueue,
		InjectConfirm:    bs.InjectConfirm,
		StopCooldown:     bs.StopCooldown,
		ReactionTracker:  bs.ReactionTracker,
		SessionState:     bs.SessionState,
		HookSessionLocks: &bs.HookSessionLocks,
		SessionEvents:    bs.SessionEvents,
		ResolveChat: func(target string) (*tele.Chat, string, int) {
			return helpers.ResolveChat(bs.SessionState, target)
		},
		FormatPaneID: notify.FormatPaneID,
		AltSnippet:   "[Image",
	}
	return helpers.SafeInjectText(p, tmuxTarget, text, submit...)
}

// processUserInput handles shared logic for OnText and OnVoice after routing.
func processUserInput(bs *types.BotState, c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
	imgPath := ""
	if len(imagePath) > 0 {
		imgPath = imagePath[0]
	}
	key := stores.MergeKey(c.Chat().ID)
	if buf := bs.MergeBuffers.Get(key); buf != nil {
		content := text
		if isVoice {
			content = voicePrefix + " " + text
		}
		items, notifyMsgID, chatID, _ := bs.MergeBuffers.AddAndGetInfo(key, content)
		logger.Info(fmt.Sprintf("Merge add: key=%s items=%d text=%s", key, len(items), helpers.TruncateStr(content, 200)))
		if isVoice {
			bot.Reply(c.Message(), voicePrefix+" "+text)
		}
		if notifyMsgID != 0 {
			preview := BuildMergeNotifyText(fmt.Sprintf("📝 Collecting (%d messages)", len(items)), items)
			menu := &tele.ReplyMarkup{}
			btnSubmit := menu.Data("📤 Submit", "merge_submit")
			btnCancel := menu.Data("❌ Cancel", "merge_cancel")
			menu.Inline(menu.Row(btnSubmit, btnCancel))
			editMsg := &tele.Message{ID: notifyMsgID, Chat: &tele.Chat{ID: chatID}}
			helpers.RetryEdit(bot, editMsg, preview, menu, tele.ModeHTML)
		}
		return nil
	}
	text = restoreSpoilers(text, c.Message().Entities)
	injectionText := text
	if isVoice {
		injectionText = voicePrefix + " " + text
	}
	if c.Message().OriginalUnixtime != 0 {
		return nil
	}
	sendFeedback := func(tmuxTarget string) {
		recordPending(bs, tmuxTarget, c.Message().Chat.ID, c.Message().ID)
		if isVoice {
			bot.Reply(c.Message(), voicePrefix+" "+text)
		}
	}
	if c.Message().ReplyTo == nil {
		if c.Chat().Type != "group" && c.Chat().Type != "supergroup" {
			return nil
		}
		tmuxStr, _, err := resolveGroupTarget(bs, c.Chat().ID, c.Message().ThreadID)
		if err != nil {
			if err.Error() == "no targets bound" {
				return nil
			}
			if err.Error() == "multiple sessions bound" {
				return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
			}
			return c.Reply("❌ tmux session not found.")
		}
		if !checkSessionAlive(bs, tmuxStr) {
			return c.Reply("⚠️ Session is no longer running. Tmux route has been unbound.")
		}
		sendFeedback(tmuxStr)
		if err := InjectMessage(bs, tmuxStr, injectionText, imgPath); err != nil {
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Group quick reply: target=%s voice=%v text=%s", tmuxStr, isVoice, helpers.TruncateStr(text, 200)))
		return nil
	}
	replyTo := c.Message().ReplyTo
	if val, ok := bs.LaunchPending.Load(replyTo.ID); ok {
		state := val.(*LaunchState)
		customValue := strings.TrimSpace(c.Text())
		bs.LaunchPending.Delete(replyTo.ID)
		switch state.Step {
		case "session":
			state.SessionName = customValue
			c.Bot().Edit(c.Message().ReplyTo, fmt.Sprintf("📦 Session name\n✅ %s", state.SessionName))
			if state.WorkDir == "" {
				AskWorkDir(bs, c.Bot(), state.ChatID, state)
			} else {
				go ExecuteLaunch(bs, c.Bot(), state.ChatID, state)
			}
		case "workdir":
			if strings.HasPrefix(customValue, "~") {
				home, _ := os.UserHomeDir()
				customValue = home + customValue[1:]
			}
			state.WorkDir = customValue
			c.Bot().Edit(c.Message().ReplyTo, fmt.Sprintf("📂 Working directory\n✅ %s", state.WorkDir))
			go ExecuteLaunch(bs, c.Bot(), state.ChatID, state)
		}
		return nil
	}
	if _, ok := bs.PendingPerms.GetTarget(replyTo.ID); ok {
		targetPtr, err := helpers.ExtractTmuxTargetFromText(replyTo.Text)
		if err == nil && targetPtr != nil {
			tmuxStr := injector.FormatTarget(*targetPtr)
			if injector.SessionExists(*targetPtr) {
				if permMsgID, found := bs.PendingPerms.FindByTmuxTarget(tmuxStr); found {
					doCancelPerm(bs, permMsgID)
					injector.SendKeys(*targetPtr, "Escape")
					for i := 0; i < 20; i++ {
						time.Sleep(500 * time.Millisecond)
						if !helpers.IsSessionRunning(tmuxStr) {
							break
						}
					}
				}
				sendFeedback(tmuxStr)
				InjectMessage(bs, injector.FormatTarget(*targetPtr), injectionText, imgPath)
				logger.Info(fmt.Sprintf("Permission cancelled via reply + inject: msg_id=%d target=%s voice=%v text=%s", replyTo.ID, tmuxStr, isVoice, helpers.TruncateStr(text, 200)))
			}
		}
		return nil
	}
	if entry, ok := bs.ToolNotifs.Get(replyTo.ID); ok {
		target, err := injector.ParseTarget(entry.TmuxTarget)
		if err != nil || !injector.SessionExists(target) {
			return c.Reply("❌ tmux session not found.")
		}
		switch entry.ToolName {
		case "AskUserQuestion":
			sendFeedback(entry.TmuxTarget)
			if err := InjectMessage(bs, entry.TmuxTarget, injectionText, imgPath); err != nil {
				logger.Error(fmt.Sprintf("AskUserQuestion safeInject failed: %v", err))
			}
			logger.Info(fmt.Sprintf("AskUserQuestion reply via safeInject: msg_id=%d target=%s voice=%v text=%s", replyTo.ID, entry.TmuxTarget, isVoice, helpers.TruncateStr(text, 200)))
			return nil
		}
		logger.Info(fmt.Sprintf("Tool reply: tool=%s msg_id=%d target=%s voice=%v text=%s", entry.ToolName, replyTo.ID, entry.TmuxTarget, isVoice, helpers.TruncateStr(text, 200)))
		recordPending(bs, entry.TmuxTarget, c.Message().Chat.ID, c.Message().ID)
		return nil
	}
	target, err := resolveReplyTarget(bs, c.Message().ReplyTo.Text)
	if err != nil {
		return c.Reply("❌ No tmux session info found in the original message.")
	}
	if !checkSessionAlive(bs, injector.FormatTarget(target)) {
		return c.Reply("⚠️ Session is no longer running. Tmux route has been unbound.")
	}
	sendFeedback(injector.FormatTarget(target))
	if replyTo.Text != "" && (c.Chat().Type == "group" || c.Chat().Type == "supergroup") {
		var lines []string
		for _, line := range strings.Split(replyTo.Text, "\n") {
			lines = append(lines, "> "+line)
		}
		injectionText = strings.Join(lines, "\n") + "\n\n" + injectionText
	}
	if err := InjectMessage(bs, injector.FormatTarget(target), injectionText, imgPath); err != nil {
		return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
	}
	logger.Info(fmt.Sprintf("Injected reply to %s voice=%v text=%s", injector.FormatTarget(target), isVoice, helpers.TruncateStr(text, 200)))
	return nil
}

// RegisterMessageHandlers registers OnText and OnVoice handlers.
func RegisterMessageHandlers(bs *types.BotState) {
	bot := bs.Bot
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
					c.Message().Text == "/p" || strings.HasPrefix(c.Message().Text, "/p@") ||
					c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@") ||
					c.Message().Text == "/stop" || strings.HasPrefix(c.Message().Text, "/stop@") ||
					c.Message().Text == "/t" || strings.HasPrefix(c.Message().Text, "/t@") ||
					c.Message().Text == "/reload" || strings.HasPrefix(c.Message().Text, "/reload@") ||
					c.Message().Text == "/r" || strings.HasPrefix(c.Message().Text, "/r@")
				if isCmd {
					_, target, err := resolveGroupTarget(bs, c.Chat().ID, c.Message().ThreadID)
					if err != nil {
						if err.Error() == "multiple sessions bound" {
							return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
						}
						return c.Reply("❌ tmux session not found.")
					}
					if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
						return handlePermCommand(c, target)
					}
					if c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") ||
						c.Message().Text == "/p" || strings.HasPrefix(c.Message().Text, "/p@") {
						return handleCaptureCommand(bs, c, target)
					}
					if c.Message().Text == "/reload" || strings.HasPrefix(c.Message().Text, "/reload@") ||
						c.Message().Text == "/r" || strings.HasPrefix(c.Message().Text, "/r@") {
						return handleReloadCommand(bs, c, target)
					}
					return handleEscapeCommand(c, target)
				}
			}
		} else {
			if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo.Text)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handlePermCommand(c, target)
			}
			if c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") ||
				c.Message().Text == "/p" || strings.HasPrefix(c.Message().Text, "/p@") {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo.Text)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleCaptureCommand(bs, c, target)
			}
			if c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@") ||
				c.Message().Text == "/stop" || strings.HasPrefix(c.Message().Text, "/stop@") ||
				c.Message().Text == "/t" || strings.HasPrefix(c.Message().Text, "/t@") {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo.Text)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleEscapeCommand(c, target)
			}
			if c.Message().Text == "/reload" || strings.HasPrefix(c.Message().Text, "/reload@") ||
				c.Message().Text == "/r" || strings.HasPrefix(c.Message().Text, "/r@") {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo.Text)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleReloadCommand(bs, c, target)
			}
		}
		return processUserInput(bs, c, bot, c.Message().Text, false, voicePrefix)
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
			return processUserInput(bs, c, bot, text, true, voicePrefix)
		}
		text, err := transcribeVoice(bot, c.Message().Voice.FileID)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ %v", err))
		}
		if text == "" {
			return c.Reply("❌ Transcription produced empty text.")
		}
		return processUserInput(bs, c, bot, text, true, voicePrefix)
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
		caption := c.Message().Caption
		return processUserInput(bs, c, bot, caption, false, voicePrefix, localPath)
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
		caption := c.Message().Caption
		return processUserInput(bs, c, bot, caption, false, voicePrefix, localPath)
	})
}

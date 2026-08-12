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
	logger.Info(fmt.Sprintf("Voice transcribed: engine=%s text=%s", engine, text))
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

// resolveReplyTarget extracts and validates a tmux target from a reply-to notification message (its
// 📟 line, or the Pages msg_id fallback for rich notifications whose .Text is empty).
func resolveReplyTarget(bs *types.BotState, replyTo *tele.Message) (injector.TmuxTarget, error) {
	targetPtr, err := extractReplyTarget(bs, replyTo)
	if err != nil || targetPtr == nil {
		logger.Debug(fmt.Sprintf("resolveReplyTarget: no target: %v", err))
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
		PendingWait:      bs.PendingWait,
		InjectQueue:      bs.InjectQueue,
		InjectConfirm:    bs.InjectConfirm,
		StopCooldown:     bs.StopCooldown,
		ReactionTracker:  bs.ReactionTracker,
		SessionState:     bs.SessionState,
		HookRunning:      bs.HookRunning,
		HookSessionLocks: &bs.HookSessionLocks,
		SessionEvents:    bs.SessionEvents,
		PendingMsgStore:  bs.PendingMsgStore,
		ResolveChat: func(target string) (*tele.Chat, string, int) {
			return helpers.ResolveChat(bs.SessionState, target)
		},
		FormatPaneID: notify.FormatPaneID,
		AltSnippet:   "[Image",
	}
	return helpers.SafeInjectText(p, tmuxTarget, text, submit...)
}

// processUserInput handles shared logic for OnText and OnVoice after routing.
// stripBotMention removes a trailing @<botUsername> that Telegram appends to a slash-command's first token
// in groups (e.g. "/reload@mybot arg" -> "/reload arg"), so the literal command reaches the pane. Non-slash
// text and text without the suffix are returned unchanged.
func stripBotMention(text, botUsername string) string {
	if botUsername == "" || !strings.HasPrefix(text, "/") {
		return text
	}
	suffix := "@" + botUsername
	if i := strings.IndexAny(text, " \n\t"); i >= 0 {
		return strings.TrimSuffix(text[:i], suffix) + text[i:]
	}
	return strings.TrimSuffix(text, suffix)
}

// isRestartCommand reports whether text is the /restart command or one of its aliases (/rs, /r), with or
// without the @botname mention Telegram appends to slash commands in groups. It is the single source of
// truth for which slash commands the bot intercepts as a restart. Round-4 Item 6 renamed the old /reload
// to /restart and un-intercepted /reload so the literal reaches the pane for the backend's own hot reload —
// so /reload (and /reload@bot) MUST NOT match here.
func isRestartCommand(text string) bool {
	return text == "/restart" || strings.HasPrefix(text, "/restart@") ||
		text == "/rs" || strings.HasPrefix(text, "/rs@") ||
		text == "/r" || strings.HasPrefix(text, "/r@")
}

func processUserInput(bs *types.BotState, c tele.Context, bot *tele.Bot, text string, isVoice bool, voicePrefix string, imagePath ...string) error {
	imgPath := ""
	if len(imagePath) > 0 {
		imgPath = imagePath[0]
	}
	// Strip the @botname suffix Telegram appends to a slash-command in groups, so the literal command
	// (e.g. /reload — now un-intercepted and owned by the backend's own hot reload) reaches the pane.
	if bot != nil && bot.Me != nil {
		text = stripBotMention(text, bot.Me.Username)
	}
	key := stores.MergeKey(c.Chat().ID)
	if buf := bs.MergeBuffers.Get(key); buf != nil {
		content := text
		if isVoice {
			content = voicePrefix + " " + text
		}
		items, notifyMsgID, chatID, _ := bs.MergeBuffers.AddAndGetInfo(key, content)
		logger.Info(fmt.Sprintf("Merge add: key=%s items=%d text=%s", key, len(items), content))
		if isVoice {
			c.Reply(voicePrefix + " " + text)
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
			c.Reply(voicePrefix + " " + text)
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
		// Check for @Name prefix to initiate @ channel
		if strings.HasPrefix(text, "@") {
			parts := strings.SplitN(text[1:], " ", 2)
			if len(parts) >= 1 && parts[0] != "" {
				targetName := parts[0]
				initiatorInfo := bs.SessionState.FindInfoByTarget(tmuxStr)
				if initiatorInfo != nil && initiatorInfo.Name != "" {
					targetInfo := bs.SessionState.FindByName(targetName)
					if targetInfo != nil {
						go openAtChannel(bs, initiatorInfo.Name, targetName, 3, 0, text)
						return nil
					}
				}
			}
		}
		// Forward TG message to @ channel targets (initiator auto-forward)
		detectedAtTarget := ""
		initiatorInfo := bs.SessionState.FindInfoByTarget(tmuxStr)
		if initiatorInfo != nil && initiatorInfo.Name != "" {
			atTargets := bs.AtChannels.GetTargets(initiatorInfo.Name)
			if len(atTargets) > 0 {
				cfg, _ := config.LoadAppConfig()
				displayName := cfg.DisplayName
				if displayName == "" {
					displayName = "User"
				}
				initiatorName := initiatorInfo.Name
				for _, peer := range atTargets {
					peerInfo := bs.SessionState.FindByName(peer)
					if peerInfo == nil {
						continue
					}
					recipient := initiatorName
					if detectedAtTarget != "" {
						recipient = detectedAtTarget
					}
					content := fmt.Sprintf("[%s → %s]: %s", displayName, recipient, injectionText)
					instructions := fmt.Sprintf("`%s`(user) sent a message to `%s`.",
						displayName, recipient)
					forwardMsg := helpers.BuildAtMsg(initiatorName, peer, instructions, helpers.BuildAtForwardContent("", content))
					go func(target, msg string) {
						safeInjectText(bs, target, msg)
						peerChat, _, peerTopicID := helpers.ResolveChat(bs.SessionState, target)
						if peerChat != nil {
							var notifyOpts []interface{}
							if peerTopicID > 0 {
								notifyOpts = append(notifyOpts, &tele.SendOptions{ThreadID: peerTopicID})
							}
							helpers.RetrySend(bs.Bot, peerChat, msg, notifyOpts...)
						}
					}(peerInfo.TmuxTarget, forwardMsg)
				}
			}
		}
		sendFeedback(tmuxStr)
		if err := InjectMessage(bs, tmuxStr, injectionText, imgPath); err != nil {
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Group quick reply: target=%s voice=%v text=%s", tmuxStr, isVoice, text))
		return nil
	}
	replyTo := c.Message().ReplyTo
	if val, ok := bs.SettingsMenuMsgs.Load(replyTo.ID); ok {
		if marker, isStr := val.(string); isStr && marker == "displayname" {
			newName := strings.TrimSpace(c.Text())
			if newName == "" {
				return c.Reply("❌ Name cannot be empty.")
			}
			cfg, err := config.LoadAppConfig()
			if err != nil {
				return c.Reply("❌ Failed to load config.")
			}
			cfg.DisplayName = newName
			if err := config.SaveAppConfig(cfg); err != nil {
				return c.Reply("❌ Failed to save config.")
			}
			bs.SettingsMenuMsgs.Delete(replyTo.ID)
			bot.Edit(c.Message().ReplyTo, fmt.Sprintf("👤 <b>Display Name</b>\n✅ Set to: %s", newName), tele.ModeHTML)
			logger.Info(fmt.Sprintf("Display name set: %s", newName))
			return nil
		}
	}
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
	// Use FindByMsgIDSnapshot — safe because replyTo.ID is a real TG message ID
	if snap, ok := bs.PendingWait.FindByMsgIDSnapshot(replyTo.ID); ok {
		switch snap.ToolName {
		default:
			// Cancel perm via DoCancelPerm (msgID-based — safe for TG message paths, any non-AskQ tool is a PermReq)
			doCancelPerm(bs, replyTo.ID)
			targetPtr, err := extractReplyTarget(bs, replyTo)
			if err == nil && targetPtr != nil {
				tmuxStr := injector.FormatTarget(*targetPtr)
				if injector.SessionExists(*targetPtr) {
					for i := 0; i < 20; i++ {
						time.Sleep(500 * time.Millisecond)
						if !helpers.IsSessionRunning(bs.HookRunning, tmuxStr) {
							break
						}
					}
					sendFeedback(tmuxStr)
					InjectMessage(bs, tmuxStr, injectionText, imgPath)
					logger.Info(fmt.Sprintf("Permission cancelled via reply + inject: msg_id=%d target=%s voice=%v text=%s", replyTo.ID, tmuxStr, isVoice, text))
				}
			}
			return nil
		case "AskUserQuestion":
			target, err := injector.ParseTarget(snap.TmuxTarget)
			if err != nil || !injector.SessionExists(target) {
				return c.Reply("❌ tmux session not found.")
			}
			sendFeedback(snap.TmuxTarget)
			if err := InjectMessage(bs, snap.TmuxTarget, injectionText, imgPath); err != nil {
				logger.Error(fmt.Sprintf("AskUserQuestion safeInject failed: %v", err))
			}
			logger.Info(fmt.Sprintf("AskUserQuestion reply via safeInject: msg_id=%d target=%s voice=%v text=%s", replyTo.ID, snap.TmuxTarget, isVoice, text))
			return nil
		}
	}
	target, err := resolveReplyTarget(bs, c.Message().ReplyTo)
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
	logger.Info(fmt.Sprintf("Injected reply to %s voice=%v text=%s", injector.FormatTarget(target), isVoice, text))
	return nil
}

func openAtChannel(bs *types.BotState, initiator, target string, rounds, lines int, message string) {
	initiatorInfo := bs.SessionState.FindByName(initiator)
	targetInfo := bs.SessionState.FindByName(target)
	if initiatorInfo == nil || targetInfo == nil {
		return
	}
	isNew := bs.AtChannels.Open(initiator, target)
	cfg, _ := config.LoadAppConfig()
	displayName := cfg.DisplayName
	if displayName == "" {
		displayName = "User"
	}
	if isNew {
		// Read context from initiator's transcript
		r := rounds
		if r == 0 {
			r = 3
		}
		contextStr, _ := helpers.ReadContextBlock(initiatorInfo.TranscriptPath, r, 0, initiatorInfo.Backend, initiator, displayName)
		// Replayed rounds stay READ-ONLY; the live message (or a no-message placeholder) is the LIVE TRIGGER.
		var triggerLine string
		if message != "" {
			triggerLine = fmt.Sprintf("[%s → %s]: %s", displayName, target, message)
		} else {
			triggerLine = fmt.Sprintf("@ channel opened by %s; no accompanying message.", displayName)
		}
		targetBody := helpers.BuildAtForwardContent(contextStr, triggerLine)
		initEndCmd := helpers.AtEndCommand(bs.ConfigDir, bs.Port, initiator, target)
		targetReplyCmd := helpers.AtReplyCommand(bs.ConfigDir, bs.Port, target, initiator)
		targetEndCmd := helpers.AtEndCommand(bs.ConfigDir, bs.Port, target, initiator)
		// Initiator pane: no-content message (just instructions)
		initiatorInstructions := fmt.Sprintf("`%s`(user) opened a channel to `%s`. `%s` will receive the last %d rounds of your conversation and see your ongoing output until the channel is closed. `%s` can reply to you via this channel. Run `%s` to close the channel.",
			displayName, target, target, r, target, initEndCmd)
		initiatorContent := ""
		if message != "" {
			initiatorContent = fmt.Sprintf("[%s → %s]: %s", displayName, target, message)
		}
		initiatorMsg := helpers.BuildAtMsg(initiator, target, initiatorInstructions, initiatorContent)
		safeInjectText(bs, initiatorInfo.TmuxTarget, initiatorMsg)
		// Target pane: full context with instructions
		targetInstructions := fmt.Sprintf("`%s`(user) mentioned you in `%s` session. The message below has two blocks: READ-ONLY PRIOR CONTEXT (lines prefixed `HISTORY> `) is replayed history for reference only — do NOT act on it; LIVE TRIGGER (lines prefixed `TRIGGER> `) is the live message directed to you. You will continue to receive updates from `%s` until the channel is closed. Run `%s` to reply, or `%s` to close the channel.",
			displayName, initiator, initiator, targetReplyCmd, targetEndCmd)
		targetMsg := helpers.BuildAtMsg(initiator, target, targetInstructions, targetBody)
		safeInjectText(bs, targetInfo.TmuxTarget, targetMsg)
		// TG notifications
		targetHeader := helpers.BuildAtHeader(initiator, target) + "\n---\n" + targetInstructions + "\n---\n"
		initiatorChat, _, initiatorTopicID := helpers.ResolveChat(bs.SessionState, initiatorInfo.TmuxTarget)
		if initiatorChat != nil {
			var opts []interface{}
			if initiatorTopicID > 0 {
				opts = append(opts, &tele.SendOptions{ThreadID: initiatorTopicID})
			}
			helpers.RetrySend(bs.Bot, initiatorChat, initiatorMsg, opts...)
		}
		targetChat, _, targetTopicID := helpers.ResolveChat(bs.SessionState, targetInfo.TmuxTarget)
		if targetChat != nil {
			var opts []interface{}
			if targetTopicID > 0 {
				opts = append(opts, &tele.SendOptions{ThreadID: targetTopicID})
			}
			helpers.SendPagedForward(bs.Bot, targetChat, targetHeader, targetBody, bs.Pages, "", opts...)
		}
		logger.Info(fmt.Sprintf("@ channel opened via TG: %s -> %s rounds=%d", initiator, target, rounds))
		// Auto-forward open message to other existing channels
		otherTargets := bs.AtChannels.GetTargets(initiator)
		for _, other := range otherTargets {
			if other == target {
				continue
			}
			otherInfo := bs.SessionState.FindByName(other)
			if otherInfo == nil {
				continue
			}
			fwdContent := initiatorContent
			if fwdContent == "" {
				fwdContent = fmt.Sprintf("[%s → %s]: @%s", displayName, target, target)
			}
			fwdInstr := fmt.Sprintf("`%s`(user) sent a message to `%s`.", displayName, target)
			fwdMsg := helpers.BuildAtMsg(initiator, other, fwdInstr, helpers.BuildAtForwardContent("", fwdContent))
			go func(t, msg string) {
				safeInjectText(bs, t, msg)
				otherChat, _, otherTopicID := helpers.ResolveChat(bs.SessionState, t)
				if otherChat != nil {
					var fwdOpts []interface{}
					if otherTopicID > 0 {
						fwdOpts = append(fwdOpts, &tele.SendOptions{ThreadID: otherTopicID})
					}
					helpers.RetrySend(bs.Bot, otherChat, msg, fwdOpts...)
				}
			}(otherInfo.TmuxTarget, fwdMsg)
		}
	} else if message != "" {
		// Channel already exists: forward message to target
		targetInstructions := fmt.Sprintf("`%s`(user) mentioned you in `%s` session.",
			displayName, initiator)
		initiatorContent := fmt.Sprintf("[%s → %s]: %s", displayName, target, message)
		initiatorMsg := helpers.BuildAtMsg(initiator, target, "", initiatorContent)
		content := fmt.Sprintf("[%s → %s]: %s", displayName, target, message)
		targetMsg := helpers.BuildAtMsg(initiator, target, targetInstructions, helpers.BuildAtForwardContent("", content))
		// Initiator TG only (no inject)
		initiatorChat, _, initiatorTopicID := helpers.ResolveChat(bs.SessionState, initiatorInfo.TmuxTarget)
		if initiatorChat != nil {
			var opts []interface{}
			if initiatorTopicID > 0 {
				opts = append(opts, &tele.SendOptions{ThreadID: initiatorTopicID})
			}
			helpers.RetrySend(bs.Bot, initiatorChat, initiatorMsg, opts...)
		}
		safeInjectText(bs, initiatorInfo.TmuxTarget, initiatorMsg)
		// Target: inject + TG
		safeInjectText(bs, targetInfo.TmuxTarget, targetMsg)
		targetChat, _, targetTopicID := helpers.ResolveChat(bs.SessionState, targetInfo.TmuxTarget)
		if targetChat != nil {
			var opts []interface{}
			if targetTopicID > 0 {
				opts = append(opts, &tele.SendOptions{ThreadID: targetTopicID})
			}
			helpers.RetrySend(bs.Bot, targetChat, targetMsg, opts...)
		}
		logger.Info(fmt.Sprintf("@ channel already open: %s -> %s message=%s", initiator, target, message))
		// Auto-forward existing channel message to other open channels
		otherTargets := bs.AtChannels.GetTargets(initiator)
		for _, other := range otherTargets {
			if other == target {
				continue
			}
			otherInfo := bs.SessionState.FindByName(other)
			if otherInfo == nil {
				continue
			}
			fwdContent := fmt.Sprintf("[%s → %s]: %s", displayName, target, message)
			fwdInstr := fmt.Sprintf("`%s`(user) sent a message to `%s`.", displayName, target)
			fwdMsg := helpers.BuildAtMsg(initiator, other, fwdInstr, helpers.BuildAtForwardContent("", fwdContent))
			go func(t, msg string) {
				safeInjectText(bs, t, msg)
				otherChat, _, otherTopicID := helpers.ResolveChat(bs.SessionState, t)
				if otherChat != nil {
					var fwdOpts []interface{}
					if otherTopicID > 0 {
						fwdOpts = append(fwdOpts, &tele.SendOptions{ThreadID: otherTopicID})
					}
					helpers.RetrySend(bs.Bot, otherChat, msg, fwdOpts...)
				}
			}(otherInfo.TmuxTarget, fwdMsg)
		}
	}
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
					isRestartCommand(c.Message().Text)
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
					if isRestartCommand(c.Message().Text) {
						return handleRestartCommand(bs, c, target)
					}
					return handleEscapeCommand(c, target)
				}
			}
		} else {
			if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handlePermCommand(c, target)
			}
			if c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") ||
				c.Message().Text == "/p" || strings.HasPrefix(c.Message().Text, "/p@") {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleCaptureCommand(bs, c, target)
			}
			if c.Message().Text == "/bot_escape" || strings.HasPrefix(c.Message().Text, "/bot_escape@") ||
				c.Message().Text == "/stop" || strings.HasPrefix(c.Message().Text, "/stop@") ||
				c.Message().Text == "/t" || strings.HasPrefix(c.Message().Text, "/t@") {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleEscapeCommand(c, target)
			}
			if isRestartCommand(c.Message().Text) {
				target, err := resolveReplyTarget(bs, c.Message().ReplyTo)
				if err != nil {
					return c.Reply("❌ No tmux session info found.")
				}
				return handleRestartCommand(bs, c, target)
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

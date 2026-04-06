package handlers

// helpers.go contains internal helper functions for the handlers package.
// These bridge cmd/helpers functions with BotState fields.

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// safeInjectText is a convenience wrapper that builds SafeInjectTextParams from BotState.
func safeInjectText(bs *types.BotState, tmuxTarget string, text string, submit ...bool) error {
	p := helpers.SafeInjectTextParams{
		Bot:             bs.Bot,
		ToolNotifs:      bs.ToolNotifs,
		PendingFiles:    bs.PendingFiles,
		PendingPerms:    bs.PendingPerms,
		InjectQueue:     bs.InjectQueue,
		InjectConfirm:   bs.InjectConfirm,
		StopCooldown:    bs.StopCooldown,
		ReactionTracker: bs.ReactionTracker,
		SessionState:    bs.SessionState,
		ResolveChat: func(target string) (*tele.Chat, string, int) {
			return helpers.ResolveChat(bs.SessionState, target)
		},
		FormatPaneID: notify.FormatPaneID,
	}
	return helpers.SafeInjectText(p, tmuxTarget, text, submit...)
}

// checkSessionAlive checks if a tmux session still exists; cleans up dead sessions.
func checkSessionAlive(bs *types.BotState, tmuxTarget string) bool {
	return helpers.CheckSessionAlive(tmuxTarget, func(t string) {
		helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, t)
	})
}

// recordPending records a message for later ✍ reaction when UserPromptSubmit fires.
func recordPending(bs *types.BotState, tmuxTarget string, chatID int64, msgID int) {
	helpers.RecordPending(bs.ReactionTracker, tmuxTarget, chatID, msgID)
}

// handleStalePending checks if a pending entry is stale.
func handleStalePending(bs *types.BotState, msgID int, uuid string) bool {
	return helpers.HandleStalePending(msgID, uuid, func(mid int, u string, reason string) {
		helpers.CleanupPendingState(bs.Bot, bs.ToolNotifs, bs.PendingPerms, bs.PendingFiles, mid, u, reason)
	})
}

// doCancelPerm cancels a PermissionRequest.
func doCancelPerm(bs *types.BotState, msgID int) string {
	return helpers.DoCancelPerm(bs.Bot, bs.PendingPerms, bs.PendingFiles, func(text string) (*injector.TmuxTarget, error) {
		return helpers.ExtractTmuxTargetFromText(text)
	}, msgID)
}

// doCancelAsk cancels an AskUserQuestion.
func doCancelAsk(bs *types.BotState, msgID int) string {
	return helpers.DoCancelAsk(bs.Bot, bs.ToolNotifs, bs.PendingFiles, func(text string) (*injector.TmuxTarget, error) {
		return helpers.ExtractTmuxTargetFromText(text)
	}, msgID)
}

// doDecidePerm resolves a PermissionRequest.
func doDecidePerm(bs *types.BotState, msgID int, decision string) (*stores.PermDecision, error) {
	return helpers.DoDecidePerm(
		bs.Bot,
		bs.PendingPerms,
		bs.PendingFiles,
		bs.ReactionTracker,
		func(target string) bool { return checkSessionAlive(bs, target) },
		func(text string) (*injector.TmuxTarget, error) { return helpers.ExtractTmuxTargetFromText(text) },
		msgID,
		decision,
	)
}

// doRespondAsk responds to AskUserQuestion.
func doRespondAsk(bs *types.BotState, msgID int, answers map[string]string, frozenLabel string) error {
	return helpers.DoRespondAsk(bs.Bot, bs.ToolNotifs, bs.PendingFiles, bs.ReactionTracker, msgID, answers, frozenLabel)
}

// doChatAsk handles chat mode for AskUserQuestion.
func doChatAsk(bs *types.BotState, msgID int) error {
	return helpers.DoChatAsk(bs.Bot, bs.ToolNotifs, bs.PendingFiles, bs.ReactionTracker, msgID)
}

// isSessionRunning checks if CC is running by reading tmux pane title.
func isSessionRunning(bs *types.BotState, tmuxTarget string) bool {
	return helpers.IsSessionRunning(bs.SessionState, tmuxTarget)
}

// isSessionBusy checks if CC is busy.
func isSessionBusy(bs *types.BotState, tmuxTarget string) bool {
	return helpers.IsSessionBusy(bs.SessionState, tmuxTarget)
}

// handlePermCommand handles /bot_perm_<cmd>.
func handlePermCommand(c tele.Context, target injector.TmuxTarget) error {
	cmd := strings.TrimPrefix(c.Message().Text, "/bot_perm_")
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}
	if cmd == "status" {
		mode, _, err := helpers.DetectPermMode(target)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Detect mode failed: %v", err))
		}
		return c.Reply(fmt.Sprintf("🔐 Current mode: %s", mode))
	}
	finalMode, err := helpers.SwitchPermMode(target, cmd)
	if err != nil {
		currentMode, _, detectErr := helpers.DetectPermMode(target)
		if detectErr == nil && currentMode == "question" {
			return c.Reply(fmt.Sprintf("❌ Switch failed: CC is currently in question state (AskUserQuestion dialog). Answer or cancel the question first.\nError: %v", err))
		}
		if detectErr == nil {
			return c.Reply(fmt.Sprintf("❌ Switch failed: current state is '%s'. Error: %v", currentMode, err))
		}
		return c.Reply(fmt.Sprintf("❌ Switch failed: %v", err))
	}
	return c.Reply(fmt.Sprintf("🔐 Switched to %s mode", finalMode))
}

// handleCaptureCommand handles /bot_capture.
func handleCaptureCommand(bs *types.BotState, c tele.Context, target injector.TmuxTarget) error {
	logger.Debug(fmt.Sprintf("handleCaptureCommand: target=%v", target))
	content, err := injector.CapturePane(target)
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Capture failed: %v", err))
	}
	logger.Debug(fmt.Sprintf("handleCaptureCommand: captured %d bytes", len(content)))
	if content == "" {
		return c.Reply("(empty pane)")
	}
	content = helpers.ShortenSeparators(content)
	const maxRunes = 4000
	chunks := helpers.SplitBody(content, maxRunes)
	logger.Debug(fmt.Sprintf("handleCaptureCommand: sending reply (%d chunks)", len(chunks)))
	if len(chunks) == 1 {
		_, err := helpers.RetrySend(c.Bot(), c.Chat(), chunks[0])
		return err
	}
	lastPage := len(chunks)
	kb := helpers.BuildPageKeyboard(lastPage, len(chunks))
	text := chunks[lastPage-1] + fmt.Sprintf("\n\n📄 %d/%d", lastPage, len(chunks))
	sent, err := helpers.RetrySend(c.Bot(), c.Chat(), text, kb)
	if err != nil {
		return err
	}
	bs.Pages.Store(sent.ID, "", &stores.PageEntry{
		Chunks:   chunks,
		PermRows: []tele.Row{},
		RawMode:  true,
	})
	return nil
}

// handleEscapeCommand handles /bot_escape.
func handleEscapeCommand(c tele.Context, target injector.TmuxTarget) error {
	if err := injector.SendKeys(target, "Escape"); err != nil {
		return c.Reply(fmt.Sprintf("❌ Escape failed: %v", err))
	}
	return c.Reply("⏹ Escape sent")
}

// checkAllSessionVersions checks all active sessions for version updates.
func checkAllSessionVersions(bs *types.BotState) int {
	latest := helpers.GetInstalledCCVersion()
	if latest == "" {
		return 0
	}
	found := 0
	for sid, info := range bs.SessionState.All() {
		current := helpers.ReadSessionCCVersion(sid)
		if current == "" || current == latest {
			continue
		}
		bs.VersionNotified.Store(info.TmuxTarget, current+"→"+latest)
		sendVersionNotification(bs, info.TmuxTarget, current, latest)
		found++
	}
	return found
}

// checkSessionVersionByTarget checks a specific target and sends notification if update available.
func checkSessionVersionByTarget(bs *types.BotState, tmuxTarget string) bool {
	sessionID, found := bs.SessionState.FindByTarget(tmuxTarget)
	if !found {
		return false
	}
	current := helpers.ReadSessionCCVersion(sessionID)
	if current == "" {
		return false
	}
	latest := helpers.GetInstalledCCVersion()
	if latest == "" || current == latest {
		return false
	}
	bs.VersionNotified.Store(tmuxTarget, current+"→"+latest)
	sendVersionNotification(bs, tmuxTarget, current, latest)
	return true
}

// sendVersionNotification sends a TG notification about a CC version update.
func sendVersionNotification(bs *types.BotState, tmuxTarget, current, latest string) {
	chat, _, topicID := helpers.ResolveChat(bs.SessionState, tmuxTarget)
	if chat == nil {
		return
	}
	sel := &tele.ReplyMarkup{}
	sel.Inline(sel.Row(sel.Data("🔄 Upgrade", "upgrade", tmuxTarget)))
	paneLabel := notify.FormatPaneID(tmuxTarget)
	if info := bs.SessionState.FindInfoByTarget(tmuxTarget); info != nil && info.Name != "" {
		paneLabel += " (" + info.Name + ")"
	}
	text := fmt.Sprintf("🆕 CC update available\n📟 %s\n\n<b>%s</b> → <b>%s</b>", paneLabel, current, latest)
	var sendOpts []interface{}
	sendOpts = append(sendOpts, sel, tele.ModeHTML)
	if topicID > 0 {
		sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
	}
	helpers.RetrySend(bs.Bot, chat, text, sendOpts...)
}

// doUpgradeSession exits CC and restarts it in the same tmux pane.
var upgradeMutexes sync.Map

func getUpgradeMutex(cwd string) *sync.Mutex {
	v, _ := upgradeMutexes.LoadOrStore(cwd, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func doUpgradeSession(bs *types.BotState, tmuxTarget string) error {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return fmt.Errorf("parse target: %w", err)
	}
	info := bs.SessionState.FindInfoByTarget(tmuxTarget)
	cwd := ""
	if info != nil {
		cwd = info.CWD
	}
	if cwd != "" {
		mu := getUpgradeMutex(cwd)
		mu.Lock()
		defer mu.Unlock()
	}
	pidOut, err := exec.Command("tmux", "display-message", "-p", "-t", target.PaneID, "#{pane_pid}").Output()
	if err != nil {
		return fmt.Errorf("get pane pid: %w", err)
	}
	shellPID := strings.TrimSpace(string(pidOut))
	cmdOut, err := exec.Command("ps", "--ppid", shellPID, "-o", "cmd", "--no-headers").Output()
	if err != nil {
		return fmt.Errorf("get child process cmd: %w", err)
	}
	claudeCmd := strings.TrimSpace(string(cmdOut))
	if claudeCmd == "" {
		return fmt.Errorf("no child process found for shell pid %s", shellPID)
	}
	permMode, _, _ := helpers.DetectPermMode(target)
	permFlag := ""
	switch permMode {
	case "bypass":
		permFlag = " --permission-mode bypassPermissions"
	case "plan":
		permFlag = " --permission-mode plan"
	case "auto":
		permFlag = " --permission-mode acceptEdits"
	}
	if !strings.Contains(claudeCmd, "--continue") && !strings.Contains(claudeCmd, "--resume") {
		claudeCmd += " --continue"
	}
	if permFlag != "" && !strings.Contains(claudeCmd, "--permission-mode") {
		claudeCmd += permFlag
	}
	logger.Info(fmt.Sprintf("doUpgradeSession: detected command=%s permMode=%s target=%s", claudeCmd, permMode, tmuxTarget))
	ch := make(chan struct{}, 1)
	bs.PendingUpgradeRestart.Store(tmuxTarget, ch)
	injector.SendKeys(target, "/exit", "Enter")
	logger.Info(fmt.Sprintf("doUpgradeSession: sent /exit to %s", tmuxTarget))
	select {
	case <-ch:
		logger.Info(fmt.Sprintf("doUpgradeSession: SessionEnd received for %s", tmuxTarget))
	case <-time.After(30 * time.Second):
		logger.Info(fmt.Sprintf("doUpgradeSession: SessionEnd timeout for %s, proceeding anyway", tmuxTarget))
	}
	bs.PendingUpgradeRestart.Delete(tmuxTarget)
	time.Sleep(3 * time.Second)
	logger.Info(fmt.Sprintf("doUpgradeSession: restarting CC with command=%s target=%s", claudeCmd, tmuxTarget))
	if err := injector.InjectText(target, claudeCmd); err != nil {
		return fmt.Errorf("inject restart command: %w", err)
	}
	bs.VersionNotified.Delete(tmuxTarget)
	return nil
}

// handleUsageCommand handles /bot_usage.
func handleUsageCommand(bs *types.BotState, c tele.Context) error {
	sel := &tele.ReplyMarkup{}
	sel.Inline(sel.Row(
		sel.Data("📟 tmux", "usage_src", "tmux"),
		sel.Data("🌐 api", "usage_src", "api"),
	))
	_, err := helpers.RetrySend(bs.Bot, c.Chat(), "📊 Select usage source:", sel, tele.ModeHTML)
	return err
}

var usageCache *helpers.UsageCacheEntry

// handleUsageCommandAPI fetches CC usage from the Anthropic OAuth API.
func handleUsageCommandAPI(bs *types.BotState, c tele.Context, existingMsg *tele.Message) error {
	bot := bs.Bot
	var msg *tele.Message
	if existingMsg != nil {
		msg = existingMsg
	} else {
		var err error
		msg, err = helpers.RetrySend(bot, c.Chat(), "⏳ Fetching CC usage...", tele.ModeHTML)
		if err != nil {
			return err
		}
	}
	var apiErr error
	var formatted string
	formatted, usageCache, apiErr = helpers.FetchUsageFormatted(usageCache)
	if apiErr != nil {
		logger.Error(fmt.Sprintf("handleUsageCommand: %v", apiErr))
		helpers.RetryEdit(bot, msg, fmt.Sprintf("❌ %s", apiErr.Error()), tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: usage fetched len=%d", len(formatted)))
	helpers.RetryEdit(bot, msg, formatted, tele.ModeHTML)
	logger.Info("handleUsageCommand: done")
	return nil
}

// handleUsageCommandTmux fetches CC usage via tmux.
func handleUsageCommandTmux(bs *types.BotState, c tele.Context, existingMsg *tele.Message) error {
	bot := bs.Bot
	var msg *tele.Message
	if existingMsg != nil {
		msg = existingMsg
	} else {
		var err error
		msg, err = helpers.RetrySend(bot, c.Chat(), "⏳ Fetching CC usage...", tele.ModeHTML)
		if err != nil {
			return err
		}
	}
	sessionName := fmt.Sprintf("tg-cli-usage-%d", time.Now().UnixMilli())
	configDir := config.GetConfigDir()
	cmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-c", configDir, "-x", "120", "-y", "40")
	if err := cmd.Run(); err != nil {
		logger.Error(fmt.Sprintf("handleUsageCommand: failed to create temp session err=%v", err))
		helpers.RetryEdit(bot, msg, "❌ Failed to create temp session", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: temp session created session=%s", sessionName))
	defer exec.Command("tmux", "kill-session", "-t", sessionName).Run()
	target, _ := injector.ParseTarget(sessionName)
	logger.Info(fmt.Sprintf("handleUsageCommand: starting claude session=%s", sessionName))
	injector.SendKeys(target, "claude", "Enter")
	if !helpers.WaitForPaneContent(target, "❯", 30*time.Second) {
		logger.Error("handleUsageCommand: CC failed to initialize (timeout waiting for ❯)")
		helpers.RetryEdit(bot, msg, "❌ CC failed to initialize", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: CC ready session=%s", sessionName))
	logger.Info("handleUsageCommand: injecting /usage")
	injector.SendKeys(target, "/usage", "Enter")
	if !helpers.WaitForPaneContent(target, "used", 10*time.Second) {
		logger.Error("handleUsageCommand: failed to get usage data (timeout waiting for 'used')")
		helpers.RetryEdit(bot, msg, "❌ Failed to get usage data", tele.ModeHTML)
		return nil
	}
	logger.Info("handleUsageCommand: usage output detected")
	time.Sleep(1 * time.Second)
	content, err := injector.CapturePane(target)
	if err != nil {
		logger.Error(fmt.Sprintf("handleUsageCommand: failed to capture pane err=%v", err))
		helpers.RetryEdit(bot, msg, "❌ Failed to capture usage data", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: pane captured len=%d", len(content)))
	formatted := helpers.ParseUsageOutput(content)
	if formatted == "" {
		logger.Error("handleUsageCommand: failed to parse usage data (empty result)")
		helpers.RetryEdit(bot, msg, "❌ Failed to parse usage data", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: parsed output len=%d", len(formatted)))
	chunks := helpers.SplitBody(formatted, 4000)
	if len(chunks) <= 1 {
		helpers.RetryEdit(bot, msg, formatted, tele.ModeHTML)
	} else {
		kb := helpers.BuildPageKeyboard(1, len(chunks))
		helpers.RetryEdit(bot, msg, chunks[0]+fmt.Sprintf("\n\n📄 1/%d", len(chunks)), kb, tele.ModeHTML)
		bs.Pages.Store(msg.ID, "", &stores.PageEntry{Chunks: chunks, PermRows: []tele.Row{}})
	}
	logger.Info("handleUsageCommand: done")
	return nil
}

// buildVoiceText builds the voice settings display text.
func buildVoiceText(cfg config.AppConfig) string {
	engine := cfg.VoiceEngine
	if engine == "" {
		engine = "whisper"
	}
	lang := cfg.Language
	if lang == "" {
		lang = "auto"
	}
	text := fmt.Sprintf("🎙 Voice Settings\nEngine: %s", engine)
	if engine == "whisper" {
		model := helpers.CurrentWhisperModelName()
		if model == "" {
			model = "none"
		}
		text += fmt.Sprintf("\nModel: %s", model)
	}
	text += fmt.Sprintf("\nLanguage: %s", lang)
	return text
}

// buildVoiceMenu builds the voice settings inline keyboard.
func buildVoiceMenu(engine string) *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	btnWhisper := menu.Data("🔊 whisper", "voice", "engine:whisper")
	btnSenseVoice := menu.Data("🔊 sensevoice", "voice", "engine:sensevoice")
	btnLangAuto := menu.Data("🌐 auto", "voice", "lang:auto")
	btnLangZh := menu.Data("🇨🇳 zh", "voice", "lang:zh")
	btnLangEn := menu.Data("🇺🇸 en", "voice", "lang:en")
	btnLangJa := menu.Data("🇯🇵 ja", "voice", "lang:ja")
	rows := []tele.Row{
		menu.Row(btnWhisper, btnSenseVoice),
	}
	if engine == "" || engine == "whisper" {
		rows = append(rows,
			menu.Row(
				menu.Data("tiny", "voice", "model:tiny"),
				menu.Data("base", "voice", "model:base"),
				menu.Data("small", "voice", "model:small"),
			),
			menu.Row(
				menu.Data("medium", "voice", "model:medium"),
				menu.Data("turbo", "voice", "model:large-v3-turbo"),
				menu.Data("large", "voice", "model:large-v3"),
			),
		)
	}
	rows = append(rows, menu.Row(btnLangAuto, btnLangZh, btnLangEn, btnLangJa))
	menu.Inline(rows...)
	return menu
}

// handleVoiceCommand handles /bot_voice.
func handleVoiceCommand(c tele.Context) error {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
	}
	engine := cfg.VoiceEngine
	if engine == "" {
		engine = "whisper"
	}
	text := buildVoiceText(cfg)
	menu := buildVoiceMenu(engine)
	_, err = helpers.RetrySend(c.Bot(), c.Chat(), text, menu, tele.ModeHTML)
	return err
}

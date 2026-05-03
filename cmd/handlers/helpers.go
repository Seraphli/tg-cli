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
	pidOut, err := injector.TmuxCmd(target, "display-message", "-p", "-t", target.PaneID, "#{pane_pid}").Output()
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
	if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, target, claudeCmd); err != nil {
		return fmt.Errorf("inject restart command: %w", err)
	}
	bs.VersionNotified.Delete(tmuxTarget)
	return nil
}

// handleReloadCommand handles /reload command by exiting and restarting CC in the same tmux pane.
// Reuses doUpgradeSession since the underlying "exit and re-enter" flow is identical to the version upgrade path.
func handleReloadCommand(bs *types.BotState, c tele.Context, target injector.TmuxTarget) error {
	tmuxTarget := injector.FormatTarget(target)
	if helpers.IsSessionRunning(tmuxTarget) {
		return c.Reply("⚠️ Session is busy. Wait for idle before reloading.")
	}
	paneLabel := notify.FormatPaneID(tmuxTarget)
	msg, err := helpers.RetrySend(bs.Bot, c.Chat(), fmt.Sprintf("🔄 Reloading CC...\n📟 %s", paneLabel), tele.ModeHTML)
	if err != nil {
		return err
	}
	go func() {
		if err := doUpgradeSession(bs, tmuxTarget); err != nil {
			logger.Error(fmt.Sprintf("handleReloadCommand: doUpgradeSession failed: %v", err))
			helpers.RetryEdit(bs.Bot, msg, fmt.Sprintf("❌ Reload failed: %s\n📟 %s", err.Error(), paneLabel), tele.ModeHTML)
			return
		}
		helpers.RetryEdit(bs.Bot, msg, fmt.Sprintf("✅ CC reloaded\n📟 %s", paneLabel), tele.ModeHTML)
	}()
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
	cmd := injector.GlobalTmuxCmd("new-session", "-d", "-s", sessionName, "-c", configDir, "-x", "120", "-y", "40")
	if err := cmd.Run(); err != nil {
		logger.Error(fmt.Sprintf("handleUsageCommand: failed to create temp session err=%v", err))
		helpers.RetryEdit(bot, msg, "❌ Failed to create temp session", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: temp session created session=%s", sessionName))
	defer injector.GlobalTmuxCmd("kill-session", "-t", sessionName).Run()
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

func buildSettingsTopText() string {
	return "⚙️ <b>Bot Settings</b>\nSelect a category:"
}

func buildSettingsTopMenu() *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	menu.Inline(
		menu.Row(
			menu.Data("🔒 Permission", "settings", "perm"),
			menu.Data("🎤 Voice", "settings", "voice"),
		),
		menu.Row(
			menu.Data("📂 CWD", "settings", "cwd"),
			menu.Data("🔔 Tool Notify", "settings", "toolnotify"),
		),
		menu.Row(
			menu.Data("🗺 Routes", "settings", "routes"),
			menu.Data("📬 Mailbox", "settings", "mailbox"),
		),
		menu.Row(
			menu.Data("📊 Status", "settings", "status"),
			menu.Data("⏰ Cron", "settings", "cron"),
		),
	)
	return menu
}

func appendBackButton(menu *tele.ReplyMarkup) {
	btn := menu.Data("⬅️ Back", "settings", "main")
	menu.InlineKeyboard = append(menu.InlineKeyboard, []tele.InlineButton{*btn.Inline()})
}

// IsSettingsMenu reports whether the given message ID is a settings menu message.
func IsSettingsMenu(bs *types.BotState, msgID int) bool {
	_, ok := bs.SettingsMenuMsgs.Load(msgID)
	return ok
}

func buildPermSubMenu(currentMode string) *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	modes := []struct{ label, data string }{
		{"Default", "default"},
		{"Plan", "plan"},
		{"Auto-edit", "auto"},
		{"Bypass", "bypass"},
	}
	var row1, row2 []tele.Btn
	for _, m := range modes {
		label := m.label
		if m.data == currentMode {
			label = "✅ " + label
		}
		btn := menu.Data(label, "settings_perm", m.data)
		if len(row1) < 2 {
			row1 = append(row1, btn)
		} else {
			row2 = append(row2, btn)
		}
	}
	menu.Inline(
		menu.Row(row1...),
		menu.Row(row2...),
	)
	return menu
}

// showSettingsVoice displays the voice settings sub-menu in the settings message.
func showSettingsVoice(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		helpers.RetryEdit(bot, msg, "❌ Failed to load config", tele.ModeHTML)
		return
	}
	engine := cfg.VoiceEngine
	if engine == "" {
		engine = "whisper"
	}
	text := buildVoiceText(cfg)
	menu := buildVoiceMenu(engine)
	appendBackButton(menu)
	helpers.RetryEdit(bot, msg, text, menu, tele.ModeHTML)
}

func buildCwdMenu() *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	btnTmux := menu.Data("📟 tmux CWD", "cwd", "tmux")
	btnPayload := menu.Data("📦 payload CWD", "cwd", "payload")
	menu.Inline(menu.Row(btnTmux, btnPayload))
	return menu
}

// showSettingsCwd displays the CWD source settings sub-menu in the settings message.
func showSettingsCwd(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		helpers.RetryEdit(bot, msg, "❌ Failed to load config", tele.ModeHTML)
		return
	}
	current := cfg.CWDSource
	if current == "" {
		current = "tmux"
	}
	menu := buildCwdMenu()
	appendBackButton(menu)
	helpers.RetryEdit(bot, msg, fmt.Sprintf("📂 <b>CWD Source</b>\nCurrent: %s\n\nSelect source for working directory:", current), menu, tele.ModeHTML)
}

// showSettingsToolNotify displays the tool notification settings sub-menu in the settings message.
func showSettingsToolNotify(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		helpers.RetryEdit(bot, msg, "❌ Failed to load config", tele.ModeHTML)
		return
	}
	enabled := cfg.ToolNotifyEnabled == nil || *cfg.ToolNotifyEnabled
	menu := &tele.ReplyMarkup{}
	var rows []tele.Row
	var textParts []string
	textParts = append(textParts, "🔔 <b>Tool Notifications</b>")
	if msg.Chat.Type == tele.ChatGroup || msg.Chat.Type == tele.ChatSuperGroup {
		creds, _ := config.LoadCredentials()
		routeFound := false
		for key, route := range creds.NameRouteMap {
			if route.ChatID == msg.Chat.ID {
				routeFound = true
				label := "✅ Forward to this group: ON"
				if route.ToolNotifyOff {
					label = "⬜ Forward to this group: OFF"
				}
				rows = append(rows, menu.Row(menu.Data(label, "tools_route_toggle", key)))
				if route.ToolNotifyOff {
					textParts = append(textParts, "Forward: ⬜ OFF")
				} else {
					textParts = append(textParts, "Forward: ✅ ON")
				}
				break
			}
		}
		if !routeFound {
			textParts = append(textParts, "💡 No route bound to this group")
		}
	}
	globalLabel := "✅ Global: ON"
	if !enabled {
		globalLabel = "⬜ Global: OFF"
	}
	rows = append(rows, menu.Row(menu.Data(globalLabel, "verbose", "toggle")))
	globalStatus := "✅ ON"
	if !enabled {
		globalStatus = "⬜ OFF"
	}
	textParts = append(textParts, "Global: "+globalStatus)
	toolsMenu := buildToolsMenu(cfg.ToolNotifyList)
	for _, row := range toolsMenu.InlineKeyboard {
		var btns []tele.Btn
		for _, btn := range row {
			btns = append(btns, tele.Btn{Text: btn.Text, Unique: btn.Unique, Data: btn.Data})
		}
		rows = append(rows, menu.Row(btns...))
	}
	menu.Inline(rows...)
	appendBackButton(menu)
	if len(cfg.ToolNotifyList) > 0 {
		textParts = append(textParts, "Tools: "+strings.Join(cfg.ToolNotifyList, ", "))
	} else {
		textParts = append(textParts, "Tools: (none selected)")
	}
	helpers.RetryEdit(bot, msg, strings.Join(textParts, "\n"), menu, tele.ModeHTML)
}

// showSettingsPerm displays the permission mode settings sub-menu in the settings message.
// Resolves the tmux target from active sessions (works for both private and group chats).
func showSettingsPerm(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	var tmuxStr string
	if msg.Chat.Type == tele.ChatGroup || msg.Chat.Type == tele.ChatSuperGroup {
		str, _, err := resolveGroupTarget(bs, msg.Chat.ID, msg.ThreadID)
		if err != nil {
			menu := &tele.ReplyMarkup{}
			appendBackButton(menu)
			errMsg := "💡 No active sessions found."
			if err.Error() == "multiple sessions bound" {
				errMsg = "💡 Multiple sessions bound to this group. Reply to a notification message to target a specific session."
			}
			helpers.RetryEdit(bot, msg, "🔒 <b>Permission Mode</b>\n\n"+errMsg, menu, tele.ModeHTML)
			return
		}
		tmuxStr = str
	} else {
		sessions := bs.SessionState.All()
		if len(sessions) == 0 {
			menu := &tele.ReplyMarkup{}
			appendBackButton(menu)
			helpers.RetryEdit(bot, msg, "🔒 <b>Permission Mode</b>\n\n💡 No active sessions found.", menu, tele.ModeHTML)
			return
		}
		if len(sessions) > 1 {
			menu := &tele.ReplyMarkup{}
			appendBackButton(menu)
			helpers.RetryEdit(bot, msg, "🔒 <b>Permission Mode</b>\n\n💡 Multiple sessions active. Reply to a notification message to target a specific session.", menu, tele.ModeHTML)
			return
		}
		for _, info := range sessions {
			tmuxStr = info.TmuxTarget
		}
	}
	target, err := injector.ParseTarget(tmuxStr)
	if err != nil {
		menu := &tele.ReplyMarkup{}
		appendBackButton(menu)
		helpers.RetryEdit(bot, msg, fmt.Sprintf("🔒 <b>Permission Mode</b>\n\n❌ Invalid target: %v", err), menu, tele.ModeHTML)
		return
	}
	mode, _, err := helpers.DetectPermMode(target)
	if err != nil {
		menu := &tele.ReplyMarkup{}
		appendBackButton(menu)
		helpers.RetryEdit(bot, msg, fmt.Sprintf("🔒 <b>Permission Mode</b>\n\n❌ Detect mode failed: %v", err), menu, tele.ModeHTML)
		return
	}
	menu := buildPermSubMenu(mode)
	bs.SettingsMenuMsgs.Store(msg.ID, tmuxStr)
	appendBackButton(menu)
	helpers.RetryEdit(bot, msg, fmt.Sprintf("🔒 <b>Permission Mode</b>\n📟 %s\nCurrent: %s", notify.FormatPaneID(tmuxStr), mode), menu, tele.ModeHTML)
}

func buildRoutesSections(bot *tele.Bot, bs *types.BotState) []string {
	creds, _ := config.LoadCredentials()
	sessions := bs.SessionState.All()
	var sections []string
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
			if info := bs.SessionState.FindByName(name); info != nil {
				paneID = " (" + notify.FormatPaneID(info.TmuxTarget) + ")"
			}
			lines = append(lines, fmt.Sprintf("  %s%s → %s%s", name, paneID, chatName, topicStr))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	var unnamedLines []string
	for _, info := range sessions {
		if info.Name != "" {
			continue
		}
		label := notify.FormatPaneID(info.TmuxTarget)
		unnamedLines = append(unnamedLines, fmt.Sprintf("  %s → %s", label, notify.CompressPath(info.CWD)))
	}
	if len(unnamedLines) > 0 {
		sections = append(sections, "📟 Unnamed sessions:\n"+strings.Join(unnamedLines, "\n"))
	}
	return sections
}

// showSettingsRoutes displays the route bindings sub-menu in the settings message.
func showSettingsRoutes(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	sections := buildRoutesSections(bot, bs)
	creds, _ := config.LoadCredentials()
	menu := &tele.ReplyMarkup{}
	var rows []tele.Row
	var keys []string
	idx := 1
	for name := range creds.NameRouteMap {
		keys = append(keys, name)
		rows = append(rows, menu.Row(menu.Data(fmt.Sprintf("🗑 Unbind: %s", name), "unbind_select", fmt.Sprintf("%d", idx))))
		idx++
	}
	if msg.Chat.Type == tele.ChatGroup || msg.Chat.Type == tele.ChatSuperGroup {
		sessions := bs.SessionState.All()
		if len(sessions) > 0 {
			rows = append(rows, menu.Row(menu.Data("🔗 Bind session to this group", "settings_bind", "")))
		}
	}
	menu.Inline(rows...)
	appendBackButton(menu)
	if len(keys) > 0 {
		bs.UnbindMenuItems.Store(msg.ID, keys)
	}
	var text string
	if len(sections) == 0 {
		text = "🗺 <b>Routes</b>\nNo active route bindings or sessions."
	} else {
		text = "🗺 <b>Routes</b>\n" + strings.Join(sections, "\n\n")
	}
	helpers.RetryEdit(bot, msg, text, menu, tele.ModeHTML)
}

func buildMailboxContent(chatType tele.ChatType, chatID int64, creds config.Credentials) (string, *tele.ReplyMarkup) {
	menu := &tele.ReplyMarkup{}
	if chatType == tele.ChatGroup || chatType == tele.ChatSuperGroup {
		if creds.MailboxChatID == chatID {
			menu.Inline(menu.Row(menu.Data("✅ Bound (click to unbind)", "mailbox_unbind")))
			return fmt.Sprintf("📬 <b>Mailbox Group</b>\nStatus: ✅ Bound as mailbox group\nChat ID: %d", chatID), menu
		}
		if creds.MailboxChatID != 0 {
			menu.Inline(menu.Row(menu.Data("📬 Re-bind to this group", "mailbox_bind")))
			return fmt.Sprintf("📬 <b>Mailbox Group</b>\nStatus: Bound to another group (%d)\nChat ID: %d", creds.MailboxChatID, chatID), menu
		}
		menu.Inline(menu.Row(menu.Data("📬 Bind as mailbox group", "mailbox_bind")))
		return fmt.Sprintf("📬 <b>Mailbox Group</b>\nStatus: No mailbox group set\nChat ID: %d", chatID), menu
	}
	if creds.MailboxChatID != 0 {
		return fmt.Sprintf("📬 <b>Mailbox</b>\nBound to chat %d", creds.MailboxChatID), menu
	}
	return "📬 <b>Mailbox</b>\nNo mailbox group bound. Send /bot_mailbox in a group to bind it.", menu
}

// showSettingsMailbox displays the mailbox binding settings sub-menu in the settings message.
func showSettingsMailbox(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	creds, _ := config.LoadCredentials()
	text, menu := buildMailboxContent(msg.Chat.Type, msg.Chat.ID, creds)
	appendBackButton(menu)
	helpers.RetryEdit(bot, msg, text, menu, tele.ModeHTML)
}

// showSettingsStatus displays the bot status sub-menu in the settings message.
func showSettingsStatus(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	creds, _ := config.LoadCredentials()
	paired := len(creds.PairingAllow.IDs) > 0 || creds.PairingAllow.DefaultChatID != ""
	statusLine := "❌ Not paired"
	if paired {
		statusLine = "✅ Paired and running"
	}
	menu := &tele.ReplyMarkup{}
	appendBackButton(menu)
	helpers.RetryEdit(bot, msg, fmt.Sprintf("📊 <b>Status</b>\nBot: %s", statusLine), menu, tele.ModeHTML)
}

func buildCronContent(jobs []*stores.CronJob, menu *tele.ReplyMarkup) (string, []tele.Row) {
	var text strings.Builder
	var rows []tele.Row
	for _, j := range jobs {
		modeIcon := "🖥️"
		if j.Mode == "inject" {
			modeIcon = "💉"
		}
		onceTag := ""
		if j.Once {
			onceTag = " [once]"
		}
		agentInfo := ""
		if j.AgentName != "" {
			agentInfo = fmt.Sprintf(" → %s", j.AgentName)
		}
		lastRunStr := "never"
		if !j.LastRun.IsZero() {
			lastRunStr = helpers.RelativeTime(j.LastRun)
		}
		nameStr := ""
		if j.Name != "" {
			nameStr = fmt.Sprintf(" <b>%s</b>", j.Name)
		}
		text.WriteString(fmt.Sprintf("%s <code>%s</code>%s%s%s\n📅 %s | Last: %s\n📝 %s\n\n",
			modeIcon, j.ID[:8], nameStr, onceTag, agentInfo, j.Schedule, lastRunStr, j.Prompt))
		btnLabel := "🗑 " + j.ID[:8]
		if j.Name != "" {
			btnLabel = "🗑 " + j.Name
		}
		rows = append(rows, menu.Row(menu.Data(btnLabel, "cron_delete", j.ID)))
	}
	return text.String(), rows
}

// showSettingsCron displays the cron jobs sub-menu in the settings message.
func showSettingsCron(bot *tele.Bot, bs *types.BotState, msg *tele.Message) {
	jobs := bs.CronJobs.All()
	menu := &tele.ReplyMarkup{}
	if len(jobs) == 0 {
		appendBackButton(menu)
		helpers.RetryEdit(bot, msg, "⏰ <b>Cron Jobs</b>\nNo cron jobs configured.", menu, tele.ModeHTML)
		return
	}
	body, rows := buildCronContent(jobs, menu)
	menu.Inline(rows...)
	appendBackButton(menu)
	helpers.RetryEdit(bot, msg, "⏰ <b>Cron Jobs</b>\n\n"+body, menu, tele.ModeHTML)
}

package cmd

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	"gopkg.in/natefinch/lumberjack.v2"
	tele "gopkg.in/telebot.v3"
)

func processTranscriptUpdates(bs *BotState, sessionID, transcriptPath string, isQuestion ...bool) string {
	if transcriptPath == "" || sessionID == "" {
		return ""
	}
	lock := bs.SessionCounts.GetLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	// Initialize count for unknown sessions (e.g. after bot restart) to avoid sending historical content
	if _, known := bs.SessionCounts.Counts[sessionID]; !known {
		texts := helpers.ReadAssistantTexts(transcriptPath)
		count := len(texts)
		// For AskUserQuestion, backtrack by 1 so the preceding assistant message is included
		if len(isQuestion) > 0 && isQuestion[0] && count > 0 {
			count--
		}
		bs.SessionCounts.Counts[sessionID] = count
		logger.Debug(fmt.Sprintf("Initialized session count: session=%s count=%d", sessionID, count))
	}
	notified := bs.SessionCounts.Counts[sessionID]
	// Read immediately first, then retry with shorter delays
	texts := helpers.ReadAssistantTexts(transcriptPath)
	if len(texts) <= notified {
		for retry := 0; retry < 5; retry++ {
			time.Sleep(500 * time.Millisecond)
			texts = helpers.ReadAssistantTexts(transcriptPath)
			if len(texts) > notified {
				break
			}
		}
	}
	if len(texts) <= notified {
		return ""
	}
	var newTexts []string
	for i := notified; i < len(texts); i++ {
		if strings.TrimSpace(texts[i]) != "" {
			newTexts = append(newTexts, strings.TrimSpace(texts[i]))
		}
	}
	bs.SessionCounts.Counts[sessionID] = len(texts)
	return strings.Join(newTexts, "\n\n")
}

func sendEventNotification(bs *BotState, chat *tele.Chat, chatID, sessionID, event, project, cwd, tmuxTarget, body, toolName, agentName string, topicID int) int {
	b := bs.Bot
	var sentMsgID int
	backend := "cc"
	if info := bs.SessionState.FindInfoByTarget(tmuxTarget); info != nil && info.Backend != "" {
		backend = info.Backend
	}
	cliCmd := helpers.GetPaneCLICommand(tmuxTarget)
	nd := notify.NotificationData{
		Event:          event,
		Project:        project,
		CWD:            cwd,
		TmuxTarget:     tmuxTarget,
		ToolName:       toolName,
		AgentName:      agentName,
		Backend:        backend,
		CLICommand:     cliCmd,
		ContextUsedPct: -1,
	}
	if usedPct, usedTokens, windowSize, ok := helpers.ReadContextUsage(sessionID); ok {
		nd.ContextUsedPct = usedPct
		nd.ContextUsedTokens = usedTokens
		nd.ContextWindowSize = windowSize
	}
	cfg, _ := config.LoadAppConfig()
	// useRich: send via rich Bot API path for all non-raw events (ToolUse included — body is pre-built rich HTML)
	useRich := cfg.NotifyFormat != "raw"
	parseMode := tele.ModeHTML
	if cfg.NotifyFormat == "raw" {
		// Raw mode: send body as-is without HTML conversion or table image rendering
		parseMode = ""
	}
	headerLen := notify.HeaderLen(nd)
	// detailsReplacer strips <details>/<summary> tags for legacy-safe LegacyHTML fallback.
	// <details> and <summary> are not valid in Telegram legacy HTML — inner content is preserved.
	detailsReplacer := strings.NewReplacer(
		"<details>", "", "</details>", "",
		"<summary>", "", "</summary>", "",
	)
	paginationMax := 4000
	if cfg.PaginationMaxRunes > 0 {
		paginationMax = cfg.PaginationMaxRunes
	}
	maxBodyRunes := paginationMax - headerLen - 100
	if useRich {
		// Rich path: for ToolUse body is pre-built rich HTML (with <details>); for other events render markdown.
		var richBody, legacyBody string
		var skipEntityDetection bool
		if event == "ToolUse" {
			// body is already rich HTML from BuildToolNotifyText — use as-is
			richBody = body
			// Legacy fallback: strip <details>/<summary> tags; inner <pre>/emoji content is legacy-valid
			legacyBody = detailsReplacer.Replace(body)
			skipEntityDetection = true // args are code/paths; entity detection is harmful
		} else {
			rawBody := body
			richBody = markdown.RenderRichHTML(rawBody)
			legacyBody = markdown.RenderTelegramHTML(rawBody)
			skipEntityDetection = false
		}
		logger.Debug(fmt.Sprintf("TG message [%s] full_body:\n%s", event, helpers.FinalizeRichHTML(richBody)))
		// Use RichMaxRunes threshold for the rich path (up to 32768 API limit)
		richMax := 30000
		if cfg.RichMaxRunes > 0 {
			richMax = cfg.RichMaxRunes
		}
		maxRichBody := richMax - headerLen - 100
		chunks := helpers.SplitBody(richBody, maxRichBody)
		legacyChunks := helpers.SplitBody(legacyBody, maxRichBody)
		if len(chunks) <= 1 {
			nd.Body = richBody
			richText := notify.BuildNotificationText(nd)
			nd.Body = legacyBody
			legacyText := notify.BuildNotificationText(nd)
			sent, err := helpers.RetrySendRich(b, chat, richText, helpers.RichSendOpts{
				TopicID:             topicID,
				SkipEntityDetection: skipEntityDetection,
				LegacyHTML:          legacyText,
			})
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send notification: %v html=%s", err, richText))
			} else {
				if sent != nil {
					sentMsgID = sent.ID
				}
				logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s body_len=%d body=%s", chatID, event, project, tmuxTarget, len([]rune(richBody)), richBody))
				logger.Debug(fmt.Sprintf("TG message sent [%s] full_text:\n%s", event, richText))
				recordReplyTarget(bs, sentMsgID, sessionID, tmuxTarget, chat.ID)
			}
		} else {
			nd.Body = chunks[0]
			nd.Page = 1
			nd.TotalPages = len(chunks)
			richText := notify.BuildNotificationText(nd)
			legacyChunk0 := ""
			if len(legacyChunks) > 0 {
				legacyChunk0 = legacyChunks[0]
			}
			nd.Body = legacyChunk0
			legacyText := notify.BuildNotificationText(nd)
			kb := helpers.BuildPageKeyboard(1, len(chunks))
			sent, err := helpers.RetrySendRich(b, chat, richText, helpers.RichSendOpts{
				TopicID:             topicID,
				Markup:              kb,
				SkipEntityDetection: skipEntityDetection,
				LegacyHTML:          legacyText,
			})
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send notification: %v html=%s", err, richText))
			} else {
				sentMsgID = sent.ID
				bs.Pages.Store(sent.ID, sessionID, &stores.PageEntry{
					Chunks:            chunks,
					Rich:              true,
					Event:             event,
					Project:           project,
					CWD:               cwd,
					TmuxTarget:        tmuxTarget,
					ChatID:            chat.ID,
					CLICommand:        nd.CLICommand,
					AgentName:         nd.AgentName,
					Backend:           nd.Backend,
					ContextUsedPct:    nd.ContextUsedPct,
					ContextUsedTokens: nd.ContextUsedTokens,
					ContextWindowSize: nd.ContextWindowSize,
				})
				logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s (%d pages, msg_id=%d) body_len=%d body=%s", chatID, event, project, tmuxTarget, len(chunks), sent.ID, len([]rune(richBody)), richBody))
				logger.Debug(fmt.Sprintf("TG message sent [%s] page=1/%d full_text:\n%s", event, len(chunks), richText))
			}
		}
	} else {
		// Legacy path: raw mode only — send body verbatim (no HTML conversion, no table images).
		logger.Debug(fmt.Sprintf("TG message [%s] full_body:\n%s", event, body))
		chunks := helpers.SplitBody(body, maxBodyRunes)
		var sendOpts []interface{}
		if topicID > 0 {
			sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
		}
		if parseMode != "" {
			sendOpts = append(sendOpts, parseMode)
		}
		if len(chunks) <= 1 {
			nd.Body = body
			text := notify.BuildNotificationText(nd)
			sent, err := helpers.RetrySend(b, chat, text, sendOpts...)
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send notification: %v html=%s", err, text))
			} else {
				if sent != nil {
					sentMsgID = sent.ID
				}
				logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s body_len=%d body=%s", chatID, event, project, tmuxTarget, len([]rune(body)), body))
				logger.Debug(fmt.Sprintf("TG message sent [%s] full_text:\n%s", event, text))
				recordReplyTarget(bs, sentMsgID, sessionID, tmuxTarget, chat.ID)
			}
		} else {
			nd.Body = chunks[0]
			nd.Page = 1
			nd.TotalPages = len(chunks)
			text := notify.BuildNotificationText(nd)
			kb := helpers.BuildPageKeyboard(1, len(chunks))
			opts := append([]interface{}{kb}, sendOpts...)
			sent, err := helpers.RetrySend(b, chat, text, opts...)
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send notification: %v html=%s", err, text))
			} else {
				sentMsgID = sent.ID
				bs.Pages.Store(sent.ID, sessionID, &stores.PageEntry{
					Chunks:            chunks,
					Event:             event,
					Project:           project,
					CWD:               cwd,
					TmuxTarget:        tmuxTarget,
					ChatID:            chat.ID,
					CLICommand:        nd.CLICommand,
					AgentName:         nd.AgentName,
					Backend:           nd.Backend,
					ContextUsedPct:    nd.ContextUsedPct,
					ContextUsedTokens: nd.ContextUsedTokens,
					ContextWindowSize: nd.ContextWindowSize,
				})
				logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s (%d pages, msg_id=%d) body_len=%d body=%s", chatID, event, project, tmuxTarget, len(chunks), sent.ID, len([]rune(body)), body))
				logger.Debug(fmt.Sprintf("TG message sent [%s] page=1/%d full_text:\n%s", event, len(chunks), text))
			}
		}
	}
	return sentMsgID
}

// recordReplyTarget records a single-chunk notification's msg_id -> tmux_target in the Pages store so a
// later reply to it can resolve the target by msg_id. Rich Bot API 10.1 notifications leave the
// Telegram Message .Text empty (their content is a rich_message field telebot does not model), so the
// 📟-line text parse fails on reply — the Pages record is the fallback. Multi-chunk notifications
// already persist a full PageEntry carrying TmuxTarget; this covers the single-chunk path. The entry
// has no chunks/buttons so page/collapse callbacks never touch it; it is cleared with the session via
// CleanupSession.
func recordReplyTarget(bs *BotState, msgID int, sessionID, tmuxTarget string, chatID int64) {
	if msgID == 0 || tmuxTarget == "" {
		return
	}
	bs.Pages.Store(msgID, sessionID, &stores.PageEntry{TmuxTarget: tmuxTarget, ChatID: chatID})
}

var typingLogWriter io.Writer

func initTypingLog(configDir string) {
	typingLogWriter = &lumberjack.Logger{
		Filename:   filepath.Join(configDir, "typing.log"),
		MaxSize:    5,
		MaxBackups: 3,
		Compress:   false,
	}
}

func typingLog(format string, args ...interface{}) {
	if typingLogWriter == nil {
		return
	}
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	typingLogWriter.Write([]byte(fmt.Sprintf("[%s] %s\n", ts, msg)))
}

// sendTypingForTarget sends typing action for a specific session target.
// Returns the chatID key used (for dedup tracking), or 0 if not sent.
func sendTypingForTarget(bot *tele.Bot, info stores.SessionInfo, creds *config.Credentials, sentChats map[int64]bool) int64 {
	if info.Name != "" {
		if route, ok := creds.NameRouteMap[info.Name]; ok {
			key := route.ChatID*1000 + int64(route.TopicID)
			if sentChats != nil && sentChats[key] {
				return 0
			}
			var err error
			if route.TopicID > 0 {
				err = bot.Notify(&tele.Chat{ID: route.ChatID}, tele.Typing, route.TopicID)
			} else {
				err = bot.Notify(&tele.Chat{ID: route.ChatID}, tele.Typing)
			}
			if err != nil {
				typingLog("Typing send failed: chat=%d topic=%d target=%s err=%v", route.ChatID, route.TopicID, info.TmuxTarget, err)
				return 0
			}
			typingLog("Typing sent: chat=%d topic=%d target=%s", route.ChatID, route.TopicID, info.TmuxTarget)
			return key
		}
	}
	// Unbound session — use default chat
	defaultChatIDStr := pairing.GetDefaultChatID()
	if defaultChatIDStr == "" {
		return 0
	}
	chatID, _ := strconv.ParseInt(defaultChatIDStr, 10, 64)
	if chatID == 0 {
		return 0
	}
	if sentChats != nil && sentChats[chatID] {
		return 0
	}
	err := bot.Notify(&tele.Chat{ID: chatID}, tele.Typing)
	if err != nil {
		typingLog("Typing send failed: chat=%d target=%s err=%v", chatID, info.TmuxTarget, err)
		return 0
	}
	typingLog("Typing sent: chat=%d target=%s", chatID, info.TmuxTarget)
	return chatID
}

func resolveChat(bs *BotState, tmuxTarget string) (*tele.Chat, string, int) {
	creds, err := config.LoadCredentials()
	if err == nil && tmuxTarget != "" {
		sid, found := bs.SessionState.FindByTarget(tmuxTarget)
		if found {
			info := bs.SessionState.FindInfoByTarget(tmuxTarget)
			// Priority 1: name route
			if info != nil && info.Name != "" {
				if route, ok := creds.NameRouteMap[info.Name]; ok {
					logger.Info(fmt.Sprintf("Route resolved: name=%s → chat=%d topic=%d (name route)", info.Name, route.ChatID, route.TopicID))
					return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
				}
			}
			// Priority 2: session ID route
			if route, ok := creds.NameRouteMap[sid]; ok {
				logger.Info(fmt.Sprintf("Route resolved: sessionID=%s → chat=%d topic=%d (session route)", sid[:8], route.ChatID, route.TopicID))
				return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
			}
		}
	}
	chatIDStr := pairing.GetDefaultChatID()
	if chatIDStr == "" {
		return nil, "", 0
	}
	chatIDInt, _ := strconv.ParseInt(chatIDStr, 10, 64)
	return &tele.Chat{ID: chatIDInt}, chatIDStr, 0
}

// cleanStaleRoutes is a no-op. Routes are permanent — never delete NameRouteMap entries automatically.
// Users manage routes via /bot_bind and /bot_unbind.
func cleanStaleRoutes(bs *BotState) {
	// Routes are permanent — never delete NameRouteMap entries automatically.
	// Users manage routes via /bot_bind and /bot_unbind.
}

// injectRouteWindow is the 5-second window opened on an MD-final (or a PreToolUse/Stop supplement) when
// the inject queue has items. The next hook event within the window (or a timeout) selects the delivery mode.
const injectRouteWindow = 5 * time.Second

// flushInjectQueue is the idle-ticker entry (bot.go). The pane is not running, so route in idle mode
// immediately (no window) — SafeInjectText WITHOUT Force takes the idle inject path.
func flushInjectQueue(bs *BotState, tmuxTarget string, toolUseID string) {
	if !bs.InjectQueue.HasItems(tmuxTarget) {
		return
	}
	// R3 permission guard: never inject into a permission picker — keep the queue.
	if snap, ok := bs.PendingWait.FindByTmuxTarget(tmuxTarget); ok && snap.ToolName != "AskUserQuestion" {
		logger.Info(fmt.Sprintf("flushInjectQueue: PermissionRequest pending, keeping queue for target=%s", tmuxTarget))
		return
	}
	// R4 exactly-once claim: guard the idle-ticker path with the SAME claim so it never double-injects
	// against an in-flight MD-final/PreToolUse/Stop routing window.
	if _, _, won := bs.InjectRoute.ArmRoute(tmuxTarget, injectRouteWindow); !won {
		return
	}
	defer bs.InjectRoute.Release(tmuxTarget)
	deliverInjectQueue(bs, tmuxTarget, toolUseID, stores.InjectModeIdle)
}

// routeInjectQueue is the event-driven inject-queue trigger (R2/R5). It is triggered ONLY by the MD-final in
// handleMessageDisplay (R10-item1 removed the PreToolUse/Stop supplement triggers). It runs the routing
// DECISION on the caller's (Hook FIFO) goroutine cheaply — apply the permission guard, claim the target
// exactly-once (ArmRoute), then spawn a goroutine that waits up to injectRouteWindow for the NEXT hook event
// (delivered via InjectRoute.SignalEvent from the PreToolUse/Stop handlers), routes to the chosen mode, and
// dispatches the actual DELIVERY OFF the Hook FIFO.
func routeInjectQueue(bs *BotState, tmuxTarget, toolUseID string) {
	if !bs.InjectQueue.HasItems(tmuxTarget) {
		return
	}
	// R3 permission guard on the DECISION path: a non-AskQ PendingWait (PermissionRequest) is active →
	// do not deliver, keep the queue until the permission resolves.
	if snap, ok := bs.PendingWait.FindByTmuxTarget(tmuxTarget); ok && snap.ToolName != "AskUserQuestion" {
		logger.Info(fmt.Sprintf("routeInjectQueue: PermissionRequest pending, keeping queue for target=%s", tmuxTarget))
		return
	}
	// R2 exactly-once claim: MD-final can fire multiple times per turn. Only the ONE caller that wins the
	// claim opens a routing window.
	eventCh, toolCh, won := bs.InjectRoute.ArmRoute(tmuxTarget, injectRouteWindow)
	if !won {
		return
	}
	logger.Info(fmt.Sprintf("routeInjectQueue: MD-final trigger armed routing window for target=%s", tmuxTarget))
	go func() {
		defer bs.InjectRoute.Release(tmuxTarget)
		var event, tool string
		select {
		case event = <-eventCh:
			select {
			case tool = <-toolCh:
			default:
			}
		case <-time.After(injectRouteWindow):
			event, tool = "", "" // timeout → queued-command
		}
		mode := stores.RouteInjectMode(event, tool)
		logger.Info(fmt.Sprintf("routeInjectQueue: routing target=%s event=%q tool=%q mode=%d", tmuxTarget, event, tool, mode))
		deliverInjectQueue(bs, tmuxTarget, toolUseID, mode)
	}()
}

// deliverInjectQueue merges all queued items for a target and delivers them per the chosen mode. Runs OFF
// the Hook FIFO (a goroutine or the idle-ticker Message-FIFO dispatch). Modes:
//   - InjectModeQueuedCommand : Force inject-while-busy (existing queued-command path)
//   - InjectModeAskQCustomReply: deliver as AskQ custom reply (Force → SafeInjectText detects AskQ, R1 wait)
//   - InjectModeIdle          : inject WITHOUT Force (existing idle path; waits for idle)
func deliverInjectQueue(bs *BotState, tmuxTarget, toolUseID string, mode stores.InjectMode) {
	if !bs.InjectQueue.HasItems(tmuxTarget) {
		return
	}
	// R10-item3: idle-mode delivery WAITS for the pane to go idle before flushing. A Stop-routed idle
	// delivery may fire while CC is still busy; poll IsSessionRunning every 500ms up to a 30s bound. If it
	// goes idle, proceed; if it stays busy past the bound, leave the queue intact for the idle ticker to
	// retry. The idle-ticker entry only fires when the pane is already idle (fast no-op); the routeInjectQueue
	// goroutine runs OFF the Hook FIFO so this wait never blocks the FIFO.
	if mode == stores.InjectModeIdle {
		waitDeadline := time.Now().Add(30 * time.Second)
		for helpers.IsSessionRunning(bs.HookRunning, tmuxTarget) {
			if time.Now().After(waitDeadline) {
				logger.Info(fmt.Sprintf("deliverInjectQueue: idle wait timed out, leaving queue for retry (target=%s, mode=idle)", tmuxTarget))
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	notifyMsgID, hasNotify := bs.InjectQueue.GetNotifyMsg(tmuxTarget)
	injectID := bs.InjectQueue.GetInjectID(tmuxTarget)
	items := bs.InjectQueue.Flush(tmuxTarget)
	if len(items) == 0 {
		return
	}
	var texts []string
	for _, item := range items {
		texts = append(texts, item.Text)
	}
	merged := strings.Join(texts, "\n")
	logger.Info(fmt.Sprintf("flushInjectQueue: merging %d items for target=%s merged_len=%d mode=%d", len(items), tmuxTarget, len(merged), mode))
	chat, _, _ := resolveChat(bs, tmuxTarget)
	if injectID == "" {
		injectID = fmt.Sprintf("%x", time.Now().UnixNano()%0xFFFFFF)
	}
	logger.Info(fmt.Sprintf("flushInjectQueue: [%s] starting flush for target=%s items=%d mode=%d", injectID, tmuxTarget, len(items), mode))
	// Fix 19: HTML-escape the merged inject text so raw '<'/'>'/'&' in user content renders
	// correctly in the rich notification. The flush edits below use RetryEditRich (rich messages
	// support ~32768 chars, so the full merged content is not truncated by the 4096 plain-text cap).
	msgContent := "──────\n" + markdown.EscapeHTML(strings.Join(texts, "\n")) + "\n──────"
	// Diagnostic: log a tmux pane snapshot immediately before/after the Enter keypress so the injected
	// message landing (or stranding behind an AskQ popup) can be read from bot.log. Tail last 30 lines.
	diag := func(phase, pane string) {
		lines := strings.Split(pane, "\n")
		if len(lines) > 30 {
			lines = lines[len(lines)-30:]
		}
		logger.Info(fmt.Sprintf("INJECT_DIAG: [%s] phase=%s session=%s tool_use_id=%s pane_tail:\n%s",
			injectID, phase, tmuxTarget, toolUseID, strings.Join(lines, "\n")))
	}
	// Idle mode waits for idle (via SafeInjectText's StopCooldown WaitIfNeeded on the non-Force path); the
	// queued-command/AskQ modes use Force so the inject/answer lands while CC is busy. AskQ-custom-reply
	// routes through the AskQ handshake in SafeInjectText (R1 bounded wait for the PendingWait snapshot).
	force := mode != stores.InjectModeIdle
	// AN3 re-queue guardrail: phase1 may re-queue (CC busy again after the idle wait, or a PermissionRequest
	// appeared) instead of injecting. requeued tells the success branch below to log/notify accurately rather
	// than reporting a re-queue as "Injected".
	var requeued bool
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
		ResolveChat: func(t string) (*tele.Chat, string, int) {
			return resolveChat(bs, t)
		},
		FormatPaneID:   notify.FormatPaneID,
		Force:          force,
		InjectDiag:     diag,
		AwaitAskQReady: mode == stores.InjectModeAskQCustomReply,
		Requeued:       &requeued,
	}
	err := helpers.SafeInjectText(p, tmuxTarget, merged)
	if err != nil {
		// Error classification with sentinels
		if errors.Is(err, injector.ErrSubmitAfterPaste) || errors.Is(err, helpers.ErrInjectNotConfirmed) {
			// Post-paste error — do NOT re-enqueue (text may be in pane)
			logger.Error(fmt.Sprintf("flushInjectQueue: [%s] post-paste error (not re-enqueueing): target=%s err=%v", injectID, tmuxTarget, err))
		} else {
			// Pre-paste error — safe to re-enqueue
			logger.Error(fmt.Sprintf("flushInjectQueue: [%s] pre-paste error (re-enqueueing %d items): target=%s err=%v", injectID, len(items), tmuxTarget, err))
			bs.InjectQueue.ReEnqueue(tmuxTarget, items)
		}
		if hasNotify && chat != nil {
			editMsg := &tele.Message{ID: notifyMsgID, Chat: chat}
			helpers.RetryEditRich(bs.Bot, editMsg, fmt.Sprintf("❌ Inject failed [%s] (%d)\n📟 %s\n%s", injectID, len(items), notify.FormatPaneID(tmuxTarget), msgContent), helpers.RichSendOpts{})
		}
		return
	}
	// AN3: phase1 re-queued instead of injecting (CC busy again after the idle wait, or a PermissionRequest
	// appeared). The re-queue already sent its own "⏳ Queued" notification — do NOT log "inject completed" or
	// edit the notify message to "✅ Injected". Log accurately and return.
	if requeued {
		logger.Info(fmt.Sprintf("flushInjectQueue: [%s] deferred: CC busy again after idle wait, re-queued (not injected): target=%s", injectID, tmuxTarget))
		return
	}
	logger.Info(fmt.Sprintf("flushInjectQueue: [%s] inject completed: target=%s notify_msg_id=%d", injectID, tmuxTarget, notifyMsgID))
	if hasNotify && chat != nil {
		editMsg := &tele.Message{ID: notifyMsgID, Chat: chat}
		helpers.RetryEditRich(bs.Bot, editMsg, fmt.Sprintf("✅ Injected [%s] (%d)\n📟 %s\n%s", injectID, len(items), notify.FormatPaneID(tmuxTarget), msgContent), helpers.RichSendOpts{})
	}
}

// checkSessionVersion checks a single session for CC version updates.
func checkSessionVersion(bs *BotState, tmuxTarget string) {
	time.Sleep(2 * time.Second)
	_, found := bs.SessionState.FindByTarget(tmuxTarget)
	if !found {
		return
	}
	current := helpers.ReadSessionCCVersion(tmuxTarget)
	if current == "" {
		return
	}
	latest := helpers.GetInstalledCCVersion()
	if latest == "" || current == latest {
		return
	}
	notifyKey := current + "→" + latest
	if prev, ok := bs.VersionNotified.Load(tmuxTarget); ok && prev.(string) == notifyKey {
		return
	}
	bs.VersionNotified.Store(tmuxTarget, notifyKey)
	logger.Info(fmt.Sprintf("CC version update detected: target=%s current=%s latest=%s", tmuxTarget, current, latest))
	sendVersionNotification(bs, tmuxTarget, current, latest)
}

func sendVersionNotification(bs *BotState, tmuxTarget, current, latest string) {
	chat, _, topicID := resolveChat(bs, tmuxTarget)
	if chat == nil {
		return
	}
	sel := &tele.ReplyMarkup{}
	sel.Inline(sel.Row(
		sel.Data("🔄 Upgrade", "upgrade", tmuxTarget),
	))
	paneLabel := notify.FormatPaneID(tmuxTarget)
	if info := bs.SessionState.FindInfoByTarget(tmuxTarget); info != nil && info.Name != "" {
		paneLabel += " (" + info.Name + ")"
	}
	// Escape dynamic fields; text uses only <b> and plain content valid in both rich and legacy HTML
	text := fmt.Sprintf("🆕 CC update available\n📟 %s\n\n<b>%s</b> → <b>%s</b>",
		markdown.EscapeRich(paneLabel), markdown.EscapeRich(current), markdown.EscapeRich(latest))
	helpers.RetrySendRich(bs.Bot, chat, text, helpers.RichSendOpts{
		TopicID:    topicID,
		Markup:     sel,
		LegacyHTML: text,
	})
}

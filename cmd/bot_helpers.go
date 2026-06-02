package cmd

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
	"gopkg.in/natefinch/lumberjack.v2"
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
	// Extract tables for image rendering, convert remaining to Telegram HTML
	var tableImages [][]byte
	parseMode := tele.ModeHTML
	if cfg.NotifyFormat == "raw" {
		// Raw mode: send body as-is without HTML conversion or table image rendering
		parseMode = ""
	} else if event != "ToolUse" {
		tableMode := cfg.TableMode
		if tableMode == "" {
			tableMode = "image"
		}
		if tableMode == "image" {
			tables := markdown.ExtractTableData(body)
			if len(tables) > 0 {
				body = markdown.RemoveTables(body)
				for _, t := range tables {
					img, err := markdown.RenderTableImageChromeFormatted(t.Headers, t.Rows, t.HeadersHTML, t.RowsHTML)
					if err != nil {
						logger.Info(fmt.Sprintf("Chrome table render failed (falling back to code): %v", err))
						img, err = markdown.RenderTableImage(t.Headers, t.Rows)
						if err != nil {
							logger.Error(fmt.Sprintf("Table image render failed: %v", err))
							continue
						}
					}
					tableImages = append(tableImages, img)
				}
			}
		}
		body = markdown.RenderTelegramHTML(body)
	}
	logger.Debug(fmt.Sprintf("TG message [%s] full_body:\n%s", event, body))
	headerLen := notify.HeaderLen(nd)
	paginationMax := 4000
	if cfg.PaginationMaxRunes > 0 {
		paginationMax = cfg.PaginationMaxRunes
	}
	maxBodyRunes := paginationMax - headerLen - 100
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
			logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s body_len=%d body=%s", chatID, event, project, tmuxTarget, len([]rune(body)), helpers.TruncateStr(body, 200)))
			logger.Debug(fmt.Sprintf("TG message sent [%s] full_text:\n%s", event, text))
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
			logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s (%d pages, msg_id=%d) body_len=%d body=%s", chatID, event, project, tmuxTarget, len(chunks), sent.ID, len([]rune(body)), helpers.TruncateStr(body, 200)))
			logger.Debug(fmt.Sprintf("TG message sent [%s] page=1/%d full_text:\n%s", event, len(chunks), text))
		}
	}
	// Send table images as separate Photo messages
	for i, imgBytes := range tableImages {
		photo := &tele.Photo{
			File: tele.FromReader(bytes.NewReader(imgBytes)),
		}
		var photoOpts []interface{}
		if topicID > 0 {
			photoOpts = append(photoOpts, &tele.SendOptions{ThreadID: topicID})
		}
		_, err := helpers.RetrySend(b, chat, photo, photoOpts...)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send table image %d: %v", i+1, err))
		} else {
			logger.Info(fmt.Sprintf("Table image %d sent to chat %s for event %s", i+1, chatID, event))
		}
	}
	return sentMsgID
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

// flushInjectQueue merges all queued items for a target and injects as one combined message.
// Handles three states: idle → inject, AskQ → answer, PermReq → skip (keep queue).
func flushInjectQueue(bs *BotState, tmuxTarget string) {
	if !bs.InjectQueue.HasItems(tmuxTarget) {
		return
	}
	// Check PermissionRequest — if pending, do NOT flush (keep queue for later)
	if _, ok := bs.PendingPerms.FindByTmuxTarget(tmuxTarget); ok {
		logger.Info(fmt.Sprintf("flushInjectQueue: PermissionRequest pending, keeping queue for target=%s", tmuxTarget))
		return
	}
	// Capture notify message ID and inject ID before flush clears them
	notifyMsgID, hasNotify := bs.InjectQueue.GetNotifyMsg(tmuxTarget)
	injectID := bs.InjectQueue.GetInjectID(tmuxTarget)
	items := bs.InjectQueue.Flush(tmuxTarget)
	if len(items) == 0 {
		return
	}
	// Merge all items into one text
	var texts []string
	for _, item := range items {
		texts = append(texts, item.Text)
	}
	merged := strings.Join(texts, "\n")
	logger.Info(fmt.Sprintf("flushInjectQueue: merging %d items for target=%s merged_len=%d", len(items), tmuxTarget, len(merged)))
	// Resolve chat for TG notification updates
	chat, _, topicID := resolveChat(bs, tmuxTarget)
	if injectID == "" {
		injectID = fmt.Sprintf("%x", time.Now().UnixNano()%0xFFFFFF)
	}
	logger.Info(fmt.Sprintf("flushInjectQueue: [%s] starting flush for target=%s items=%d", injectID, tmuxTarget, len(items)))
	// Build message list for notifications (with delimiters)
	msgContent := "──────\n" + strings.Join(texts, "\n") + "\n──────"
	// Inject the merged text in a goroutine to avoid blocking the hook handler
	go func(target, text, id, msgList string, itemCount int, notifyID int, hasNotifyMsg bool, chat *tele.Chat, topicID int) {
		// Wait for Stop hook to finish (CC returns to idle after hook exits)
		bs.StopCooldown.WaitIfNeeded(target, 1500*time.Millisecond)
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
			ResolveChat: func(t string) (*tele.Chat, string, int) {
				return resolveChat(bs, t)
			},
			FormatPaneID: notify.FormatPaneID,
			Force:        true,
		}
		if err := helpers.SafeInjectText(p, target, text); err != nil {
			logger.Error(fmt.Sprintf("flushInjectQueue: [%s] inject failed: target=%s err=%v", id, target, err))
			if hasNotifyMsg && chat != nil {
				editMsg := &tele.Message{ID: notifyID, Chat: chat}
				helpers.RetryEdit(bs.Bot, editMsg, fmt.Sprintf("❌ Inject failed [%s] (%d)\n📟 %s\n%s", id, itemCount, notify.FormatPaneID(target), msgList))
			}
			return
		}
		// SafeInjectText handles confirmation internally (CapturePane/PostToolUse)
		logger.Info(fmt.Sprintf("flushInjectQueue: [%s] inject completed: target=%s", id, target))
		if hasNotifyMsg && chat != nil {
			editMsg := &tele.Message{ID: notifyID, Chat: chat}
			helpers.RetryEdit(bs.Bot, editMsg, fmt.Sprintf("✅ Injected [%s] (%d)\n📟 %s\n%s", id, itemCount, notify.FormatPaneID(target), msgList))
		}
	}(tmuxTarget, merged, injectID, msgContent, len(items), notifyMsgID, hasNotify, chat, topicID)
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
	text := fmt.Sprintf("🆕 CC update available\n📟 %s\n\n<b>%s</b> → <b>%s</b>", paneLabel, current, latest)
	var sendOpts []interface{}
	sendOpts = append(sendOpts, sel, tele.ModeHTML)
	if topicID > 0 {
		sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
	}
	helpers.RetrySend(bs.Bot, chat, text, sendOpts...)
}


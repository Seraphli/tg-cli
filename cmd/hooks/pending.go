package hooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// Callbacks holds cmd-level functions injected into hooks to avoid circular imports.
type Callbacks struct {
	ResolveChat              func(bs *types.BotState, tmuxTarget string) (*tele.Chat, string, int)
	ProcessTranscriptUpdates func(bs *types.BotState, sessionID, transcriptPath string, isQuestion ...bool) string
	SendEventNotification    func(bs *types.BotState, chat *tele.Chat, chatID, sessionID, event, project, cwd, tmuxTarget, body, toolName, agentName string, topicID int) int
	TypingLog                func(format string, args ...interface{})
	FlushInjectQueue         func(bs *types.BotState, tmuxTarget string)
	CheckSessionVersion      func(bs *types.BotState, tmuxTarget string)
	StreamFlush              func(bs *types.BotState, sessionID string, stop bool)
}

// GetHookSessionLock returns (or creates) the mutex for a session.
func GetHookSessionLock(bs *types.BotState, sessionID string) *sync.Mutex {
	v, _ := bs.HookSessionLocks.LoadOrStore(sessionID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// CancelPendingWaitBySession cancels all unresolved pending wait entries for a session.
// Used by Stop/PreToolUse/UserPromptSubmit/SessionEnd to signal CC has moved on.
// No Remove — live handler / sweep removes after delivery.
func CancelPendingWaitBySession(bs *types.BotState, sessionID string) {
	if sessionID == "" {
		return
	}
	for _, entry := range bs.PendingWait.FindBySession(sessionID) {
		if entry.Resolved {
			continue
		}
		// Freeze TG button to show "⌨️ Answered on desktop"
		helpers.FreezeWaitEntryOnDesktop(bs.Bot, bs.ToolNotifs, bs.PendingPerms, entry, "⌨️ Answered on desktop")
		if notifEntry, ok := bs.ToolNotifs.Get(entry.MsgID); ok && !notifEntry.Resolved {
			bs.ToolNotifs.MarkResolved(entry.MsgID)
		}
		if _, ok := bs.PendingPerms.GetTarget(entry.MsgID); ok {
			bs.PendingPerms.Resolve(entry.MsgID, stores.PermDecision{Behavior: "allow", Message: "Answered on desktop"})
		}
		bs.PendingWait.Push(entry.UUID, stores.WaitEvent{Type: "cancel"})
		bs.PendingFiles.Remove(entry.MsgID)
		logger.Info(fmt.Sprintf("CancelPendingWaitBySession: uuid=%s session=%s", entry.UUID, sessionID))
	}
}

// sendPendingMessage posts the TG question/permission button message for a new pending entry.
// Returns msgID, chatID, topicID. The entry fields (SessionID, TmuxTarget, ToolName, etc.) must be set.
func sendPendingMessage(bs *types.BotState, cb Callbacks, entry *stores.PendingWaitEntry) (int, int64, int, error) {
	var p helpers.HookPayload
	if err := json.Unmarshal(entry.Payload, &p); err != nil {
		return 0, 0, 0, fmt.Errorf("unmarshal payload: %w", err)
	}
	p.TmuxTarget = notify.FormatPaneID(p.TmuxTarget)
	cwdForRoute := p.CWD
	info := bs.SessionState.FindInfoByTarget(p.TmuxTarget)
	if info != nil && info.CWD != "" {
		cwdForRoute = info.CWD
	}
	agentName := ""
	if info != nil {
		agentName = info.Name
	}
	chat, chatID, topicID := cb.ResolveChat(bs, p.TmuxTarget)
	if chat == nil {
		return 0, 0, 0, fmt.Errorf("no chat for target %s", p.TmuxTarget)
	}
	if p.AgentID == "" {
		if p.Backend == "codex" {
			if updateBody := cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath, true); updateBody != "" {
				chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
				cb.SendEventNotification(bs, chat, chatID, p.SessionID, "PreToolUse", p.Project, cwdForRoute, p.TmuxTarget, updateBody, "", agentName, topicID)
				bs.CompactTools.Reset(p.SessionID)
				logger.Info(fmt.Sprintf("PreToolUse Update sent for pending request %s (chat=%d)", entry.UUID, chatIDInt))
			}
		} else {
			cb.StreamFlush(bs, p.SessionID, false)
		}
	}
	if p.ToolName == "AskUserQuestion" {
		var askInput struct {
			Questions []struct {
				Header   string `json:"header"`
				Question string `json:"question"`
				Options  []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
				MultiSelect bool `json:"multiSelect"`
			} `json:"questions"`
		}
		json.Unmarshal(p.ToolInput, &askInput)
		if len(askInput.Questions) == 0 {
			return 0, 0, 0, fmt.Errorf("no questions in payload")
		}
		var qMetas []stores.QuestionMeta
		var questionEntries []notify.QuestionEntry
		for _, q := range askInput.Questions {
			var opts []notify.QuestionOption
			var labels []string
			for _, o := range q.Options {
				opts = append(opts, notify.QuestionOption{Label: o.Label, Description: o.Description})
				labels = append(labels, o.Label)
			}
			qMetas = append(qMetas, stores.QuestionMeta{
				QuestionText: q.Question, Header: q.Header,
				NumOptions: len(q.Options), OptionLabels: labels,
				MultiSelect: q.MultiSelect, SelectedOptions: make(map[int]bool),
				SelectedOption: -1,
			})
			questionEntries = append(questionEntries, notify.QuestionEntry{
				Header: q.Header, Question: q.Question, Options: opts, MultiSelect: q.MultiSelect,
			})
		}
		ctxPct, ctxUsed, ctxWindow, ctxOk := helpers.ReadContextUsage(p.SessionID)
		qData := notify.QuestionData{
			Project: p.Project, CWD: cwdForRoute, TmuxTarget: p.TmuxTarget, Questions: questionEntries,
			AgentName: agentName, CLICommand: helpers.GetPaneCLICommand(p.TmuxTarget), ContextUsedPct: -1,
		}
		if ctxOk {
			qData.ContextUsedPct = ctxPct
			qData.ContextUsedTokens = ctxUsed
			qData.ContextWindowSize = ctxWindow
		}
		text := notify.BuildQuestionText(qData)
		markup := &tele.ReplyMarkup{}
		var rows []tele.Row
		hasSubmit := len(askInput.Questions) > 1
		for _, q := range askInput.Questions {
			if q.MultiSelect {
				hasSubmit = true
			}
		}
		btnToolCancel := markup.Data("❌ Cancel", "tool", "AskUserQuestion|cancel")
		if len(askInput.Questions) == 1 && !askInput.Questions[0].MultiSelect {
			q := askInput.Questions[0]
			var buttons []tele.Btn
			for i, o := range q.Options {
				buttons = append(buttons, markup.Data(o.Label, "tool", fmt.Sprintf("AskUserQuestion|0:%d", i)))
			}
			for i := 0; i < len(buttons); i += 2 {
				if i+1 < len(buttons) {
					rows = append(rows, markup.Row(buttons[i], buttons[i+1]))
				} else {
					rows = append(rows, markup.Row(buttons[i]))
				}
			}
			chatBtn := markup.Data("💬 Chat about this", "tool", "AskUserQuestion|chat")
			rows = append(rows, markup.Row(chatBtn))
		} else {
			for qIdx, q := range askInput.Questions {
				for optIdx, o := range q.Options {
					label := o.Label
					if len(askInput.Questions) > 1 {
						label = fmt.Sprintf("Q%d: %s", qIdx+1, o.Label)
					}
					rows = append(rows, markup.Row(markup.Data(label, "tool", fmt.Sprintf("AskUserQuestion|%d:%d", qIdx, optIdx))))
				}
			}
			if hasSubmit {
				rows = append(rows, markup.Row(markup.Data("📤 Submit", "tool", "AskUserQuestion|submit")))
			}
			rows = append(rows, markup.Row(markup.Data("💬 Chat about this", "tool", "AskUserQuestion|chat")))
		}
		rows = append(rows, markup.Row(btnToolCancel))
		markup.Inline(rows...)
		var askSendOpts []interface{}
		askSendOpts = append(askSendOpts, markup)
		if topicID > 0 {
			askSendOpts = append(askSendOpts, &tele.SendOptions{ThreadID: topicID})
		}
		askSendOpts = append(askSendOpts, tele.ModeHTML)
		sent, err := helpers.RetrySend(bs.Bot, chat, text, askSendOpts...)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("send AskUserQuestion: %w", err)
		}
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		bs.ToolNotifs.Store(sent.ID, &stores.ToolNotifyEntry{
			TmuxTarget: p.TmuxTarget, ToolName: "AskUserQuestion",
			Questions: qMetas, ChatID: chatIDInt, MsgText: text,
			PendingUUID: entry.UUID,
		})
		var qSummaries []string
		for _, q := range askInput.Questions {
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			qSummaries = append(qSummaries, fmt.Sprintf("%s:[%s]", q.Header, strings.Join(labels, ",")))
		}
		contentSummary := strings.Join(qSummaries, " | ")
		logger.Info(fmt.Sprintf("TG question message sent full_text:\n%s", text))
		logger.Info(fmt.Sprintf("AskUserQuestion sent: msg_id=%d questions=%d tmux=%s content=%s uuid=%s", sent.ID, len(askInput.Questions), p.TmuxTarget, contentSummary, entry.UUID))
		// Forward AskUserQuestion to @ channel peers
		if agentName != "" {
			atTargets := bs.AtChannels.GetTargets(agentName)
			buffered := bs.AtChannels.FlushBufferEntries(agentName)
			cfg, _ := config.LoadAppConfig()
			dn := cfg.DisplayName
			if dn == "" {
				dn = "User"
			}
			for _, peerName := range atTargets {
				peerInfo := bs.SessionState.FindByName(peerName)
				if peerInfo == nil {
					continue
				}
				var contentLines []string
				for _, e := range buffered {
					contentLines = append(contentLines, fmt.Sprintf("[%s → %s]: %s", agentName, dn, e))
				}
				for _, q := range askInput.Questions {
					contentLines = append(contentLines, fmt.Sprintf("[%s → %s]: ❓ %s", agentName, dn, q.Header))
					contentLines = append(contentLines, q.Question)
					for _, o := range q.Options {
						contentLines = append(contentLines, fmt.Sprintf("- %s — %s", o.Label, o.Description))
					}
				}
				content := strings.Join(contentLines, "\n")
				instructions := fmt.Sprintf("`%s` is asking a question. Below is the update and question.", agentName)
				msg := helpers.BuildAtMsg(agentName, peerName, instructions, content)
				peerChat, _, peerTopicID := cb.ResolveChat(bs, peerInfo.TmuxTarget)
				if peerChat != nil {
					var fwdOpts []interface{}
					if peerTopicID > 0 {
						fwdOpts = append(fwdOpts, &tele.SendOptions{ThreadID: peerTopicID})
					}
					targetHeader := helpers.BuildAtHeader(agentName, peerName) + "\n---\n" + instructions + "\n---\n"
					helpers.SendPagedForward(bs.Bot, peerChat, targetHeader, content, bs.Pages, "", fwdOpts...)
				}
				go func(target, txt string) {
					injectP := helpers.SafeInjectTextParams{
						Bot:              bs.Bot,
						ToolNotifs:       bs.ToolNotifs,
						PendingFiles:     bs.PendingFiles,
						PendingWait:      bs.PendingWait,
						PendingPerms:     bs.PendingPerms,
						InjectQueue:      bs.InjectQueue,
						InjectConfirm:    bs.InjectConfirm,
						StopCooldown:     bs.StopCooldown,
						ReactionTracker:  bs.ReactionTracker,
						SessionState:     bs.SessionState,
						HookSessionLocks: &bs.HookSessionLocks,
						SessionEvents:    bs.SessionEvents,
						ResolveChat: func(t string) (*tele.Chat, string, int) {
							return cb.ResolveChat(bs, t)
						},
						FormatPaneID: notify.FormatPaneID,
					}
					helpers.SafeInjectText(injectP, target, txt)
				}(peerInfo.TmuxTarget, msg)
			}
		}
		bs.SessionWatch.Notify(agentName, stores.WatchEvent{
			Event:   "AskUserQuestion",
			Agent:   agentName,
			Summary: contentSummary,
			Detail:  text,
		})
		return sent.ID, chatIDInt, topicID, nil
	}
	// PermissionRequest
	logger.Info(fmt.Sprintf("Permission request: tool=%s project=%s uuid=%s", p.ToolName, p.Project, entry.UUID))
	var toolInput map[string]interface{}
	json.Unmarshal(p.ToolInput, &toolInput)
	logger.Info(fmt.Sprintf("Permission payload: toolInput=%s suggestions=%s", string(p.ToolInput), string(p.PermSuggestions)))
	btnLabel, sugDesc := helpers.ParseSuggestionLabel(p.PermSuggestions)
	text := notify.BuildPermissionText(notify.PermissionData{
		Project: p.Project, CWD: cwdForRoute, TmuxTarget: p.TmuxTarget,
		ToolName: p.ToolName, ToolInput: toolInput, SuggestionDesc: sugDesc,
		AgentName: agentName, CLICommand: helpers.GetPaneCLICommand(p.TmuxTarget),
	})
	markup := &tele.ReplyMarkup{}
	row1 := []tele.Btn{
		markup.Data("Allow", "perm", "allow"),
		markup.Data("Deny", "perm", "deny"),
	}
	btnPermCancel := markup.Data("❌ Cancel", "perm", "cancel")
	var permBtnRows []tele.Row
	permBtnRows = append(permBtnRows, row1)
	if btnLabel != "" {
		row2 := []tele.Btn{markup.Data(btnLabel, "perm", "sAll")}
		permBtnRows = append(permBtnRows, row2)
	}
	permBtnRows = append(permBtnRows, markup.Row(btnPermCancel))
	permChunks := helpers.SplitBody(text, 3900)
	if len(permChunks) <= 1 {
		if btnLabel != "" {
			markup.Inline(markup.Row(row1...), markup.Row(markup.Data(btnLabel, "perm", "sAll")), markup.Row(btnPermCancel))
		} else {
			markup.Inline(markup.Row(row1...), markup.Row(btnPermCancel))
		}
	} else {
		text = permChunks[0] + fmt.Sprintf("\n\n📄 1/%d", len(permChunks))
		kb := helpers.BuildPageKeyboardWithExtra(1, len(permChunks), permBtnRows)
		markup = kb
	}
	var permSendOpts []interface{}
	permSendOpts = append(permSendOpts, markup)
	if topicID > 0 {
		permSendOpts = append(permSendOpts, &tele.SendOptions{ThreadID: topicID})
	}
	permSendOpts = append(permSendOpts, tele.ModeHTML)
	sent, err := helpers.RetrySend(bs.Bot, chat, text, permSendOpts...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("send PermissionRequest: %w", err)
	}
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	if len(permChunks) > 1 {
		bs.Pages.Store(sent.ID, p.SessionID, &stores.PageEntry{
			Chunks:     permChunks,
			Event:      "PermissionRequest",
			Project:    p.Project,
			CWD:        cwdForRoute,
			TmuxTarget: p.TmuxTarget,
			PermRows:   permBtnRows,
			ChatID:     chatIDInt,
		})
	}
	logger.Info(fmt.Sprintf("Permission request sent: tool=%s project=%s tmux=%s (msg_id=%d pages=%d) uuid=%s", p.ToolName, p.Project, p.TmuxTarget, sent.ID, len(permChunks), entry.UUID))
	logger.Info(fmt.Sprintf("TG permission message sent full_text:\n%s", text))
	bs.SessionWatch.Notify(agentName, stores.WatchEvent{
		Event:   "PermissionRequest",
		Agent:   agentName,
		Summary: fmt.Sprintf("Tool: %s", p.ToolName),
		Detail:  text,
	})
	bs.PendingPerms.Create(sent.ID, p.TmuxTarget, p.PermSuggestions, text, chatIDInt, entry.UUID)
	return sent.ID, chatIDInt, topicID, nil
}

// pendingConnectHandler handles POST /pending/connect — streaming long-connection for blocking events.
func pendingConnectHandler(bs *types.BotState, cb Callbacks) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		uuid := r.URL.Query().Get("uuid")
		if uuid == "" {
			http.Error(w, "missing uuid", 400)
			return
		}
		// Optional reconnect params
		msgIDStr := r.URL.Query().Get("msg_id")
		chatIDStr := r.URL.Query().Get("chat_id")
		topicIDStr := r.URL.Query().Get("topic_id")
		msgIDQ, _ := strconv.Atoi(msgIDStr)
		chatIDQ, _ := strconv.ParseInt(chatIDStr, 10, 64)
		topicIDQ, _ := strconv.Atoi(topicIDStr)

		// Decode body as raw JSON (keep both the struct and raw for entry.Payload)
		var raw json.RawMessage
		var p helpers.HookPayload
		bodyBytes := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				bodyBytes = append(bodyBytes, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		raw = json.RawMessage(bodyBytes)
		if err := json.Unmarshal(bodyBytes, &p); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		p.TmuxTarget = notify.FormatPaneID(p.TmuxTarget)

		// Set up chunked streaming
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		rc := http.NewResponseController(w)
		send := func(v any) error {
			data, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if _, err := w.Write(append(data, '\n')); err != nil {
				return err
			}
			return rc.Flush()
		}

		gen := bs.PendingWait.BumpGeneration(uuid)
		existing, exists := bs.PendingWait.Get(uuid)
		isReconnect := exists || msgIDQ != 0

		var entry *stores.PendingWaitEntry
		if isReconnect && exists {
			// Reconnect with live store entry
			entry = existing
			logger.Info(fmt.Sprintf("hook reattached: uuid=%s msg_id=%d", uuid, entry.MsgID))
		} else if isReconnect && msgIDQ != 0 {
			// Reconnect after bot restart — store was wiped, rebuild from supplied params
			entry = &stores.PendingWaitEntry{
				UUID:               uuid,
				MsgID:              msgIDQ,
				ChatID:             chatIDQ,
				TopicID:            topicIDQ,
				ToolName:           p.ToolName,
				ToolUseID:          p.ToolUseID,
				ToolInputCanonical: helpers.CanonicalToolInput(p.ToolInput),
				SessionID:          p.SessionID,
				TmuxTarget:         p.TmuxTarget,
				Payload:            raw,
			}
			bs.PendingWait.Register(entry)
			bs.PendingFiles.Store(entry.MsgID, uuid)
			// Rebuild in-memory button state (toolNotifs / pendingPerms)
			if rbErr := helpers.RebuildInMemoryState(bs.ToolNotifs, bs.PendingFiles, bs.PendingPerms, entry.MsgID, entry.ChatID, entry.TopicID, raw, entry.ToolName, uuid, p.TmuxTarget, notify.FormatPaneID); rbErr != nil {
				logger.Error(fmt.Sprintf("pendingConnect reattach: rebuild failed uuid=%s err=%v", uuid, rbErr))
			}
			logger.Info(fmt.Sprintf("hook reattached (restart): uuid=%s msg_id=%d", uuid, entry.MsgID))
		} else {
			// First connect — build entry and send TG message
			entry = &stores.PendingWaitEntry{
				UUID:               uuid,
				ToolName:           p.ToolName,
				ToolUseID:          p.ToolUseID,
				ToolInputCanonical: helpers.CanonicalToolInput(p.ToolInput),
				SessionID:          p.SessionID,
				TmuxTarget:         p.TmuxTarget,
				Payload:            raw,
			}
			bs.PendingWait.Register(entry)
			msgID, chatID, topicID, err := sendPendingMessage(bs, cb, entry)
			if err != nil {
				logger.Error(fmt.Sprintf("pendingConnect: sendPendingMessage failed uuid=%s err=%v", uuid, err))
				bs.PendingWait.Remove(uuid)
				http.Error(w, "failed to send TG message", 500)
				return
			}
			entry.MsgID = msgID
			entry.ChatID = chatID
			entry.TopicID = topicID
			bs.PendingFiles.Store(msgID, uuid)
			logger.Info(fmt.Sprintf("hook connected: uuid=%s tool=%s msg_id=%d", uuid, p.ToolName, msgID))
		}

		// Deliver any terminal that arrived while no handler was live
		if term := bs.PendingWait.TakeTerminal(uuid); term != nil {
			if send(*term) == nil {
				bs.PendingWait.Remove(uuid)
				bs.PendingFiles.Remove(entry.MsgID)
			} else {
				bs.PendingWait.Push(uuid, *term)
			}
			return
		}

		// Send registered ack so hook knows msg_id
		type registeredMsg struct {
			Type    string `json:"type"`
			MsgID   int    `json:"msg_id"`
			ChatID  int64  `json:"chat_id"`
			TopicID int    `json:"topic_id"`
		}
		if err := send(registeredMsg{Type: "registered", MsgID: entry.MsgID, ChatID: entry.ChatID, TopicID: entry.TopicID}); err != nil {
			logger.Error(fmt.Sprintf("pendingConnect: send registered failed uuid=%s err=%v", uuid, err))
			return
		}

		// Set live and wait for answer or disconnect
		bs.PendingWait.SetLive(uuid, gen, entry.Ch)
		select {
		case ev := <-entry.Ch:
			bs.PendingWait.ClearLive(uuid, gen)
			if send(ev) == nil {
				bs.PendingWait.Remove(uuid)
				bs.PendingFiles.Remove(entry.MsgID)
			} else {
				bs.PendingWait.Push(uuid, ev)
			}
		case <-r.Context().Done():
			bs.PendingWait.ClearLive(uuid, gen)
			logger.Info(fmt.Sprintf("hook disconnected: uuid=%s", uuid))
			if config.UpgradeFlagActive() {
				// Upgrade in progress — hook will reconnect; keep entry alive
				return
			}
			// 3-second grace period: if hook reconnects, new generation clears the timer
			capturedGen := gen
			capturedUUID := uuid
			capturedMsgID := entry.MsgID
			go func() {
				time.Sleep(3 * time.Second)
				if bs.PendingWait.CurrentGeneration(capturedUUID) != capturedGen {
					return // hook reconnected — new handler owns it
				}
				if waitE, ok := bs.PendingWait.Get(capturedUUID); ok && !waitE.Resolved {
					// Hook stayed away — cancel
					helpers.FreezeWaitEntryOnDesktop(bs.Bot, bs.ToolNotifs, bs.PendingPerms, waitE, "❌ Cancelled")
					if notifE, nok := bs.ToolNotifs.Get(capturedMsgID); nok && !notifE.Resolved {
						bs.ToolNotifs.MarkResolved(capturedMsgID)
					}
					bs.PendingWait.Remove(capturedUUID)
					bs.PendingFiles.Remove(capturedMsgID)
					logger.Info(fmt.Sprintf("pendingConnect grace expired: cancelled uuid=%s", capturedUUID))
				}
			}()
		}
	}
}

// ScanPendingDir scans for launch state files on bot startup for crash recovery.
// File-based pending handling is removed; reconnecting hooks use /pending/connect.
func ScanPendingDir(bs *types.BotState, cb Callbacks, scanLaunchDir func(*types.BotState)) {
	// Scan launch state files for /bot_new crash recovery
	scanLaunchDir(bs)
}

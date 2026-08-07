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
	// RouteInjectQueue is the event-driven inject-queue trigger (R2/R5): on an MD-final it claims the target
	// exactly-once, waits up to 5s for the next hook event (PreToolUse/Stop delivered via InjectRoute.SignalEvent),
	// routes to the delivery mode, and dispatches the DELIVERY off the Hook FIFO.
	RouteInjectQueue        func(bs *types.BotState, tmuxTarget, toolUseID string)
	CheckSessionVersion     func(bs *types.BotState, tmuxTarget string)
	StreamFlush             func(bs *types.BotState, sessionID string, stop bool)
	StreamFlushAwaitNewText func(bs *types.BotState, sessionID string)
	// FlushStreamOp is the plain NON-DRAINING pre-tool flush (S6): it snapshots the stream under DataMu and
	// enqueues ONE render op onto the Message FIFO (async) WITHOUT any drainUntilComplete grace. The S6
	// queue-lookahead already drained/waited for the pre-tool MessageDisplay on the Hook FIFO, so this must
	// NOT re-drain (that would be the StreamFlush drain-then-flush path). No-op when nothing is renderable.
	FlushStreamOp func(bs *types.BotState, sessionID string)
}

// GetHookSessionLock returns (or creates) the mutex for a session.
func GetHookSessionLock(bs *types.BotState, sessionID string) *sync.Mutex {
	v, _ := bs.HookSessionLocks.LoadOrStore(sessionID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// CancelPendingWaitBySession cancels all unresolved pending wait entries for a session.
// Used by Stop/PreToolUse/UserPromptSubmit/SessionEnd to signal CC has moved on.
// Uses snapshots to avoid holding PendingWait's read lock while calling FreezeWaitEntryOnDesktop.
// No Remove — live handler / sweep removes after delivery.
//
// Round 8: skipToolName/skipCanonicalInput/skipToolUseID identify the CALLER's own tool (PreToolUse passes
// its tool). Under async hooks a tool's PermissionRequest /pending/connect can register its entry BEFORE the
// tool's PreToolUse is processed on the FIFO — without the skip, PreToolUse would cancel the permission prompt
// it is about to need. The skip entry is resolved with the same key FindMatch uses at the PostToolUse handler.
// Non-tool callers (Stop/UserPromptSubmit/SessionEnd) pass empty strings = cancel everything (no skip).
func CancelPendingWaitBySession(bs *types.BotState, sessionID, skipToolName, skipCanonicalInput, skipToolUseID string) {
	if sessionID == "" {
		return
	}
	skipUUID := ""
	if skipToolName != "" || skipToolUseID != "" {
		if m, ok := bs.PendingWait.FindMatch(sessionID, skipToolName, skipCanonicalInput, skipToolUseID); ok {
			skipUUID = m.UUID
		}
	}
	for _, snap := range bs.PendingWait.FindBySessionSnapshots(sessionID) {
		if snap.Resolved {
			continue
		}
		if skipUUID != "" && snap.UUID == skipUUID {
			continue // this tool's own pending entry — do not cancel it
		}
		// FreezeWaitEntryOnDesktop handles ResolveIfUnresolved (state, Hook FIFO) + EditOrDefer that enqueues
		// msg:freeze-edit onto the Message FIFO (B.8)
		helpers.FreezeWaitEntryOnDesktop(bs.Bot, bs.MessageQueue, bs.PendingWait, bs.PendingMsgStore, snap, "⌨️ Answered on desktop")
		logger.Info(fmt.Sprintf("CancelPendingWaitBySession: uuid=%s session=%s", snap.UUID, sessionID))
	}
}

// parseEntryFields parses payload fields (Questions, MsgText, PermSuggestions) from a raw payload
// and sets them on the entry. Called before Register so the entry is complete from the start.
func parseEntryFields(entry *stores.PendingWaitEntry, raw json.RawMessage, p *helpers.HookPayload) {
	entry.PermSuggestions = p.PermSuggestions
	if entry.ToolName != "AskUserQuestion" {
		return
	}
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
	if err := json.Unmarshal(p.ToolInput, &askInput); err != nil || len(askInput.Questions) == 0 {
		return
	}
	var qMetas []stores.QuestionMeta
	for _, q := range askInput.Questions {
		var labels []string
		for _, o := range q.Options {
			labels = append(labels, o.Label)
		}
		qMetas = append(qMetas, stores.QuestionMeta{
			QuestionText: q.Question, Header: q.Header,
			NumOptions: len(q.Options), OptionLabels: labels,
			MultiSelect: q.MultiSelect, SelectedOptions: make(map[int]bool),
			SelectedOption: -1,
		})
	}
	entry.Questions = qMetas
}

// preparePendingMessage builds a send closure for a new pending entry. ZERO TG network I/O in this function.
// Returns a func() error that: executes the TG SEND → on success calls PendingMsgStore.SetAndDrain +
// PendingWait.BackfillMsgID + sends "update" frame. On failure calls PendingMsgStore.Delete.
func preparePendingMessage(bs *types.BotState, cb Callbacks, entry *stores.PendingWaitEntry, sendFrame func(any) error) (func() error, error) {
	var p helpers.HookPayload
	if err := json.Unmarshal(entry.Payload, &p); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
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
		return nil, fmt.Errorf("no chat for target %s", p.TmuxTarget)
	}
	capturedChat := chat
	capturedChatID := chatID
	capturedTopicID := topicID
	capturedEntry := entry

	sendFn := func() error {
		result := executeSendPendingMessage(bs, cb, capturedEntry, capturedChat, capturedChatID, capturedTopicID, cwdForRoute, agentName, &p)
		if result.MainErr != nil {
			// Send failed — mark closed so deferred EDITs are discarded
			bs.PendingMsgStore.Delete(capturedEntry.UUID)
			return result.MainErr
		}
		// Store TG coordinates in PendingMsgStore (drains any deferred EDIT)
		bs.PendingMsgStore.SetAndDrain(capturedEntry.UUID, result.MsgID, result.ChatID, result.MsgText, result.TopicID)
		// Backfill MsgID/ChatID/TopicID/MsgText into PendingWait for TG-button callback correlation
		bs.PendingWait.BackfillMsgID(capturedEntry.UUID, result.MsgID, result.ChatID, result.TopicID, result.MsgText)
		// Send "update" frame to the ndjson stream (includes Rich so reconnect can restore it)
		type updateMsg struct {
			Type    string `json:"type"`
			MsgID   int    `json:"msg_id"`
			ChatID  int64  `json:"chat_id"`
			TopicID int    `json:"topic_id"`
			MsgText string `json:"msg_text,omitempty"`
			Rich    bool   `json:"rich,omitempty"`
		}
		entryRich := capturedEntry.Rich
		if err := sendFrame(updateMsg{Type: "update", MsgID: result.MsgID, ChatID: result.ChatID, TopicID: result.TopicID, MsgText: result.MsgText, Rich: entryRich}); err != nil {
			logger.Debug(fmt.Sprintf("preparePendingMessage: sendFrame update failed uuid=%s err=%v", capturedEntry.UUID, err))
		}
		return nil
	}
	return sendFn, nil
}

// executeSendPendingMessage performs the actual TG network I/O for sending the pending message.
// Called from inside the send closure dispatched via SessionEvents.TryDispatchAsync.
func executeSendPendingMessage(bs *types.BotState, cb Callbacks, entry *stores.PendingWaitEntry, chat *tele.Chat, chatID string, topicID int, cwdForRoute, agentName string, p *helpers.HookPayload) stores.SendResult {
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	if p.AgentID == "" {
		if p.Backend == "codex" {
			if updateBody := cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath, true); updateBody != "" {
				// §F #8: executeSendPendingMessage runs ON the Hook FIFO (send:pending job), so the codex Update
				// blocking send must go onto the Message FIFO. DispatchAsync in Hook-FIFO order (the msg_id is
				// not reused). The compact reset + log stay on the Hook FIFO (state ordering).
				updChat := chat
				updChatID := chatID
				updBody := updateBody
				bs.MessageQueue.DispatchAsync(p.SessionID, "msg:codex-update", func() error {
					cb.SendEventNotification(bs, updChat, updChatID, p.SessionID, "PreToolUse", p.Project, cwdForRoute, p.TmuxTarget, updBody, "", agentName, topicID)
					return nil
				})
				// S9a: discard the compact entry (Hook FIFO) + schedule the ordered map-delete (Message FIFO).
				resetCompactAndScheduleDelete(bs, p.SessionID)
				logger.Info(fmt.Sprintf("PreToolUse Update sent for pending request %s (chat=%d)", entry.UUID, chatIDInt))
			}
		} else {
			cb.StreamFlushAwaitNewText(bs, p.SessionID)
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
			return stores.SendResult{MainErr: fmt.Errorf("no questions in payload")}
		}
		var questionEntries []notify.QuestionEntry
		for _, q := range askInput.Questions {
			var opts []notify.QuestionOption
			for _, o := range q.Options {
				opts = append(opts, notify.QuestionOption{Label: o.Label, Description: o.Description})
			}
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
		// R5 (S11): route the blocking AskUserQuestion SEND onto the Message FIFO via a SYNC Dispatch —
		// executeSendPendingMessage runs ON the Hook FIFO (send:pending job), so per INV2 any blocking send
		// reachable on the Hook FIFO must go onto the Message FIFO. SYNC because the caller needs the returned
		// TG msg_id for the pending store (AskQ keeps using the TG msg_id directly — NOT the internal-id
		// mechanism, which is compact/tool-notify only). Rich send: question/options are prose → keep entity
		// detection ON (C3).
		var sent *tele.Message
		var err error
		bs.MessageQueue.Dispatch(p.SessionID, "msg:ask-send", func() error {
			sent, err = helpers.RetrySendRich(bs.Bot, chat, text, helpers.RichSendOpts{
				TopicID:    topicID,
				Markup:     markup,
				LegacyHTML: text,
			})
			return nil
		})
		if err != nil {
			return stores.SendResult{MainErr: fmt.Errorf("send AskUserQuestion: %w", err)}
		}
		entry.Rich = true
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
		logger.Info(fmt.Sprintf("AskUserQuestion sent: msg_id=%d questions=%d tmux=%s content=%s uuid=%s fmt=rich", sent.ID, len(askInput.Questions), p.TmuxTarget, contentSummary, entry.UUID))
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
				var historyLines []string
				for _, e := range buffered {
					historyLines = append(historyLines, fmt.Sprintf("[%s → %s]: %s", agentName, dn, e))
				}
				var triggerLines []string
				for _, q := range askInput.Questions {
					triggerLines = append(triggerLines, fmt.Sprintf("[%s → %s]: ❓ %s", agentName, dn, q.Header))
					triggerLines = append(triggerLines, q.Question)
					for _, o := range q.Options {
						triggerLines = append(triggerLines, fmt.Sprintf("- %s — %s", o.Label, o.Description))
					}
				}
				content := helpers.BuildAtForwardContent(strings.Join(historyLines, "\n"), strings.Join(triggerLines, "\n"))
				instructions := fmt.Sprintf("`%s` is asking a question. The message below has a READ-ONLY PRIOR CONTEXT block (lines prefixed `HISTORY> `, reference only) and a LIVE TRIGGER block (lines prefixed `TRIGGER> `) with the question.", agentName)
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
						PendingWait:      bs.PendingWait,
						InjectQueue:      bs.InjectQueue,
						InjectConfirm:    bs.InjectConfirm,
						StopCooldown:     bs.StopCooldown,
						ReactionTracker:  bs.ReactionTracker,
						SessionState:     bs.SessionState,
						HookSessionLocks: &bs.HookSessionLocks,
						SessionEvents:    bs.SessionEvents,
						PendingMsgStore:  bs.PendingMsgStore,
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
		return stores.SendResult{MsgID: sent.ID, ChatID: chatIDInt, TopicID: topicID, MsgText: text}
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
	// Rich cap: permission bodies split at the rich rune/block limit (was 3900 legacy HTML).
	richMax := 30000
	if permCfg, cfgErr := config.LoadAppConfig(); cfgErr == nil && permCfg.RichMaxRunes > 0 {
		richMax = permCfg.RichMaxRunes
	}
	permChunks := helpers.SplitBody(text, richMax)
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
	// R5 (S11): route the blocking PermissionRequest SEND onto the Message FIFO via a SYNC Dispatch —
	// executeSendPendingMessage runs ON the Hook FIFO (send:pending job), so per INV2 any blocking send
	// reachable on the Hook FIFO must go onto the Message FIFO. SYNC because the caller needs the returned TG
	// msg_id for the pending store (permission keeps using the TG msg_id directly — NOT the internal-id
	// mechanism). Rich send: permission body has old_string/new_string in <pre> (code/diff) → skip entity
	// detection so file paths/@words/flags are not turned into spurious links (C3).
	var sent *tele.Message
	var err error
	bs.MessageQueue.Dispatch(p.SessionID, "msg:perm-send", func() error {
		sent, err = helpers.RetrySendRich(bs.Bot, chat, text, helpers.RichSendOpts{
			TopicID:             topicID,
			Markup:              markup,
			SkipEntityDetection: true,
			LegacyHTML:          text,
		})
		return nil
	})
	if err != nil {
		return stores.SendResult{MainErr: fmt.Errorf("send PermissionRequest: %w", err)}
	}
	entry.Rich = true
	if len(permChunks) > 1 {
		bs.Pages.Store(sent.ID, p.SessionID, &stores.PageEntry{
			Chunks:     permChunks,
			Event:      "PermissionRequest",
			Project:    p.Project,
			CWD:        cwdForRoute,
			TmuxTarget: p.TmuxTarget,
			PermRows:   permBtnRows,
			ChatID:     chatIDInt,
			Rich:       true,
		})
	}
	logger.Info(fmt.Sprintf("Permission request sent: tool=%s project=%s tmux=%s (msg_id=%d pages=%d) uuid=%s fmt=rich", p.ToolName, p.Project, p.TmuxTarget, sent.ID, len(permChunks), entry.UUID))
	logger.Info(fmt.Sprintf("TG permission message sent full_text:\n%s", text))
	bs.SessionWatch.Notify(agentName, stores.WatchEvent{
		Event:   "PermissionRequest",
		Agent:   agentName,
		Summary: fmt.Sprintf("Tool: %s", p.ToolName),
		Detail:  text,
	})
	return stores.SendResult{MsgID: sent.ID, ChatID: chatIDInt, TopicID: topicID, MsgText: text}
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
		msgTextQ := r.URL.Query().Get("msg_text")
		richQ := r.URL.Query().Get("rich") == "1" // Rich=true when hook reconnects after restart and rich=1 was set
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

		// Set up chunked streaming with per-connection serialization mutex
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		rc := http.NewResponseController(w)
		var writerMu sync.Mutex
		streamOpen := true
		defer func() {
			writerMu.Lock()
			streamOpen = false
			writerMu.Unlock()
		}()
		errStreamClosed := fmt.Errorf("stream closed")
		sendFrame := func(v any) error {
			writerMu.Lock()
			defer writerMu.Unlock()
			if !streamOpen {
				return errStreamClosed
			}
			data, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if _, err := w.Write(append(data, '\n')); err != nil {
				return err
			}
			return rc.Flush()
		}
		// Alias for backward compat within this function
		send := sendFrame

		bs.PendingWait.BumpGeneration(uuid)
		existing, exists := bs.PendingWait.Get(uuid)
		isReconnect := exists || msgIDQ != 0

		var entry *stores.PendingWaitEntry
		if isReconnect && exists {
			// Reconnect with live store entry — use snapshot to read MsgID safely
			snap, snapOk := bs.PendingWait.GetSnapshot(uuid)
			if snapOk && snap.MsgID == 0 {
				if realMsgID, realChatID, realMsgText, _, ok := bs.PendingMsgStore.Get(uuid); ok && realMsgID != 0 {
					// Backfill under lock so all readers see the updated value
					bs.PendingWait.BackfillMsgID(uuid, realMsgID, realChatID, topicIDQ, realMsgText)
					snap, _ = bs.PendingWait.GetSnapshot(uuid)
				}
			}
			entry = existing
			logger.Info(fmt.Sprintf("hook reattached: uuid=%s msg_id=%d", uuid, snap.MsgID))
		} else if isReconnect && msgIDQ != 0 {
			// Reconnect after bot restart — store was wiped, rebuild from supplied params.
			// Parse payload fields (Questions, MsgText, PermSuggestions) BEFORE Register.
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
				Rich:               richQ, // restore persisted Rich flag (default false = legacy for old entries)
			}
			parseEntryFields(entry, raw, &p)
			if msgTextQ != "" {
				entry.MsgText = msgTextQ
			}
			// Seed PendingMsgStore with known coords BEFORE Register so any racing EDIT finds them
			bs.PendingMsgStore.SetAndDrain(uuid, msgIDQ, chatIDQ, entry.MsgText, topicIDQ)
			bs.PendingWait.Register(entry)
			logger.Info(fmt.Sprintf("hook reattached (restart): uuid=%s msg_id=%d rich=%v", uuid, entry.MsgID, richQ))
		} else {
			// First connect — parse payload fields BEFORE Register, then prepare op, TryEnqueue SEND
			entry = &stores.PendingWaitEntry{
				UUID:               uuid,
				ToolName:           p.ToolName,
				ToolUseID:          p.ToolUseID,
				ToolInputCanonical: helpers.CanonicalToolInput(p.ToolInput),
				SessionID:          p.SessionID,
				TmuxTarget:         p.TmuxTarget,
				Payload:            raw,
			}
			parseEntryFields(entry, raw, &p)
			bs.PendingWait.Register(entry)
			sendFn, prepErr := preparePendingMessage(bs, cb, entry, sendFrame)
			if prepErr != nil {
				logger.Error(fmt.Sprintf("pendingConnect: preparePendingMessage failed uuid=%s err=%v", uuid, prepErr))
				bs.PendingWait.Remove(uuid)
				http.Error(w, "failed to prepare TG message", 500)
				return
			}
			// Route SEND through per-session FIFO queue to preserve per-session ordering.
			// Fall back to inline execution if the channel is full (ordering degraded in rare saturation).
			if !bs.SessionEvents.TryDispatchAsync(p.SessionID, "send:pending", sendFn) {
				logger.Info(fmt.Sprintf("pendingConnect: SessionEvents full, running send inline uuid=%s", uuid))
				go sendFn()
			}
			logger.Info(fmt.Sprintf("hook connected: uuid=%s tool=%s (SEND dispatched)", uuid, p.ToolName))
		}

		// Deliver any terminal that arrived while no handler was live
		if term := bs.PendingWait.TakeTerminal(uuid); term != nil {
			if send(*term) == nil {
				bs.PendingWait.Remove(uuid)
			} else {
				bs.PendingWait.Push(uuid, *term)
			}
			return
		}

		// Send registered ack — use GetSnapshot for MsgID/ChatID/TopicID/Rich
		type registeredMsg struct {
			Type    string `json:"type"`
			MsgID   int    `json:"msg_id"`
			ChatID  int64  `json:"chat_id"`
			TopicID int    `json:"topic_id"`
			Rich    bool   `json:"rich,omitempty"`
		}
		var regMsgID int
		var regChatID int64
		var regTopicID int
		var regRich bool
		if snap, ok := bs.PendingWait.GetSnapshot(uuid); ok {
			regMsgID = snap.MsgID
			regChatID = snap.ChatID
			regTopicID = snap.TopicID
			regRich = snap.Rich
		}
		if regMsgID == 0 {
			if qMsgID, qChatID, _, _, ok := bs.PendingMsgStore.Get(uuid); ok && qMsgID != 0 {
				regMsgID = qMsgID
				regChatID = qChatID
			}
		}
		if err := send(registeredMsg{Type: "registered", MsgID: regMsgID, ChatID: regChatID, TopicID: regTopicID, Rich: regRich}); err != nil {
			logger.Error(fmt.Sprintf("pendingConnect: send registered failed uuid=%s err=%v", uuid, err))
			return
		}

		// Set live and wait for answer or disconnect via BeginLive
		_, liveCh, liveGen, liveFound := bs.PendingWait.BeginLive(uuid)
		if !liveFound {
			// Entry was swept or cancelled before we got here
			logger.Info(fmt.Sprintf("pendingConnect: entry gone before BeginLive uuid=%s", uuid))
			return
		}
		select {
		case ev := <-liveCh:
			bs.PendingWait.ClearLive(uuid, liveGen)
			if send(ev) == nil {
				bs.PendingWait.Remove(uuid)
			} else {
				bs.PendingWait.Push(uuid, ev)
			}
		case <-r.Context().Done():
			bs.PendingWait.ClearLive(uuid, liveGen)
			logger.Info(fmt.Sprintf("hook disconnected: uuid=%s", uuid))
			if config.UpgradeFlagActive() {
				// Upgrade in progress — hook will reconnect; keep entry alive
				return
			}
			// 3-second grace period: if hook reconnects, new generation clears the timer
			capturedGen := liveGen
			capturedUUID := uuid
			go func() {
				time.Sleep(3 * time.Second)
				if bs.PendingWait.CurrentGeneration(capturedUUID) != capturedGen {
					return // hook reconnected — new handler owns it
				}
				waitSnap, snapOk := bs.PendingWait.GetSnapshot(capturedUUID)
				if !snapOk || waitSnap.Resolved {
					return
				}
				// Hook stayed away — cancel via ResolveIfUnresolved (state) + EditOrDefer that enqueues
				// msg:freeze-edit onto the Message FIFO (B.8)
				helpers.FreezeWaitEntryOnDesktop(bs.Bot, bs.MessageQueue, bs.PendingWait, bs.PendingMsgStore, waitSnap, "❌ Cancelled")
				bs.PendingWait.Remove(capturedUUID)
				logger.Info(fmt.Sprintf("pendingConnect grace expired: cancelled uuid=%s", capturedUUID))
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

package hooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

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

// CancelPendingFilesBySession marks all pending files for a session as cancelled.
// Called when bot receives subsequent events (Stop/PreToolUse/UserPromptSubmit),
// indicating user answered in TUI and CC has moved on.
// Also cleans up toolNotifs/TG message state for any associated question messages.
func CancelPendingFilesBySession(bs *types.BotState, sessionID string) {
	if sessionID == "" {
		return
	}
	dir := helpers.PendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		pf, err := helpers.ReadPendingFile(path)
		if err != nil {
			continue
		}
		if pf.SessionID == sessionID && pf.Status == "sent" {
			pf.Status = "cancelled"
			helpers.WritePendingFile(path, pf)
			if pf.TgMsgID != 0 {
				if notifEntry, ok := bs.ToolNotifs.Get(pf.TgMsgID); ok && !notifEntry.Resolved {
					bs.ToolNotifs.MarkResolved(pf.TgMsgID)
					editMsg := &tele.Message{ID: pf.TgMsgID, Chat: &tele.Chat{ID: pf.TgChatID}}
					helpers.RetryEdit(bs.Bot, editMsg, notifEntry.MsgText, helpers.BuildFrozenMarkup(notifEntry, "⌨️ Answered on desktop"), tele.ModeHTML)
				}
				bs.PendingFiles.Remove(pf.TgMsgID)
			}
			if _, ok := bs.PendingPerms.GetTarget(pf.TgMsgID); ok {
				permChatID := bs.PendingPerms.GetChatID(pf.TgMsgID)
				permMsgText := bs.PendingPerms.GetMsgText(pf.TgMsgID)
				sugLabel, _ := helpers.ParseSuggestionLabel(bs.PendingPerms.GetSuggestions(pf.TgMsgID))
				bs.PendingPerms.Resolve(pf.TgMsgID, stores.PermDecision{Behavior: "allow", Message: "Answered on desktop"})
				editMsg := &tele.Message{ID: pf.TgMsgID, Chat: &tele.Chat{ID: permChatID}}
				helpers.RetryEdit(bs.Bot, editMsg, permMsgText, helpers.BuildFrozenPermMarkup("⌨️ Answered on desktop", sugLabel), tele.ModeHTML)
				logger.Info(fmt.Sprintf("Permission TUI answer: msg_id=%d label=⌨️ Answered on desktop", pf.TgMsgID))
			}
			if !helpers.IsHookAlive(pf.HookPID) {
				os.Remove(path)
				logger.Info(fmt.Sprintf("Removed orphan pending file (hook dead): %s (session=%s pid=%d)", entry.Name(), sessionID, pf.HookPID))
			} else {
				logger.Info(fmt.Sprintf("Cancelled pending file: %s (session=%s)", entry.Name(), sessionID))
			}
		} else if pf.SessionID == sessionID && pf.Status == "cancelled" && !helpers.IsHookAlive(pf.HookPID) {
			os.Remove(path)
			logger.Info(fmt.Sprintf("Cleaned stale cancelled file (hook dead): %s (session=%s pid=%d)", entry.Name(), sessionID, pf.HookPID))
		}
	}
}

// CleanPendingFilesBySession removes all pending files for a session.
func CleanPendingFilesBySession(sessionID string) {
	dir := helpers.PendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		pf, err := helpers.ReadPendingFile(path)
		if err != nil {
			continue
		}
		if pf.SessionID == sessionID {
			os.Remove(path)
			logger.Info(fmt.Sprintf("Cleaned pending file: %s (session=%s)", entry.Name(), sessionID))
		}
	}
}

// ProcessPendingRequest processes a pending file and sends TG message.
func ProcessPendingRequest(bs *types.BotState, cb Callbacks, uuid string) {
	dir := helpers.PendingDir()
	path := filepath.Join(dir, uuid+".json")
	pf, err := helpers.ReadPendingFile(path)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to read pending file %s: %v", uuid, err))
		return
	}
	var p helpers.HookPayload
	if err := json.Unmarshal(pf.Payload, &p); err != nil {
		logger.Error(fmt.Sprintf("Failed to parse pending payload %s: %v", uuid, err))
		return
	}
	p.TmuxTarget = notify.FormatPaneID(p.TmuxTarget)
	pf.SessionID = p.SessionID
	pf.TmuxTarget = p.TmuxTarget
	pf.ToolName = p.ToolName
	// Use stored session CWD for routing to avoid drift from cd commands in CC
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
		logger.Info(fmt.Sprintf("No chat for pending request %s, skipping", uuid))
		return
	}
	if p.AgentID == "" {
		if p.Backend == "codex" {
			if updateBody := cb.ProcessTranscriptUpdates(bs, p.SessionID, p.TranscriptPath, true); updateBody != "" {
				chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
				cb.SendEventNotification(bs, chat, chatID, p.SessionID, "PreToolUse", p.Project, cwdForRoute, p.TmuxTarget, updateBody, "", agentName, topicID)
				// Reset compact tool message after Update is sent via pending handler path
				bs.CompactTools.Reset(p.SessionID)
				logger.Info(fmt.Sprintf("PreToolUse Update sent for pending request %s (chat=%d)", uuid, chatIDInt))
			} else {
				logger.Info(fmt.Sprintf("PreToolUse Update skipped: uuid=%s reason=no_new_assistant_text", uuid))
			}
		} else {
			cb.StreamFlush(bs, p.SessionID, false) // drain+flush so pre-question 💬 lands before buttons
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
			logger.Info(fmt.Sprintf("No questions in pending request %s, skipping", uuid))
			return
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
			logger.Error(fmt.Sprintf("Failed to send AskUserQuestion: %v", err))
			return
		}
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		bs.ToolNotifs.Store(sent.ID, &stores.ToolNotifyEntry{
			TmuxTarget: p.TmuxTarget, ToolName: "AskUserQuestion",
			Questions: qMetas, ChatID: chatIDInt, MsgText: text,
			PendingUUID: uuid,
		})
		bs.PendingFiles.Store(sent.ID, uuid)
		pf.Status = "sent"
		pf.TgMsgID = sent.ID
		pf.TgChatID = chatIDInt
		pf.TgMsgText = text
		helpers.WritePendingFile(path, pf)
		logger.Info(fmt.Sprintf("TG question message sent full_text:\n%s", text))
		var qSummaries []string
		for _, q := range askInput.Questions {
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			qSummaries = append(qSummaries, fmt.Sprintf("%s:[%s]", q.Header, strings.Join(labels, ",")))
		}
		contentSummary := strings.Join(qSummaries, " | ")
		logger.Info(fmt.Sprintf("AskUserQuestion sent: msg_id=%d questions=%d tmux=%s content=%s uuid=%s", sent.ID, len(askInput.Questions), p.TmuxTarget, contentSummary, uuid))
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
				for _, entry := range buffered {
					contentLines = append(contentLines, fmt.Sprintf("[%s → %s]: %s", agentName, dn, entry))
				}
				// Format full AskQ content with ❓ header and options with descriptions
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
				// Inject to target pane = TG content
				go func(target, text string) {
					injectP := helpers.SafeInjectTextParams{
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
							return cb.ResolveChat(bs, t)
						},
						FormatPaneID: notify.FormatPaneID,
					}
					helpers.SafeInjectText(injectP, target, text)
				}(peerInfo.TmuxTarget, msg)
			}
		}
		bs.SessionWatch.Notify(agentName, stores.WatchEvent{
			Event:   "AskUserQuestion",
			Agent:   agentName,
			Summary: contentSummary,
			Detail:  text,
		})
		return
	}
	logger.Info(fmt.Sprintf("Permission request: tool=%s project=%s uuid=%s", p.ToolName, p.Project, uuid))
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
		logger.Error(fmt.Sprintf("Failed to send permission message: %v", err))
		return
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
	logger.Info(fmt.Sprintf("Permission request sent: tool=%s project=%s tmux=%s (msg_id=%d pages=%d) uuid=%s", p.ToolName, p.Project, p.TmuxTarget, sent.ID, len(permChunks), uuid))
	logger.Info(fmt.Sprintf("TG permission message sent full_text:\n%s", text))
	bs.SessionWatch.Notify(agentName, stores.WatchEvent{
		Event:   "PermissionRequest",
		Agent:   agentName,
		Summary: fmt.Sprintf("Tool: %s", p.ToolName),
		Detail:  text,
	})
	bs.PendingPerms.Create(sent.ID, p.TmuxTarget, p.PermSuggestions, text, chatIDInt, uuid)
	bs.PendingFiles.Store(sent.ID, uuid)
	pf.Status = "sent"
	pf.TgMsgID = sent.ID
	pf.TgChatID = chatIDInt
	pf.TgMsgText = text
	helpers.WritePendingFile(path, pf)
}

// ScanPendingDir scans pending directory on bot startup to rebuild in-memory state.
func ScanPendingDir(bs *types.BotState, cb Callbacks, scanLaunchDir func(*types.BotState)) {
	dir := helpers.PendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Debug(fmt.Sprintf("scanPendingDir: skip (dir not readable): %v", err))
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		uuid := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(dir, entry.Name())
		pf, err := helpers.ReadPendingFile(path)
		if err != nil {
			logger.Error(fmt.Sprintf("scanPendingDir: failed to read %s: %v", entry.Name(), err))
			continue
		}
		switch pf.Status {
		case "pending":
			// Bot wasn't running when hook wrote the file — process it now
			logger.Info(fmt.Sprintf("scanPendingDir: processing pending request %s", uuid))
			go ProcessPendingRequest(bs, cb, uuid)
		case "sent":
			// Rebuild in-memory state so button clicks work after restart
			logger.Info(fmt.Sprintf("scanPendingDir: rebuilding in-memory state for %s (status=sent)", uuid))
			if err := helpers.RebuildInMemoryState(bs.ToolNotifs, bs.PendingFiles, bs.PendingPerms, pf, notify.FormatPaneID); err != nil {
				logger.Error(fmt.Sprintf("scanPendingDir: failed to rebuild state for %s: %v", uuid, err))
			}
		case "answered":
			// Orphaned file — hook should have cleaned it up
			logger.Info(fmt.Sprintf("scanPendingDir: removing orphaned answered file %s", uuid))
			os.Remove(path)
		default:
			logger.Error(fmt.Sprintf("scanPendingDir: unknown status %q in %s", pf.Status, uuid))
		}
	}
	// Also scan launch state files for /bot_new crash recovery
	scanLaunchDir(bs)
}

// pendingNotifyHandler handles POST /pending/notify.
func pendingNotifyHandler(bs *types.BotState, cb Callbacks) http.HandlerFunc {
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
		logger.Info(fmt.Sprintf("Received pending notify: uuid=%s", uuid))
		path := filepath.Join(helpers.PendingDir(), uuid+".json")
		pf, pfErr := helpers.ReadPendingFile(path)
		var sessionID string
		if pfErr == nil && len(pf.Payload) > 0 {
			var peek struct {
				SessionID string `json:"session_id"`
			}
			json.Unmarshal(pf.Payload, &peek)
			sessionID = peek.SessionID
		}
		bs.SessionEvents.Dispatch(sessionID, "pending:"+uuid, func() error {
			ProcessPendingRequest(bs, cb, uuid)
			return nil
		})
		w.WriteHeader(200)
	}
}

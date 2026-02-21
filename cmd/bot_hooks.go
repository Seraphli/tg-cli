package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// cleanPendingFilesBySession removes all pending files for a session
func cleanPendingFilesBySession(sessionID string) {
	dir := pendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		pf, err := readPendingFile(path)
		if err != nil {
			continue
		}
		if pf.SessionID == sessionID {
			os.Remove(path)
			logger.Info(fmt.Sprintf("Cleaned pending file: %s (session=%s)", entry.Name(), sessionID))
		}
	}
}

// processPendingRequest processes a pending file and sends TG message
func processPendingRequest(bot *tele.Bot, creds *config.Credentials, uuid string) {
	dir := pendingDir()
	path := filepath.Join(dir, uuid+".json")
	pf, err := readPendingFile(path)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to read pending file %s: %v", uuid, err))
		return
	}
	var p hookPayload
	if err := json.Unmarshal(pf.Payload, &p); err != nil {
		logger.Error(fmt.Sprintf("Failed to parse pending payload %s: %v", uuid, err))
		return
	}
	pf.SessionID = p.SessionID
	pf.TmuxTarget = p.TmuxTarget
	pf.ToolName = p.ToolName
	chat, chatID := resolveChat(p.TmuxTarget)
	if chat == nil {
		logger.Info(fmt.Sprintf("No chat for pending request %s, skipping", uuid))
		return
	}
	// Send intermediate text (PreToolUse Update) before question/permission message
	if updateBody := processTranscriptUpdates(p.SessionID, p.TranscriptPath); updateBody != "" {
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		sendEventNotification(bot, chat, chatID, p.SessionID, "PreToolUse", p.Project, p.TmuxTarget, updateBody)
		logger.Info(fmt.Sprintf("PreToolUse Update sent for pending request %s (chat=%d)", uuid, chatIDInt))
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
		var qMetas []questionMeta
		var questionEntries []notify.QuestionEntry
		for _, q := range askInput.Questions {
			var opts []notify.QuestionOption
			var labels []string
			for _, o := range q.Options {
				opts = append(opts, notify.QuestionOption{Label: o.Label, Description: o.Description})
				labels = append(labels, o.Label)
			}
			qMetas = append(qMetas, questionMeta{
				questionText: q.Question, header: q.Header,
				numOptions: len(q.Options), optionLabels: labels,
				multiSelect: q.MultiSelect, selectedOptions: make(map[int]bool),
				selectedOption: -1,
			})
			questionEntries = append(questionEntries, notify.QuestionEntry{
				Header: q.Header, Question: q.Question, Options: opts, MultiSelect: q.MultiSelect,
			})
		}
		text := notify.BuildQuestionText(notify.QuestionData{
			Project: p.Project, TmuxTarget: p.TmuxTarget, Questions: questionEntries,
		})
		markup := &tele.ReplyMarkup{}
		var rows []tele.Row
		hasSubmit := len(askInput.Questions) > 1
		for _, q := range askInput.Questions {
			if q.MultiSelect {
				hasSubmit = true
			}
		}
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
		markup.Inline(rows...)
		sent, err := bot.Send(chat, text, markup)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send AskUserQuestion: %v", err))
			return
		}
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		toolNotifs.store(sent.ID, &toolNotifyEntry{
			tmuxTarget: p.TmuxTarget, toolName: "AskUserQuestion",
			questions: qMetas, chatID: chatIDInt, msgText: text,
			pendingUUID: uuid,
		})
		pendingFiles.store(sent.ID, uuid)
		pf.Status = "sent"
		pf.TgMsgID = sent.ID
		pf.TgChatID = chatIDInt
		writePendingFile(path, pf)
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
		return
	}
	logger.Info(fmt.Sprintf("Permission request: tool=%s project=%s uuid=%s", p.ToolName, p.Project, uuid))
	var toolInput map[string]interface{}
	json.Unmarshal(p.ToolInput, &toolInput)
	logger.Info(fmt.Sprintf("Permission payload: toolInput=%s suggestions=%s", string(p.ToolInput), string(p.PermSuggestions)))
	text := notify.BuildPermissionText(notify.PermissionData{
		Project: p.Project, TmuxTarget: p.TmuxTarget,
		ToolName: p.ToolName, ToolInput: toolInput,
	})
	markup := &tele.ReplyMarkup{}
	row1 := []tele.Btn{
		markup.Data("✅ Allow", "perm", "allow"),
		markup.Data("❌ Deny", "perm", "deny"),
	}
	var suggestions []json.RawMessage
	json.Unmarshal(p.PermSuggestions, &suggestions)
	var row2 []tele.Btn
	for i, s := range suggestions {
		var sug struct {
			Type         string   `json:"type"`
			Tool         string   `json:"tool"`
			AllowPattern string   `json:"allow_pattern"`
			Mode         string   `json:"mode"`
			Directories  []string `json:"directories"`
			Rules        []struct {
				ToolName    string `json:"toolName"`
				RuleContent string `json:"ruleContent"`
			} `json:"rules"`
		}
		json.Unmarshal(s, &sug)
		var label string
		switch sug.Type {
		case "setMode":
			label = "✅ " + sug.Mode
		case "addDirectories":
			dir := ""
			if len(sug.Directories) > 0 {
				dir = sug.Directories[0]
			}
			label = "✅ Allow dir: " + dir
		default:
			toolName := sug.Tool
			allowPattern := sug.AllowPattern
			if toolName == "" && len(sug.Rules) > 0 {
				toolName = sug.Rules[0].ToolName
				if allowPattern == "" {
					allowPattern = sug.Rules[0].RuleContent
				}
			}
			label = "✅ Always Allow"
			if toolName != "" {
				label += " " + toolName
			}
			if allowPattern != "" && allowPattern != "*" {
				label += " (" + allowPattern + ")"
			}
		}
		row2 = append(row2, markup.Data(label, "perm", fmt.Sprintf("s%d", i)))
	}
	var permBtnRows []tele.Row
	permBtnRows = append(permBtnRows, row1)
	if len(row2) > 0 {
		permBtnRows = append(permBtnRows, row2)
	}
	permChunks := splitBody(text, 3900)
	if len(permChunks) <= 1 {
		if len(row2) > 0 {
			markup.Inline(markup.Row(row1...), markup.Row(row2...))
		} else {
			markup.Inline(markup.Row(row1...))
		}
	} else {
		text = permChunks[0] + fmt.Sprintf("\n\n📄 1/%d", len(permChunks))
		kb := buildPageKeyboardWithExtra(1, len(permChunks), permBtnRows)
		markup = kb
	}
	sent, err := bot.Send(chat, text, markup)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to send permission message: %v", err))
		return
	}
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	if len(permChunks) > 1 {
		pages.store(sent.ID, p.SessionID, &pageEntry{
			chunks:     permChunks,
			event:      "PermissionRequest",
			project:    p.Project,
			tmuxTarget: p.TmuxTarget,
			permRows:   permBtnRows,
			chatID:     chatIDInt,
		})
	}
	logger.Info(fmt.Sprintf("Permission request sent: tool=%s project=%s tmux=%s (msg_id=%d pages=%d) uuid=%s", p.ToolName, p.Project, p.TmuxTarget, sent.ID, len(permChunks), uuid))
	logger.Info(fmt.Sprintf("TG permission message sent full_text:\n%s", text))
	suggestionsRaw, _ := json.Marshal(suggestions)
	pendingPerms.create(sent.ID, p.TmuxTarget, suggestionsRaw, text, chatIDInt, uuid)
	pendingFiles.store(sent.ID, uuid)
	pf.Status = "sent"
	pf.TgMsgID = sent.ID
	pf.TgChatID = chatIDInt
	writePendingFile(path, pf)
}

// registerHTTPHooks registers the main "/hook/" endpoint handler
func registerHTTPHooks(mux *http.ServeMux, bot *tele.Bot, creds *config.Credentials, port int) {
	mux.HandleFunc("/pending/notify", func(w http.ResponseWriter, r *http.Request) {
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
		go processPendingRequest(bot, creds, uuid)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/hook/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		event := strings.TrimPrefix(r.URL.Path, "/hook/")
		p, raw, err := parseHookPayload(r)
		if err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		logger.Info(fmt.Sprintf("Raw hook payload [%s]: %s", event, string(raw)))
		chat, chatID := resolveChat(p.TmuxTarget)
		switch event {
		case "SessionStart":
			if chat == nil || p.TmuxTarget == "" {
				w.WriteHeader(200)
				return
			}
			text := notify.BuildNotificationText(notify.NotificationData{
				Event: "SessionStart", Project: p.Project, TmuxTarget: p.TmuxTarget,
			})
			bot.Send(chat, text)
			logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionStart [%s] tmux=%s", chatID, p.Project, p.TmuxTarget))
			if p.SessionID != "" && p.TmuxTarget != "" {
				sessionState.add(p.SessionID, p.TmuxTarget)
				logger.Info(fmt.Sprintf("Session tracked: %s -> %s", p.SessionID, p.TmuxTarget))
			}
		case "SessionEnd":
			if chat != nil {
				text := notify.BuildNotificationText(notify.NotificationData{
					Event: "SessionEnd", Project: p.Project, TmuxTarget: p.TmuxTarget,
				})
				bot.Send(chat, text)
				logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionEnd [%s] tmux=%s", chatID, p.Project, p.TmuxTarget))
			}
			if p.SessionID != "" {
				sessionState.remove(p.SessionID)
				logger.Info(fmt.Sprintf("Session untracked: %s", p.SessionID))
			}
			pages.cleanupSession(p.SessionID)
			sessionCounts.cleanup(p.SessionID)
			cleanPendingFilesBySession(p.SessionID)
			logger.Info(fmt.Sprintf("Cleaned up session %s", p.SessionID))
		case "UserPromptSubmit":
			if p.SessionID != "" && p.TranscriptPath != "" {
				lock := sessionCounts.getLock(p.SessionID)
				lock.Lock()
				texts := readAssistantTexts(p.TranscriptPath)
				sessionCounts.counts[p.SessionID] = len(texts)
				lock.Unlock()
				logger.Debug(fmt.Sprintf("UserPromptSubmit position: session=%s count=%d", p.SessionID, len(texts)))
			}
			if p.TmuxTarget != "" {
				reactionTracker.clearAndRemove(bot, p.TmuxTarget)
				logger.Debug(fmt.Sprintf("Cleared reactions for tmux target: %s", p.TmuxTarget))
			}
		case "Stop":
			if chat != nil {
				body := processTranscriptUpdates(p.SessionID, p.TranscriptPath)
				sendEventNotification(bot, chat, chatID, p.SessionID, "Stop", p.Project, p.TmuxTarget, body)
			}
		case "PreToolUse":
			// PreToolUse: send intermediate notification
			// Skip processTranscriptUpdates for AskUserQuestion — /pending/notify handler will call it
			// to avoid race condition where both paths compete for sessionCounts
			if chat != nil && p.ToolName != "AskUserQuestion" {
				body := processTranscriptUpdates(p.SessionID, p.TranscriptPath)
				if body != "" {
					sendEventNotification(bot, chat, chatID, p.SessionID, "PreToolUse", p.Project, p.TmuxTarget, body)
				}
			}
		case "PermissionRequest":
			// PermissionRequest is now handled via file-based communication
			// hook.go writes pending file and polls for answer
			// This handler is no longer used by hook.go, but kept for backward compatibility
			logger.Info(fmt.Sprintf("PermissionRequest received via HTTP (legacy path): tool=%s", p.ToolName))
			w.WriteHeader(200)
			return
		default:
			// Unknown event — send notification if possible
			if chat != nil {
				body := processTranscriptUpdates(p.SessionID, p.TranscriptPath)
				sendEventNotification(bot, chat, chatID, p.SessionID, event, p.Project, p.TmuxTarget, body)
			}
		}
		w.WriteHeader(200)
	})
}

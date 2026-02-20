package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

// registerHTTPAPI registers all HTTP API endpoints
func registerHTTPAPI(mux *http.ServeMux, bot *tele.Bot, creds *config.Credentials) {
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		msgIDStr := r.URL.Query().Get("msg_id")
		pageStr := r.URL.Query().Get("page")
		msgID, err := strconv.Atoi(msgIDStr)
		if err != nil {
			http.Error(w, "invalid msg_id", 400)
			return
		}
		pageNum, err := strconv.Atoi(pageStr)
		if err != nil {
			http.Error(w, "invalid page", 400)
			return
		}
		entry, ok := pages.get(msgID)
		if !ok {
			http.Error(w, "page entry not found", 404)
			return
		}
		if pageNum < 1 || pageNum > len(entry.chunks) {
			http.Error(w, "page out of range", 400)
			return
		}
		chat := &tele.Chat{ID: entry.chatID}
		var text string
		if entry.permRows != nil {
			text = entry.chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.chunks))
		} else {
			text = notify.BuildNotificationText(notify.NotificationData{
				Event:      entry.event,
				Project:    entry.project,
				Body:       entry.chunks[pageNum-1],
				TmuxTarget: entry.tmuxTarget,
				Page:       pageNum,
				TotalPages: len(entry.chunks),
			})
		}
		kb := buildPageKeyboardWithExtra(pageNum, len(entry.chunks), entry.permRows)
		editMsg := &tele.Message{ID: msgID, Chat: chat}
		_, err = bot.Edit(editMsg, text, kb)
		if err != nil {
			logger.Error(fmt.Sprintf("Callback edit failed: %v", err))
			http.Error(w, "edit failed: "+err.Error(), 500)
			return
		}
		logger.Info(fmt.Sprintf("Callback page turn: msg_id=%d page=%d/%d", msgID, pageNum, len(entry.chunks)))
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/permission/decide", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		decision := r.URL.Query().Get("decision")
		uuid, uuidOk := pendingPerms.getUUID(msgID)
		if !uuidOk {
			uuid, uuidOk = pendingFiles.get(msgID)
		}
		msgText := pendingPerms.getMsgText(msgID)
		permChatID := pendingPerms.getChatID(msgID)
		sugLabels := parseSuggestionLabels(pendingPerms.getSuggestions(msgID))
		d, err := resolvePermission(msgID, decision, nil)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		if uuidOk {
			var updatedPerms []interface{}
			if d.UpdatedPermissions != nil {
				var perms []interface{}
				json.Unmarshal(d.UpdatedPermissions, &perms)
				updatedPerms = perms
			}
			ccOutput := buildPermCCOutput(d.Behavior, d.Message, updatedPerms)
			if err := writePendingAnswer(uuid, ccOutput); err != nil {
				logger.Error(fmt.Sprintf("Failed to write pending answer for perm: %v", err))
			}
		}
		logger.Info(fmt.Sprintf("Permission resolved via API: msg_id=%d decision=%s uuid=%s", msgID, decision, uuid))
		if permChatID != 0 && msgText != "" {
			editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: permChatID}}
			bot.Edit(editMsg, msgText, buildFrozenPermMarkup(decision, sugLabels))
		}
		respJSON, _ := json.Marshal(d)
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	})
	mux.HandleFunc("/tool/respond", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		tool := r.URL.Query().Get("tool")
		action := r.URL.Query().Get("action")
		switch tool {
		case "AskUserQuestion":
			if action == "text" {
				value := r.URL.Query().Get("value")
				entry, ok := toolNotifs.get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				if entry.resolved {
					http.Error(w, "already answered", 400)
					return
				}
				uuid, ok := pendingFiles.get(msgID)
				if !ok {
					http.Error(w, "pending file not found", 404)
					return
				}
				path := filepath.Join(pendingDir(), uuid+".json")
				pf, err := readPendingFile(path)
				if err != nil {
					http.Error(w, "failed to read pending file", 500)
					return
				}
				answers := make(map[string]string)
				if len(entry.questions) > 0 {
					answers[entry.questions[0].questionText] = value
				}
				ccOutput := buildAskCCOutput(pf.Payload, answers)
				if err := writePendingAnswer(uuid, ccOutput); err != nil {
					logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
					http.Error(w, "failed to save answer", 500)
					return
				}
				toolNotifs.markResolved(msgID)
				logger.Info(fmt.Sprintf("AskUserQuestion text via API: msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(value, 200)))
				editChat := &tele.Chat{ID: entry.chatID}
				editMsg := &tele.Message{ID: msgID, Chat: editChat}
				bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"))
			} else if action == "submit" {
				entry, ok := toolNotifs.get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				if entry.resolved {
					http.Error(w, "already answered", 400)
					return
				}
				uuid, ok := pendingFiles.get(msgID)
				if !ok {
					http.Error(w, "pending file not found", 404)
					return
				}
				path := filepath.Join(pendingDir(), uuid+".json")
				pf, err := readPendingFile(path)
				if err != nil {
					http.Error(w, "failed to read pending file", 500)
					return
				}
				answers := buildAnswers(entry)
				ccOutput := buildAskCCOutput(pf.Payload, answers)
				if err := writePendingAnswer(uuid, ccOutput); err != nil {
					logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
					http.Error(w, "failed to save answer", 500)
					return
				}
				toolNotifs.markResolved(msgID)
				logger.Info(fmt.Sprintf("AskUserQuestion submitted via API: msg_id=%d uuid=%s answers=%v", msgID, uuid, answers))
				editChat := &tele.Chat{ID: entry.chatID}
				editMsg := &tele.Message{ID: msgID, Chat: editChat}
				bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, ""))
			} else {
				qIdx, _ := strconv.Atoi(r.URL.Query().Get("question"))
				optIdx, _ := strconv.Atoi(r.URL.Query().Get("option"))
				entry, ok := toolNotifs.get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				if entry.resolved {
					http.Error(w, "already answered", 400)
					return
				}
				if qIdx >= len(entry.questions) {
					http.Error(w, "invalid question index", 400)
					return
				}
				qm := &entry.questions[qIdx]
				if qm.multiSelect {
					qm.selectedOptions[optIdx] = !qm.selectedOptions[optIdx]
					logger.Info(fmt.Sprintf("AskUserQuestion option toggled via API: msg_id=%d q=%d opt=%d state=%v label=%s", msgID, qIdx, optIdx, qm.selectedOptions[optIdx], qm.optionLabels[optIdx]))
					newMarkup := rebuildAskMarkup(entry)
					editChat := &tele.Chat{ID: entry.chatID}
					editMsg := &tele.Message{ID: msgID, Chat: editChat}
					bot.Edit(editMsg, entry.msgText, newMarkup)
				} else {
					qm.selectedOption = optIdx
					hasSubmit := len(entry.questions) > 1
					for _, q := range entry.questions {
						if q.multiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						uuid, ok := pendingFiles.get(msgID)
						if !ok {
							http.Error(w, "pending file not found", 404)
							return
						}
						path := filepath.Join(pendingDir(), uuid+".json")
						pf, err := readPendingFile(path)
						if err != nil {
							http.Error(w, "failed to read pending file", 500)
							return
						}
						answers := buildAnswers(entry)
						ccOutput := buildAskCCOutput(pf.Payload, answers)
						if err := writePendingAnswer(uuid, ccOutput); err != nil {
							logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
							http.Error(w, "failed to save answer", 500)
							return
						}
						toolNotifs.markResolved(msgID)
						logger.Info(fmt.Sprintf("AskUserQuestion auto-resolved via API: msg_id=%d uuid=%s q=%d opt=%d label=%s answers=%v", msgID, uuid, qIdx, optIdx, qm.optionLabels[optIdx], answers))
						editChat := &tele.Chat{ID: entry.chatID}
						editMsg := &tele.Message{ID: msgID, Chat: editChat}
						bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, ""))
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected via API: msg_id=%d q=%d opt=%d label=%s", msgID, qIdx, optIdx, qm.optionLabels[optIdx]))
						newMarkup := rebuildAskMarkup(entry)
						editChat := &tele.Chat{ID: entry.chatID}
						editMsg := &tele.Message{ID: msgID, Chat: editChat}
						bot.Edit(editMsg, entry.msgText, newMarkup)
					}
				}
			}
		default:
			http.Error(w, "unsupported tool", 400)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/route/bind", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			TmuxTarget string `json:"tmux_target"`
			ChatID     int64  `json:"chat_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		creds.RouteMap[req.TmuxTarget] = req.ChatID
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route bound via API: tmux=%s → chat=%d", req.TmuxTarget, req.ChatID))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/route/unbind", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			TmuxTarget string `json:"tmux_target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		delete(creds.RouteMap, req.TmuxTarget)
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route unbound via API: tmux=%s", req.TmuxTarget))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/route/list", func(w http.ResponseWriter, r *http.Request) {
		creds, err := config.LoadCredentials()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"routes": creds.RouteMap})
	})
	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Target string `json:"target"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		target, err := injector.ParseTarget(req.Target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !injector.SessionExists(target) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		logger.Info(fmt.Sprintf("Inject API: target=%s text=%s", injector.FormatTarget(target), truncateStr(req.Text, 200)))
		if err := injector.InjectText(target, req.Text); err != nil {
			logger.Error(fmt.Sprintf("Inject API failed: %v", err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/capture", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			http.Error(w, "target required", http.StatusBadRequest)
			return
		}
		t, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !injector.SessionExists(t) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		content, err := injector.CapturePane(t)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"content": content})
	})
	mux.HandleFunc("/escape", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			http.Error(w, "target required", http.StatusBadRequest)
			return
		}
		t, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !injector.SessionExists(t) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if err := injector.SendKeys(t, "Escape"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/group/text", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		text := r.URL.Query().Get("text")
		if target == "" || text == "" {
			http.Error(w, "missing target or text", 400)
			return
		}
		msgID, entry, ok := toolNotifs.findByTmuxTarget(target)
		if !ok {
			// No pending AskUserQuestion — inject text
			t, err := injector.ParseTarget(target)
			if err != nil {
				http.Error(w, "invalid target", 400)
				return
			}
			if err := injector.InjectText(t, text); err != nil {
				http.Error(w, fmt.Sprintf("inject failed: %v", err), 500)
				return
			}
			logger.Info(fmt.Sprintf("Group text API injected: target=%s text=%s", target, truncateStr(text, 200)))
			fmt.Fprintf(w, "injected")
			return
		}
		uuid, uuidOk := pendingFiles.get(msgID)
		if !uuidOk {
			http.Error(w, "pending file not found", 404)
			return
		}
		path := filepath.Join(pendingDir(), uuid+".json")
		pf, err := readPendingFile(path)
		if err != nil {
			http.Error(w, "failed to read pending file", 500)
			return
		}
		answers := make(map[string]string)
		if len(entry.questions) > 0 {
			answers[entry.questions[0].questionText] = text
		}
		ccOutput := buildAskCCOutput(pf.Payload, answers)
		if err := writePendingAnswer(uuid, ccOutput); err != nil {
			http.Error(w, "failed to write answer", 500)
			return
		}
		toolNotifs.markResolved(msgID)
		logger.Info(fmt.Sprintf("AskUserQuestion resolved via group text API: msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(text, 200)))
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
		bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"))
		fmt.Fprintf(w, "resolved")
	})
	mux.HandleFunc("/perm/switch", func(w http.ResponseWriter, r *http.Request) {
		targetStr := r.URL.Query().Get("target")
		mode := r.URL.Query().Get("mode")
		if targetStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "target required"})
			return
		}
		if mode == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "mode required"})
			return
		}
		t, err := injector.ParseTarget(targetStr)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		if !injector.SessionExists(t) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "session not found"})
			return
		}
		logger.Info(fmt.Sprintf("Perm switch API: target=%s mode=%s", injector.FormatTarget(t), mode))
		finalMode, err := switchPermMode(t, mode)
		if err != nil {
			logger.Info(fmt.Sprintf("Perm switch API failed: %v", err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "mode": finalMode})
	})
	mux.HandleFunc("/perm/status", func(w http.ResponseWriter, r *http.Request) {
		targetStr := r.URL.Query().Get("target")
		if targetStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "target required"})
			return
		}
		t, err := injector.ParseTarget(targetStr)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		if !injector.SessionExists(t) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "session not found"})
			return
		}
		mode, content, err := detectPermMode(t)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "mode": mode, "content": content})
	})
}

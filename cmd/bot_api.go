package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
				CWD:        entry.cwd,
				Body:       entry.chunks[pageNum-1],
				TmuxTarget: entry.tmuxTarget,
				Page:       pageNum,
				TotalPages: len(entry.chunks),
			})
		}
		kb := buildPageKeyboardWithExtra(pageNum, len(entry.chunks), entry.permRows)
		editMsg := &tele.Message{ID: msgID, Chat: chat}
		_, err = retryEdit(bot, editMsg, text, kb)
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
		d, err := doDecidePerm(bot, msgID, decision)
		if err != nil {
			if err.Error() == "session disconnected" {
				http.Error(w, "session disconnected", 410)
				return
			}
			http.Error(w, err.Error(), 404)
			return
		}
		respJSON, _ := json.Marshal(d)
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	})
	mux.HandleFunc("/tool/respond", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		tool := r.URL.Query().Get("tool")
		action := r.URL.Query().Get("action")
		// Pre-check session liveness before processing the response
		if entry, ok := toolNotifs.get(msgID); ok && entry.tmuxTarget != "" {
			if !checkSessionAlive(entry.tmuxTarget, bot) {
				http.Error(w, "session disconnected", 410)
				return
			}
		}
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
				answers := make(map[string]string)
				if len(entry.questions) > 0 {
					answers[entry.questions[0].questionText] = value
				}
				if err := doRespondAsk(bot, msgID, answers, "✅ Text answer"); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
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
				if err := doRespondAsk(bot, msgID, buildAnswers(entry), ""); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else if action == "chat" {
				if err := doChatAsk(bot, msgID); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
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
					retryEdit(bot, editMsg, entry.msgText, newMarkup)
				} else {
					qm.selectedOption = optIdx
					hasSubmit := len(entry.questions) > 1
					for _, q := range entry.questions {
						if q.multiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						if err := doRespondAsk(bot, msgID, buildAnswers(entry), ""); err != nil {
							http.Error(w, err.Error(), 500)
							return
						}
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected via API: msg_id=%d q=%d opt=%d label=%s", msgID, qIdx, optIdx, qm.optionLabels[optIdx]))
						newMarkup := rebuildAskMarkup(entry)
						editChat := &tele.Chat{ID: entry.chatID}
						editMsg := &tele.Message{ID: msgID, Chat: editChat}
						retryEdit(bot, editMsg, entry.msgText, newMarkup)
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
			CWD        string `json:"cwd"`
			Type       string `json:"type"`
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
		if req.Type == "project" {
			if req.CWD == "" {
				http.Error(w, "cwd required for project binding", http.StatusBadRequest)
				return
			}
			creds.ProjectRouteMap[req.CWD] = req.ChatID
		} else {
			creds.RouteMap[req.TmuxTarget] = req.ChatID
		}
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route bound via API: type=%s tmux=%s cwd=%s → chat=%d", req.Type, req.TmuxTarget, req.CWD, req.ChatID))
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
			CWD        string `json:"cwd"`
			Type       string `json:"type"`
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
		if req.Type == "project" {
			delete(creds.ProjectRouteMap, req.CWD)
		} else {
			delete(creds.RouteMap, req.TmuxTarget)
		}
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route unbound via API: type=%s tmux=%s cwd=%s", req.Type, req.TmuxTarget, req.CWD))
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
		json.NewEncoder(w).Encode(map[string]interface{}{
			"routes":         creds.RouteMap,
			"project_routes": creds.ProjectRouteMap,
		})
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
		// Strip socket prefix so the target matches stored pane IDs
		target = notify.FormatPaneID(target)
		// Check pending PermissionRequest first
		if permMsgID, ok := pendingPerms.findByTmuxTarget(target); ok {
			doCancelPerm(bot, permMsgID)
			t, err := injector.ParseTarget(target)
			if err != nil {
				http.Error(w, "invalid target", 400)
				return
			}
			go func() {
				time.Sleep(3 * time.Second)
				injector.InjectText(t, text)
			}()
			logger.Info(fmt.Sprintf("Permission cancelled via group text API + delayed inject: target=%s text=%s", target, truncateStr(text, 200)))
			fmt.Fprintf(w, "cancelled+injected")
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
			if !checkSessionAlive(target, bot) {
				http.Error(w, "session disconnected", 410)
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
		if handleStalePending(msgID, uuid, bot) {
			// Stale: hook dead or file missing, inject text instead
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
		retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"))
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
	mux.HandleFunc("/session/idle", func(w http.ResponseWriter, r *http.Request) {
		targetFilter := r.URL.Query().Get("target")
		sessions := sessionState.all()

		type sessionIdleEntry struct {
			Target string `json:"target"`
			Idle   bool   `json:"idle"`
		}
		result := make(map[string]sessionIdleEntry)
		allIdle := len(sessions) > 0 // empty sessions = not idle

		for sid, info := range sessions {
			if targetFilter != "" && info.tmuxTarget != targetFilter {
				continue
			}
			running := isSessionRunning(info.tmuxTarget)
			if running {
				allIdle = false
			}
			result[sid] = sessionIdleEntry{Target: info.tmuxTarget, Idle: !running}
		}

		// If target filter specified but no match found, not idle
		if targetFilter != "" && len(result) == 0 {
			allIdle = false
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"idle":     allIdle,
			"sessions": result,
		})
	})
	mux.HandleFunc("/pending/cancel", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.URL.Query().Get("uuid")
		if uuid == "" {
			http.Error(w, "missing uuid", 400)
			return
		}
		msgID, found := pendingFiles.findByUUID(uuid)
		if !found {
			w.WriteHeader(200)
			return
		}
		if _, ok := pendingPerms.getTarget(msgID); ok {
			doCancelPerm(bot, msgID)
		}
		if entry, ok := toolNotifs.get(msgID); ok && !entry.resolved {
			doCancelAsk(bot, msgID)
		}
		pendingFiles.remove(msgID)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/mcp/send-file", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FilePath string `json:"file_path"`
			Caption  string `json:"caption"`
			CWD      string `json:"cwd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		info, err := os.Stat(req.FilePath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("file not found: %s", req.FilePath)})
			return
		}
		if !info.Mode().IsRegular() {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not a regular file"})
			return
		}
		if info.Size() > 50*1024*1024 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("file too large: %d bytes (max 50MB for Telegram Bot API)", info.Size())})
			return
		}
		chat, _ := resolveChat("", req.CWD)
		doc := &tele.Document{
			File:     tele.FromDisk(req.FilePath),
			FileName: filepath.Base(req.FilePath),
			Caption:  req.Caption,
		}
		msg, err := retrySend(bot, chat, doc)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("telegram send failed: %v", err)})
			return
		}
		logger.Info(fmt.Sprintf("[MCP] File sent: %s to chat %d (msg_id=%d)", filepath.Base(req.FilePath), chat.ID, msg.ID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("File sent: %s", filepath.Base(req.FilePath))})
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
	mux.HandleFunc("/bot_new", func(w http.ResponseWriter, r *http.Request) {
		chatID, err := strconv.ParseInt(creds.PairingAllow.DefaultChatID, 10, 64)
		if err != nil {
			http.Error(w, "no default chat configured", 400)
			return
		}
		session := r.URL.Query().Get("session")
		workdir := r.URL.Query().Get("workdir")
		command := r.URL.Query().Get("command")
		state := &LaunchState{
			SessionName: session,
			WorkDir:     workdir,
			Command:     command,
			ChatID:      chatID,
			UUID:        generateLaunchUUID(),
		}
		if session == "" {
			askSessionName(bot, chatID, state)
		} else if workdir == "" {
			askWorkDir(bot, chatID, state)
		} else {
			go executeLaunch(bot, chatID, state)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "uuid": state.UUID})
	})
	mux.HandleFunc("/bot_new/callback", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.URL.Query().Get("uuid")
		data := r.URL.Query().Get("data")
		if uuid == "" || data == "" {
			http.Error(w, "missing uuid or data", 400)
			return
		}
		var state *LaunchState
		var msgID int
		launchPending.Range(func(key, val interface{}) bool {
			s := val.(*LaunchState)
			if s.UUID == uuid {
				state = s
				msgID = key.(int)
				return false
			}
			return true
		})
		if state == nil {
			http.Error(w, "launch not found", 404)
			return
		}
		chatID := state.ChatID
		msg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		cfg, _ := config.LoadAppConfig()
		switch {
		case data == "session_default":
			state.SessionName = cfg.DefaultSessionName
			retryEdit(bot, msg, fmt.Sprintf("📦 Session name\n✅ %s", state.SessionName))
			launchPending.Delete(msgID)
			if state.WorkDir == "" {
				askWorkDir(bot, chatID, state)
			} else {
				go executeLaunch(bot, chatID, state)
			}
		case data == "dir_select":
			state.WorkDir = state.BrowsePath
			retryEdit(bot, msg, fmt.Sprintf("📂 Working directory\n✅ %s", state.WorkDir))
			launchPending.Delete(msgID)
			go executeLaunch(bot, chatID, state)
		case data == "cd_up":
			parent := filepath.Dir(state.BrowsePath)
			if parent != state.BrowsePath {
				state.BrowsePath = parent
				state.DirPage = 0
			}
			refreshDirBrowser(bot, msg, state)
		case strings.HasPrefix(data, "cd:"):
			idx, err := strconv.Atoi(strings.TrimPrefix(data, "cd:"))
			if err == nil {
				dirs, _ := listSubDirs(state.BrowsePath, state.ShowHidden)
				if idx >= 0 && idx < len(dirs) {
					state.BrowsePath = filepath.Join(state.BrowsePath, dirs[idx])
					state.DirPage = 0
				}
			}
			refreshDirBrowser(bot, msg, state)
		case data == "toggle_hidden":
			state.ShowHidden = !state.ShowHidden
			state.DirPage = 0
			refreshDirBrowser(bot, msg, state)
		case data == "page_prev":
			if state.DirPage > 0 {
				state.DirPage--
			}
			refreshDirBrowser(bot, msg, state)
		case data == "page_next":
			state.DirPage++
			refreshDirBrowser(bot, msg, state)
		case data == "cancel":
			retryEdit(bot, msg, "❌ Launch cancelled.")
			launchPending.Delete(msgID)
			deleteLaunchState(state.UUID)
			logger.Info(fmt.Sprintf("bot_new API: cancel pressed msg_id=%d uuid=%s", msgID, state.UUID))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	mux.HandleFunc("/bot_new/reply", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.URL.Query().Get("uuid")
		text := r.URL.Query().Get("text")
		if uuid == "" || text == "" {
			http.Error(w, "missing uuid or text", 400)
			return
		}
		var state *LaunchState
		var msgID int
		launchPending.Range(func(key, val interface{}) bool {
			s := val.(*LaunchState)
			if s.UUID == uuid {
				state = s
				msgID = key.(int)
				return false
			}
			return true
		})
		if state == nil {
			http.Error(w, "launch not found", 404)
			return
		}
		chatID := state.ChatID
		msg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		launchPending.Delete(msgID)
		switch state.Step {
		case "session":
			state.SessionName = strings.TrimSpace(text)
			retryEdit(bot, msg, fmt.Sprintf("📦 Session name\n✅ %s", state.SessionName))
			if state.WorkDir == "" {
				askWorkDir(bot, chatID, state)
			} else {
				go executeLaunch(bot, chatID, state)
			}
		case "workdir":
			customValue := strings.TrimSpace(text)
			if strings.HasPrefix(customValue, "~") {
				home, _ := os.UserHomeDir()
				customValue = home + customValue[1:]
			}
			state.WorkDir = customValue
			retryEdit(bot, msg, fmt.Sprintf("📂 Working directory\n✅ %s", state.WorkDir))
			go executeLaunch(bot, chatID, state)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	mux.HandleFunc("/resume/list", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			http.Error(w, "missing target", 400)
			return
		}
		parsed, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		tmuxStr := injector.FormatTarget(parsed)
		info := sessionState.findInfoByTarget(tmuxStr)
		if info == nil {
			http.Error(w, "no session found for target", 400)
			return
		}
		currentSID, _ := sessionState.findByTarget(tmuxStr)
		var sessions []sessionListEntry
		if info.projectDir != "" {
			sessions, err = listProjectSessionsByDir(info.projectDir, 8, currentSID)
		} else {
			sessions, err = listProjectSessions(info.cwd, 8, currentSID)
		}
		if err != nil {
			http.Error(w, "failed to list sessions: "+err.Error(), 500)
			return
		}
		type sessionJSON struct {
			ID       string `json:"id"`
			Prompt   string `json:"prompt"`
			Source   string `json:"source"`
			Modified string `json:"modified"`
		}
		var result []sessionJSON
		for _, s := range sessions {
			result = append(result, sessionJSON{
				ID:       s.SessionID,
				Prompt:   s.Summary,
				Source:   s.SummarySource,
				Modified: s.Modified.Format(time.RFC3339),
			})
		}
		if result == nil {
			result = []sessionJSON{}
		}
		logger.Info(fmt.Sprintf("Resume list: target=%s sessions=%d", tmuxStr, len(result)))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": result})
	})
	mux.HandleFunc("/resume/select", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		sessionID := r.URL.Query().Get("session_id")
		if target == "" || sessionID == "" {
			http.Error(w, "missing target or session_id", 400)
			return
		}
		parsed, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if !checkSessionAlive(injector.FormatTarget(parsed), bot) {
			http.Error(w, "session not alive", 410)
			return
		}
		if err := injector.InjectText(parsed, "/resume "+sessionID); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		logger.Info(fmt.Sprintf("Resume injected via API: target=%s session=%s", injector.FormatTarget(parsed), sessionID))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
}

package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

var tmuxPaneCache sync.Map   // session name → pane ID
var pendingExitKill sync.Map // tmux target → true (set by /session/exit, consumed by SessionEnd)

type watchEvent struct {
	Event   string `json:"event"`
	Agent   string `json:"agent"`
	Summary string `json:"summary"`
	Detail  string `json:"detail"`
}

type sessionWatchStore struct {
	mu      sync.Mutex
	waiters map[string][]chan watchEvent
}

var sessionWatch = &sessionWatchStore{
	waiters: make(map[string][]chan watchEvent),
}

func (sw *sessionWatchStore) register(name string) chan watchEvent {
	ch := make(chan watchEvent, 1)
	sw.mu.Lock()
	sw.waiters[name] = append(sw.waiters[name], ch)
	sw.mu.Unlock()
	return ch
}

func (sw *sessionWatchStore) cancel(name string, ch chan watchEvent) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	list := sw.waiters[name]
	for i, c := range list {
		if c == ch {
			sw.waiters[name] = append(list[:i], list[i+1:]...)
			break
		}
	}
}

func notifySessionWatchers(agentName string, evt watchEvent) {
	if agentName == "" {
		return
	}
	sessionWatch.mu.Lock()
	waiters := sessionWatch.waiters[agentName]
	if len(waiters) > 0 {
		for _, ch := range waiters {
			select {
			case ch <- evt:
			default:
			}
		}
		sessionWatch.waiters[agentName] = nil
	}
	sessionWatch.mu.Unlock()
}

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
		if entry.rawMode {
			_, err = retryEdit(bot, editMsg, text, kb)
		} else {
			_, err = retryEdit(bot, editMsg, text, kb, tele.ModeHTML)
		}
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
					retryEdit(bot, editMsg, entry.msgText, newMarkup, tele.ModeHTML)
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
						retryEdit(bot, editMsg, entry.msgText, newMarkup, tele.ModeHTML)
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
			Name    string `json:"name"`
			ChatID  int64  `json:"chat_id"`
			TopicID int    `json:"topic_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		creds.NameRouteMap[req.Name] = config.NameRoute{ChatID: req.ChatID, TopicID: req.TopicID}
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route bound via API: name=%s → chat=%d topic=%d", req.Name, req.ChatID, req.TopicID))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/route/unbind", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
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
		delete(creds.NameRouteMap, req.Name)
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route unbound via API: name=%s", req.Name))
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
			"name_routes": creds.NameRouteMap,
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
		retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Text answer"), tele.ModeHTML)
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
			Busy   bool   `json:"busy"`
		}
		result := make(map[string]sessionIdleEntry)
		allIdle := len(sessions) > 0 // empty sessions = not idle

		for sid, info := range sessions {
			if targetFilter != "" && info.tmuxTarget != targetFilter {
				continue
			}
			running := isSessionRunning(info.tmuxTarget)
			busy := isSessionBusy(info.tmuxTarget)
			if running {
				allIdle = false
			}
			result[sid] = sessionIdleEntry{Target: info.tmuxTarget, Idle: !running, Busy: busy}
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
	mux.HandleFunc("/session/name", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session_id")
		name := r.URL.Query().Get("name")
		ok, errMsg := sessionState.setName(sessionID, name)
		if !ok {
			http.Error(w, errMsg, 400)
			return
		}
		logger.Info(fmt.Sprintf("Session name set via API: session=%s name=%s", sessionID, name))
		w.Write([]byte(`{"ok":true}`))
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
			FilePath    string `json:"file_path"`
			Caption     string `json:"caption"`
			TmuxTarget  string `json:"tmux_target"`
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
		chat, _, topicID := resolveChat(req.TmuxTarget)
		ext := strings.ToLower(filepath.Ext(req.FilePath))
		imageExts := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
		var sendable interface{}
		if imageExts[ext] {
			sendable = &tele.Photo{
				File:    tele.FromDisk(req.FilePath),
				Caption: req.Caption,
			}
		} else {
			sendable = &tele.Document{
				File:     tele.FromDisk(req.FilePath),
				FileName: filepath.Base(req.FilePath),
				Caption:  req.Caption,
			}
		}
		var sendOpts []interface{}
		if topicID > 0 {
			sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
		}
		msg, err := retrySend(bot, chat, sendable, sendOpts...)
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
		agentName := r.URL.Query().Get("name")
		state := &LaunchState{
			SessionName: session,
			WorkDir:     workdir,
			Command:     command,
			AgentName:   agentName,
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
			retryEdit(bot, msg, fmt.Sprintf("📦 Session name\n✅ %s", markdown.EscapeHTML(state.SessionName)), tele.ModeHTML)
			launchPending.Delete(msgID)
			if state.WorkDir == "" {
				askWorkDir(bot, chatID, state)
			} else {
				go executeLaunch(bot, chatID, state)
			}
		case data == "dir_select":
			state.WorkDir = state.BrowsePath
			retryEdit(bot, msg, fmt.Sprintf("📂 Working directory\n✅ %s", markdown.EscapeHTML(state.WorkDir)), tele.ModeHTML)
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
			retryEdit(bot, msg, "❌ Launch cancelled.", tele.ModeHTML)
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
			retryEdit(bot, msg, fmt.Sprintf("📦 Session name\n✅ %s", markdown.EscapeHTML(state.SessionName)), tele.ModeHTML)
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
			retryEdit(bot, msg, fmt.Sprintf("📂 Working directory\n✅ %s", markdown.EscapeHTML(state.WorkDir)), tele.ModeHTML)
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
	// Merge mode API (for E2E testing)
	mux.HandleFunc("/merge/start", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			http.Error(w, "target required", 400)
			return
		}
		parsed, err := injector.ParseTarget(target)
		if err != nil || !injector.SessionExists(parsed) {
			http.Error(w, "invalid target", 400)
			return
		}
		key := mergeKey(0)
		if mergeBuffers.get(key) != nil {
			http.Error(w, "merge already active", 409)
			return
		}
		mergeBuffers.start(key, 0, target, 0)
		logger.Info(fmt.Sprintf("Merge started via API: target=%s", target))
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/merge/add", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
			http.Error(w, "text required", 400)
			return
		}
		key := mergeKey(0)
		if mergeBuffers.get(key) == nil {
			http.Error(w, "no active merge", 404)
			return
		}
		mergeBuffers.add(key, req.Text)
		logger.Info(fmt.Sprintf("Merge add via API: text=%s", truncateStr(req.Text, 200)))
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/merge/submit", func(w http.ResponseWriter, r *http.Request) {
		key := mergeKey(0)
		buf, ok := mergeBuffers.finish(key)
		if !ok || len(buf.items) == 0 {
			http.Error(w, "no items to submit", 404)
			return
		}
		target, err := injector.ParseTarget(buf.tmuxTarget)
		if err != nil || !injector.SessionExists(target) {
			http.Error(w, "session not found", 404)
			return
		}
		merged := strings.Join(buf.items, "\n")
		if err := injector.InjectText(target, merged); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		logger.Info(fmt.Sprintf("Merge submitted via API: target=%s items=%d text=%s", buf.tmuxTarget, len(buf.items), truncateStr(merged, 200)))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "items": len(buf.items)})
	})
	// Cron job management API
	mux.HandleFunc("/cron/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Mode       string `json:"mode"`
			Schedule   string `json:"schedule"`
			Once       bool   `json:"once"`
			Prompt     string `json:"prompt"`
			AgentName  string `json:"agent_name"`
			TmuxTarget string `json:"tmux_target"`
			Name       string `json:"name"`
			CWD        string `json:"cwd"`
			MaxTurns   int    `json:"max_turns"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Mode == "" || req.Schedule == "" || req.Prompt == "" {
			http.Error(w, "mode, schedule, and prompt required", http.StatusBadRequest)
			return
		}
		if req.Mode == "inject" && req.AgentName == "" && req.TmuxTarget == "" {
			http.Error(w, "agent_name or tmux_target required for inject mode", http.StatusBadRequest)
			return
		}
		job := &cronJob{
			ID:         generateCronID(),
			Name:       req.Name,
			Mode:       req.Mode,
			Schedule:   req.Schedule,
			Once:       req.Once,
			Prompt:     req.Prompt,
			AgentName:  req.AgentName,
			TmuxTarget: req.TmuxTarget,
			CWD:        req.CWD,
			MaxTurns:   req.MaxTurns,
			CreatedAt:  time.Now(),
		}
		if err := cronJobs.add(job); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		logger.Info(fmt.Sprintf("Cron job added via API: id=%s mode=%s schedule=%s name=%s", job.ID[:8], job.Mode, job.Schedule, job.Name))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "id": job.ID})
	})
	mux.HandleFunc("/cron/list", func(w http.ResponseWriter, r *http.Request) {
		jobs := cronJobs.all()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"jobs": jobs})
	})
	mux.HandleFunc("/cron/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		idOrName := req.ID
		if idOrName == "" {
			idOrName = req.Name
		}
		if !cronJobs.remove(idOrName) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		logger.Info(fmt.Sprintf("Cron job removed via API: id_or_name=%s", idOrName))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	mux.HandleFunc("/cron/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID      string            `json:"id"`
			Updates map[string]string `json:"updates"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !cronJobs.update(req.ID, req.Updates) {
			http.Error(w, "job not found or name conflict", http.StatusNotFound)
			return
		}
		logger.Info(fmt.Sprintf("Cron job updated via API: id=%s", req.ID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	// Tmux management API
	mux.HandleFunc("/tmux/list", func(w http.ResponseWriter, r *http.Request) {
		out, err := exec.Command("tmux", "list-panes", "-a", "-F",
			"#{session_name}:#{window_index}.#{pane_index} #{pane_id} #{pane_current_path} #{pane_pid} #{pane_current_command}").Output()
		type paneEntry struct {
			Target      string `json:"target"`
			PaneID      string `json:"pane_id"`
			CWD         string `json:"cwd"`
			PID         int    `json:"pid"`
			Command     string `json:"command"`
			SessionName string `json:"session_name"`
			AgentName   string `json:"agent_name,omitempty"`
		}
		var panes []paneEntry
		if err == nil {
			sessions := sessionState.all()
			// Build pane_id → agent_name lookup from tracked sessions
			nameByPaneID := make(map[string]string)
			for _, info := range sessions {
				paneID := notify.FormatPaneID(info.tmuxTarget)
				if info.name != "" {
					nameByPaneID[paneID] = info.name
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line == "" {
					continue
				}
				// Format: "session:win.pane %N /path PID command"
				parts := strings.Fields(line)
				if len(parts) < 5 {
					continue
				}
				target := parts[0]
				paneID := parts[1]
				cwd := parts[2]
				pid, _ := strconv.Atoi(parts[3])
				command := parts[4]
				sessionName := ""
				if colonIdx := strings.Index(target, ":"); colonIdx != -1 {
					sessionName = target[:colonIdx]
				}
				entry := paneEntry{
					Target:      target,
					PaneID:      paneID,
					CWD:         cwd,
					PID:         pid,
					Command:     command,
					SessionName: sessionName,
					AgentName:   nameByPaneID[paneID],
				}
				panes = append(panes, entry)
			}
		}
		if panes == nil {
			panes = []paneEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"panes": panes})
	})
	mux.HandleFunc("/tmux/kill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Target string `json:"target"`
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
		formatted := injector.FormatTarget(target)
		if err := exec.Command("tmux", "kill-pane", "-t", notify.FormatPaneID(formatted)).Run(); err != nil {
			logger.Error(fmt.Sprintf("tmux kill-pane failed: target=%s err=%v", formatted, err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("tmux kill-pane via API: target=%s", formatted))
		cleanDeadSession(formatted, bot)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/tmux/event", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Event   string `json:"event"`
			Session string `json:"session"`
			Pane    string `json:"pane"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Info(fmt.Sprintf("tmux event received: event=%s session=%s pane=%s", req.Event, req.Session, req.Pane))
		switch req.Event {
		case "session-created":
			var notifyChat *tele.Chat
			var topicID int
			paneID := ""
			if req.Session != "" {
				info := sessionState.findByName(req.Session)
				if info != nil {
					notifyChat, _, topicID = resolveChat(info.tmuxTarget)
				}
				panes, err := injector.ListPanes(req.Session)
				if err == nil && len(panes) > 0 {
					paneID = panes[0]
					tmuxPaneCache.Store(req.Session, paneID)
				}
			}
			if notifyChat == nil {
				chatID, err := strconv.ParseInt(creds.PairingAllow.DefaultChatID, 10, 64)
				if err == nil {
					notifyChat = &tele.Chat{ID: chatID}
				}
			}
			if notifyChat != nil {
				label := req.Session
				if paneID != "" {
					label = fmt.Sprintf("%s (%s)", paneID, req.Session)
				}
				if label == "" {
					label = "(unknown)"
				}
				msgText := fmt.Sprintf("🟢 Tmux Session Created: %s", label)
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				retrySend(bot, notifyChat, msgText, sendOpts...)
				logger.Info(fmt.Sprintf("tmux session-created notification: pane=%s session=%s", paneID, req.Session))
			}
		case "session-closed":
			var notifyChat *tele.Chat
			var topicID int
			agentName := ""
			paneID := ""
			if req.Session != "" {
				if cached, ok := tmuxPaneCache.LoadAndDelete(req.Session); ok {
					paneID = cached.(string)
				}
				info := sessionState.findByName(req.Session)
				if info != nil {
					agentName = info.name
					if paneID == "" {
						paneID = notify.FormatPaneID(info.tmuxTarget)
					}
					notifyChat, _, topicID = resolveChat(info.tmuxTarget)
				}
			}
			if notifyChat == nil {
				chatID, err := strconv.ParseInt(creds.PairingAllow.DefaultChatID, 10, 64)
				if err == nil {
					notifyChat = &tele.Chat{ID: chatID}
				}
			}
			for _, si := range sessionState.all() {
				target, err := injector.ParseTarget(si.tmuxTarget)
				if err != nil || !injector.SessionExists(target) {
					cleanDeadSession(si.tmuxTarget, bot)
				}
			}
			if notifyChat != nil {
				label := req.Session
				if paneID != "" {
					label = fmt.Sprintf("%s (%s)", paneID, req.Session)
				}
				if agentName != "" {
					label = fmt.Sprintf("%s [%s]", label, agentName)
				}
				if label == "" {
					label = "(unknown)"
				}
				msgText := fmt.Sprintf("🔴 Tmux Session Closed: %s", label)
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				retrySend(bot, notifyChat, msgText, sendOpts...)
				logger.Info(fmt.Sprintf("tmux session-closed notification: pane=%s session=%s agent=%s", paneID, req.Session, agentName))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/session/list", func(w http.ResponseWriter, r *http.Request) {
		sessions := sessionState.all()
		type sessionListItem struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Target     string `json:"target"`
			CWD        string `json:"cwd"`
			ProjectDir string `json:"project_dir"`
			Running    bool   `json:"running"`
		}
		items := make([]sessionListItem, 0, len(sessions))
		for sid, info := range sessions {
			items = append(items, sessionListItem{
				ID:         sid,
				Name:       info.name,
				Target:     info.tmuxTarget,
				CWD:        info.cwd,
				ProjectDir: info.projectDir,
				Running:    isSessionRunning(info.tmuxTarget),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": items})
	})
	mux.HandleFunc("/session/log", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		linesParam := r.URL.Query().Get("lines")
		maxLines := 20
		if linesParam != "" {
			if n, err := strconv.Atoi(linesParam); err == nil && n > 0 {
				maxLines = n
			}
		}
		noTools := r.URL.Query().Get("no_tools") == "true" || r.URL.Query().Get("no_tools") == "1"

		// Tools that are filtered out when no_tools=true (AskUserQuestion is NOT in this list)
		filteredTools := map[string]bool{
			"Bash": true, "Read": true, "Write": true, "Edit": true,
			"Glob": true, "Grep": true, "Agent": true, "WebFetch": true,
			"WebSearch": true, "NotebookEdit": true,
		}

		// Find session by name and get session ID for context lookup
		var info *sessionInfo
		var sessionID string
		for sid, si := range sessionState.all() {
			if si.name == name {
				cp := si
				info = &cp
				sessionID = sid
				break
			}
		}
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		// Determine the JSONL directory
		home, err := os.UserHomeDir()
		if err != nil {
			http.Error(w, "cannot determine home dir", http.StatusInternalServerError)
			return
		}
		var jsonlDir string
		if info.projectDir != "" {
			jsonlDir = info.projectDir
		} else {
			jsonlDir = filepath.Join(home, ".claude", "projects", projectSlug(info.cwd))
		}

		// Only read the current session's JSONL file
		jsonlPath := filepath.Join(jsonlDir, sessionID+".jsonl")
		if _, err := os.Stat(jsonlPath); err != nil {
			http.Error(w, "transcript not found", http.StatusNotFound)
			return
		}
		type fileEntry struct {
			path    string
			modTime time.Time
		}
		jsonlFiles := []fileEntry{{path: jsonlPath, modTime: time.Now()}}

		type logEntry struct {
			Type       string `json:"type"`
			Timestamp  string `json:"timestamp"`
			Text       string `json:"text"`
			Tool       string `json:"tool"`
			ToolDetail string `json:"tool_detail"`
		}

		// formatToolDetail formats human-readable tool details based on tool name and input
		formatToolDetail := func(toolName string, input map[string]interface{}) string {
			truncate := func(s string, n int) string {
				if len(s) > n {
					return s[:n] + "..."
				}
				return s
			}
			switch toolName {
			case "AskUserQuestion":
				// AskUserQuestion input has a "questions" array
				if questions, ok := input["questions"].([]interface{}); ok && len(questions) > 0 {
					var parts []string
					for qi, q := range questions {
						qMap, _ := q.(map[string]interface{})
						if qMap == nil {
							continue
						}
						header, _ := qMap["header"].(string)
						question, _ := qMap["question"].(string)
						line := fmt.Sprintf("Q%d %q: %s", qi+1, header, question)
						if opts, ok := qMap["options"].([]interface{}); ok {
							for i, o := range opts {
								oMap, _ := o.(map[string]interface{})
								if oMap != nil {
									label, _ := oMap["label"].(string)
									line += fmt.Sprintf("\n  %d. %s", i+1, label)
								} else {
									line += fmt.Sprintf("\n  %d. %v", i+1, o)
								}
							}
						}
						parts = append(parts, line)
					}
					return strings.Join(parts, "\n")
				}
				return "AskUserQuestion"
			case "Bash":
				cmd, _ := input["command"].(string)
				desc, _ := input["description"].(string)
				if desc != "" {
					return truncate(cmd, 200) + "\nℹ️ " + desc
				}
				return truncate(cmd, 200)
			case "Edit":
				fp, _ := input["file_path"].(string)
				oldStr, _ := input["old_string"].(string)
				newStr, _ := input["new_string"].(string)
				detail := fp
				if oldStr != "" {
					detail += "\nOld: " + truncate(oldStr, 80)
				}
				if newStr != "" {
					detail += "\nNew: " + truncate(newStr, 80)
				}
				return detail
			case "Write", "Read":
				fp, _ := input["file_path"].(string)
				return fp
			case "Glob", "Grep":
				pat, _ := input["pattern"].(string)
				return pat
			case "Agent":
				desc, _ := input["description"].(string)
				return truncate(desc, 200)
			default:
				return toolName
			}
		}

		var result []logEntry

		for _, jf := range jsonlFiles {
			if len(result) >= maxLines {
				break
			}
			f, err := os.Open(jf.path)
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
			var fileEntries []logEntry
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				var raw struct {
					Type      string          `json:"type"`
					Model     string          `json:"model"`
					Timestamp string          `json:"timestamp"`
					Message   json.RawMessage `json:"message"`
				}
				if json.Unmarshal([]byte(line), &raw) != nil {
					continue
				}
				// Skip progress and synthetic entries
				if raw.Type == "progress" || raw.Model == "<synthetic>" {
					continue
				}
				if raw.Type == "user" {
					var msg struct {
						Content json.RawMessage `json:"content"`
					}
					if json.Unmarshal(raw.Message, &msg) != nil {
						continue
					}
					// Try string content
					var contentStr string
					if json.Unmarshal(msg.Content, &contentStr) == nil {
						fileEntries = append(fileEntries, logEntry{Type: "user", Timestamp: raw.Timestamp, Text: contentStr})
						continue
					}
					// Try array content
					var contentArr []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if json.Unmarshal(msg.Content, &contentArr) != nil {
						continue
					}
					hasToolResult := false
					for _, c := range contentArr {
						if c.Type == "tool_result" {
							hasToolResult = true
							break
						}
					}
					if noTools && hasToolResult {
						var parts []string
						for _, c := range contentArr {
							if c.Type == "text" && c.Text != "" {
								parts = append(parts, c.Text)
							}
						}
						if len(parts) > 0 {
							fileEntries = append(fileEntries, logEntry{Type: "user", Timestamp: raw.Timestamp, Text: strings.Join(parts, "\n")})
						}
						continue
					}
					var parts []string
					for _, c := range contentArr {
						if c.Type == "text" && c.Text != "" {
							parts = append(parts, c.Text)
						}
					}
					if len(parts) > 0 {
						fileEntries = append(fileEntries, logEntry{Type: "user", Timestamp: raw.Timestamp, Text: strings.Join(parts, "\n")})
					}
				} else if raw.Type == "assistant" {
					var msg struct {
						Content json.RawMessage `json:"content"`
					}
					if json.Unmarshal(raw.Message, &msg) != nil {
						continue
					}
					// Parse content blocks as raw to handle both text and tool_use
					var contentBlocks []json.RawMessage
					if json.Unmarshal(msg.Content, &contentBlocks) != nil {
						continue
					}
					var textParts []string
					toolName := ""
					toolDetail := ""
					allFilteredTools := true
					hasAnyBlock := false
					for _, block := range contentBlocks {
						var b struct {
							Type  string                 `json:"type"`
							Text  string                 `json:"text"`
							Name  string                 `json:"name"`
							Input map[string]interface{} `json:"input"`
						}
						if json.Unmarshal(block, &b) != nil {
							continue
						}
						hasAnyBlock = true
						if b.Type == "text" && b.Text != "" {
							textParts = append(textParts, b.Text)
							allFilteredTools = false
						} else if b.Type == "tool_use" {
							if !filteredTools[b.Name] {
								allFilteredTools = false
							}
							// Use first tool_use found for tool/tool_detail fields
							if toolName == "" {
								toolName = b.Name
								toolDetail = formatToolDetail(b.Name, b.Input)
							}
						}
					}
					if !hasAnyBlock {
						continue
					}
					// When no_tools=true: skip if ALL blocks are filtered tools (keep if text or AskUserQuestion)
					if noTools && allFilteredTools && len(textParts) == 0 {
						continue
					}
					if len(textParts) > 0 || toolName != "" {
						fileEntries = append(fileEntries, logEntry{
							Type:       "assistant",
							Timestamp:  raw.Timestamp,
							Text:       strings.Join(textParts, "\n"),
							Tool:       toolName,
							ToolDetail: toolDetail,
						})
					}
				}
			}
			f.Close()
			result = append(result, fileEntries...)
		}

		// Trim to maxLines (keep the most recent entries)
		if len(result) > maxLines {
			result = result[len(result)-maxLines:]
		}

		// Build context usage strings
		ctxPct := 0
		ctxUsed := ""
		ctxTotal := ""
		if usedPct, usedTokens, windowSize, ok := readContextUsage(sessionID); ok {
			ctxPct = usedPct
			formatK := func(n int) string {
				return fmt.Sprintf("%.1fk", float64(n)/1000)
			}
			ctxUsed = formatK(usedTokens)
			ctxTotal = formatK(windowSize)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"target":        info.tmuxTarget,
			"context_pct":   ctxPct,
			"context_used":  ctxUsed,
			"context_total": ctxTotal,
			"messages":      result,
		})
	})
	mux.HandleFunc("/session/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name     string `json:"name"`
			Text     string `json:"text"`
			From     string `json:"from"`
			NoHeader bool   `json:"noHeader"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info := sessionState.findByName(req.Name)
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		injectText := req.Text
		if req.From != "" && !req.NoHeader {
			injectText = fmt.Sprintf("---\n💬 Message from agent [%s]\n---\n%s", req.From, req.Text)
		}
		if err := safeInjectText(bot, info.tmuxTarget, injectText); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Session send via API: name=%s target=%s from=%s text=%s", req.Name, info.tmuxTarget, req.From, truncateStr(req.Text, 200)))
		if !req.NoHeader {
			// Send TG notification to the target session's chat
			chat, _, topicID := resolveChat(info.tmuxTarget)
			if chat != nil {
				fromLine := ""
				if req.From != "" {
					fromLine = fmt.Sprintf("📤 From: %s\n", req.From)
				}
				notifyText := fmt.Sprintf("💬 CLI Send\n%s━━━━━━━━━━\n%s", fromLine, req.Text)
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				chunks := splitBody(notifyText, 4000)
				if len(chunks) <= 1 {
					retrySend(bot, chat, notifyText, sendOpts...)
				} else {
					retrySend(bot, chat, chunks[0]+fmt.Sprintf("\n\n📄 1/%d", len(chunks)), sendOpts...)
				}
				logger.Info(fmt.Sprintf("Session send notification: target=%s text=%s", req.Name, truncateStr(req.Text, 200)))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/session/exit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info := sessionState.findByName(req.Name)
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		target := info.tmuxTarget
		// Cancel pending PermissionRequest
		if permMsgID, ok := pendingPerms.findByTmuxTarget(target); ok {
			doCancelPerm(bot, permMsgID)
			t, err := injector.ParseTarget(target)
			if err == nil {
				injector.SendKeys(t, "Escape")
			}
		}
		// Cancel pending AskUserQuestion
		if askMsgID, _, ok := toolNotifs.findByTmuxTarget(target); ok {
			doCancelAsk(bot, askMsgID)
		}
		// Wait for pending cleanup
		time.Sleep(1 * time.Second)
		// Inject /exit command
		t, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, "invalid target: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := injector.InjectText(t, "/exit"); err != nil {
			http.Error(w, "inject failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		pendingExitKill.Store(target, true)
		logger.Info(fmt.Sprintf("Session exit via API: name=%s target=%s", req.Name, target))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/session/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Session string `json:"session"`
			WorkDir string `json:"workdir"`
			Command string `json:"command"`
			Name    string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		chatID, err := strconv.ParseInt(creds.PairingAllow.DefaultChatID, 10, 64)
		if err != nil {
			http.Error(w, "no default chat configured", http.StatusBadRequest)
			return
		}
		state := &LaunchState{
			SessionName: req.Session,
			WorkDir:     req.WorkDir,
			Command:     req.Command,
			AgentName:   req.Name,
			ChatID:      chatID,
			UUID:        generateLaunchUUID(),
		}
		if req.Session != "" && req.WorkDir != "" {
			go executeLaunch(bot, chatID, state)
		} else if req.Session == "" {
			askSessionName(bot, chatID, state)
		} else {
			askWorkDir(bot, chatID, state)
		}
		logger.Info(fmt.Sprintf("Session new via API: session=%s workdir=%s name=%s uuid=%s", req.Session, req.WorkDir, req.Name, state.UUID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "uuid": state.UUID})
	})
	mux.HandleFunc("/session/watch", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		ch := sessionWatch.register(name)
		select {
		case evt := <-ch:
			logger.Info(fmt.Sprintf("Session watch event: agent=%s event=%s", name, evt.Event))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(evt)
		case <-r.Context().Done():
			sessionWatch.cancel(name, ch)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "timeout"})
		}
	})
}

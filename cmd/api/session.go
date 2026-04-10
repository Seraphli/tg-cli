package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/handlers"
	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

func registerSession(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot
	mux.HandleFunc("/session/idle", func(w http.ResponseWriter, r *http.Request) {
		targetFilter := r.URL.Query().Get("target")
		sessions := bs.SessionState.All()

		type sessionIdleEntry struct {
			Target string `json:"target"`
			Idle   bool   `json:"idle"`
			Busy   bool   `json:"busy"`
		}
		result := make(map[string]sessionIdleEntry)
		allIdle := len(sessions) > 0 // empty sessions = not idle

		for sid, info := range sessions {
			if targetFilter != "" && info.TmuxTarget != targetFilter {
				continue
			}
			running := helpers.IsSessionRunning(info.TmuxTarget)
			if running {
				allIdle = false
			}
			result[sid] = sessionIdleEntry{Target: info.TmuxTarget, Idle: !running, Busy: running}
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
		ok, errMsg := bs.SessionState.SetName(sessionID, name)
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
		msgID, found := bs.PendingFiles.FindByUUID(uuid)
		if !found {
			w.WriteHeader(200)
			return
		}
		if _, ok := bs.PendingPerms.GetTarget(msgID); ok {
			helpers.DoCancelPerm(
				bot,
				bs.PendingPerms,
				bs.PendingFiles,
				func(text string) (*injector.TmuxTarget, error) {
					return helpers.ExtractTmuxTargetFromText(text)
				},
				msgID,
			)
		}
		if entry, ok := bs.ToolNotifs.Get(msgID); ok && !entry.Resolved {
			helpers.DoCancelAsk(
				bot,
				bs.ToolNotifs,
				bs.PendingFiles,
				func(text string) (*injector.TmuxTarget, error) {
					return helpers.ExtractTmuxTargetFromText(text)
				},
				msgID,
			)
		}
		bs.PendingFiles.Remove(msgID)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/session/list", func(w http.ResponseWriter, r *http.Request) {
		sessions := bs.SessionState.All()
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
				Name:       info.Name,
				Target:     info.TmuxTarget,
				CWD:        info.CWD,
				ProjectDir: info.ProjectDir,
				Running:    helpers.IsSessionRunning(info.TmuxTarget),
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
		var info *stores.SessionInfo
		var sessionID string
		for sid, si := range bs.SessionState.All() {
			if si.Name == name {
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

		// Use stored transcript path directly (works for both CC and Codex)
		jsonlPath := info.TranscriptPath
		if jsonlPath == "" {
			home, _ := os.UserHomeDir()
			jsonlPath = filepath.Join(home, ".claude", "projects", helpers.ProjectSlug(info.CWD), sessionID+".jsonl")
		}
		if _, err := os.Stat(jsonlPath); err != nil {
			http.Error(w, "transcript not found", http.StatusNotFound)
			return
		}
		type fileEntry struct {
			path    string
			modTime time.Time
		}
		jsonlFiles := []fileEntry{{path: jsonlPath, modTime: time.Now()}}

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

		var result []helpers.TranscriptLogEntry

		for _, jf := range jsonlFiles {
			if len(result) >= maxLines {
				break
			}
			f, err := os.Open(jf.path)
			if err != nil {
				continue
			}
			var fileEntries []helpers.TranscriptLogEntry
			if info.Backend == "codex" {
				fileEntries = helpers.ParseCodexTranscript(f, noTools, filteredTools, formatToolDetail)
			} else {
				fileEntries = helpers.ParseCCTranscript(f, noTools, filteredTools, formatToolDetail)
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
		if usedPct, usedTokens, windowSize, ok := helpers.ReadContextUsage(sessionID); ok {
			ctxPct = usedPct
			formatK := func(n int) string {
				return fmt.Sprintf("%.1fk", float64(n)/1000)
			}
			ctxUsed = formatK(usedTokens)
			ctxTotal = formatK(windowSize)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"target":        info.TmuxTarget,
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
		if req.From == "" {
			http.Error(w, "from required", http.StatusBadRequest)
			return
		}
		info := bs.SessionState.FindByName(req.Name)
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		injectText := req.Text
		if !req.NoHeader {
			injectText = fmt.Sprintf("---\n💬 Message from agent [%s]\n---\n%s", req.From, req.Text)
		}
		p := buildSafeInjectParams(bs)
		if err := helpers.SafeInjectText(p, info.TmuxTarget, injectText); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Session send via API: name=%s target=%s from=%s noHeader=%t text=%s injectText=%q", req.Name, info.TmuxTarget, req.From, req.NoHeader, helpers.TruncateStr(req.Text, 200), helpers.TruncateStr(injectText, 300)))
		// Send TG notification to the target session's chat
		chat, _, topicID := helpers.ResolveChat(bs.SessionState, info.TmuxTarget)
		if chat != nil {
			fromLine := fmt.Sprintf("📤 From: %s\n", req.From)
			header := "💬 CLI Send"
			if req.NoHeader {
				header = "📨 CLI Send (silent)"
			}
			notifyText := fmt.Sprintf("%s\n%s━━━━━━━━━━\n%s", header, fromLine, req.Text)
			var sendOpts []interface{}
			if topicID > 0 {
				sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
			}
			chunks := helpers.SplitBody(notifyText, 4000)
			if len(chunks) <= 1 {
				helpers.RetrySend(bot, chat, notifyText, sendOpts...)
			} else {
				helpers.RetrySend(bot, chat, chunks[0]+fmt.Sprintf("\n\n📄 1/%d", len(chunks)), sendOpts...)
			}
			logger.Info(fmt.Sprintf("Session send notification: target=%s from=%s text=%s", req.Name, req.From, helpers.TruncateStr(req.Text, 200)))
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
		info := bs.SessionState.FindByName(req.Name)
		if info == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		target := info.TmuxTarget
		// Cancel pending PermissionRequest
		if permMsgID, ok := bs.PendingPerms.FindByTmuxTarget(target); ok {
			helpers.DoCancelPerm(
				bot,
				bs.PendingPerms,
				bs.PendingFiles,
				func(text string) (*injector.TmuxTarget, error) {
					return helpers.ExtractTmuxTargetFromText(text)
				},
				permMsgID,
			)
			t, err := injector.ParseTarget(target)
			if err == nil {
				injector.SendKeys(t, "Escape")
			}
		}
		// Cancel pending AskUserQuestion
		if askMsgID, _, ok := bs.ToolNotifs.FindByTmuxTarget(target); ok {
			helpers.DoCancelAsk(
				bot,
				bs.ToolNotifs,
				bs.PendingFiles,
				func(text string) (*injector.TmuxTarget, error) {
					return helpers.ExtractTmuxTargetFromText(text)
				},
				askMsgID,
			)
		}
		// Wait for pending cleanup
		time.Sleep(1 * time.Second)
		// Inject /exit command
		t, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, "invalid target: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Store PendingExitKill BEFORE inject to avoid race with SessionEnd hook
		bs.PendingExitKill.Store(target, true)
		if err := injector.InjectText(t, "/exit"); err != nil {
			bs.PendingExitKill.Delete(target)
			http.Error(w, "inject failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
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
		chatID, err := strconv.ParseInt(bs.Creds.PairingAllow.DefaultChatID, 10, 64)
		if err != nil {
			http.Error(w, "no default chat configured", http.StatusBadRequest)
			return
		}
		state := &handlers.LaunchState{
			SessionName: req.Session,
			WorkDir:     req.WorkDir,
			Command:     req.Command,
			AgentName:   req.Name,
			ChatID:      chatID,
			UUID:        handlers.GenerateLaunchUUID(),
		}
		if req.Session != "" && req.WorkDir != "" {
			go handlers.ExecuteLaunch(bs, bot, chatID, state)
		} else if req.Session == "" {
			handlers.AskSessionName(bs, bot, chatID, state)
		} else {
			handlers.AskWorkDir(bs, bot, chatID, state)
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
		ch := bs.SessionWatch.Register(name)
		select {
		case evt := <-ch:
			logger.Info(fmt.Sprintf("Session watch event: agent=%s event=%s", name, evt.Event))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(evt)
		case <-r.Context().Done():
			bs.SessionWatch.Cancel(name, ch)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "timeout"})
		}
	})
}

// buildSafeInjectParams constructs SafeInjectTextParams from BotState.
func buildSafeInjectParams(bs *types.BotState) helpers.SafeInjectTextParams {
	return helpers.SafeInjectTextParams{
		Bot:             bs.Bot,
		ToolNotifs:      bs.ToolNotifs,
		PendingFiles:    bs.PendingFiles,
		PendingPerms:    bs.PendingPerms,
		InjectQueue:     bs.InjectQueue,
		InjectConfirm:   bs.InjectConfirm,
		StopCooldown:    bs.StopCooldown,
		ReactionTracker: bs.ReactionTracker,
		SessionState:    bs.SessionState,
		ResolveChat: func(t string) (*tele.Chat, string, int) {
			return helpers.ResolveChat(bs.SessionState, t)
		},
		FormatPaneID: notify.FormatPaneID,
	}
}

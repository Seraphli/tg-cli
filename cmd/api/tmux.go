package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

func registerTmux(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot
	creds := bs.Creds
	mux.HandleFunc("/tmux/list", func(w http.ResponseWriter, r *http.Request) {
		out, err := injector.GlobalTmuxCmd("list-panes", "-a", "-F",
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
			sessions := bs.SessionState.All()
			// Build pane_id → agent_name lookup from tracked sessions
			nameByPaneID := make(map[string]string)
			for _, info := range sessions {
				paneID := notify.FormatPaneID(info.TmuxTarget)
				if info.Name != "" {
					nameByPaneID[paneID] = info.Name
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
		if err := injector.TmuxCmd(target, "kill-pane", "-t", target.PaneID).Run(); err != nil {
			logger.Error(fmt.Sprintf("tmux kill-pane failed: target=%s err=%v", formatted, err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("tmux kill-pane via API: target=%s", formatted))
		helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, formatted)
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
				info := bs.SessionState.FindByName(req.Session)
				if info != nil {
					notifyChat, _, topicID = helpers.ResolveChat(bs.SessionState, info.TmuxTarget)
				}
				panes, err := injector.ListPanes(req.Session)
				if err == nil && len(panes) > 0 {
					paneID = panes[0]
					bs.TmuxPaneCache.Store(req.Session, paneID)
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
				helpers.RetrySend(bot, notifyChat, msgText, sendOpts...)
				logger.Info(fmt.Sprintf("tmux session-created notification: pane=%s session=%s", paneID, req.Session))
			}
		case "session-closed":
			var notifyChat *tele.Chat
			var topicID int
			agentName := ""
			paneID := ""
			if req.Session != "" {
				if cached, ok := bs.TmuxPaneCache.LoadAndDelete(req.Session); ok {
					paneID = cached.(string)
				}
				info := bs.SessionState.FindByName(req.Session)
				if info != nil {
					agentName = info.Name
					if paneID == "" {
						paneID = notify.FormatPaneID(info.TmuxTarget)
					}
					notifyChat, _, topicID = helpers.ResolveChat(bs.SessionState, info.TmuxTarget)
				}
			}
			if notifyChat == nil {
				chatID, err := strconv.ParseInt(creds.PairingAllow.DefaultChatID, 10, 64)
				if err == nil {
					notifyChat = &tele.Chat{ID: chatID}
				}
			}
			for _, si := range bs.SessionState.All() {
				target, err := injector.ParseTarget(si.TmuxTarget)
				if err != nil || !injector.SessionExists(target) {
					helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, si.TmuxTarget)
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
				helpers.RetrySend(bot, notifyChat, msgText, sendOpts...)
				logger.Info(fmt.Sprintf("tmux session-closed notification: pane=%s session=%s agent=%s", paneID, req.Session, agentName))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
}

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
)

func registerResume(mux *http.ServeMux, bs *types.BotState) {
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
		info := bs.SessionState.FindInfoByTarget(tmuxStr)
		if info == nil {
			http.Error(w, "no session found for target", 400)
			return
		}
		currentSID, _ := bs.SessionState.FindByTarget(tmuxStr)
		var sessions []helpers.SessionListEntry
		if info.ProjectDir != "" {
			sessions, err = helpers.ListProjectSessionsByDir(info.ProjectDir, 8, currentSID)
		} else {
			sessions, err = helpers.ListProjectSessions(info.CWD, 8, currentSID)
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
		if !helpers.CheckSessionAlive(injector.FormatTarget(parsed), func(t string) {
			helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, t)
		}) {
			http.Error(w, "session not alive", 410)
			return
		}
		if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, parsed, "/resume "+sessionID); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		logger.Info(fmt.Sprintf("Resume injected via API: target=%s session=%s", injector.FormatTarget(parsed), sessionID))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
}

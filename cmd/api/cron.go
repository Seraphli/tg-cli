package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/logger"
)

func registerCron(mux *http.ServeMux, bs *types.BotState) {
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
			NoHeader   bool   `json:"no_header"`
			Fresh      bool   `json:"fresh"`
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
		job := &stores.CronJob{
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
			NoHeader:   req.NoHeader,
			Fresh:      req.Fresh,
			CreatedAt:  time.Now(),
		}
		if err := bs.CronJobs.Add(job); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		logger.Info(fmt.Sprintf("Cron job added via API: id=%s mode=%s schedule=%s name=%s", job.ID[:8], job.Mode, job.Schedule, job.Name))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "id": job.ID})
	})
	mux.HandleFunc("/cron/list", func(w http.ResponseWriter, r *http.Request) {
		jobs := bs.CronJobs.All()
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
		if !bs.CronJobs.Remove(idOrName) {
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
		if !bs.CronJobs.Update(req.ID, req.Updates) {
			http.Error(w, "job not found or name conflict", http.StatusNotFound)
			return
		}
		logger.Info(fmt.Sprintf("Cron job updated via API: id=%s", req.ID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
}

// generateCronID generates a random 16-char hex ID for cron jobs.
func generateCronID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

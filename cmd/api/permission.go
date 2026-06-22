package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
)

func registerPermission(mux *http.ServeMux, bs *types.BotState) {
	mux.HandleFunc("/permission/decide", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		decision := r.URL.Query().Get("decision")
		// Use FindByMsgIDSnapshot internally (TG-button path — real msgID from callback)
		d, err := helpers.DoDecidePerm(
			bs.Bot,
			bs.PendingWait,
			bs.ReactionTracker,
			bs.NotifOpQueue,
			func(target string) bool {
				return helpers.CheckSessionAlive(target, func(t string) {
					helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, t)
				})
			},
			func(text string) (*injector.TmuxTarget, error) {
				return helpers.ExtractTmuxTargetFromText(text)
			},
			msgID,
			decision,
		)
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
		logger.Info("Perm switch API: target=" + injector.FormatTarget(t) + " mode=" + mode)
		finalMode, err := helpers.SwitchPermMode(t, mode)
		if err != nil {
			logger.Info("Perm switch API failed: " + err.Error())
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
		mode, content, err := helpers.DetectPermMode(t)
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

package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
)

func registerRouting(mux *http.ServeMux, bs *types.BotState) {
	// POST /route/bind {"name","chat_id","topic_id"}.
	// topic_id 0 or absent = automatic: one getChat decides. A forum supergroup gets the session's own topic
	// (reused from the credentials record or created); any other chat is bound whole, topic 0, with its title.
	// A failing getChat or createForumTopic answers 502 with Telegram's error text and writes nothing.
	// A non-zero topic_id = raw bind to exactly that chat and topic: no topic is created, and the one getChat
	// only fetches the title (a failed fetch stores an empty title and the bind proceeds).
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
		info, infoErr := helpers.FetchChatInfo(bs.Bot, req.ChatID)
		if infoErr != nil && req.TopicID == 0 {
			http.Error(w, infoErr.Error(), http.StatusBadGateway)
			return
		}
		route := config.NameRoute{ChatID: req.ChatID, TopicID: req.TopicID, Title: info.Title}
		if req.TopicID == 0 && info.IsForum {
			var err error
			if route, err = helpers.BindForumTopic(bs.Bot, req.Name, req.ChatID, info.Title); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
		} else {
			creds, err := config.LoadCredentials()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			creds.NameRouteMap[req.Name] = route
			if err := config.SaveCredentials(creds); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		logger.Info(fmt.Sprintf("Route bound via API: name=%s → chat=%d topic=%d title=%q", req.Name, route.ChatID, route.TopicID, route.Title))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "chat_id": route.ChatID, "topic_id": route.TopicID, "title": route.Title})
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
}

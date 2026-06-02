package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
)

// RegisterTestEndpoints registers test-only HTTP endpoints unconditionally.
func RegisterTestEndpoints(mux *http.ServeMux, bs *types.BotState) {
	mux.HandleFunc("/test/callback", func(w http.ResponseWriter, r *http.Request) {
		msgIDStr := r.URL.Query().Get("msg_id")
		unique := r.URL.Query().Get("unique")
		data := r.URL.Query().Get("data")
		msgID, err := strconv.Atoi(msgIDStr)
		if err != nil {
			http.Error(w, "invalid msg_id", http.StatusBadRequest)
			return
		}
		logger.Info(fmt.Sprintf("Test callback: msg_id=%d unique=%s data=%s", msgID, unique, data))
		w.Header().Set("Content-Type", "application/json")

		switch unique {
		case "ce":
			entry, ok := bs.Pages.Get(msgID)
			if !ok {
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "not_found", "msg_id": msgID})
				return
			}
			var text string
			if data == "c" {
				entry.Collapsed = true
				text = strings.SplitN(entry.Header, "\n", 2)[0]
			} else {
				entry.Collapsed = false
				text = entry.Header + entry.Chunks[0]
				if len(entry.Chunks) > 1 {
					// Multi-page: would show pagination buttons too
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":    "ok",
				"msg_id":    msgID,
				"unique":    unique,
				"data":      data,
				"collapsed": entry.Collapsed,
				"text":      text,
			})
		case "p":
			entry, ok := bs.Pages.Get(msgID)
			if !ok {
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "not_found", "msg_id": msgID})
				return
			}
			pageNum, _ := strconv.Atoi(data)
			if pageNum < 1 || pageNum > len(entry.Chunks) {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "invalid_page",
					"msg_id": msgID,
					"page":   pageNum,
					"total":  len(entry.Chunks),
				})
				return
			}
			text := entry.Header + entry.Chunks[pageNum-1]
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":      "ok",
				"msg_id":      msgID,
				"unique":      unique,
				"page":        pageNum,
				"total_pages": len(entry.Chunks),
				"text":        text,
			})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "ok",
				"msg_id": msgID,
				"unique": unique,
				"data":   data,
			})
		}
	})

	mux.HandleFunc("/test/page_entry", func(w http.ResponseWriter, r *http.Request) {
		msgIDStr := r.URL.Query().Get("msg_id")
		msgID, err := strconv.Atoi(msgIDStr)
		if err != nil {
			http.Error(w, "invalid msg_id", http.StatusBadRequest)
			return
		}
		entry, ok := bs.Pages.Get(msgID)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"exists": false,
				"msg_id": msgID,
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"exists":    true,
			"msg_id":    msgID,
			"chunks":    len(entry.Chunks),
			"header":    entry.Header,
			"collapsed": entry.Collapsed,
		})
	})

	mux.HandleFunc("/test/config/compact", func(w http.ResponseWriter, r *http.Request) {
		cfg, err := config.LoadAppConfig()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "error": err.Error()})
			return
		}
		cfg.ToolNotifyCompact = !cfg.ToolNotifyCompact
		if err := config.SaveAppConfig(cfg); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"compact": cfg.ToolNotifyCompact,
		})
	})

	logger.Info("Test endpoints registered: /test/callback (enhanced), /test/page_entry")
}

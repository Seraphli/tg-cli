package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Seraphli/tg-cli/cmd/types"
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
		chatIDStr := r.URL.Query().Get("chat_id")
		chatID, _ := strconv.ParseInt(chatIDStr, 10, 64)
		if chatID == 0 {
			http.Error(w, "invalid chat_id", http.StatusBadRequest)
			return
		}
		logger.Info(fmt.Sprintf("Test callback: msg_id=%d unique=%s data=%s chat_id=%d", msgID, unique, data, chatID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"msg_id": msgID,
			"unique": unique,
			"data":   data,
		})
	})
	logger.Info("Test callback endpoint registered at /test/callback")
}

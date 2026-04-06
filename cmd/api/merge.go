package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
)

func registerMerge(mux *http.ServeMux, bs *types.BotState) {
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
		key := stores.MergeKey(0)
		if bs.MergeBuffers.Get(key) != nil {
			http.Error(w, "merge already active", 409)
			return
		}
		bs.MergeBuffers.Start(key, 0, target, 0)
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
		key := stores.MergeKey(0)
		if bs.MergeBuffers.Get(key) == nil {
			http.Error(w, "no active merge", 404)
			return
		}
		bs.MergeBuffers.Add(key, req.Text)
		logger.Info(fmt.Sprintf("Merge add via API: text=%s", helpers.TruncateStr(req.Text, 200)))
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/merge/submit", func(w http.ResponseWriter, r *http.Request) {
		key := stores.MergeKey(0)
		buf, ok := bs.MergeBuffers.Finish(key)
		if !ok || len(buf.Items) == 0 {
			http.Error(w, "no items to submit", 404)
			return
		}
		target, err := injector.ParseTarget(buf.TmuxTarget)
		if err != nil || !injector.SessionExists(target) {
			http.Error(w, "session not found", 404)
			return
		}
		merged := strings.Join(buf.Items, "\n")
		if err := injector.InjectText(target, merged); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		logger.Info(fmt.Sprintf("Merge submitted via API: target=%s items=%d text=%s", buf.TmuxTarget, len(buf.Items), helpers.TruncateStr(merged, 200)))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "items": len(buf.Items)})
	})
}

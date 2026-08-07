package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/Seraphli/tg-cli/cmd/handlers"
	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// testUpdateSeq generates monotonic synthetic ids for /test/update (seeded high to avoid
// colliding with real Telegram msg_ids in tests).
var testUpdateSeq int64 = 900000000

func rowUniques(row tele.Row) []string {
	var out []string
	for _, b := range row {
		out = append(out, b.Unique)
	}
	return out
}

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
				text = handlers.CollapseEntry(entry)
			} else {
				text, _ = handlers.ExpandEntry(entry)
			}
			row := handlers.CaptureExtraRow(entry.Header == handlers.CaptureHeader, data != "c")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":    "ok",
				"msg_id":    msgID,
				"unique":    unique,
				"data":      data,
				"collapsed": entry.Collapsed,
				"text":      text,
				"buttons":   rowUniques(row),
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
			text := handlers.NavigateEntry(entry, pageNum)
			row := handlers.CaptureExtraRow(entry.Header == handlers.CaptureHeader, true)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":      "ok",
				"msg_id":      msgID,
				"unique":      unique,
				"page":        pageNum,
				"total_pages": len(entry.Chunks),
				"text":        text,
				"buttons":     rowUniques(row),
			})
		case "del":
			chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
			err := bs.Bot.Delete(&tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}})
			if err != nil {
				logger.Info(fmt.Sprintf("del callback: delete failed msg_id=%d err=%v", msgID, err))
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "error", "msg_id": msgID, "error": err.Error()})
				return
			}
			logger.Info(fmt.Sprintf("del callback: deleted msg_id=%d", msgID))
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "deleted", "msg_id": msgID})
		case "settings":
			chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
			msg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID, Type: tele.ChatPrivate}}
			status := "ok"
			if !handlers.RenderSettingsSubmenu(bs, msg, data) {
				status = "unknown_submenu"
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "msg_id": msgID, "unique": "settings", "data": data})
		case "tool":
			// AskUserQuestion multiSelect toggle via the SAME callback re-edit path (Fix 15). data="q:opt".
			// Returns the resulting button labels so tests can assert the ✅ prefix on the toggled option.
			snap, ok := bs.PendingWait.FindByMsgIDSnapshot(msgID)
			if !ok || snap.ToolName != "AskUserQuestion" {
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "expired", "msg_id": msgID})
				return
			}
			qo := strings.SplitN(data, ":", 2)
			if len(qo) < 2 {
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "invalid_data", "msg_id": msgID})
				return
			}
			qIdx, _ := strconv.Atoi(qo[0])
			optIdx, _ := strconv.Atoi(qo[1])
			editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: snap.ChatID}}
			labels, editErr := handlers.ToggleAskAndReedit(bs, editMsg, snap, qIdx, optIdx)
			if labels == nil {
				json.NewEncoder(w).Encode(map[string]interface{}{"status": "expired", "msg_id": msgID})
				return
			}
			resp := map[string]interface{}{"status": "ok", "msg_id": msgID, "unique": "tool", "labels": labels}
			if editErr != nil {
				// The store toggle succeeded but the re-edit failed — this is the Fix 15 regression signal.
				resp["status"] = "edit_error"
				resp["error"] = editErr.Error()
			}
			json.NewEncoder(w).Encode(resp)
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
			"exists":       true,
			"msg_id":       msgID,
			"chunks":       len(entry.Chunks),
			"header":       entry.Header,
			"collapsed":    entry.Collapsed,
			"current_page": entry.CurrentPage,
			"raw_mode":     entry.RawMode,
			"rich":         entry.Rich,
		})
	})

	mux.HandleFunc("/test/capture_message", func(w http.ResponseWriter, r *http.Request) {
		// Accept both GET (with --data-urlencode via curl -G) and POST JSON
		var target, content string
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body failed", http.StatusBadRequest)
				return
			}
			var req map[string]string
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			target = req["target"]
			content = req["content"]
		} else {
			target = r.URL.Query().Get("target")
			content = r.URL.Query().Get("content")
		}
		if target == "" {
			http.Error(w, "target required", http.StatusBadRequest)
			return
		}
		chat, _, _ := helpers.ResolveChat(bs.SessionState, target)
		if chat == nil {
			http.Error(w, "target not found", http.StatusNotFound)
			return
		}
		if content == "" {
			// Capture live pane content
			t, err := injector.ParseTarget(target)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid target: %v", err), http.StatusBadRequest)
				return
			}
			content, err = injector.CapturePane(t)
			if err != nil {
				http.Error(w, fmt.Sprintf("capture failed: %v", err), http.StatusInternalServerError)
				return
			}
		}
		sent, err := handlers.SendCaptureReply(bs.Bot, chat, bs.Pages, content)
		if err != nil {
			http.Error(w, fmt.Sprintf("send failed: %v", err), http.StatusInternalServerError)
			return
		}
		entry, _ := bs.Pages.Get(sent.ID)
		// Extract button metadata from the sent message's inline keyboard.
		// telebot serializes Unique+Data into Data ("\f<unique>|<data>") on send and
		// clears Unique; parse it back so tests can assert the real unique/data.
		var buttons []map[string]string
		if sent.ReplyMarkup != nil {
			for _, row := range sent.ReplyMarkup.InlineKeyboard {
				for _, btn := range row {
					unique := btn.Unique
					data := btn.Data
					if unique == "" && strings.HasPrefix(data, "\f") {
						parts := strings.SplitN(strings.TrimPrefix(data, "\f"), "|", 2)
						unique = parts[0]
						if len(parts) > 1 {
							data = parts[1]
						} else {
							data = ""
						}
					}
					buttons = append(buttons, map[string]string{
						"text":   btn.Text,
						"unique": unique,
						"data":   data,
					})
				}
			}
		}
		var chunks int
		var currentPage int
		if entry != nil {
			chunks = len(entry.Chunks)
			currentPage = entry.CurrentPage
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"msg_id":       sent.ID,
			"chat_id":      chat.ID,
			"chunks":       chunks,
			"current_page": currentPage,
			"buttons":      buttons,
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

	mux.HandleFunc("/test/settings_message", func(w http.ResponseWriter, r *http.Request) {
		chatID, err := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid chat_id", http.StatusBadRequest)
			return
		}
		sent, err := handlers.SendSettingsMenu(bs, &tele.Chat{ID: chatID})
		if err != nil {
			http.Error(w, fmt.Sprintf("send failed: %v", err), http.StatusInternalServerError)
			return
		}
		var buttons []map[string]string
		if sent.ReplyMarkup != nil {
			for _, row := range sent.ReplyMarkup.InlineKeyboard {
				for _, btn := range row {
					unique := btn.Unique
					data := btn.Data
					if unique == "" && strings.HasPrefix(data, "\f") {
						parts := strings.SplitN(strings.TrimPrefix(data, "\f"), "|", 2)
						unique = parts[0]
						if len(parts) > 1 {
							data = parts[1]
						} else {
							data = ""
						}
					}
					buttons = append(buttons, map[string]string{"text": btn.Text, "unique": unique, "data": data})
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"msg_id": sent.ID, "chat_id": chatID, "buttons": buttons})
	})

	mux.HandleFunc("/test/update", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		var req struct {
			ChatID       int64  `json:"chat_id"`
			ChatType     string `json:"chat_type"`
			TopicID      int    `json:"topic_id"`
			SenderID     int64  `json:"sender_id"`
			Text         string `json:"text"`
			ReplyToMsgID int    `json:"reply_to_msg_id"`
			Document     *struct {
				FileID   string `json:"file_id"`
				FileName string `json:"file_name"`
			} `json:"document"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		chatType := req.ChatType
		if chatType == "" {
			chatType = "private" // explicit default; the ReplyTo /p route does not depend on it
		}
		synthID := int(atomic.AddInt64(&testUpdateSeq, 1))
		msg := &tele.Message{
			ID:       synthID,
			Chat:     &tele.Chat{ID: req.ChatID, Type: tele.ChatType(chatType)},
			Sender:   &tele.User{ID: req.SenderID},
			ThreadID: req.TopicID,
			Text:     req.Text,
		}
		if req.ReplyToMsgID != 0 {
			msg.ReplyTo = &tele.Message{ID: req.ReplyToMsgID, Chat: msg.Chat}
		}
		if req.Document != nil {
			msg.Document = &tele.Document{File: tele.File{FileID: req.Document.FileID}, FileName: req.Document.FileName}
		}
		logger.Info(fmt.Sprintf("Test update: synth_id=%d chat=%d type=%s sender=%d text=%q reply_to=%d has_doc=%t",
			synthID, req.ChatID, chatType, req.SenderID, req.Text, req.ReplyToMsgID, req.Document != nil))
		// Dispatch through the REAL poller entry so bot.Use markIncoming middleware +
		// command/media routing run exactly as in production. Dispatch is async
		// (Synchronous unset), so fire-and-return {ok:true}.
		bs.Bot.ProcessUpdate(tele.Update{ID: synthID, Message: msg})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})

	logger.Info("Test endpoints registered: /test/callback (enhanced), /test/page_entry, /test/capture_message, /test/settings_message, /test/update")
}

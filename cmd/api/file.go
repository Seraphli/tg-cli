package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

func registerFile(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot
	mux.HandleFunc("/file/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FilePath   string `json:"file_path"`
			Caption    string `json:"caption"`
			TmuxTarget string `json:"tmux_target"`
			CWD        string `json:"cwd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		info, err := os.Stat(req.FilePath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("file not found: %s", req.FilePath)})
			return
		}
		if !info.Mode().IsRegular() {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not a regular file"})
			return
		}
		if info.Size() > 50*1024*1024 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("file too large: %d bytes (max 50MB for Telegram Bot API)", info.Size())})
			return
		}
		chat, _, topicID := helpers.ResolveChat(bs.SessionState, req.TmuxTarget)
		// CWD fallback: if tmuxTarget route not found, try finding session by CWD
		if req.CWD != "" && (chat == nil || pairing.GetDefaultChatID() == strconv.FormatInt(chat.ID, 10)) {
			if cwdInfo := bs.SessionState.FindByCWD(req.CWD); cwdInfo != nil {
				if c, _, t := helpers.ResolveChat(bs.SessionState, cwdInfo.TmuxTarget); c != nil && pairing.GetDefaultChatID() != strconv.FormatInt(c.ID, 10) {
					chat = c
					topicID = t
					logger.Info(fmt.Sprintf("[File] Route resolved via CWD fallback: cwd=%s → chat=%d topic=%d", req.CWD, c.ID, t))
				}
			}
		}
		ext := strings.ToLower(filepath.Ext(req.FilePath))
		imageExts := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
		var sendable interface{}
		if imageExts[ext] {
			sendable = &tele.Photo{
				File:    tele.FromDisk(req.FilePath),
				Caption: req.Caption,
			}
		} else {
			sendable = &tele.Document{
				File:     tele.FromDisk(req.FilePath),
				FileName: filepath.Base(req.FilePath),
				Caption:  req.Caption,
			}
		}
		var sendOpts []interface{}
		if topicID > 0 {
			sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
		}
		msg, err := helpers.RetrySend(bot, chat, sendable, sendOpts...)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("telegram send failed: %v", err)})
			return
		}
		logger.Info(fmt.Sprintf("[File] File sent: %s to chat %d (msg_id=%d)", filepath.Base(req.FilePath), chat.ID, msg.ID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("File sent: %s", filepath.Base(req.FilePath))})
	})
}

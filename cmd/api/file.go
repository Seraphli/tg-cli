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
			Async      bool   `json:"async"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			logger.Info("[File] rejected: invalid request body")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid request"})
			return
		}
		info, err := os.Stat(req.FilePath)
		if err != nil {
			logger.Info(fmt.Sprintf("[File] rejected: %s — file not found", req.FilePath))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("file not found: %s", req.FilePath)})
			return
		}
		if !info.Mode().IsRegular() {
			logger.Info(fmt.Sprintf("[File] rejected: %s — not a regular file", req.FilePath))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not a regular file"})
			return
		}
		if info.Size() > 50*1024*1024 {
			logger.Info(fmt.Sprintf("[File] rejected: %s — too large (%d bytes, max 50MB)", req.FilePath, info.Size()))
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
		if chat == nil {
			logger.Info(fmt.Sprintf("[File] rejected: %s — no target chat resolved", req.FilePath))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no target chat resolved"})
			return
		}
		var sendOpts []interface{}
		if topicID > 0 {
			sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
		}
		fileName := filepath.Base(req.FilePath)
		fileSize := info.Size()
		if req.Async {
			go func() {
				msg, err := helpers.RetrySend(bot, chat, sendable, sendOpts...)
				if err != nil {
					logger.Error(fmt.Sprintf("[File] send failed: %s (%d bytes) to chat %d: %v", fileName, fileSize, chat.ID, err))
					helpers.RetrySend(bot, chat, fmt.Sprintf("❌ send-file failed: %s — %v", fileName, err), sendOpts...)
					return
				}
				logger.Info(fmt.Sprintf("[File] File sent: %s to chat %d (msg_id=%d)", fileName, chat.ID, msg.ID))
			}()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("queued: %s", fileName)})
			return
		}
		msg, err := helpers.RetrySend(bot, chat, sendable, sendOpts...)
		if err != nil {
			logger.Error(fmt.Sprintf("[File] send failed: %s (%d bytes) to chat %d: %v", fileName, fileSize, chat.ID, err))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("telegram send failed: %v", err)})
			return
		}
		logger.Info(fmt.Sprintf("[File] File sent: %s to chat %d (msg_id=%d)", fileName, chat.ID, msg.ID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": fmt.Sprintf("File sent: %s", fileName)})
	})
}

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/cmd/handlers"
	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	tele "gopkg.in/telebot.v3"
)

func registerLaunch(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot
	creds := bs.Creds
	mux.HandleFunc("/bot_new", func(w http.ResponseWriter, r *http.Request) {
		chatID, err := strconv.ParseInt(creds.PairingAllow.DefaultChatID, 10, 64)
		if err != nil {
			http.Error(w, "no default chat configured", 400)
			return
		}
		session := r.URL.Query().Get("session")
		workdir := r.URL.Query().Get("workdir")
		command := r.URL.Query().Get("command")
		agentName := r.URL.Query().Get("name")
		state := &handlers.LaunchState{
			SessionName: session,
			WorkDir:     workdir,
			Command:     command,
			AgentName:   agentName,
			ChatID:      chatID,
			UUID:        handlers.GenerateLaunchUUID(),
		}
		if session == "" {
			handlers.AskSessionName(bs, bot, chatID, state)
		} else if workdir == "" {
			handlers.AskWorkDir(bs, bot, chatID, state)
		} else {
			go handlers.ExecuteLaunch(bs, bot, chatID, state)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "uuid": state.UUID})
	})
	mux.HandleFunc("/bot_new/callback", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.URL.Query().Get("uuid")
		data := r.URL.Query().Get("data")
		if uuid == "" || data == "" {
			http.Error(w, "missing uuid or data", 400)
			return
		}
		var state *handlers.LaunchState
		var msgID int
		bs.LaunchPending.Range(func(key, val interface{}) bool {
			s := val.(*handlers.LaunchState)
			if s.UUID == uuid {
				state = s
				msgID = key.(int)
				return false
			}
			return true
		})
		if state == nil {
			http.Error(w, "launch not found", 404)
			return
		}
		chatID := state.ChatID
		msg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		cfg, _ := config.LoadAppConfig()
		switch {
		case data == "session_default":
			state.SessionName = cfg.DefaultSessionName
			helpers.RetryEdit(bot, msg, fmt.Sprintf("📦 Session name\n✅ %s", markdown.EscapeHTML(state.SessionName)), tele.ModeHTML)
			bs.LaunchPending.Delete(msgID)
			if state.WorkDir == "" {
				handlers.AskWorkDir(bs, bot, chatID, state)
			} else {
				go handlers.ExecuteLaunch(bs, bot, chatID, state)
			}
		case data == "dir_select":
			state.WorkDir = state.BrowsePath
			helpers.RetryEdit(bot, msg, fmt.Sprintf("📂 Working directory\n✅ %s", markdown.EscapeHTML(state.WorkDir)), tele.ModeHTML)
			bs.LaunchPending.Delete(msgID)
			go handlers.ExecuteLaunch(bs, bot, chatID, state)
		case data == "cd_up":
			parent := filepath.Dir(state.BrowsePath)
			if parent != state.BrowsePath {
				state.BrowsePath = parent
				state.DirPage = 0
			}
			handlers.RefreshDirBrowser(bot, msg, state)
		case strings.HasPrefix(data, "cd:"):
			idx, err := strconv.Atoi(strings.TrimPrefix(data, "cd:"))
			if err == nil {
				dirs, _ := handlers.ListSubDirs(state.BrowsePath, state.ShowHidden)
				if idx >= 0 && idx < len(dirs) {
					state.BrowsePath = filepath.Join(state.BrowsePath, dirs[idx])
					state.DirPage = 0
				}
			}
			handlers.RefreshDirBrowser(bot, msg, state)
		case data == "toggle_hidden":
			state.ShowHidden = !state.ShowHidden
			state.DirPage = 0
			handlers.RefreshDirBrowser(bot, msg, state)
		case data == "page_prev":
			if state.DirPage > 0 {
				state.DirPage--
			}
			handlers.RefreshDirBrowser(bot, msg, state)
		case data == "page_next":
			state.DirPage++
			handlers.RefreshDirBrowser(bot, msg, state)
		case data == "cancel":
			helpers.RetryEdit(bot, msg, "❌ Launch cancelled.", tele.ModeHTML)
			bs.LaunchPending.Delete(msgID)
			handlers.DeleteLaunchState(state.UUID)
			logger.Info(fmt.Sprintf("bot_new API: cancel pressed msg_id=%d uuid=%s", msgID, state.UUID))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
	mux.HandleFunc("/bot_new/reply", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.URL.Query().Get("uuid")
		text := r.URL.Query().Get("text")
		if uuid == "" || text == "" {
			http.Error(w, "missing uuid or text", 400)
			return
		}
		var state *handlers.LaunchState
		var msgID int
		bs.LaunchPending.Range(func(key, val interface{}) bool {
			s := val.(*handlers.LaunchState)
			if s.UUID == uuid {
				state = s
				msgID = key.(int)
				return false
			}
			return true
		})
		if state == nil {
			http.Error(w, "launch not found", 404)
			return
		}
		chatID := state.ChatID
		msg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		bs.LaunchPending.Delete(msgID)
		switch state.Step {
		case "session":
			state.SessionName = strings.TrimSpace(text)
			helpers.RetryEdit(bot, msg, fmt.Sprintf("📦 Session name\n✅ %s", markdown.EscapeHTML(state.SessionName)), tele.ModeHTML)
			if state.WorkDir == "" {
				handlers.AskWorkDir(bs, bot, chatID, state)
			} else {
				go handlers.ExecuteLaunch(bs, bot, chatID, state)
			}
		case "workdir":
			customValue := strings.TrimSpace(text)
			if strings.HasPrefix(customValue, "~") {
				home, _ := os.UserHomeDir()
				customValue = home + customValue[1:]
			}
			state.WorkDir = customValue
			helpers.RetryEdit(bot, msg, fmt.Sprintf("📂 Working directory\n✅ %s", markdown.EscapeHTML(state.WorkDir)), tele.ModeHTML)
			go handlers.ExecuteLaunch(bs, bot, chatID, state)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})
}

package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

func registerPagination(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		msgIDStr := r.URL.Query().Get("msg_id")
		pageStr := r.URL.Query().Get("page")
		msgID, err := strconv.Atoi(msgIDStr)
		if err != nil {
			http.Error(w, "invalid msg_id", 400)
			return
		}
		pageNum, err := strconv.Atoi(pageStr)
		if err != nil {
			http.Error(w, "invalid page", 400)
			return
		}
		entry, ok := bs.Pages.Get(msgID)
		if !ok {
			http.Error(w, "page entry not found", 404)
			return
		}
		if pageNum < 1 || pageNum > len(entry.Chunks) {
			http.Error(w, "page out of range", 400)
			return
		}
		chat := &tele.Chat{ID: entry.ChatID}
		var text string
		if entry.PermRows != nil {
			text = entry.Chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.Chunks))
		} else {
			text = notify.BuildNotificationText(notify.NotificationData{
				Event:             entry.Event,
				Project:           entry.Project,
				CWD:               entry.CWD,
				Body:              entry.Chunks[pageNum-1],
				TmuxTarget:        entry.TmuxTarget,
				Page:              pageNum,
				TotalPages:        len(entry.Chunks),
				CLICommand:        entry.CLICommand,
				AgentName:         entry.AgentName,
				Backend:           entry.Backend,
				ContextUsedPct:    entry.ContextUsedPct,
				ContextUsedTokens: entry.ContextUsedTokens,
				ContextWindowSize: entry.ContextWindowSize,
			})
		}
		kb := helpers.BuildPageKeyboardWithExtra(pageNum, len(entry.Chunks), entry.PermRows)
		editMsg := &tele.Message{ID: msgID, Chat: chat}
		if entry.RawMode {
			_, err = helpers.RetryEdit(bot, editMsg, text, kb)
		} else {
			_, err = helpers.RetryEdit(bot, editMsg, text, kb, tele.ModeHTML)
		}
		if err != nil {
			logger.Error(fmt.Sprintf("Callback edit failed: %v", err))
			http.Error(w, "edit failed: "+err.Error(), 500)
			return
		}
		logger.Info(fmt.Sprintf("Callback page turn: msg_id=%d page=%d/%d cli=%q context_pct=%d agent=%q", msgID, pageNum, len(entry.Chunks), entry.CLICommand, entry.ContextUsedPct, entry.AgentName))
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
}

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
		var text, legacyText string
		if entry.PermRows != nil {
			text = entry.Chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.Chunks))
			legacyText = text
		} else {
			// S12b: Chunks/LegacyChunks are BODY chunks — re-wrap in the header via BuildNotificationText.
			// The rich payload gets the <hr/> boundary for Cron/SessionSend; the legacy payload never does.
			nd := notify.NotificationData{
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
				CronJobID:         entry.CronJobID,
				CronName:          entry.CronName,
				CronNoHeader:      entry.CronNoHeader,
				SendFrom:          entry.SendFrom,
				SendNoHeader:      entry.SendNoHeader,
				DeliveryStatus:    entry.DeliveryStatus,
			}
			text = notify.BuildNotificationText(nd)
			if entry.Event == "Cron" || entry.Event == "SessionSend" {
				text = helpers.InsertRichHr(text)
			}
			// Legacy chunk paired 1:1 with Chunks; fall back to the rich text when absent (backward compat).
			if len(entry.Chunks) == len(entry.LegacyChunks) && pageNum-1 < len(entry.LegacyChunks) {
				ndLegacy := nd
				ndLegacy.Body = entry.LegacyChunks[pageNum-1]
				legacyText = notify.BuildNotificationText(ndLegacy)
			} else {
				legacyText = text
			}
		}
		kb := helpers.BuildPageKeyboardWithExtra(pageNum, len(entry.Chunks), entry.PermRows)
		editMsg := &tele.Message{ID: msgID, Chat: chat}
		if entry.RawMode {
			_, err = helpers.RetryEdit(bot, editMsg, text, kb)
		} else if entry.Rich {
			// G1 mixed-era: a rich-sent message re-renders rich. Permission/capture/forward
			// content is code/raw → skip entity detection; standard notifications are prose (C3).
			_, err = helpers.RetryEditRich(bot, editMsg, text, helpers.RichSendOpts{
				Markup:              kb,
				SkipEntityDetection: entry.PermRows != nil || entry.Header != "",
				LegacyHTML:          legacyText,
			})
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

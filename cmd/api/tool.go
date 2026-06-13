package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/types"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

func registerTool(mux *http.ServeMux, bs *types.BotState) {
	bot := bs.Bot
	mux.HandleFunc("/tool/respond", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		tool := r.URL.Query().Get("tool")
		action := r.URL.Query().Get("action")
		// Pre-check session liveness before processing the response
		if entry, ok := bs.ToolNotifs.Get(msgID); ok && entry.TmuxTarget != "" {
			if !helpers.CheckSessionAlive(entry.TmuxTarget, func(t string) {
				helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, t)
			}) {
				http.Error(w, "session disconnected", 410)
				return
			}
		}
		switch tool {
		case "AskUserQuestion":
			if action == "text" {
				value := r.URL.Query().Get("value")
				entry, ok := bs.ToolNotifs.Get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				if entry.Resolved {
					http.Error(w, "already answered", 400)
					return
				}
				answers := make(map[string]string)
				if len(entry.Questions) > 0 {
					answers[entry.Questions[0].QuestionText] = value
				}
				if err := helpers.DoRespondAsk(bot, bs.ToolNotifs, bs.PendingFiles, bs.PendingWait, bs.ReactionTracker, msgID, answers, "✅ Text answer"); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else if action == "submit" {
				entry, ok := bs.ToolNotifs.Get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				if entry.Resolved {
					http.Error(w, "already answered", 400)
					return
				}
				if err := helpers.DoRespondAsk(bot, bs.ToolNotifs, bs.PendingFiles, bs.PendingWait, bs.ReactionTracker, msgID, helpers.BuildAnswers(entry), ""); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else if action == "chat" {
				if err := helpers.DoChatAsk(bot, bs.ToolNotifs, bs.PendingFiles, bs.PendingWait, bs.ReactionTracker, msgID); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else {
				qIdx, _ := strconv.Atoi(r.URL.Query().Get("question"))
				optIdx, _ := strconv.Atoi(r.URL.Query().Get("option"))
				entry, ok := bs.ToolNotifs.Get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				if entry.Resolved {
					http.Error(w, "already answered", 400)
					return
				}
				if qIdx >= len(entry.Questions) {
					http.Error(w, "invalid question index", 400)
					return
				}
				qm := &entry.Questions[qIdx]
				if qm.MultiSelect {
					qm.SelectedOptions[optIdx] = !qm.SelectedOptions[optIdx]
					logger.Info(fmt.Sprintf("AskUserQuestion option toggled via API: msg_id=%d q=%d opt=%d state=%v label=%s", msgID, qIdx, optIdx, qm.SelectedOptions[optIdx], qm.OptionLabels[optIdx]))
					newMarkup := helpers.RebuildAskMarkup(entry)
					editChat := &tele.Chat{ID: entry.ChatID}
					editMsg := &tele.Message{ID: msgID, Chat: editChat}
					helpers.RetryEdit(bot, editMsg, entry.MsgText, newMarkup, tele.ModeHTML)
				} else {
					qm.SelectedOption = optIdx
					hasSubmit := len(entry.Questions) > 1
					for _, q := range entry.Questions {
						if q.MultiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						if err := helpers.DoRespondAsk(bot, bs.ToolNotifs, bs.PendingFiles, bs.PendingWait, bs.ReactionTracker, msgID, helpers.BuildAnswers(entry), ""); err != nil {
							http.Error(w, err.Error(), 500)
							return
						}
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected via API: msg_id=%d q=%d opt=%d label=%s", msgID, qIdx, optIdx, qm.OptionLabels[optIdx]))
						newMarkup := helpers.RebuildAskMarkup(entry)
						editChat := &tele.Chat{ID: entry.ChatID}
						editMsg := &tele.Message{ID: msgID, Chat: editChat}
						helpers.RetryEdit(bot, editMsg, entry.MsgText, newMarkup, tele.ModeHTML)
					}
				}
			}
		default:
			http.Error(w, "unsupported tool", 400)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/group/text", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		text := r.URL.Query().Get("text")
		if target == "" || text == "" {
			http.Error(w, "missing target or text", 400)
			return
		}
		// Strip socket prefix so the target matches stored pane IDs
		target = notify.FormatPaneID(target)
		// Check pending PermissionRequest first
		if permMsgID, ok := bs.PendingPerms.FindByTmuxTarget(target); ok {
			helpers.DoCancelPerm(
				bot,
				bs.PendingPerms,
				bs.PendingFiles,
				bs.PendingWait,
				func(text string) (*injector.TmuxTarget, error) {
					return helpers.ExtractTmuxTargetFromText(text)
				},
				permMsgID,
			)
			t, err := injector.ParseTarget(target)
			if err != nil {
				http.Error(w, "invalid target", 400)
				return
			}
			go func() {
				time.Sleep(3 * time.Second)
				helpers.QueuedInject(bs.SessionEvents, bs.SessionState, t, text)
			}()
			logger.Info(fmt.Sprintf("Permission cancelled via group text API + delayed inject: target=%s text=%s", target, helpers.TruncateStr(text, 200)))
			fmt.Fprintf(w, "cancelled+injected")
			return
		}
		msgID, entry, ok := bs.ToolNotifs.FindByTmuxTarget(target)
		if !ok {
			// No pending AskUserQuestion — inject text
			t, err := injector.ParseTarget(target)
			if err != nil {
				http.Error(w, "invalid target", 400)
				return
			}
			if !helpers.CheckSessionAlive(target, func(t string) {
				helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, t)
			}) {
				http.Error(w, "session disconnected", 410)
				return
			}
			if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, t, text); err != nil {
				http.Error(w, fmt.Sprintf("inject failed: %v", err), 500)
				return
			}
			logger.Info(fmt.Sprintf("Group text API injected: target=%s text=%s", target, helpers.TruncateStr(text, 200)))
			fmt.Fprintf(w, "injected")
			return
		}
		uuid, uuidOk := bs.PendingFiles.Get(msgID)
		if !uuidOk {
			http.Error(w, "pending entry not found", 404)
			return
		}
		// Check wait store liveness instead of file-based stale check
		waitEntry, waitOk := bs.PendingWait.Get(uuid)
		if !waitOk {
			// Stale: wait entry missing, inject text instead
			helpers.CleanupPendingState(bot, bs.ToolNotifs, bs.PendingPerms, bs.PendingFiles, bs.PendingWait, msgID, uuid, "wait entry missing")
			t, err := injector.ParseTarget(target)
			if err != nil {
				http.Error(w, "invalid target", 400)
				return
			}
			if err := helpers.QueuedInject(bs.SessionEvents, bs.SessionState, t, text); err != nil {
				http.Error(w, fmt.Sprintf("inject failed: %v", err), 500)
				return
			}
			logger.Info(fmt.Sprintf("Group text API injected (stale): target=%s text=%s", target, helpers.TruncateStr(text, 200)))
			fmt.Fprintf(w, "injected")
			return
		}
		answers := make(map[string]string)
		if len(entry.Questions) > 0 {
			answers[entry.Questions[0].QuestionText] = text
		}
		ccOutput := helpers.BuildAskCCOutput(waitEntry.Payload, answers)
		helpers.WritePendingAnswer(bs.PendingWait, uuid, ccOutput)
		bs.ToolNotifs.MarkResolved(msgID)
		logger.Info(fmt.Sprintf("AskUserQuestion resolved via group text API: msg_id=%d uuid=%s text=%s", msgID, uuid, helpers.TruncateStr(text, 200)))
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.ChatID}}
		helpers.RetryEdit(bot, editMsg, entry.MsgText, helpers.BuildFrozenMarkup(entry, "✅ Text answer"), tele.ModeHTML)
		fmt.Fprintf(w, "resolved")
	})
}

package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/stores"
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
		// Use FindByMsgIDSnapshot — safe because msgID is a real TG button message ID
		snap, snapOk := bs.PendingWait.FindByMsgIDSnapshot(msgID)
		// Pre-check session liveness before processing the response
		if snapOk && snap.TmuxTarget != "" {
			if !helpers.CheckSessionAlive(snap.TmuxTarget, func(t string) {
				helpers.CleanDeadSession(bs.SessionState, bs.Pages, bs.SessionCounts, t)
			}) {
				http.Error(w, "session disconnected", 410)
				return
			}
		}
		switch tool {
		case "AskUserQuestion":
			if !snapOk {
				http.Error(w, "not found", 404)
				return
			}
			if snap.Resolved {
				http.Error(w, "already answered", 400)
				return
			}
			if action == "text" {
				value := r.URL.Query().Get("value")
				answers := make(map[string]string)
				if len(snap.Questions) > 0 {
					answers[snap.Questions[0].QuestionText] = value
				}
				if err := helpers.DoRespondAsk(bot, bs.PendingWait, bs.ReactionTracker, bs.NotifOpQueue, msgID, answers, "✅ Text answer"); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else if action == "submit" {
				// Get current questions from store for building answers
				questions, hasQ := bs.PendingWait.GetQuestions(snap.UUID)
				if !hasQ {
					http.Error(w, "not found", 404)
					return
				}
				if err := helpers.DoRespondAsk(bot, bs.PendingWait, bs.ReactionTracker, bs.NotifOpQueue, msgID, helpers.BuildAnswers(questions), ""); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else if action == "chat" {
				if err := helpers.DoChatAsk(bot, bs.PendingWait, bs.ReactionTracker, bs.NotifOpQueue, msgID); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else {
				qIdx, _ := strconv.Atoi(r.URL.Query().Get("question"))
				optIdx, _ := strconv.Atoi(r.URL.Query().Get("option"))
				if qIdx >= len(snap.Questions) {
					http.Error(w, "invalid question index", 400)
					return
				}
				if snap.Questions[qIdx].MultiSelect {
					// Toggle option — use store method for atomic update
					questions, err := bs.PendingWait.ToggleQuestionOption(snap.UUID, qIdx, optIdx)
					if err != nil {
						http.Error(w, "not found", 404)
						return
					}
					logger.Info(fmt.Sprintf("AskUserQuestion option toggled via API: msg_id=%d q=%d opt=%d label=%s", msgID, qIdx, optIdx, snap.Questions[qIdx].OptionLabels[optIdx]))
					newMarkup := helpers.RebuildAskMarkup(questions)
					editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: snap.ChatID}}
					helpers.RetryEdit(bot, editMsg, snap.MsgText, newMarkup, tele.ModeHTML)
				} else {
					// Select option — use store method for atomic update
					questions, err := bs.PendingWait.SelectQuestionOption(snap.UUID, qIdx, optIdx)
					if err != nil {
						http.Error(w, "not found", 404)
						return
					}
					hasSubmit := len(questions) > 1
					for _, q := range questions {
						if q.MultiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						if err := helpers.DoRespondAsk(bot, bs.PendingWait, bs.ReactionTracker, bs.NotifOpQueue, msgID, helpers.BuildAnswers(questions), ""); err != nil {
							http.Error(w, err.Error(), 500)
							return
						}
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected via API: msg_id=%d q=%d opt=%d label=%s", msgID, qIdx, optIdx, snap.Questions[qIdx].OptionLabels[optIdx]))
						newMarkup := helpers.RebuildAskMarkup(questions)
						editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: snap.ChatID}}
						helpers.RetryEdit(bot, editMsg, snap.MsgText, newMarkup, tele.ModeHTML)
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
		// Normalize target — strip socket prefix so it matches stored pane IDs
		target = notify.FormatPaneID(target)
		// Use FindByTmuxTarget (target-based path) + branch on snap.ToolName
		pwSnap, hasPending := bs.PendingWait.FindByTmuxTarget(target)
		if hasPending && pwSnap.ToolName != "AskUserQuestion" {
			// Cancel perm via snapshot then delayed inject
			helpers.CancelPermBySnapshot(bot, bs.PendingWait, bs.NotifOpQueue, notify.FormatPaneID, *pwSnap)
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
		if !hasPending {
			// No pending wait — inject text directly
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
		// Pending AskUserQuestion — resolve via snap.UUID
		waitEntry, waitOk := bs.PendingWait.Get(pwSnap.UUID)
		if !waitOk {
			// Stale: wait entry missing, clean and inject
			helpers.CleanupPendingState(bot, bs.PendingWait, bs.NotifOpQueue, pwSnap.MsgID, pwSnap.UUID, "wait entry missing")
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
		// Build answers from snap.Questions
		answers := make(map[string]string)
		if len(pwSnap.Questions) > 0 {
			answers[pwSnap.Questions[0].QuestionText] = text
		} else {
			answers["question"] = text
		}
		ccOutput := helpers.BuildAskCCOutput(waitEntry.Payload, answers)
		// Pre-build markup from snap before CAS
		frozenMarkup := helpers.BuildFrozenMarkup(pwSnap.Questions, "✅ Text answer")
		capturedUUID := pwSnap.UUID
		won, _, _ := bs.PendingWait.ResolveIfUnresolved(pwSnap.UUID, stores.WaitEvent{
			Type:   "answer",
			Output: ccOutput,
		})
		if won {
			bs.NotifOpQueue.TryEnqueue(stores.NotifOp{
				Type:         stores.OpEDIT,
				UUID:         capturedUUID,
				FreezeLabel:  "✅ Text answer",
				FrozenMarkup: frozenMarkup,
				EditFunc: func(eID int, eChatID int64, editMsgText string) {
					editMsg := &tele.Message{ID: eID, Chat: &tele.Chat{ID: eChatID}}
					helpers.RetryEdit(bot, editMsg, editMsgText, frozenMarkup, tele.ModeHTML)
					logger.Info(fmt.Sprintf("group text API: AskQ EDIT completed msg_id=%d", eID))
				},
			})
			logger.Info(fmt.Sprintf("AskUserQuestion resolved via group text API: uuid=%s text=%s", pwSnap.UUID, helpers.TruncateStr(text, 200)))
		}
		fmt.Fprintf(w, "resolved")
	})
}

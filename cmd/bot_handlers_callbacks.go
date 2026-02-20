package cmd

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	tele "gopkg.in/telebot.v3"
)

func registerCallbackHandlers(bot *tele.Bot) {
	bot.Handle(&tele.InlineButton{Unique: "p"}, func(c tele.Context) error {
		pageNum, err := strconv.Atoi(c.Data())
		if err != nil {
			return c.Respond()
		}
		entry, ok := pages.get(c.Message().ID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Page expired"})
		}
		if pageNum < 1 || pageNum > len(entry.chunks) {
			return c.Respond()
		}
		var text string
		if entry.permRows != nil {
			text = entry.chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.chunks))
		} else {
			text = notify.BuildNotificationText(notify.NotificationData{
				Event:      entry.event,
				Project:    entry.project,
				Body:       entry.chunks[pageNum-1],
				TmuxTarget: entry.tmuxTarget,
				Page:       pageNum,
				TotalPages: len(entry.chunks),
			})
		}
		kb := buildPageKeyboardWithExtra(pageNum, len(entry.chunks), entry.permRows)
		_, err = bot.Edit(c.Message(), text, kb)
		if err != nil {
			logger.Debug(fmt.Sprintf("edit page error: %v", err))
		}
		return c.Respond()
	})

	bot.Handle(&tele.InlineButton{Unique: "perm"}, func(c tele.Context) error {
		decision := c.Data()
		uuid, uuidOk := pendingPerms.getUUID(c.Message().ID)
		if !uuidOk {
			uuid, uuidOk = pendingFiles.get(c.Message().ID)
		}
		sugLabels := parseSuggestionLabels(pendingPerms.getSuggestions(c.Message().ID))
		d, err := resolvePermission(c.Message().ID, decision, nil)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Expired or invalid"})
		}
		if uuidOk {
			var updatedPerms []interface{}
			if d.UpdatedPermissions != nil {
				var perms []interface{}
				json.Unmarshal(d.UpdatedPermissions, &perms)
				updatedPerms = perms
			}
			ccOutput := buildPermCCOutput(d.Behavior, d.Message, updatedPerms)
			if err := writePendingAnswer(uuid, ccOutput); err != nil {
				logger.Error(fmt.Sprintf("Failed to write pending answer for perm: %v", err))
			}
		}
		logger.Info(fmt.Sprintf("Permission resolved via TG button: msg_id=%d decision=%s uuid=%s", c.Message().ID, decision, uuid))
		bot.Edit(c.Message(), c.Message().Text, buildFrozenPermMarkup(decision, sugLabels))
		displayText := decision
		if strings.HasPrefix(decision, "s") {
			displayText = "Always Allow"
		}
		targetPtr, err := extractTmuxTarget(c.Message().Text)
		if err == nil && targetPtr != nil {
			reactAndTrack(bot, c.Message().Chat, c.Message(), injector.FormatTarget(*targetPtr))
		}
		return c.Respond(&tele.CallbackResponse{Text: "✅ " + displayText})
	})

	bot.Handle(&tele.InlineButton{Unique: "tool"}, func(c tele.Context) error {
		parts := strings.SplitN(c.Data(), "|", 2)
		if len(parts) < 2 {
			return c.Respond(&tele.CallbackResponse{Text: "Invalid data"})
		}
		toolName := parts[0]
		switch toolName {
		case "AskUserQuestion":
			entry, ok := toolNotifs.get(c.Message().ID)
			if !ok {
				return c.Respond(&tele.CallbackResponse{Text: "Expired"})
			}
			if entry.resolved {
				return c.Respond(&tele.CallbackResponse{Text: "Already answered"})
			}
			if parts[1] == "chat" {
				uuid, ok := pendingFiles.get(c.Message().ID)
				if !ok {
					return c.Respond(&tele.CallbackResponse{Text: "Pending file not found"})
				}
				path := filepath.Join(pendingDir(), uuid+".json")
				pf, err := readPendingFile(path)
				if err != nil {
					return c.Respond(&tele.CallbackResponse{Text: "Failed to read pending file"})
				}
				answers := map[string]string{"__chat": "true"}
				ccOutput := buildAskCCOutput(pf.Payload, answers)
				if err := writePendingAnswer(uuid, ccOutput); err != nil {
					logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
					return c.Respond(&tele.CallbackResponse{Text: "Failed to save answer"})
				}
				toolNotifs.markResolved(c.Message().ID)
				bot.Edit(c.Message(), c.Message().Text, buildFrozenMarkup(entry, "💬 Chat mode selected"))
				logger.Info(fmt.Sprintf("AskUserQuestion 'Chat about this' selected: msg_id=%d uuid=%s", c.Message().ID, uuid))
				return c.Respond(&tele.CallbackResponse{Text: "Chat mode"})
			} else if parts[1] == "submit" {
				uuid, ok := pendingFiles.get(c.Message().ID)
				if !ok {
					return c.Respond(&tele.CallbackResponse{Text: "Pending file not found"})
				}
				path := filepath.Join(pendingDir(), uuid+".json")
				pf, err := readPendingFile(path)
				if err != nil {
					return c.Respond(&tele.CallbackResponse{Text: "Failed to read pending file"})
				}
				answers := buildAnswers(entry)
				ccOutput := buildAskCCOutput(pf.Payload, answers)
				if err := writePendingAnswer(uuid, ccOutput); err != nil {
					logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
					return c.Respond(&tele.CallbackResponse{Text: "Failed to save answer"})
				}
				toolNotifs.markResolved(c.Message().ID)
				bot.Edit(c.Message(), c.Message().Text, buildFrozenMarkup(entry, ""))
				logger.Info(fmt.Sprintf("AskUserQuestion submitted: msg_id=%d uuid=%s answers=%v", c.Message().ID, uuid, answers))
				return c.Respond(&tele.CallbackResponse{Text: "✅ Submitted"})
			} else {
				split := strings.SplitN(parts[1], ":", 2)
				qIdx, _ := strconv.Atoi(split[0])
				optIdx, _ := strconv.Atoi(split[1])
				if qIdx >= len(entry.questions) {
					return c.Respond(&tele.CallbackResponse{Text: "Invalid question"})
				}
				qm := &entry.questions[qIdx]
				if qm.multiSelect {
					qm.selectedOptions[optIdx] = !qm.selectedOptions[optIdx]
					logger.Info(fmt.Sprintf("AskUserQuestion multiSelect toggle: msg_id=%d q=%d opt=%d state=%v label=%s", c.Message().ID, qIdx, optIdx, qm.selectedOptions[optIdx], qm.optionLabels[optIdx]))
					newMarkup := rebuildAskMarkup(entry)
					bot.Edit(c.Message(), c.Message().Text, newMarkup)
					return c.Respond(&tele.CallbackResponse{Text: "Toggled"})
				} else {
					qm.selectedOption = optIdx
					hasSubmit := len(entry.questions) > 1
					for _, q := range entry.questions {
						if q.multiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						uuid, ok := pendingFiles.get(c.Message().ID)
						if !ok {
							return c.Respond(&tele.CallbackResponse{Text: "Pending file not found"})
						}
						path := filepath.Join(pendingDir(), uuid+".json")
						pf, err := readPendingFile(path)
						if err != nil {
							return c.Respond(&tele.CallbackResponse{Text: "Failed to read pending file"})
						}
						answers := buildAnswers(entry)
						ccOutput := buildAskCCOutput(pf.Payload, answers)
						if err := writePendingAnswer(uuid, ccOutput); err != nil {
							logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
							return c.Respond(&tele.CallbackResponse{Text: "Failed to save answer"})
						}
						toolNotifs.markResolved(c.Message().ID)
						bot.Edit(c.Message(), c.Message().Text, buildFrozenMarkup(entry, ""))
						logger.Info(fmt.Sprintf("AskUserQuestion auto-resolved: msg_id=%d uuid=%s answers=%v", c.Message().ID, uuid, answers))
						return c.Respond(&tele.CallbackResponse{Text: "✅ Selected"})
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected: msg_id=%d q=%d opt=%d label=%s", c.Message().ID, qIdx, optIdx, qm.optionLabels[optIdx]))
						newMarkup := rebuildAskMarkup(entry)
						bot.Edit(c.Message(), c.Message().Text, newMarkup)
						return c.Respond(&tele.CallbackResponse{Text: "Selected"})
					}
				}
			}
		}
		if entry, ok := toolNotifs.get(c.Message().ID); ok {
			reactAndTrack(bot, c.Message().Chat, c.Message(), entry.tmuxTarget)
		}
		return c.Respond()
	})
}

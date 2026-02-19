package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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
		_, err := resolvePermission(c.Message().ID, decision, nil)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Expired or invalid"})
		}
		logger.Info(fmt.Sprintf("Permission resolved via TG button: msg_id=%d decision=%s", c.Message().ID, decision))
		bot.Edit(c.Message(), c.Message().Text)
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
			if parts[1] == "chat" {
				answers := map[string]string{"__chat": "true"}
				if !pendingAsks.resolve(c.Message().ID, answers) {
					target, _ := injector.ParseTarget(entry.tmuxTarget)
					numOptions := 0
					if len(entry.questions) > 0 {
						numOptions = entry.questions[0].numOptions
					}
					for i := 0; i < numOptions+1; i++ {
						injector.SendKeys(target, "Down")
						time.Sleep(100 * time.Millisecond)
					}
					injector.SendKeys(target, "Enter")
				}
				logger.Info(fmt.Sprintf("AskUserQuestion 'Chat about this' selected: msg_id=%d", c.Message().ID))
				return c.Respond(&tele.CallbackResponse{Text: "Chat mode"})
			} else if parts[1] == "submit" {
				answers := buildAnswers(entry)
				if !pendingAsks.resolve(c.Message().ID, answers) {
					return c.Respond(&tele.CallbackResponse{Text: "Already submitted"})
				}
				logger.Info(fmt.Sprintf("AskUserQuestion submitted: msg_id=%d answers=%v", c.Message().ID, answers))
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
						answers := buildAnswers(entry)
						if !pendingAsks.resolve(c.Message().ID, answers) {
							return c.Respond(&tele.CallbackResponse{Text: "Already submitted"})
						}
						logger.Info(fmt.Sprintf("AskUserQuestion auto-resolved: msg_id=%d answers=%v", c.Message().ID, answers))
						bot.Edit(c.Message(), c.Message().Text)
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

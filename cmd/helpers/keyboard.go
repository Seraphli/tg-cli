package helpers

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/cmd/stores"
	tele "gopkg.in/telebot.v3"
)

// BuildPageKeyboard returns a ReplyMarkup with ◀️ N/M ▶️ inline buttons.
// Callback data format: p\x00<pageNum> (where pageNum is the 1-based page number as string).
func BuildPageKeyboard(currentPage, totalPages int) *tele.ReplyMarkup {
	return BuildPageKeyboardWithExtra(currentPage, totalPages, nil)
}

// BuildPageKeyboardWithExtra returns page navigation buttons plus optional extra rows
// (e.g. permission Allow/Deny buttons).
func BuildPageKeyboardWithExtra(currentPage, totalPages int, extraRows []tele.Row) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var allRows []tele.Row
	allRows = append(allRows, extraRows...)
	// Page navigation row
	var pageRow tele.Row
	if currentPage > 1 {
		pageRow = append(pageRow, markup.Data("◀️", "p", fmt.Sprintf("%d", currentPage-1)))
	}
	pageRow = append(pageRow, markup.Data(fmt.Sprintf("%d/%d", currentPage, totalPages), "p", fmt.Sprintf("%d", currentPage)))
	if currentPage < totalPages {
		pageRow = append(pageRow, markup.Data("▶️", "p", fmt.Sprintf("%d", currentPage+1)))
	}
	allRows = append(allRows, pageRow)
	markup.Inline(allRows...)
	return markup
}

// RebuildAskMarkup rebuilds the inline keyboard for an AskUserQuestion entry.
func RebuildAskMarkup(entry *stores.ToolNotifyEntry) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row

	hasSubmit := len(entry.Questions) > 1
	for _, q := range entry.Questions {
		if q.MultiSelect {
			hasSubmit = true
		}
	}

	if len(entry.Questions) == 1 && !entry.Questions[0].MultiSelect {
		// Single question, single select
		q := entry.Questions[0]
		var buttons []tele.Btn
		for i, label := range q.OptionLabels {
			displayLabel := label
			if q.SelectedOption == i {
				displayLabel = "✅ " + label
			}
			buttons = append(buttons, markup.Data(displayLabel, "tool", fmt.Sprintf("AskUserQuestion|0:%d", i)))
		}
		for i := 0; i < len(buttons); i += 2 {
			if i+1 < len(buttons) {
				rows = append(rows, markup.Row(buttons[i], buttons[i+1]))
			} else {
				rows = append(rows, markup.Row(buttons[i]))
			}
		}
	} else {
		// Multi-question or multiSelect
		for qIdx, q := range entry.Questions {
			for optIdx, label := range q.OptionLabels {
				displayLabel := label
				if len(entry.Questions) > 1 {
					displayLabel = fmt.Sprintf("Q%d: %s", qIdx+1, label)
				}
				if q.MultiSelect && q.SelectedOptions[optIdx] {
					displayLabel = "✅ " + displayLabel
				} else if !q.MultiSelect && q.SelectedOption == optIdx {
					displayLabel = "✅ " + displayLabel
				}
				rows = append(rows, markup.Row(markup.Data(displayLabel, "tool", fmt.Sprintf("AskUserQuestion|%d:%d", qIdx, optIdx))))
			}
		}
		if hasSubmit {
			rows = append(rows, markup.Row(markup.Data("📤 Submit", "tool", "AskUserQuestion|submit")))
		}
	}
	rows = append(rows, markup.Row(markup.Data("💬 Chat about this", "tool", "AskUserQuestion|chat")))

	markup.Inline(rows...)
	return markup
}

// BuildFrozenMarkup creates a frozen version of the inline keyboard markup after user selection.
// Shows selected options with ✅ prefix, no Submit/Chat buttons.
// Buttons remain visible but handler checks resolved flag.
func BuildFrozenMarkup(entry *stores.ToolNotifyEntry, footer string) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row

	if footer != "" {
		rows = append(rows, markup.Row(markup.Data(footer, "tool", "noop")))
		markup.Inline(rows...)
		return markup
	}

	if len(entry.Questions) == 1 && !entry.Questions[0].MultiSelect {
		// Single question, single select - show all options with ✅ on selected
		q := entry.Questions[0]
		var buttons []tele.Btn
		for i, label := range q.OptionLabels {
			displayLabel := label
			if q.SelectedOption == i {
				displayLabel = "✅ " + label
			}
			buttons = append(buttons, markup.Data(displayLabel, "tool", fmt.Sprintf("AskUserQuestion|0:%d", i)))
		}
		for i := 0; i < len(buttons); i += 2 {
			if i+1 < len(buttons) {
				rows = append(rows, markup.Row(buttons[i], buttons[i+1]))
			} else {
				rows = append(rows, markup.Row(buttons[i]))
			}
		}
	} else {
		// Multi-question or multiSelect - show all options with ✅ on selected
		for qIdx, q := range entry.Questions {
			for optIdx, label := range q.OptionLabels {
				displayLabel := label
				if len(entry.Questions) > 1 {
					displayLabel = fmt.Sprintf("Q%d: %s", qIdx+1, label)
				}
				if q.MultiSelect && q.SelectedOptions[optIdx] {
					displayLabel = "✅ " + displayLabel
				} else if !q.MultiSelect && q.SelectedOption == optIdx {
					displayLabel = "✅ " + displayLabel
				}
				rows = append(rows, markup.Row(markup.Data(displayLabel, "tool", fmt.Sprintf("AskUserQuestion|%d:%d", qIdx, optIdx))))
			}
		}
	}

	if footer != "" {
		rows = append(rows, markup.Row(markup.Data(footer, "tool", "noop")))
	}
	markup.Inline(rows...)
	return markup
}

// BuildFrozenPermMarkup creates frozen markup for PermissionRequest showing the selected decision.
func BuildFrozenPermMarkup(selectedDecision string, suggestionLabel string) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row

	if strings.Contains(selectedDecision, "Cancel") {
		rows = append(rows, markup.Row(markup.Data("❌ Cancelled", "perm", "noop")))
		markup.Inline(rows...)
		return markup
	}

	allowLabel := "Allow"
	denyLabel := "Deny"
	if selectedDecision == "allow" {
		allowLabel = "✅ Allow"
	} else if selectedDecision == "deny" {
		denyLabel = "✅ Deny"
	}

	rows = append(rows, markup.Row(
		markup.Data(allowLabel, "perm", "allow"),
		markup.Data(denyLabel, "perm", "deny"),
	))

	if suggestionLabel != "" {
		label := suggestionLabel
		if selectedDecision == "sAll" || strings.HasPrefix(selectedDecision, "s") {
			label = "✅ " + suggestionLabel
		}
		rows = append(rows, markup.Row(markup.Data(label, "perm", "sAll")))
	}

	markup.Inline(rows...)
	return markup
}

// BuildAnswers builds a map of question text to selected answer label.
func BuildAnswers(entry *stores.ToolNotifyEntry) map[string]string {
	answers := make(map[string]string)
	for _, q := range entry.Questions {
		if q.MultiSelect {
			var selected []string
			for oi := 0; oi < q.NumOptions; oi++ {
				if q.SelectedOptions[oi] {
					selected = append(selected, q.OptionLabels[oi])
				}
			}
			answers[q.QuestionText] = strings.Join(selected, ", ")
		} else if q.SelectedOption >= 0 {
			answers[q.QuestionText] = q.OptionLabels[q.SelectedOption]
		}
	}
	return answers
}

// BuildResumeKeyboard builds an inline keyboard with one button per session.
func BuildResumeKeyboard(sessions []SessionListEntry) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	for i, s := range sessions {
		label := fmt.Sprintf("%d • %s", i+1, RelativeTime(s.Modified))
		rows = append(rows, markup.Row(markup.Data(label, "resume", s.SessionID)))
	}
	markup.Inline(rows...)
	return markup
}

// RelativeTime formats a time as a human-readable relative string.
func RelativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ParseSuggestionLabel parses suggestions and returns a short button label and a detailed description.
func ParseSuggestionLabel(suggestionsRaw json.RawMessage) (btnLabel string, description string) {
	var suggestions []json.RawMessage
	json.Unmarshal(suggestionsRaw, &suggestions)
	if len(suggestions) == 0 {
		return "", ""
	}
	var descs []string
	for _, s := range suggestions {
		var sug struct {
			Type         string   `json:"type"`
			Tool         string   `json:"tool"`
			AllowPattern string   `json:"allow_pattern"`
			Mode         string   `json:"mode"`
			Directories  []string `json:"directories"`
			Rules        []struct {
				ToolName    string `json:"toolName"`
				RuleContent string `json:"ruleContent"`
			} `json:"rules"`
		}
		json.Unmarshal(s, &sug)
		var desc string
		switch sug.Type {
		case "addDirectories":
			dir := ""
			if len(sug.Directories) > 0 {
				dir = filepath.Base(sug.Directories[len(sug.Directories)-1])
			}
			desc = "allow access to " + dir + "/"
		case "addRules":
			var ruleParts []string
			for _, r := range sug.Rules {
				rc := strings.ReplaceAll(r.RuleContent, "//", "/")
				if strings.Contains(rc, " ") {
					rc = "*"
				}
				if rc != "*" {
					// Show only the last meaningful path component + glob (e.g., ".tg-cli/**")
					parts := strings.Split(rc, "/")
					for i := len(parts) - 1; i >= 0; i-- {
						if parts[i] != "" && parts[i] != "**" && parts[i] != "*" {
							rc = parts[i] + "/" + strings.Join(parts[i+1:], "/")
							break
						}
					}
				}
				if rc != "*" {
					ruleParts = append(ruleParts, rc)
				}
			}
			if len(ruleParts) > 0 {
				desc = "don't ask again for: " + strings.Join(ruleParts, ", ")
			} else {
				desc = "don't ask again"
			}
		case "toolAlwaysAllow":
			desc = "always allow " + sug.Tool
		case "setMode":
			desc = "switch to " + sug.Mode + " mode"
		default:
			toolName := sug.Tool
			allowPattern := sug.AllowPattern
			if toolName == "" && len(sug.Rules) > 0 {
				toolName = sug.Rules[0].ToolName
				if allowPattern == "" {
					allowPattern = sug.Rules[0].RuleContent
				}
			}
			if allowPattern != "" {
				allowPattern = strings.ReplaceAll(allowPattern, "//", "/")
			}
			desc = "always allow"
			if toolName != "" {
				desc += " " + toolName
			}
			if allowPattern != "" && allowPattern != "*" {
				if strings.Contains(allowPattern, " ") {
					allowPattern = "*"
				}
				if allowPattern != "*" {
					desc += " (" + allowPattern + ")"
				}
			}
		}
		descs = append(descs, desc)
	}
	return "Always Allow", strings.Join(descs, "; ")
}

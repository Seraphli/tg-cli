package notify

import (
	"fmt"
	"strings"
)

type NotificationData struct {
	Event             string
	Project           string
	Body              string
	TmuxTarget        string
	Page              int // 0 = no pagination
	TotalPages        int
	ContextUsedPct    int // -1 means no data
	ContextWindowSize int
	ContextUsedTokens int
}

type PermissionData struct {
	Project    string
	TmuxTarget string
	ToolName   string
	ToolInput  map[string]interface{}
}

type QuestionOption struct {
	Label       string
	Description string
}

type QuestionEntry struct {
	Header      string
	Question    string
	Options     []QuestionOption
	MultiSelect bool
}

type QuestionData struct {
	Project    string
	TmuxTarget string
	Header     string
	Question   string
	Options    []QuestionOption
	Questions  []QuestionEntry
}

func formatTokens(v float64) string {
	if v >= 1_000_000 {
		return fmt.Sprintf("%.1fM", v/1_000_000)
	}
	return fmt.Sprintf("%.1fk", v/1000)
}

func BuildNotificationText(data NotificationData) string {
	var emoji, status string
	switch {
	case data.Event == "SessionStart":
		emoji = "🟢"
		status = "Session Started"
	case data.Event == "SessionEnd":
		emoji = "🔴"
		status = "Session Ended"
	case data.Event == "PreToolUse":
		emoji = "💬"
		status = "Update"
	default:
		emoji = "✅"
		status = "Task Completed"
	}
	statusLine := emoji + " " + status
	if data.Page > 0 {
		statusLine += fmt.Sprintf(" (%d/%d)", data.Page, data.TotalPages)
	}
	lines := []string{
		statusLine,
		"Project: " + data.Project,
	}
	if data.TmuxTarget != "" {
		lines = append(lines, "📟 "+data.TmuxTarget)
	}
	if data.ContextUsedPct >= 0 {
		used := float64(data.ContextUsedTokens)
		usedStr := formatTokens(used)
		totalStr := fmt.Sprintf("%dk", data.ContextWindowSize/1000)
		lines = append(lines, fmt.Sprintf("📊 Context: %d%% (%s/%s)", data.ContextUsedPct, usedStr, totalStr))
	}
	if data.Body != "" {
		lines = append(lines, "", "💬 Claude:", data.Body)
	}
	return strings.Join(lines, "\n")
}

func HeaderLen(data NotificationData) int {
	d := data
	d.Body = ""
	return len([]rune(BuildNotificationText(d)))
}

func BuildPermissionText(data PermissionData) string {
	lines := []string{
		"🔐 Permission Request",
		"Project: " + data.Project,
	}
	if data.TmuxTarget != "" {
		lines = append(lines, "📟 "+data.TmuxTarget)
	}
	lines = append(lines, "", "🔧 Tool: "+data.ToolName)
	// Show key fields from tool_input
	for _, key := range []string{"command", "file_path", "old_string", "new_string", "replace_all", "url", "query", "pattern", "prompt"} {
		if v, ok := data.ToolInput[key]; ok {
			s := fmt.Sprintf("%v", v)
			if key == "old_string" || key == "new_string" {
				lines = append(lines, key+":\n```\n"+s+"\n```")
			} else {
				lines = append(lines, key+": "+s)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func BuildQuestionText(data QuestionData) string {
	lines := []string{
		"❓ Question",
		"Project: " + data.Project,
	}
	if data.TmuxTarget != "" {
		lines = append(lines, "📟 "+data.TmuxTarget)
	}
	if len(data.Questions) > 1 {
		for qIdx, q := range data.Questions {
			multiTag := ""
			if q.MultiSelect {
				multiTag = " (多选)"
			}
			lines = append(lines, "", fmt.Sprintf("**Q%d: %s**%s", qIdx+1, q.Header, multiTag))
			lines = append(lines, q.Question)
			for i, opt := range q.Options {
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, opt.Label))
				if opt.Description != "" {
					lines = append(lines, "  → "+opt.Description)
				}
			}
		}
	} else if len(data.Questions) == 1 {
		q := data.Questions[0]
		if q.Header != "" {
			lines = append(lines, "", "📋 "+q.Header)
		}
		lines = append(lines, "", q.Question)
		for i, opt := range q.Options {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, opt.Label))
			if opt.Description != "" {
				lines = append(lines, "  → "+opt.Description)
			}
		}
	} else {
		if data.Header != "" {
			lines = append(lines, "", "📋 "+data.Header)
		}
		lines = append(lines, "", data.Question)
		for i, opt := range data.Options {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, opt.Label))
			if opt.Description != "" {
				lines = append(lines, "  → "+opt.Description)
			}
		}
	}
	return strings.Join(lines, "\n")
}

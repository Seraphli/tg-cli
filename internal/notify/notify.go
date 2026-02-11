package notify

import (
	"fmt"
	"strings"
)

type NotificationData struct {
	Event      string
	Project    string
	Body       string
	TmuxTarget string
	Page       int // 0 = no pagination
	TotalPages int
}

type PermissionData struct {
	Project    string
	TmuxTarget string
	ToolName   string
	ToolInput  map[string]interface{}
}

type QuestionData struct {
	Project    string
	TmuxTarget string
	Question   string
	Options    []string
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
	for _, key := range []string{"command", "file_path", "url", "query", "pattern", "prompt"} {
		if v, ok := data.ToolInput[key]; ok {
			s := fmt.Sprintf("%v", v)
			if len(s) > 500 {
				s = s[:500] + "..."
			}
			lines = append(lines, key+": "+s)
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
	lines = append(lines, "", data.Question)
	for i, opt := range data.Options {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, opt))
	}
	return strings.Join(lines, "\n")
}

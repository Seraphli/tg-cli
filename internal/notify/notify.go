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

func BuildNotificationText(data NotificationData) string {
	var emoji, status string
	switch {
	case data.Event == "SessionStart":
		emoji = "🟢"
		status = "Started"
	case data.Event == "SubagentStop":
		emoji = "⏳"
		status = "Waiting"
	default:
		emoji = "✅"
		status = "Completed"
	}
	statusLine := emoji + " Task " + status
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

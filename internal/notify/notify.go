package notify

import "strings"

type NotificationData struct {
	Event      string
	Project    string
	Body       string
	TmuxTarget string
}

func BuildNotificationText(data NotificationData) string {
	isWaiting := data.Event == "SubagentStop"
	var emoji, status string
	if isWaiting {
		emoji = "⏳"
		status = "Waiting"
	} else {
		emoji = "✅"
		status = "Completed"
	}
	lines := []string{
		emoji + " Task " + status,
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

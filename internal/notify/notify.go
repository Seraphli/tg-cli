package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type NotificationData struct {
	Event             string
	Project           string
	CWD               string
	Body              string
	TmuxTarget        string
	ToolName          string
	Page              int // 0 = no pagination
	TotalPages        int
	ContextUsedPct    int // -1 means no data
	ContextWindowSize int
	ContextUsedTokens int
}

type PermissionData struct {
	Project    string
	CWD        string
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
	Project           string
	CWD               string
	TmuxTarget        string
	Header            string
	Question          string
	Options           []QuestionOption
	Questions         []QuestionEntry
	ContextUsedPct    int
	ContextUsedTokens int
	ContextWindowSize int
}

// CompressPath shortens a filesystem path by abbreviating intermediate components to their first character.
func CompressPath(path string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}
	sep := string(os.PathSeparator)
	parts := strings.Split(path, sep)
	if len(parts) <= 2 {
		return path
	}
	for i := 1; i < len(parts)-1; i++ {
		if len(parts[i]) > 0 {
			parts[i] = string([]rune(parts[i])[0])
		}
	}
	return strings.Join(parts, sep)
}

// FormatPaneID extracts the pane ID from a tmux target string (strips the /tmp/... suffix after '@').
func FormatPaneID(tmuxTarget string) string {
	if idx := strings.Index(tmuxTarget, "@"); idx != -1 {
		return tmuxTarget[:idx]
	}
	return tmuxTarget
}

func formatTokens(v float64) string {
	if v >= 1_000_000 {
		return fmt.Sprintf("%.1fM", v/1_000_000)
	}
	return fmt.Sprintf("%.1fk", v/1000)
}

// projectDisplay returns a display string for the project: compressed CWD if available, else raw project.
func projectDisplay(project, cwd string) string {
	if cwd != "" {
		return CompressPath(cwd)
	}
	return project
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
	case data.Event == "ToolUse":
		emoji = "🔧"
		status = data.ToolName
		if status == "" {
			status = "Tool Call"
		}
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
		"Project: " + projectDisplay(data.Project, data.CWD),
	}
	if data.TmuxTarget != "" {
		lines = append(lines, "📟 "+FormatPaneID(data.TmuxTarget))
	}
	if data.ContextUsedPct >= 0 {
		used := float64(data.ContextUsedTokens)
		usedStr := formatTokens(used)
		totalStr := formatTokens(float64(data.ContextWindowSize))
		lines = append(lines, fmt.Sprintf("📊 Context: %d%% (%s/%s)", data.ContextUsedPct, usedStr, totalStr))
	}
	if data.Body != "" {
		lines = append(lines, "", data.Body)
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
		"Project: " + projectDisplay(data.Project, data.CWD),
	}
	if data.TmuxTarget != "" {
		lines = append(lines, "📟 "+FormatPaneID(data.TmuxTarget))
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

// truncToolStr truncates a string to maxLen runes, appending ellipsis if truncated.
func truncToolStr(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "…"
}

// BuildToolNotifyText formats a tool call notification message for Telegram.
// Displays the raw toolInput JSON as-is.
func BuildToolNotifyText(toolName string, toolInput json.RawMessage, cwd string) string {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, toolInput, "", "  "); err != nil {
		return string(toolInput)
	}
	result := pretty.String()
	r := []rune(result)
	if len(r) > 1000 {
		result = string(r[:1000]) + "…"
	}
	return result
}

func BuildQuestionText(data QuestionData) string {
	lines := []string{
		"❓ Question",
		"Project: " + projectDisplay(data.Project, data.CWD),
	}
	if data.TmuxTarget != "" {
		lines = append(lines, "📟 "+FormatPaneID(data.TmuxTarget))
	}
	if data.ContextUsedPct >= 0 {
		used := float64(data.ContextUsedTokens)
		usedStr := formatTokens(used)
		totalStr := formatTokens(float64(data.ContextWindowSize))
		lines = append(lines, fmt.Sprintf("📊 Context: %d%% (%s/%s)", data.ContextUsedPct, usedStr, totalStr))
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

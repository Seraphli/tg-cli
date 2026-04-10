package notify

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Seraphli/tg-cli/internal/markdown"
)

type NotificationData struct {
	Event             string
	Project           string
	CWD               string
	Body              string
	TmuxTarget        string
	ToolName          string
	AgentName         string
	Backend           string // "cc" or "codex"
	CLICommand        string
	Page              int    // 0 = no pagination
	TotalPages        int
	ContextUsedPct    int // -1 means no data
	ContextWindowSize int
	ContextUsedTokens int
}

type PermissionData struct {
	Project        string
	CWD            string
	TmuxTarget     string
	ToolName       string
	ToolInput      map[string]interface{}
	SuggestionDesc string
	AgentName      string
	CLICommand     string
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
	AgentName         string
	CLICommand        string
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

// HeaderInfo contains common header fields for all notification types.
type HeaderInfo struct {
	Project           string
	CWD               string
	TmuxTarget        string
	CLICommand        string
	ContextUsedPct    int // -1 means no data
	ContextUsedTokens int
	ContextWindowSize int
}

// buildHeader generates the common header lines for notifications.
func buildHeader(firstLine string, h HeaderInfo) []string {
	lines := []string{
		firstLine,
		"📂 " + markdown.EscapeHTML(projectDisplay(h.Project, h.CWD)),
	}
	if h.TmuxTarget != "" {
		lines = append(lines, "📟 "+markdown.EscapeHTML(FormatPaneID(h.TmuxTarget)))
	}
	if h.CLICommand != "" {
		lines = append(lines, "🖥 "+markdown.EscapeHTML(h.CLICommand))
	}
	if h.ContextUsedPct >= 0 {
		used := float64(h.ContextUsedTokens)
		usedStr := formatTokens(used)
		totalStr := formatTokens(float64(h.ContextWindowSize))
		lines = append(lines, fmt.Sprintf("📊 Context: %d%% (%s/%s)", h.ContextUsedPct, usedStr, totalStr))
	}
	return lines
}

// FormatPaneID returns the full tmux target string as-is.
func FormatPaneID(tmuxTarget string) string {
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
	case data.Event == "PostToolUse":
		emoji = "🔧"
		status = "Tool Done"
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
	var statusLine string
	if data.AgentName != "" {
		statusLine = emoji + " [" + markdown.EscapeHTML(data.AgentName) + "] " + status
	} else {
		statusLine = emoji + " " + status
	}
	if data.Page > 0 {
		statusLine += fmt.Sprintf(" (%d/%d)", data.Page, data.TotalPages)
	}
	lines := buildHeader(statusLine, HeaderInfo{
		Project: data.Project, CWD: data.CWD, TmuxTarget: data.TmuxTarget,
		CLICommand: data.CLICommand, ContextUsedPct: data.ContextUsedPct,
		ContextUsedTokens: data.ContextUsedTokens, ContextWindowSize: data.ContextWindowSize,
	})
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
	firstLine := "🔐 Permission Request"
	if data.AgentName != "" {
		firstLine = "🔐 [" + markdown.EscapeHTML(data.AgentName) + "] Permission Request"
	}
	lines := buildHeader(firstLine, HeaderInfo{
		Project: data.Project, CWD: data.CWD, TmuxTarget: data.TmuxTarget,
		CLICommand: data.CLICommand, ContextUsedPct: -1,
	})
	lines = append(lines, "", "🔧 Tool: "+markdown.EscapeHTML(data.ToolName))
	// Show key fields from tool_input
	for _, key := range []string{"command", "description", "file_path", "old_string", "new_string", "replace_all", "url", "query", "pattern", "prompt"} {
		if v, ok := data.ToolInput[key]; ok {
			s := fmt.Sprintf("%v", v)
			if key == "old_string" || key == "new_string" {
				lines = append(lines, key+":\n<pre>"+markdown.EscapeHTML(s)+"</pre>")
			} else if key == "description" {
				lines = append(lines, "ℹ️ "+markdown.EscapeHTML(s))
			} else {
				lines = append(lines, key+": "+markdown.EscapeHTML(s))
			}
		}
	}
	if data.SuggestionDesc != "" {
		lines = append(lines, "", "💡 Always Allow: "+markdown.EscapeHTML(data.SuggestionDesc))
	}
	return strings.Join(lines, "\n")
}

// BuildToolNotifyText formats a tool call notification message for Telegram.
// Each tool type gets a human-readable format with relevant emojis.
func BuildToolNotifyText(toolName string, toolInput json.RawMessage, cwd string) string {
	var fields map[string]interface{}
	if err := json.Unmarshal(toolInput, &fields); err != nil {
		return string(toolInput)
	}
	var b strings.Builder
	esc := func(v interface{}) string {
		return markdown.EscapeHTML(fmt.Sprintf("%v", v))
	}
	switch toolName {
	case "Bash":
		if cmd, ok := fields["command"]; ok {
			fmt.Fprintf(&b, "💻 %s", esc(cmd))
		}
		if desc, ok := fields["description"]; ok {
			fmt.Fprintf(&b, "\nℹ️ %s", esc(desc))
		}
		if timeout, ok := fields["timeout"]; ok {
			fmt.Fprintf(&b, "\n⏱️ timeout: %s", esc(timeout))
		}
		if bg, ok := fields["run_in_background"]; ok {
			fmt.Fprintf(&b, "\n🔄 background: %s", esc(bg))
		}
	case "Edit":
		if fp, ok := fields["file_path"]; ok {
			fmt.Fprintf(&b, "📄 %s", esc(fp))
		}
		if ra, ok := fields["replace_all"]; ok {
			fmt.Fprintf(&b, "\n🔄 replace_all: %s", esc(ra))
		}
		oldStr := ""
		if old, ok := fields["old_string"]; ok {
			oldStr = fmt.Sprintf("%v", old)
		}
		newStr := ""
		if ns, ok := fields["new_string"]; ok {
			newStr = fmt.Sprintf("%v", ns)
		}
		if oldStr != "" {
			fmt.Fprintf(&b, "\n\nOld:\n<pre>%s</pre>", markdown.ExpandTabs(esc(oldStr)))
		} else {
			fmt.Fprintf(&b, "\n\nOld: (empty)")
		}
		if newStr != "" {
			fmt.Fprintf(&b, "\n\nNew:\n<pre>%s</pre>", markdown.ExpandTabs(esc(newStr)))
		} else {
			fmt.Fprintf(&b, "\n\nNew: (empty)")
		}
	case "Write":
		if fp, ok := fields["file_path"]; ok {
			fmt.Fprintf(&b, "📄 %s", esc(fp))
		}
		if content, ok := fields["content"]; ok {
			fmt.Fprintf(&b, "\n\n<pre>%s</pre>", markdown.ExpandTabs(esc(content)))
		}
	case "Read":
		if fp, ok := fields["file_path"]; ok {
			fmt.Fprintf(&b, "📄 %s", esc(fp))
		}
		if offset, ok := fields["offset"]; ok {
			fmt.Fprintf(&b, "\n📍 offset: %s", esc(offset))
		}
		if limit, ok := fields["limit"]; ok {
			fmt.Fprintf(&b, "\n📏 limit: %s", esc(limit))
		}
	case "Glob":
		if pattern, ok := fields["pattern"]; ok {
			fmt.Fprintf(&b, "🔍 %s", esc(pattern))
		}
		if path, ok := fields["path"]; ok {
			fmt.Fprintf(&b, "\n📂 %s", esc(path))
		}
	case "Grep":
		if pattern, ok := fields["pattern"]; ok {
			fmt.Fprintf(&b, "🔍 %s", esc(pattern))
		}
		if path, ok := fields["path"]; ok {
			fmt.Fprintf(&b, "\n📂 %s", esc(path))
		}
		for _, key := range []string{"output_mode", "glob", "type", "-n", "-B", "-A", "-C", "-i"} {
			if v, ok := fields[key]; ok {
				fmt.Fprintf(&b, "\n%s: %s", key, esc(v))
			}
		}
	case "Agent":
		if desc, ok := fields["description"]; ok {
			fmt.Fprintf(&b, "ℹ️ %s", esc(desc))
		}
		if st, ok := fields["subagent_type"]; ok {
			fmt.Fprintf(&b, "\n🤖 %s", esc(st))
		}
		if model, ok := fields["model"]; ok {
			fmt.Fprintf(&b, "\n🏷️ model: %s", esc(model))
		}
		if bg, ok := fields["run_in_background"]; ok {
			fmt.Fprintf(&b, "\n🔄 background: %s", esc(bg))
		}
		if iso, ok := fields["isolation"]; ok {
			fmt.Fprintf(&b, "\n📦 isolation: %s", esc(iso))
		}
	case "WebFetch":
		if url, ok := fields["url"]; ok {
			fmt.Fprintf(&b, "🌐 %s", esc(url))
		}
		if prompt, ok := fields["prompt"]; ok {
			fmt.Fprintf(&b, "\nℹ️ %s", esc(prompt))
		}
	case "WebSearch":
		if query, ok := fields["query"]; ok {
			fmt.Fprintf(&b, "🔍 %s", esc(query))
		}
	case "AskUserQuestion":
		questions, _ := fields["questions"].([]interface{})
		for qi, q := range questions {
			qMap, _ := q.(map[string]interface{})
			if qMap == nil {
				continue
			}
			if qi > 0 {
				b.WriteString("\n\n")
			}
			header, _ := qMap["header"].(string)
			question, _ := qMap["question"].(string)
			if header != "" {
				fmt.Fprintf(&b, "❓ %s\n", esc(header))
			}
			if question != "" {
				b.WriteString(esc(question))
			}
			options, _ := qMap["options"].([]interface{})
			for oi, o := range options {
				oMap, _ := o.(map[string]interface{})
				if oMap == nil {
					continue
				}
				label, _ := oMap["label"].(string)
				desc, _ := oMap["description"].(string)
				if desc != "" {
					fmt.Fprintf(&b, "\n%d. %s — %s", oi+1, esc(label), esc(desc))
				} else {
					fmt.Fprintf(&b, "\n%d. %s", oi+1, esc(label))
				}
			}
		}
	default:
		// Unknown tool: key: value fallback; complex types use JSON
		for k, v := range fields {
			switch v.(type) {
			case string, float64, bool:
				fmt.Fprintf(&b, "%s: %s\n", k, esc(v))
			default:
				jsonBytes, err := json.MarshalIndent(v, "", "  ")
				if err == nil {
					fmt.Fprintf(&b, "%s:\n<pre>%s</pre>\n", k, markdown.EscapeHTML(string(jsonBytes)))
				} else {
					fmt.Fprintf(&b, "%s: %s\n", k, esc(v))
				}
			}
		}
	}
	result := b.String()
	return result
}

// BuildToolResultText formats a tool result for appending to PostToolUse notifications.
// Bash results show stdout and stderr separately; other tools show all fields.
func BuildToolResultText(toolName string, toolResponse json.RawMessage) string {
	if len(toolResponse) == 0 {
		return "✅ Done"
	}
	var strResult string
	if err := json.Unmarshal(toolResponse, &strResult); err == nil {
		return "✅ Result:\n<pre>" + markdown.EscapeHTML(strResult) + "</pre>"
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(toolResponse, &fields); err == nil {
		if toolName == "Bash" {
			var b strings.Builder
			b.WriteString("✅ Result:\n")
			if stdout, ok := fields["stdout"]; ok {
				s := fmt.Sprintf("%v", stdout)
				if s != "" {
					fmt.Fprintf(&b, "<pre>%s</pre>", markdown.EscapeHTML(s))
				}
			}
			if stderr, ok := fields["stderr"]; ok {
				s := fmt.Sprintf("%v", stderr)
				if s != "" {
					fmt.Fprintf(&b, "\nstderr:\n<pre>%s</pre>", markdown.EscapeHTML(s))
				}
			}
			result := b.String()
			if result == "✅ Result:\n" {
				return "✅ Done"
			}
			return result
		}
		if toolName == "AskUserQuestion" {
			var b strings.Builder
			b.WriteString("✅ Result:\n")
			if answers, ok := fields["answers"].(map[string]interface{}); ok {
				for q, a := range answers {
					fmt.Fprintf(&b, "%s → %s\n", markdown.EscapeHTML(fmt.Sprintf("%v", q)), markdown.EscapeHTML(fmt.Sprintf("%v", a)))
				}
			}
			result := b.String()
			if result == "✅ Result:\n" {
				return "✅ Done"
			}
			return result
		}
		var b strings.Builder
		b.WriteString("✅ Result:\n")
		for k, v := range fields {
			switch v.(type) {
			case string, float64, bool:
				fmt.Fprintf(&b, "%s: %s\n", k, markdown.EscapeHTML(fmt.Sprintf("%v", v)))
			default:
				jsonBytes, err := json.MarshalIndent(v, "", "  ")
				if err == nil {
					fmt.Fprintf(&b, "%s:\n<pre>%s</pre>\n", k, markdown.EscapeHTML(string(jsonBytes)))
				} else {
					fmt.Fprintf(&b, "%s: %s\n", k, markdown.EscapeHTML(fmt.Sprintf("%v", v)))
				}
			}
		}
		return b.String()
	}
	return "✅ Result:\n<pre>" + markdown.EscapeHTML(string(toolResponse)) + "</pre>"
}

func BuildQuestionText(data QuestionData) string {
	firstLine := "❓ Question"
	if data.AgentName != "" {
		firstLine = "❓ [" + markdown.EscapeHTML(data.AgentName) + "] Question"
	}
	lines := buildHeader(firstLine, HeaderInfo{
		Project: data.Project, CWD: data.CWD, TmuxTarget: data.TmuxTarget,
		CLICommand: data.CLICommand, ContextUsedPct: data.ContextUsedPct,
		ContextUsedTokens: data.ContextUsedTokens, ContextWindowSize: data.ContextWindowSize,
	})
	if len(data.Questions) > 1 {
		for qIdx, q := range data.Questions {
			multiTag := ""
			if q.MultiSelect {
				multiTag = " (多选)"
			}
			lines = append(lines, "", fmt.Sprintf("<b>Q%d: %s</b>%s", qIdx+1, markdown.EscapeHTML(q.Header), multiTag))
			lines = append(lines, markdown.EscapeHTML(q.Question))
			for i, opt := range q.Options {
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, markdown.EscapeHTML(opt.Label)))
				if opt.Description != "" {
					lines = append(lines, "  → "+markdown.EscapeHTML(opt.Description))
				}
			}
		}
	} else if len(data.Questions) == 1 {
		q := data.Questions[0]
		if q.Header != "" {
			lines = append(lines, "", "📋 "+markdown.EscapeHTML(q.Header))
		}
		lines = append(lines, "", markdown.EscapeHTML(q.Question))
		for i, opt := range q.Options {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, markdown.EscapeHTML(opt.Label)))
			if opt.Description != "" {
				lines = append(lines, "  → "+markdown.EscapeHTML(opt.Description))
			}
		}
	} else {
		if data.Header != "" {
			lines = append(lines, "", "📋 "+markdown.EscapeHTML(data.Header))
		}
		lines = append(lines, "", markdown.EscapeHTML(data.Question))
		for i, opt := range data.Options {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, markdown.EscapeHTML(opt.Label)))
			if opt.Description != "" {
				lines = append(lines, "  → "+markdown.EscapeHTML(opt.Description))
			}
		}
	}
	return strings.Join(lines, "\n")
}

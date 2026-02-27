package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	tele "gopkg.in/telebot.v3"
)

func scanCustomCommands() map[string]customCmd {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	commandsDir := filepath.Join(home, ".claude", "commands")
	result := make(map[string]customCmd)
	filepath.Walk(commandsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(commandsDir, path)
		name := strings.TrimSuffix(rel, ".md")
		// Build CC command name: dir/file → dir:file
		parts := strings.Split(name, string(filepath.Separator))
		ccName := strings.Join(parts, ":")
		// Build TG command name: replace : and - with _
		tgName := strings.ReplaceAll(ccName, ":", "_")
		tgName = strings.ReplaceAll(tgName, "-", "_")
		// Read first line for description
		desc := "Custom command: /" + ccName
		f, err := os.Open(path)
		if err == nil {
			scanner := bufio.NewScanner(f)
			if scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				line = strings.TrimLeft(line, "# ")
				if len(line) > 0 {
					desc = truncateStr(line, 200)
				}
			}
			f.Close()
		}
		result[tgName] = customCmd{desc: desc, ccName: ccName}
		return nil
	})
	return result
}

// splitBody splits body text into chunks fitting within maxRuneLen.
// Tries to split at paragraph boundaries (\n\n), then line boundaries (\n),
// falling back to hard rune-boundary split.
func splitBody(body string, maxRuneLen int) []string {
	runes := []rune(body)
	if len(runes) <= maxRuneLen {
		return []string{body}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxRuneLen {
			chunks = append(chunks, string(runes))
			break
		}
		chunk := string(runes[:maxRuneLen])
		if idx := strings.LastIndex(chunk, "\n\n"); idx > 0 {
			end := len([]rune(chunk[:idx]))
			chunks = append(chunks, string(runes[:end]))
			runes = runes[end+2:]
		} else if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
			end := len([]rune(chunk[:idx]))
			chunks = append(chunks, string(runes[:end]))
			runes = runes[end+1:]
		} else {
			chunks = append(chunks, chunk)
			runes = runes[maxRuneLen:]
		}
	}
	return chunks
}

func readAssistantTexts(transcriptPath string) []string {
	content, err := os.ReadFile(transcriptPath)
	if err != nil {
		return nil
	}
	var texts []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if typ, _ := entry["type"].(string); typ != "assistant" {
			continue
		}
		if model, _ := entry["model"].(string); model == "<synthetic>" {
			continue
		}
		msg, _ := entry["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		contentArr, _ := msg["content"].([]interface{})
		if contentArr == nil {
			continue
		}
		var textParts []string
		for _, c := range contentArr {
			cMap, _ := c.(map[string]interface{})
			if cMap == nil {
				continue
			}
			if cType, _ := cMap["type"].(string); cType == "text" {
				if text, ok := cMap["text"].(string); ok {
					textParts = append(textParts, text)
				}
			}
		}
		if len(textParts) > 0 {
			joined := strings.Join(textParts, "\n")
			if joined != "No response requested." {
				texts = append(texts, joined)
			}
		}
	}
	return texts
}

func processTranscriptUpdates(sessionID, transcriptPath string) string {
	if transcriptPath == "" || sessionID == "" {
		return ""
	}
	lock := sessionCounts.getLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	// Initialize count for unknown sessions (e.g. after bot restart) to avoid sending historical content
	if _, known := sessionCounts.counts[sessionID]; !known {
		texts := readAssistantTexts(transcriptPath)
		sessionCounts.counts[sessionID] = len(texts)
		logger.Debug(fmt.Sprintf("Initialized session count: session=%s count=%d", sessionID, len(texts)))
	}
	time.Sleep(2 * time.Second)
	texts := readAssistantTexts(transcriptPath)
	notified := sessionCounts.counts[sessionID]
	if len(texts) <= notified {
		return ""
	}
	var newTexts []string
	for i := notified; i < len(texts); i++ {
		if strings.TrimSpace(texts[i]) != "" {
			newTexts = append(newTexts, strings.TrimSpace(texts[i]))
		}
	}
	sessionCounts.counts[sessionID] = len(texts)
	return strings.Join(newTexts, "\n\n")
}

func truncateStr(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "..."
	}
	return s
}

const autoCompactPct = 80

func readContextUsage(sessionID string) (usedPct int, usedTokens int, windowSize int, ok bool) {
	path := filepath.Join(os.TempDir(), "tg-cli", "context", sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, false
	}
	var ctx map[string]interface{}
	if err := json.Unmarshal(data, &ctx); err != nil {
		return 0, 0, 0, false
	}
	size, sizeOk := ctx["context_window_size"].(float64)
	if !sizeOk {
		return 0, 0, 0, false
	}
	currentUsage, cuOk := ctx["current_usage"].(map[string]interface{})
	if !cuOk {
		return 0, 0, 0, false
	}
	inputTokens, _ := currentUsage["input_tokens"].(float64)
	cacheCreation, _ := currentUsage["cache_creation_input_tokens"].(float64)
	cacheRead, _ := currentUsage["cache_read_input_tokens"].(float64)
	used := inputTokens + cacheCreation + cacheRead
	effectiveLimit := size * autoCompactPct / 100
	pct := int(used / effectiveLimit * 100)
	return pct, int(used), int(effectiveLimit), true
}

func sendEventNotification(b *tele.Bot, chat *tele.Chat, chatID, sessionID, event, project, cwd, tmuxTarget, body string) {
	nd := notify.NotificationData{
		Event:          event,
		Project:        project,
		CWD:            cwd,
		TmuxTarget:     tmuxTarget,
		ContextUsedPct: -1,
	}
	if usedPct, usedTokens, windowSize, ok := readContextUsage(sessionID); ok {
		nd.ContextUsedPct = usedPct
		nd.ContextUsedTokens = usedTokens
		nd.ContextWindowSize = windowSize
	}
	headerLen := notify.HeaderLen(nd)
	maxBodyRunes := 4000 - headerLen - 100
	chunks := splitBody(body, maxBodyRunes)
	if len(chunks) <= 1 {
		nd.Body = body
		text := notify.BuildNotificationText(nd)
		_, err := b.Send(chat, text)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
		} else {
			logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s body_len=%d body=%s", chatID, event, project, tmuxTarget, len([]rune(body)), truncateStr(body, 200)))
			logger.Info(fmt.Sprintf("TG message sent [%s] full_text:\n%s", event, text))
		}
	} else {
		nd.Body = chunks[0]
		nd.Page = 1
		nd.TotalPages = len(chunks)
		text := notify.BuildNotificationText(nd)
		kb := buildPageKeyboard(1, len(chunks))
		sent, err := b.Send(chat, text, kb)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
		} else {
			pages.store(sent.ID, sessionID, &pageEntry{
				chunks:     chunks,
				event:      event,
				project:    project,
				cwd:        cwd,
				tmuxTarget: tmuxTarget,
				chatID:     chat.ID,
			})
			logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s (%d pages, msg_id=%d) body_len=%d body=%s", chatID, event, project, tmuxTarget, len(chunks), sent.ID, len([]rune(body)), truncateStr(body, 200)))
			logger.Info(fmt.Sprintf("TG message sent [%s] page=1/%d full_text:\n%s", event, len(chunks), text))
		}
	}
}

// buildPageKeyboard returns a ReplyMarkup with ◀️ N/M ▶️ inline buttons.
// Callback data format: p\x00<pageNum> (where pageNum is the 1-based page number as string).
func buildPageKeyboard(currentPage, totalPages int) *tele.ReplyMarkup {
	return buildPageKeyboardWithExtra(currentPage, totalPages, nil)
}

// buildPageKeyboardWithExtra returns page navigation buttons plus optional extra rows
// (e.g. permission Allow/Deny buttons).
func buildPageKeyboardWithExtra(currentPage, totalPages int, extraRows []tele.Row) *tele.ReplyMarkup {
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

// extractTmuxTarget extracts tmux target from notification text.
// If the parsed target has no socket, attempts to restore it from sessionState.
func extractTmuxTarget(text string) (*injector.TmuxTarget, error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "📟 ") {
			raw := strings.TrimPrefix(line, "📟 ")
			target, err := injector.ParseTarget(raw)
			if err != nil {
				return nil, err
			}
			if target.Socket == "" {
				if info := sessionState.findInfoByTarget(target.PaneID); info != nil {
					full, _ := injector.ParseTarget(info.tmuxTarget)
					if full.Socket != "" {
						target.Socket = full.Socket
					}
				}
			}
			return &target, nil
		}
	}
	return nil, fmt.Errorf("no tmux target found")
}

func resolvePermission(msgID int, decision string, suggestionsOverride json.RawMessage) (permDecision, error) {
	d := permDecision{}
	suggestions := suggestionsOverride
	if suggestions == nil {
		suggestions = pendingPerms.getSuggestions(msgID)
	}
	switch {
	case decision == "allow":
		d.Behavior = "allow"
	case decision == "deny":
		d.Behavior = "deny"
	case strings.HasPrefix(decision, "s"):
		idx, err := strconv.Atoi(decision[1:])
		if err != nil {
			return d, fmt.Errorf("invalid suggestion index")
		}
		d.Behavior = "allow"
		var sugArr []json.RawMessage
		json.Unmarshal(suggestions, &sugArr)
		if idx < len(sugArr) {
			d.UpdatedPermissions, _ = json.Marshal([]json.RawMessage{sugArr[idx]})
		}
	default:
		return d, fmt.Errorf("unknown decision: %s", decision)
	}
	if !pendingPerms.resolve(msgID, d) {
		return d, fmt.Errorf("no pending permission for msg_id %d", msgID)
	}
	return d, nil
}

func buildAnswers(entry *toolNotifyEntry) map[string]string {
	answers := make(map[string]string)
	for _, q := range entry.questions {
		if q.multiSelect {
			var selected []string
			for oi := 0; oi < q.numOptions; oi++ {
				if q.selectedOptions[oi] {
					selected = append(selected, q.optionLabels[oi])
				}
			}
			answers[q.questionText] = strings.Join(selected, ", ")
		} else if q.selectedOption >= 0 {
			answers[q.questionText] = q.optionLabels[q.selectedOption]
		}
	}
	return answers
}

func rebuildAskMarkup(entry *toolNotifyEntry) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row

	hasSubmit := len(entry.questions) > 1
	for _, q := range entry.questions {
		if q.multiSelect {
			hasSubmit = true
		}
	}

	if len(entry.questions) == 1 && !entry.questions[0].multiSelect {
		// Single question, single select
		q := entry.questions[0]
		var buttons []tele.Btn
		for i, label := range q.optionLabels {
			displayLabel := label
			if q.selectedOption == i {
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
		for qIdx, q := range entry.questions {
			for optIdx, label := range q.optionLabels {
				displayLabel := label
				if len(entry.questions) > 1 {
					displayLabel = fmt.Sprintf("Q%d: %s", qIdx+1, label)
				}
				if q.multiSelect && q.selectedOptions[optIdx] {
					displayLabel = "✅ " + displayLabel
				} else if !q.multiSelect && q.selectedOption == optIdx {
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

// buildFrozenMarkup creates a frozen version of the inline keyboard markup after user selection.
// Shows selected options with ✅ prefix, no Submit/Chat buttons.
// Buttons remain visible but handler checks resolved flag.
func buildFrozenMarkup(entry *toolNotifyEntry, footer string) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row

	if len(entry.questions) == 1 && !entry.questions[0].multiSelect {
		// Single question, single select - show all options with ✅ on selected
		q := entry.questions[0]
		var buttons []tele.Btn
		for i, label := range q.optionLabels {
			displayLabel := label
			if q.selectedOption == i {
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
		for qIdx, q := range entry.questions {
			for optIdx, label := range q.optionLabels {
				displayLabel := label
				if len(entry.questions) > 1 {
					displayLabel = fmt.Sprintf("Q%d: %s", qIdx+1, label)
				}
				if q.multiSelect && q.selectedOptions[optIdx] {
					displayLabel = "✅ " + displayLabel
				} else if !q.multiSelect && q.selectedOption == optIdx {
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

// parseSuggestionLabels extracts human-readable labels from suggestion JSON
func parseSuggestionLabels(suggestionsRaw json.RawMessage) []string {
	var suggestions []json.RawMessage
	json.Unmarshal(suggestionsRaw, &suggestions)
	var labels []string
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
		var label string
		switch sug.Type {
		case "setMode":
			label = "✅ " + sug.Mode
		case "addDirectories":
			dir := ""
			if len(sug.Directories) > 0 {
				dir = sug.Directories[0]
			}
			label = "✅ Allow dir: " + dir
		default:
			toolName := sug.Tool
			allowPattern := sug.AllowPattern
			if toolName == "" && len(sug.Rules) > 0 {
				toolName = sug.Rules[0].ToolName
				if allowPattern == "" {
					allowPattern = sug.Rules[0].RuleContent
				}
			}
			label = "✅ Always Allow"
			if toolName != "" {
				label += " " + toolName
			}
			if allowPattern != "" && allowPattern != "*" {
				label += " (" + allowPattern + ")"
			}
		}
		labels = append(labels, label)
	}
	return labels
}

// buildFrozenPermMarkup creates frozen markup for PermissionRequest showing the selected decision.
func buildFrozenPermMarkup(selectedDecision string, suggestions []string) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row

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

	for i, sug := range suggestions {
		label := sug
		if selectedDecision == fmt.Sprintf("s%d", i) {
			label = "✅ " + sug
		}
		rows = append(rows, markup.Row(markup.Data(label, "perm", fmt.Sprintf("s%d", i))))
	}

	markup.Inline(rows...)
	return markup
}

func selectToolOption(msgID int, optIdx int) error {
	entry, ok := toolNotifs.get(msgID)
	if !ok {
		return fmt.Errorf("no tool notification for msg_id %d", msgID)
	}
	target, err := injector.ParseTarget(entry.tmuxTarget)
	if err != nil {
		return err
	}
	switch entry.toolName {
	case "AskUserQuestion":
		for i := 0; i < optIdx; i++ {
			if err := injector.SendKeys(target, "Down"); err != nil {
				return err
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		return injector.SendKeys(target, "Enter")
	default:
		return fmt.Errorf("unsupported tool: %s", entry.toolName)
	}
}

// detectPermMode captures pane content and detects the current CC permission mode.
// Returns (mode, rawContent, error). Mode is one of: "default", "plan", "auto", "bypass", "unknown".
func detectPermMode(t injector.TmuxTarget) (string, string, error) {
	content, err := injector.CapturePane(t)
	if err != nil {
		return "", "", err
	}
	// Only check the bottom 5 lines where CC TUI mode indicator appears.
	// Searching full pane causes false positives from conversation content.
	lines := strings.Split(content, "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	bottom := strings.ToLower(strings.Join(lines, "\n"))
	switch {
	case strings.Contains(bottom, "bypass"):
		return "bypass", content, nil
	case strings.Contains(bottom, "plan"):
		return "plan", content, nil
	case strings.Contains(bottom, "accept edits"):
		return "auto", content, nil
	default:
		return "default", content, nil
	}
}

// switchPermMode cycles BTab until the target mode is reached.
// Returns the final mode name or error if target mode is not available.
func switchPermMode(t injector.TmuxTarget, targetMode string) (string, error) {
	startMode, _, err := detectPermMode(t)
	if err != nil {
		return "", fmt.Errorf("detect mode: %w", err)
	}
	if startMode == targetMode {
		return startMode, nil
	}
	for i := 0; i < 10; i++ {
		injector.SendKeys(t, "BTab")
		time.Sleep(500 * time.Millisecond)
		currentMode, _, err := detectPermMode(t)
		if err != nil {
			return "", fmt.Errorf("detect mode after BTab: %w", err)
		}
		if currentMode == targetMode {
			return currentMode, nil
		}
		// If we've cycled back to the starting mode, target is not available
		if i > 0 && currentMode == startMode {
			return "", fmt.Errorf("mode %q not available in BTab cycle (cycled back to %q)", targetMode, startMode)
		}
	}
	return "", fmt.Errorf("failed to reach mode %q after 10 BTab presses", targetMode)
}

// handlePermCommand handles /bot_perm_<cmd> — detects or switches CC permission mode via BTab cycling.
func handlePermCommand(c tele.Context, target injector.TmuxTarget) error {
	cmd := strings.TrimPrefix(c.Message().Text, "/bot_perm_")
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}
	if cmd == "status" {
		mode, content, err := detectPermMode(target)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Detect mode failed: %v", err))
		}
		_ = content
		return c.Reply(fmt.Sprintf("🔐 Current mode: %s", mode))
	}
	// All other values are treated as target mode
	finalMode, err := switchPermMode(target, cmd)
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Switch failed: %v", err))
	}
	return c.Reply(fmt.Sprintf("🔐 Switched to %s mode", finalMode))
}

// handleCaptureCommand handles /bot_capture — captures pane content and replies with it.
func shortenSeparators(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		runes := []rune(trimmed)
		if len(runes) < 10 {
			continue
		}
		sepCount := 0
		for _, r := range runes {
			switch {
			case r >= 0x2500 && r <= 0x257F:
				sepCount++
			case r == '-' || r == '=' || r == '_':
				sepCount++
			}
		}
		if sepCount*100/len(runes) >= 80 {
			firstSep := runes[0]
			lines[i] = string([]rune{firstSep, firstSep, firstSep})
		}
	}
	return strings.Join(lines, "\n")
}

func handleCaptureCommand(c tele.Context, target injector.TmuxTarget) error {
	logger.Debug(fmt.Sprintf("handleCaptureCommand: target=%v", target))
	content, err := injector.CapturePane(target)
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Capture failed: %v", err))
	}
	logger.Debug(fmt.Sprintf("handleCaptureCommand: captured %d bytes", len(content)))
	if content == "" {
		return c.Reply("(empty pane)")
	}
	content = shortenSeparators(content)
	const maxRunes = 4000
	r := []rune(content)
	if len(r) > maxRunes {
		content = "...(truncated)\n\n" + string(r[len(r)-maxRunes:])
	}
	logger.Debug("handleCaptureCommand: sending reply")
	return c.Reply(content)
}

// handleEscapeCommand handles /bot_escape — sends Escape key to interrupt Claude Code.
func handleEscapeCommand(c tele.Context, target injector.TmuxTarget) error {
	if err := injector.SendKeys(target, "Escape"); err != nil {
		return c.Reply(fmt.Sprintf("❌ Escape failed: %v", err))
	}
	return c.Reply("⏹ Escape sent")
}

func getPaneTitle(tmuxTarget string) string {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return ""
	}
	title, err := injector.GetPaneTitle(target)
	if err != nil {
		return ""
	}
	return title
}

func isSessionRunning(tmuxTarget string) bool {
	title := getPaneTitle(tmuxTarget)
	if title == "" {
		return false
	}
	return !strings.HasPrefix(title, "✳")
}

// hookPayload represents the CC payload enriched by hook.go
type hookPayload struct {
	HookEventName   string          `json:"hook_event_name"`
	SessionID       string          `json:"session_id"`
	CWD             string          `json:"cwd"`
	TranscriptPath  string          `json:"transcript_path"`
	ToolName        string          `json:"tool_name"`
	ToolInput       json.RawMessage `json:"tool_input"`
	PermSuggestions json.RawMessage `json:"permission_suggestions"`
	TmuxTarget      string          `json:"tmux_target"`
	Project         string          `json:"project"`
	Source          string          `json:"source"`
}

func parseHookPayload(r *http.Request) (*hookPayload, []byte, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, err
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, body, err
	}
	return &p, body, nil
}

func resolveChat(tmuxTarget, cwd string) (*tele.Chat, string) {
	creds, err := config.LoadCredentials()
	if err == nil {
		if cwd != "" && len(creds.ProjectRouteMap) > 0 {
			if chatID, ok := creds.ProjectRouteMap[cwd]; ok {
				logger.Info(fmt.Sprintf("Route resolved: cwd=%s → chat=%d (project route)", cwd, chatID))
				return &tele.Chat{ID: chatID}, strconv.FormatInt(chatID, 10)
			}
		}
		if tmuxTarget != "" && len(creds.RouteMap) > 0 {
			if chatID, ok := creds.RouteMap[tmuxTarget]; ok {
				logger.Info(fmt.Sprintf("Route resolved: tmux=%s → chat=%d (tmux route)", tmuxTarget, chatID))
				return &tele.Chat{ID: chatID}, strconv.FormatInt(chatID, 10)
			}
		}
	}
	chatID := pairing.GetDefaultChatID()
	if chatID == "" {
		return nil, ""
	}
	chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
	return &tele.Chat{ID: chatIDInt}, chatID
}

// checkSessionAlive checks if a tmux session still exists; cleans up dead sessions.
func checkSessionAlive(tmuxTarget string, bot *tele.Bot) bool {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return false
	}
	if injector.SessionExists(target) {
		return true
	}
	cleanDeadSession(tmuxTarget, bot)
	return false
}

// cleanDeadSession cleans up state and notifies the user when a tmux session dies.
func cleanDeadSession(tmuxTarget string, bot *tele.Bot) {
	paneID := tmuxTarget
	if idx := strings.Index(paneID, "@"); idx != -1 {
		paneID = paneID[:idx]
	}
	if sid, found := sessionState.findByTarget(tmuxTarget); found {
		sessionState.remove(sid)
		pages.cleanupSession(sid)
		sessionCounts.cleanup(sid)
		cleanPendingFilesBySession(sid)
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		return
	}
	if chatID, ok := creds.RouteMap[tmuxTarget]; ok {
		delete(creds.RouteMap, tmuxTarget)
		config.SaveCredentials(creds)
		bot.Send(&tele.Chat{ID: chatID}, fmt.Sprintf("⚠️ Session disconnected\n📟 %s\nTmux route auto-unbound.", paneID))
		logger.Info(fmt.Sprintf("Auto-unbound dead session: tmux=%s chat=%d", tmuxTarget, chatID))
	}
}

// PendingFile represents a pending CC event stored as a file
type PendingFile struct {
	UUID       string          `json:"uuid"`
	Event      string          `json:"event"`
	ToolName   string          `json:"tool_name"`
	Status     string          `json:"status"`
	Payload    json.RawMessage `json:"payload"`
	TgMsgID    int             `json:"tg_msg_id"`
	TgChatID   int64           `json:"tg_chat_id"`
	SessionID  string          `json:"session_id"`
	TmuxTarget string          `json:"tmux_target"`
	CCOutput   json.RawMessage `json:"cc_output"`
	CreatedAt  string          `json:"created_at"`
	HookPID    int             `json:"hook_pid"`
}

// pendingDir returns /tmp/<config-dir-basename>/pending, creating it if needed
func pendingDir() string {
	base := filepath.Base(config.GetConfigDir())
	dir := filepath.Join("/tmp", base, "pending")
	os.MkdirAll(dir, 0755)
	return dir
}

// readPendingFile reads and unmarshals a pending file
func readPendingFile(path string) (*PendingFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pf PendingFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, err
	}
	return &pf, nil
}

// writePendingFile atomically writes a pending file
func writePendingFile(path string, pf *PendingFile) error {
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// writePendingAnswer updates pending file with answer and status=answered
func writePendingAnswer(uuid string, ccOutput json.RawMessage) error {
	path := filepath.Join(pendingDir(), uuid+".json")
	pf, err := readPendingFile(path)
	if err != nil {
		return fmt.Errorf("read pending file: %w", err)
	}
	pf.Status = "answered"
	pf.CCOutput = ccOutput
	return writePendingFile(path, pf)
}

// isHookAlive checks if the hook process with given PID is still running.
func isHookAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// handleStalePending checks if a pending entry is stale (hook dead or file missing).
// Returns true if stale (cleanup done), false if still alive.
func handleStalePending(msgID int, uuid string, bot *tele.Bot) bool {
	path := filepath.Join(pendingDir(), uuid+".json")
	pf, err := readPendingFile(path)
	if err != nil {
		cleanupPendingState(msgID, uuid, bot, "file missing")
		return true
	}
	if pf.Status == "sent" && !isHookAlive(pf.HookPID) {
		os.Remove(path)
		cleanupPendingState(msgID, uuid, bot, fmt.Sprintf("hook dead (pid=%d)", pf.HookPID))
		return true
	}
	return false
}

// cleanupPendingState cleans up bot memory state and freezes TG buttons.
func cleanupPendingState(msgID int, uuid string, bot *tele.Bot, reason string) {
	if entry, ok := toolNotifs.get(msgID); ok && !entry.resolved {
		toolNotifs.markResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
		bot.Edit(editMsg, entry.msgText, buildFrozenMarkup(entry, "❌ Cancelled"))
	}
	if _, ok := pendingPerms.getTarget(msgID); ok {
		pendingPerms.resolve(msgID, permDecision{Behavior: "deny", Message: "Cancelled (hook dead)"})
	}
	pendingFiles.remove(msgID)
	logger.Info(fmt.Sprintf("Stale pending cleanup: msg_id=%d uuid=%s reason=%s", msgID, uuid, reason))
}

// buildAskCCOutput builds CC output for AskUserQuestion
func buildAskCCOutput(payload json.RawMessage, answers map[string]string) json.RawMessage {
	var p map[string]interface{}
	json.Unmarshal(payload, &p)
	toolInput, _ := p["tool_input"].(map[string]interface{})
	questions, _ := toolInput["questions"].([]interface{})
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": "allow",
				"updatedInput": map[string]interface{}{
					"questions": questions,
					"answers":   answers,
				},
			},
		},
	}
	result, _ := json.Marshal(output)
	return result
}

// buildPermCCOutput builds CC output for PermissionRequest
func buildPermCCOutput(decision string, message string, updatedPerms []interface{}) json.RawMessage {
	output := map[string]interface{}{
		"hookSpecificOutput": map[string]interface{}{
			"hookEventName": "PermissionRequest",
			"decision": map[string]interface{}{
				"behavior": decision,
			},
		},
	}
	decisionMap := output["hookSpecificOutput"].(map[string]interface{})["decision"].(map[string]interface{})
	if message != "" {
		decisionMap["message"] = message
	}
	if updatedPerms != nil {
		decisionMap["updatedPermissions"] = updatedPerms
	}
	result, _ := json.Marshal(output)
	return result
}

// scanPendingDir scans pending directory on bot startup to rebuild in-memory state
func scanPendingDir(bot *tele.Bot, creds *config.Credentials) {
	dir := pendingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Debug(fmt.Sprintf("scanPendingDir: skip (dir not readable): %v", err))
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		uuid := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(dir, entry.Name())
		pf, err := readPendingFile(path)
		if err != nil {
			logger.Error(fmt.Sprintf("scanPendingDir: failed to read %s: %v", entry.Name(), err))
			continue
		}
		switch pf.Status {
		case "pending":
			// Bot wasn't running when hook wrote the file — process it now
			logger.Info(fmt.Sprintf("scanPendingDir: processing pending request %s", uuid))
			go processPendingRequest(bot, creds, uuid)
		case "sent":
			// Rebuild in-memory state so button clicks work after restart
			logger.Info(fmt.Sprintf("scanPendingDir: rebuilding in-memory state for %s (status=sent)", uuid))
			if err := rebuildInMemoryState(bot, pf, path); err != nil {
				logger.Error(fmt.Sprintf("scanPendingDir: failed to rebuild state for %s: %v", uuid, err))
			}
		case "answered":
			// Orphaned file — hook should have cleaned it up
			logger.Info(fmt.Sprintf("scanPendingDir: removing orphaned answered file %s", uuid))
			os.Remove(path)
		default:
			logger.Error(fmt.Sprintf("scanPendingDir: unknown status %q in %s", pf.Status, uuid))
		}
	}
}

// sessionListEntry holds metadata for a discovered CC session.
type sessionListEntry struct {
	SessionID     string
	Summary       string
	SummarySource string // "assistant" or "user"
	Modified      time.Time
}

// projectSlug converts an absolute path to a CC project slug by replacing
// all slashes with dashes.
func projectSlug(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// listProjectSessions scans ~/.claude/projects/<slug>/ for session JSONL files,
// returns up to limit entries sorted by mtime descending.
// excludeID is an optional session ID to skip (e.g. the currently active session).
func listProjectSessions(cwd string, limit int, excludeID string) ([]sessionListEntry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".claude", "projects", projectSlug(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type fileInfo struct {
		path    string
		name    string
		modTime time.Time
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			path:    filepath.Join(dir, e.Name()),
			name:    strings.TrimSuffix(e.Name(), ".jsonl"),
			modTime: info.ModTime(),
		})
	}
	// Sort by mtime descending
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j].modTime.After(files[j-1].modTime); j-- {
			files[j], files[j-1] = files[j-1], files[j]
		}
	}
	var result []sessionListEntry
	for _, f := range files {
		if len(result) >= limit {
			break
		}
		if excludeID != "" && f.name == excludeID {
			continue
		}
		summary, source := readLastMeaningfulEntry(f.path, 4000)
		if summary == "" {
			continue
		}
		result = append(result, sessionListEntry{
			SessionID:     f.name,
			Summary:       summary,
			SummarySource: source,
			Modified:      f.modTime,
		})
	}
	return result, nil
}

// readFirstHumanPrompt reads the first human prompt text from a JSONL session file.
// Returns "No prompt" if not found.
func readFirstHumanPrompt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "No prompt"
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var cmdFallback string
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		if lineCount > 20 {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Type   string `json:"type"`
			IsMeta bool   `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Type != "user" || entry.IsMeta {
			continue
		}
		// Try string content first (most common for user prompts)
		var contentStr string
		if json.Unmarshal(entry.Message.Content, &contentStr) == nil && contentStr != "" {
			if !isSystemTagContent(contentStr) {
				return contentStr
			}
			if name := extractCommandName(contentStr); name != "" && cmdFallback == "" {
				cmdFallback = name
			}
			continue
		}
		// Try array content format
		var contentArr []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &contentArr) == nil {
			for _, c := range contentArr {
				if c.Type == "text" && c.Text != "" {
					if !isSystemTagContent(c.Text) {
						return c.Text
					}
					if name := extractCommandName(c.Text); name != "" && cmdFallback == "" {
						cmdFallback = name
					}
				}
			}
		}
	}
	if cmdFallback != "" {
		return cmdFallback
	}
	return "No prompt"
}

// extractCommandName extracts command name from <command-name>...</command-name> tag.
func extractCommandName(content string) string {
	const tag = "<command-name>"
	const endTag = "</command-name>"
	start := strings.Index(content, tag)
	if start == -1 {
		return ""
	}
	start += len(tag)
	end := strings.Index(content[start:], endTag)
	if end == -1 {
		return ""
	}
	return content[start : start+end]
}

// isSystemTagContent checks if a string starts with a known CC system tag prefix.
func isSystemTagContent(s string) bool {
	prefixes := []string{"<local-command-", "<command-", "<task-notification", "<bash-input", "<system-reminder"}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// readLastMeaningfulEntry scans a JSONL transcript file from end to start using
// reverse chunk reading (32KB chunks), returning the first meaningful entry
// (non-synthetic assistant output or non-command user input).
// Returns (text, source) where source is "assistant" or "user", or ("", "") if nothing found.
func readLastMeaningfulEntry(path string, maxLen int) (string, string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", ""
	}
	fileSize := info.Size()
	if fileSize == 0 {
		return "", ""
	}
	const chunkSize = 32 * 1024
	// Remainder carries a partial line from the beginning of the previous chunk
	var remainder []byte
	offset := fileSize
	for offset > 0 {
		readSize := int64(chunkSize)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, offset); err != nil {
			return "", ""
		}
		// Append remainder from previous iteration to end of this chunk
		if len(remainder) > 0 {
			buf = append(buf, remainder...)
			remainder = nil
		}
		// Split into lines; first segment may be partial (carry to next iteration)
		lines := bytes.Split(buf, []byte("\n"))
		// If we haven't reached the start of the file, the first segment is partial
		if offset > 0 {
			remainder = lines[0]
			lines = lines[1:]
		}
		// Process lines from end to start
		for i := len(lines) - 1; i >= 0; i-- {
			line := bytes.TrimSpace(lines[i])
			if len(line) == 0 {
				continue
			}
			var entry struct {
				Type    string `json:"type"`
				IsMeta  bool   `json:"isMeta"`
				Model   string `json:"model"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &entry) != nil {
				continue
			}
			if entry.Type == "assistant" && entry.Model != "<synthetic>" {
				var contentArr []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(entry.Message.Content, &contentArr) == nil {
					var parts []string
					for _, c := range contentArr {
						if c.Type == "text" && c.Text != "" {
							parts = append(parts, c.Text)
						}
					}
					if len(parts) > 0 {
						text := strings.Join(parts, "\n")
						if text == "No response requested." {
							continue
						}
						return truncateStr(text, maxLen), "assistant"
					}
				}
				continue
			}
			if entry.Type == "user" && !entry.IsMeta {
				// Try string content
				var contentStr string
				if json.Unmarshal(entry.Message.Content, &contentStr) == nil && contentStr != "" {
					if !isSystemTagContent(contentStr) {
						return truncateStr(contentStr, maxLen), "user"
					}
					continue
				}
				// Try array content
				var contentArr []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(entry.Message.Content, &contentArr) == nil {
					for _, c := range contentArr {
						if c.Type == "text" && c.Text != "" {
							if !isSystemTagContent(c.Text) {
								return truncateStr(c.Text, maxLen), "user"
							}
						}
					}
				}
			}
		}
	}
	return "", ""
}

// readLastAssistantText reads the last assistant text from a JSONL transcript file.
// Returns empty string if not found. Truncates to maxLen characters.
func readLastAssistantText(path string, maxLen int) string {
	texts := readAssistantTexts(path)
	if len(texts) == 0 {
		return ""
	}
	last := texts[len(texts)-1]
	return truncateStr(last, maxLen)
}

// relativeTime formats a time as a human-readable relative string ("Xm ago", "Xh ago", "Xd ago").
func relativeTime(t time.Time) string {
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

// buildResumeKeyboard builds an inline keyboard with one button per session.
// Button label: "📝 <prompt truncated to 40> • <relativeTime>".
// Callback unique: "resume", data: session ID.
func buildResumeKeyboard(sessions []sessionListEntry) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	for i, s := range sessions {
		label := fmt.Sprintf("%d • %s", i+1, relativeTime(s.Modified))
		rows = append(rows, markup.Row(markup.Data(label, "resume", s.SessionID)))
	}
	markup.Inline(rows...)
	return markup
}

// rebuildInMemoryState reconstructs in-memory maps from a status=sent pending file
func rebuildInMemoryState(bot *tele.Bot, pf *PendingFile, path string) error {
	var p hookPayload
	if err := json.Unmarshal(pf.Payload, &p); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	pf.TmuxTarget = notify.FormatPaneID(pf.TmuxTarget)
	if pf.ToolName == "AskUserQuestion" {
		var askInput struct {
			Questions []struct {
				Header   string `json:"header"`
				Question string `json:"question"`
				Options  []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
				MultiSelect bool `json:"multiSelect"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(p.ToolInput, &askInput); err != nil {
			return fmt.Errorf("unmarshal tool_input: %w", err)
		}
		if len(askInput.Questions) == 0 {
			return fmt.Errorf("no questions in payload")
		}
		var qMetas []questionMeta
		for _, q := range askInput.Questions {
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			qMetas = append(qMetas, questionMeta{
				questionText: q.Question, header: q.Header,
				numOptions: len(q.Options), optionLabels: labels,
				multiSelect: q.MultiSelect, selectedOptions: make(map[int]bool),
				selectedOption: -1,
			})
		}
		var qSummaries []string
		for _, q := range askInput.Questions {
			var labels []string
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			qSummaries = append(qSummaries, fmt.Sprintf("%s:[%s]", q.Header, strings.Join(labels, ",")))
		}
		contentSummary := strings.Join(qSummaries, " | ")
		toolNotifs.store(pf.TgMsgID, &toolNotifyEntry{
			tmuxTarget: pf.TmuxTarget, toolName: "AskUserQuestion",
			questions: qMetas, chatID: pf.TgChatID, msgText: "",
			pendingUUID: pf.UUID,
		})
		pendingFiles.store(pf.TgMsgID, pf.UUID)
		logger.Info(fmt.Sprintf("scanPendingDir: rebuilt AskUserQuestion state: msg_id=%d questions=%d tmux=%s content=%s uuid=%s", pf.TgMsgID, len(askInput.Questions), pf.TmuxTarget, contentSummary, pf.UUID))
		return nil
	}
	// PermissionRequest: rebuild pendingPerms
	var suggestions []json.RawMessage
	json.Unmarshal(p.PermSuggestions, &suggestions)
	suggestionsRaw, _ := json.Marshal(suggestions)
	pendingPerms.create(pf.TgMsgID, pf.TmuxTarget, suggestionsRaw, "", pf.TgChatID, pf.UUID)
	pendingFiles.store(pf.TgMsgID, pf.UUID)
	logger.Info(fmt.Sprintf("scanPendingDir: rebuilt PermissionRequest state: msg_id=%d tool=%s tmux=%s uuid=%s", pf.TgMsgID, pf.ToolName, pf.TmuxTarget, pf.UUID))
	return nil
}

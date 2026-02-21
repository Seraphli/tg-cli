package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
					if len(line) > 200 {
						line = line[:200]
					}
					desc = line
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
			texts = append(texts, strings.Join(textParts, "\n"))
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

func sendEventNotification(b *tele.Bot, chat *tele.Chat, chatID, sessionID, event, project, tmuxTarget, body string) {
	headerLen := notify.HeaderLen(notify.NotificationData{
		Event:      event,
		Project:    project,
		TmuxTarget: tmuxTarget,
	})
	maxBodyRunes := 4000 - headerLen - 100
	chunks := splitBody(body, maxBodyRunes)
	if len(chunks) <= 1 {
		text := notify.BuildNotificationText(notify.NotificationData{
			Event:      event,
			Project:    project,
			Body:       body,
			TmuxTarget: tmuxTarget,
		})
		_, err := b.Send(chat, text)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
		} else {
			logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s body_len=%d body=%s", chatID, event, project, tmuxTarget, len([]rune(body)), truncateStr(body, 200)))
			logger.Info(fmt.Sprintf("TG message sent [%s] full_text:\n%s", event, text))
		}
	} else {
		text := notify.BuildNotificationText(notify.NotificationData{
			Event:      event,
			Project:    project,
			Body:       chunks[0],
			TmuxTarget: tmuxTarget,
			Page:       1,
			TotalPages: len(chunks),
		})
		kb := buildPageKeyboard(1, len(chunks))
		sent, err := b.Send(chat, text, kb)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
		} else {
			pages.store(sent.ID, sessionID, &pageEntry{
				chunks:     chunks,
				event:      event,
				project:    project,
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
func extractTmuxTarget(text string) (*injector.TmuxTarget, error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "📟 ") {
			raw := strings.TrimPrefix(line, "📟 ")
			target, err := injector.ParseTarget(raw)
			if err != nil {
				return nil, err
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
func handleCaptureCommand(c tele.Context, target injector.TmuxTarget) error {
	content, err := injector.CapturePane(target)
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Capture failed: %v", err))
	}
	if content == "" {
		return c.Reply("(empty pane)")
	}
	const maxLen = 4000
	if len(content) > maxLen {
		content = "...(truncated, showing last 4000 chars)\n\n" + content[len(content)-maxLen:]
	}
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

func resolveChat(tmuxTarget string) (*tele.Chat, string) {
	if tmuxTarget != "" {
		creds, err := config.LoadCredentials()
		if err == nil && len(creds.RouteMap) > 0 {
			if chatID, ok := creds.RouteMap[tmuxTarget]; ok {
				logger.Info(fmt.Sprintf("Route resolved: tmux=%s → chat=%d (from routeMap)", tmuxTarget, chatID))
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

// PendingFile represents a pending CC event stored as a file
type PendingFile struct {
	UUID        string          `json:"uuid"`
	Event       string          `json:"event"`
	ToolName    string          `json:"tool_name"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload"`
	TgMsgID     int             `json:"tg_msg_id"`
	TgChatID    int64           `json:"tg_chat_id"`
	SessionID   string          `json:"session_id"`
	TmuxTarget  string          `json:"tmux_target"`
	CCOutput    json.RawMessage `json:"cc_output"`
	CreatedAt   string          `json:"created_at"`
}

// pendingDir returns /tmp/tg-cli/pending/, creating it if needed
func pendingDir() string {
	dir := "/tmp/tg-cli/pending"
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

// rebuildInMemoryState reconstructs in-memory maps from a status=sent pending file
func rebuildInMemoryState(bot *tele.Bot, pf *PendingFile, path string) error {
	var p hookPayload
	if err := json.Unmarshal(pf.Payload, &p); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
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

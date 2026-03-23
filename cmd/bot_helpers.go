package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
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
		// Read description: parse YAML frontmatter if present, else use first line
		desc := "Custom command: /" + ccName
		f, err := os.Open(path)
		if err == nil {
			scanner := bufio.NewScanner(f)
			if scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "---" {
					// YAML frontmatter: scan until closing --- and extract description
					for scanner.Scan() {
						fmLine := strings.TrimSpace(scanner.Text())
						if fmLine == "---" {
							break
						}
						if strings.HasPrefix(fmLine, "description:") {
							desc = truncateStr(strings.TrimSpace(strings.TrimPrefix(fmLine, "description:")), 200)
						}
					}
				} else {
					line = strings.TrimLeft(line, "# ")
					if len(line) > 0 {
						desc = truncateStr(line, 200)
					}
				}
			}
			f.Close()
		}
		result[tgName] = customCmd{desc: desc, ccName: ccName}
		return nil
	})
	return result
}

// htmlTags lists Telegram-supported HTML tags that need open/close tracking.
var htmlTags = []string{"b", "i", "code", "pre", "s", "a", "blockquote"}

// findUnclosedTags returns a list of unclosed tag names (in open order) in s.
func findUnclosedTags(s string) []string {
	var stack []string
	i := 0
	for i < len(s) {
		if s[i] != '<' {
			i++
			continue
		}
		end := strings.Index(s[i:], ">")
		if end < 0 {
			break
		}
		tag := s[i+1 : i+end]
		i += end + 1
		closing := strings.HasPrefix(tag, "/")
		if closing {
			name := strings.ToLower(strings.TrimSpace(tag[1:]))
			for j := len(stack) - 1; j >= 0; j-- {
				if stack[j] == name {
					stack = append(stack[:j], stack[j+1:]...)
					break
				}
			}
		} else {
			// Self-closing or unknown — only track known Telegram tags
			name := strings.ToLower(strings.Fields(tag)[0])
			for _, t := range htmlTags {
				if name == t {
					stack = append(stack, name)
					break
				}
			}
		}
	}
	return stack
}

// closingTags returns closing HTML tags for the given open tag names (reverse order).
func closingTags(open []string) string {
	var b strings.Builder
	for i := len(open) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "</%s>", open[i])
	}
	return b.String()
}

// openingTags returns opening HTML tags for the given tag names.
func openingTags(open []string) string {
	var b strings.Builder
	for _, t := range open {
		fmt.Fprintf(&b, "<%s>", t)
	}
	return b.String()
}

// splitBody splits body text into chunks fitting within maxRuneLen.
// Tries to split at paragraph boundaries (\n\n), then line boundaries (\n),
// falling back to hard rune-boundary split.
// Checks for unclosed HTML tags after each split and appends/prepends closing/opening tags.
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
		var end int
		var skip int
		if idx := strings.LastIndex(chunk, "\n\n"); idx > 0 {
			end = len([]rune(chunk[:idx]))
			skip = 2
		} else if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
			end = len([]rune(chunk[:idx]))
			skip = 1
		} else {
			end = maxRuneLen
			skip = 0
		}
		part := string(runes[:end])
		unclosed := findUnclosedTags(part)
		if len(unclosed) > 0 {
			chunks = append(chunks, part+closingTags(unclosed))
			runes = []rune(openingTags(unclosed) + string(runes[end+skip:]))
		} else {
			chunks = append(chunks, part)
			runes = runes[end+skip:]
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

func processTranscriptUpdates(sessionID, transcriptPath string, isQuestion ...bool) string {
	if transcriptPath == "" || sessionID == "" {
		return ""
	}
	lock := sessionCounts.getLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	// Initialize count for unknown sessions (e.g. after bot restart) to avoid sending historical content
	if _, known := sessionCounts.counts[sessionID]; !known {
		texts := readAssistantTexts(transcriptPath)
		count := len(texts)
		// For AskUserQuestion, backtrack by 1 so the preceding assistant message is included
		if len(isQuestion) > 0 && isQuestion[0] && count > 0 {
			count--
		}
		sessionCounts.counts[sessionID] = count
		logger.Debug(fmt.Sprintf("Initialized session count: session=%s count=%d", sessionID, count))
	}
	notified := sessionCounts.counts[sessionID]
	var texts []string
	for retry := 0; retry < 5; retry++ {
		time.Sleep(2 * time.Second)
		texts = readAssistantTexts(transcriptPath)
		if len(texts) > notified {
			break
		}
	}
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

func sendEventNotification(b *tele.Bot, chat *tele.Chat, chatID, sessionID, event, project, cwd, tmuxTarget, body, toolName, agentName string, topicID int) {
	nd := notify.NotificationData{
		Event:          event,
		Project:        project,
		CWD:            cwd,
		TmuxTarget:     tmuxTarget,
		ToolName:       toolName,
		AgentName:      agentName,
		ContextUsedPct: -1,
	}
	if usedPct, usedTokens, windowSize, ok := readContextUsage(sessionID); ok {
		nd.ContextUsedPct = usedPct
		nd.ContextUsedTokens = usedTokens
		nd.ContextWindowSize = windowSize
	}
	cfg, _ := config.LoadAppConfig()
	// Extract tables for image rendering, convert remaining to Telegram HTML
	var tableImages [][]byte
	parseMode := tele.ModeHTML
	if cfg.NotifyFormat == "raw" {
		// Raw mode: send body as-is without HTML conversion or table image rendering
		parseMode = ""
	} else if event != "ToolUse" {
		tableMode := cfg.TableMode
		if tableMode == "" {
			tableMode = "image"
		}
		if tableMode == "image" {
			tables := markdown.ExtractTableData(body)
			if len(tables) > 0 {
				body = markdown.RemoveTables(body)
				for _, t := range tables {
					img, err := markdown.RenderTableImageChrome(t.Headers, t.Rows)
					if err != nil {
						logger.Info(fmt.Sprintf("Chrome table render failed (falling back to code): %v", err))
						img, err = markdown.RenderTableImage(t.Headers, t.Rows)
						if err != nil {
							logger.Error(fmt.Sprintf("Table image render failed: %v", err))
							continue
						}
					}
					tableImages = append(tableImages, img)
				}
			}
		}
		body = markdown.RenderTelegramHTML(body)
	}
	headerLen := notify.HeaderLen(nd)
	paginationMax := 4000
	if cfg.PaginationMaxRunes > 0 {
		paginationMax = cfg.PaginationMaxRunes
	}
	maxBodyRunes := paginationMax - headerLen - 100
	chunks := splitBody(body, maxBodyRunes)
	var sendOpts []interface{}
	if topicID > 0 {
		sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
	}
	if parseMode != "" {
		sendOpts = append(sendOpts, parseMode)
	}
	if len(chunks) <= 1 {
		nd.Body = body
		text := notify.BuildNotificationText(nd)
		_, err := retrySend(b, chat, text, sendOpts...)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send notification: %v html=%s", err, truncateStr(text, 500)))
		} else {
			logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] tmux=%s body_len=%d body=%s", chatID, event, project, tmuxTarget, len([]rune(body)), truncateStr(body, 200)))
			logger.Debug(fmt.Sprintf("TG message sent [%s] full_text:\n%s", event, text))
		}
	} else {
		nd.Body = chunks[0]
		nd.Page = 1
		nd.TotalPages = len(chunks)
		text := notify.BuildNotificationText(nd)
		kb := buildPageKeyboard(1, len(chunks))
		opts := append([]interface{}{kb}, sendOpts...)
		sent, err := retrySend(b, chat, text, opts...)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send notification: %v html=%s", err, truncateStr(text, 500)))
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
			logger.Debug(fmt.Sprintf("TG message sent [%s] page=1/%d full_text:\n%s", event, len(chunks), text))
		}
	}
	// Send table images as separate Photo messages
	for i, imgBytes := range tableImages {
		photo := &tele.Photo{
			File: tele.FromReader(bytes.NewReader(imgBytes)),
		}
		var photoOpts []interface{}
		if topicID > 0 {
			photoOpts = append(photoOpts, &tele.SendOptions{ThreadID: topicID})
		}
		_, err := retrySend(b, chat, photo, photoOpts...)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send table image %d: %v", i+1, err))
		} else {
			logger.Info(fmt.Sprintf("Table image %d sent to chat %s for event %s", i+1, chatID, event))
		}
	}
}

// retrySend sends a Telegram message with retries.
// On FloodError it waits the RetryAfter duration; on GroupError it auto-migrates chat ID;
// on other errors it retries up to 3 times with backoff.
func retrySend(b *tele.Bot, to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	var msg *tele.Message
	var err error
	attempt := 0
	for {
		msg, err = b.Send(to, what, opts...)
		if err == nil {
			return msg, nil
		}
		attempt++
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			wait := time.Duration(floodErr.RetryAfter) * time.Second
			logger.Info(fmt.Sprintf("FloodError, waiting %v (attempt %d)", wait, attempt))
			time.Sleep(wait)
			continue
		}
		var groupErr tele.GroupError
		if errors.As(err, &groupErr) {
			newID := groupErr.MigratedTo
			if chat, ok := to.(*tele.Chat); ok && newID != 0 {
				logger.Info(fmt.Sprintf("GroupError: migrating chat %d → %d", chat.ID, newID))
				if merr := config.MigrateChat(chat.ID, newID); merr != nil {
					logger.Error(fmt.Sprintf("Auto-migrate failed: %v", merr))
				}
				chat.ID = newID
				continue
			}
		}
		if attempt >= 3 {
			logger.Error(fmt.Sprintf("Send failed after %d attempts: %v", attempt, err))
			return nil, err
		}
		wait := time.Duration(attempt) * time.Second
		logger.Error(fmt.Sprintf("Send failed (attempt %d): %v, retrying in %v", attempt, err, wait))
		time.Sleep(wait)
	}
}

// retryEdit edits a Telegram message with retries.
// On FloodError it waits the RetryAfter duration; on other errors it retries up to 3 times with backoff.
func retryEdit(b *tele.Bot, msg tele.Editable, what interface{}, opts ...interface{}) (*tele.Message, error) {
	var result *tele.Message
	var err error
	attempt := 0
	for {
		result, err = b.Edit(msg, what, opts...)
		if err == nil {
			return result, nil
		}
		attempt++
		var floodErr tele.FloodError
		if errors.As(err, &floodErr) {
			wait := time.Duration(floodErr.RetryAfter) * time.Second
			logger.Info(fmt.Sprintf("FloodError on edit, waiting %v (attempt %d)", wait, attempt))
			time.Sleep(wait)
			continue
		}
		if attempt >= 3 {
			logger.Error(fmt.Sprintf("Edit failed after %d attempts: %v", attempt, err))
			return nil, err
		}
		wait := time.Duration(attempt) * time.Second
		logger.Error(fmt.Sprintf("Edit failed (attempt %d): %v, retrying in %v", attempt, err, wait))
		time.Sleep(wait)
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
	case decision == "sAll":
		d.Behavior = "allow"
		var sugArr []json.RawMessage
		json.Unmarshal(suggestions, &sugArr)
		d.UpdatedPermissions, _ = json.Marshal(sugArr)
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

	if footer != "" {
		rows = append(rows, markup.Row(markup.Data(footer, "tool", "noop")))
		markup.Inline(rows...)
		return markup
	}

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

// parseSuggestionLabel parses suggestions and returns a short button label and a detailed description.
// btnLabel is always "Always Allow" when suggestions exist, otherwise empty.
// description summarizes all suggestions joined by "; ".
func parseSuggestionLabel(suggestionsRaw json.RawMessage) (btnLabel string, description string) {
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

// recordPending records a message for later ✍ reaction when UserPromptSubmit fires.
func recordPending(tmuxTarget string, chatID int64, msgID int) {
	reactionTracker.recordPending(tmuxTarget, chatID, msgID)
}

// buildFrozenPermMarkup creates frozen markup for PermissionRequest showing the selected decision.
func buildFrozenPermMarkup(selectedDecision string, suggestionLabel string) *tele.ReplyMarkup {
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
// Returns (mode, rawContent, error). Mode is one of: "default", "plan", "auto", "bypass", "question".
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
	case strings.Contains(bottom, "options") || strings.Contains(bottom, "answer"):
		// CC is showing an AskUserQuestion dialog
		return "question", content, nil
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
		// Detect current state to give informative error message
		currentMode, _, detectErr := detectPermMode(target)
		if detectErr == nil && currentMode == "question" {
			return c.Reply(fmt.Sprintf("❌ Switch failed: CC is currently in question state (AskUserQuestion dialog). Answer or cancel the question first.\nError: %v", err))
		}
		if detectErr == nil {
			return c.Reply(fmt.Sprintf("❌ Switch failed: current state is '%s'. Error: %v", currentMode, err))
		}
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
	chunks := splitBody(content, maxRunes)
	logger.Debug(fmt.Sprintf("handleCaptureCommand: sending reply (%d chunks)", len(chunks)))
	if len(chunks) == 1 {
		_, err := retrySend(c.Bot(), c.Chat(), chunks[0])
		return err
	}
	lastPage := len(chunks)
	kb := buildPageKeyboard(lastPage, len(chunks))
	text := chunks[lastPage-1] + fmt.Sprintf("\n\n📄 %d/%d", lastPage, len(chunks))
	sent, err := retrySend(c.Bot(), c.Chat(), text, kb)
	if err != nil {
		return err
	}
	pages.store(sent.ID, "", &pageEntry{
		chunks:   chunks,
		permRows: []tele.Row{},
		rawMode:  true,
	})
	return nil
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

// isSessionBusy checks if CC is busy by reading tmux pane title.
// CC shows ✳ prefix when idle, any other prefix when running.
func isSessionBusy(tmuxTarget string) bool {
	return isSessionRunning(tmuxTarget)
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
	Source               string          `json:"source"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
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

func resolveChat(tmuxTarget string) (*tele.Chat, string, int) {
	creds, err := config.LoadCredentials()
	if err == nil && tmuxTarget != "" {
		sid, found := sessionState.findByTarget(tmuxTarget)
		if found {
			info := sessionState.findInfoByTarget(tmuxTarget)
			// Priority 1: name route
			if info != nil && info.name != "" {
				if route, ok := creds.NameRouteMap[info.name]; ok {
					logger.Info(fmt.Sprintf("Route resolved: name=%s → chat=%d topic=%d (name route)", info.name, route.ChatID, route.TopicID))
					return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
				}
			}
			// Priority 2: session ID route
			if route, ok := creds.NameRouteMap[sid]; ok {
				logger.Info(fmt.Sprintf("Route resolved: sessionID=%s → chat=%d topic=%d (session route)", sid[:8], route.ChatID, route.TopicID))
				return &tele.Chat{ID: route.ChatID}, strconv.FormatInt(route.ChatID, 10), route.TopicID
			}
		}
	}
	chatIDStr := pairing.GetDefaultChatID()
	if chatIDStr == "" {
		return nil, "", 0
	}
	chatIDInt, _ := strconv.ParseInt(chatIDStr, 10, 64)
	return &tele.Chat{ID: chatIDInt}, chatIDStr, 0
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

// cleanDeadSession cleans up state for a dead tmux session.
func cleanDeadSession(tmuxTarget string, bot *tele.Bot) {
	if sid, found := sessionState.findByTarget(tmuxTarget); found {
		sessionState.remove(sid)
		pages.cleanupSession(sid)
		sessionCounts.cleanup(sid)
		cleanPendingFilesBySession(sid)
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
	TgMsgText  string          `json:"tg_msg_text"`
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
		retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "❌ Cancelled"), tele.ModeHTML)
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

// doCancelPerm cancels a PermissionRequest: disk write + ESC + resolve + edit TG msg
func doCancelPerm(bot *tele.Bot, msgID int) string {
	sugLabel, _ := parseSuggestionLabel(pendingPerms.getSuggestions(msgID))
	uuid, uuidOk := pendingFiles.get(msgID)
	if uuidOk {
		path := filepath.Join(pendingDir(), uuid+".json")
		pf, err := readPendingFile(path)
		if err == nil {
			pf.Status = "cancelled"
			writePendingFile(path, pf)
		}
	}
	msgText := pendingPerms.getMsgText(msgID)
	chatID := pendingPerms.getChatID(msgID)
	targetPtr, err := extractTmuxTarget(msgText)
	if err == nil && targetPtr != nil {
		injector.SendKeys(*targetPtr, "Escape")
	}
	pendingPerms.resolve(msgID, permDecision{Behavior: "deny", Message: "Cancelled by user (Esc)"})
	if chatID != 0 && msgText != "" {
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		retryEdit(bot, editMsg, msgText, buildFrozenPermMarkup("❌ Cancelled", sugLabel), tele.ModeHTML)
	}
	logger.Info(fmt.Sprintf("Permission cancelled: msg_id=%d uuid=%s", msgID, uuid))
	return uuid
}

// doCancelAsk cancels an AskUserQuestion: disk write + ESC + resolve + edit TG msg
func doCancelAsk(bot *tele.Bot, msgID int) string {
	uuid, uuidOk := pendingFiles.get(msgID)
	if uuidOk {
		path := filepath.Join(pendingDir(), uuid+".json")
		pf, err := readPendingFile(path)
		if err == nil {
			pf.Status = "cancelled"
			writePendingFile(path, pf)
		}
	}
	if entry, ok := toolNotifs.get(msgID); ok {
		targetPtr, err := extractTmuxTarget(entry.msgText)
		if err == nil && targetPtr != nil {
			injector.SendKeys(*targetPtr, "Escape")
		}
		toolNotifs.markResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
		retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "❌ Cancelled"), tele.ModeHTML)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion cancelled: msg_id=%d uuid=%s", msgID, uuid))
	return uuid
}

// doDecidePerm resolves a PermissionRequest: resolve + writePendingAnswer + edit + recordPending
func doDecidePerm(bot *tele.Bot, msgID int, decision string) (*permDecision, error) {
	if permTarget, ok := pendingPerms.getTarget(msgID); ok && permTarget != "" && !checkSessionAlive(permTarget, bot) {
		return nil, fmt.Errorf("session disconnected")
	}
	uuid, uuidOk := pendingPerms.getUUID(msgID)
	if !uuidOk {
		uuid, uuidOk = pendingFiles.get(msgID)
	}
	sugLabel, _ := parseSuggestionLabel(pendingPerms.getSuggestions(msgID))
	msgText := pendingPerms.getMsgText(msgID)
	chatID := pendingPerms.getChatID(msgID)
	d, err := resolvePermission(msgID, decision, nil)
	if err != nil {
		return nil, err
	}
	if uuidOk {
		var updatedPerms []interface{}
		if d.UpdatedPermissions != nil {
			var perms []interface{}
			json.Unmarshal(d.UpdatedPermissions, &perms)
			updatedPerms = perms
		}
		ccOutput := buildPermCCOutput(d.Behavior, d.Message, updatedPerms)
		if err := writePendingAnswer(uuid, ccOutput); err != nil {
			logger.Error(fmt.Sprintf("Failed to write pending answer for perm: %v", err))
		}
	}
	logger.Info(fmt.Sprintf("Permission resolved: msg_id=%d decision=%s uuid=%s", msgID, decision, uuid))
	if chatID != 0 && msgText != "" {
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: chatID}}
		retryEdit(bot, editMsg, msgText, buildFrozenPermMarkup(decision, sugLabel), tele.ModeHTML)
	}
	targetPtr, err2 := extractTmuxTarget(msgText)
	if err2 == nil && targetPtr != nil {
		recordPending(injector.FormatTarget(*targetPtr), chatID, msgID)
	}
	return &d, nil
}

// doRespondAsk responds to AskUserQuestion: handleStalePending + writePendingAnswer + edit + recordPending
func doRespondAsk(bot *tele.Bot, msgID int, answers map[string]string, frozenLabel string) error {
	uuid, ok := pendingFiles.get(msgID)
	if !ok {
		return fmt.Errorf("pending file not found")
	}
	if handleStalePending(msgID, uuid, bot) {
		return fmt.Errorf("hook dead (stale pending)")
	}
	path := filepath.Join(pendingDir(), uuid+".json")
	pf, err := readPendingFile(path)
	if err != nil {
		cleanupPendingState(msgID, uuid, bot, "file missing on respond")
		return fmt.Errorf("question expired")
	}
	ccOutput := buildAskCCOutput(pf.Payload, answers)
	if err := writePendingAnswer(uuid, ccOutput); err != nil {
		logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
		return fmt.Errorf("failed to save answer")
	}
	if entry, entryOk := toolNotifs.get(msgID); entryOk {
		toolNotifs.markResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
		retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, frozenLabel), tele.ModeHTML)
		recordPending(entry.tmuxTarget, entry.chatID, msgID)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion responded: msg_id=%d uuid=%s answers=%v", msgID, uuid, answers))
	return nil
}

// doChatAsk handles chat mode for AskUserQuestion: handleStalePending + write __chat answer + edit
func doChatAsk(bot *tele.Bot, msgID int) error {
	uuid, ok := pendingFiles.get(msgID)
	if !ok {
		return fmt.Errorf("pending file not found")
	}
	if handleStalePending(msgID, uuid, bot) {
		return fmt.Errorf("question expired")
	}
	path := filepath.Join(pendingDir(), uuid+".json")
	pf, err := readPendingFile(path)
	if err != nil {
		cleanupPendingState(msgID, uuid, bot, "file missing on chat button")
		return fmt.Errorf("question expired")
	}
	answers := map[string]string{"__chat": "true"}
	ccOutput := buildAskCCOutput(pf.Payload, answers)
	if err := writePendingAnswer(uuid, ccOutput); err != nil {
		logger.Error(fmt.Sprintf("Failed to write pending answer: %v", err))
		return fmt.Errorf("failed to save answer")
	}
	if entry, entryOk := toolNotifs.get(msgID); entryOk {
		toolNotifs.markResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
		retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "💬 Chat mode selected"), tele.ModeHTML)
		recordPending(entry.tmuxTarget, entry.chatID, msgID)
	}
	logger.Info(fmt.Sprintf("AskUserQuestion chat mode: msg_id=%d uuid=%s", msgID, uuid))
	return nil
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
	// Also scan launch state files for /bot_new crash recovery
	scanLaunchDir(bot)
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
	s := strings.ReplaceAll(cwd, "/", "-")
	return strings.ReplaceAll(s, "_", "-")
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

// listProjectSessionsByDir scans a specific directory for session JSONL files,
// returns up to limit entries sorted by mtime descending.
// excludeID is an optional session ID to skip (e.g. the currently active session).
func listProjectSessionsByDir(dir string, limit int, excludeID string) ([]sessionListEntry, error) {
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
			questions: qMetas, chatID: pf.TgChatID, msgText: pf.TgMsgText,
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
	pendingPerms.create(pf.TgMsgID, pf.TmuxTarget, suggestionsRaw, pf.TgMsgText, pf.TgChatID, pf.UUID)
	pendingFiles.store(pf.TgMsgID, pf.UUID)
	logger.Info(fmt.Sprintf("scanPendingDir: rebuilt PermissionRequest state: msg_id=%d tool=%s tmux=%s uuid=%s", pf.TgMsgID, pf.ToolName, pf.TmuxTarget, pf.UUID))
	return nil
}

// cleanStaleRoutes is a no-op. Routes are permanent — never delete NameRouteMap entries automatically.
// Users manage routes via /bot_bind and /bot_unbind.
func cleanStaleRoutes(bot *tele.Bot) {
	// Routes are permanent — never delete NameRouteMap entries automatically.
	// Users manage routes via /bot_bind and /bot_unbind.
}

// getPaneLabel returns a human-readable label (session:window.pane) for the given tmux target.
func getPaneLabel(tmuxTarget string) string {
	paneID := notify.FormatPaneID(tmuxTarget)
	out, err := exec.Command("tmux", "display-message", "-t", paneID, "-p", "#{session_name}:#{window_name}.#{pane_index}").Output()
	if err != nil {
		return paneID
	}
	return strings.TrimSpace(string(out))
}

// getPaneCWD returns the current working directory of the given tmux pane.
func getPaneCWD(paneID string) string {
	out, err := exec.Command("tmux", "display-message", "-t", paneID, "-p", "#{pane_current_path}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// shouldNotifyTool checks if the user has configured notifications for the given tool name.
func shouldNotifyTool(toolName string) bool {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return false
	}
	if cfg.ToolNotifyEnabled != nil && !*cfg.ToolNotifyEnabled {
		return false
	}
	// Check MCP tools against "MCP" toggle
	if strings.HasPrefix(toolName, "mcp__") {
		toolName = "MCP"
	}
	for _, t := range cfg.ToolNotifyList {
		if t == toolName {
			return true
		}
	}
	return false
}

// usageCacheEntry holds cached usage API response.
type usageCacheEntry struct {
	data      []byte
	fetchedAt time.Time
}

var usageCache *usageCacheEntry

// usageAPIResponse matches the Anthropic OAuth usage API response shape.
type usageAPIResponse struct {
	FiveHour       *usagePeriod    `json:"five_hour"`
	SevenDay       *usagePeriod    `json:"seven_day"`
	SevenDaySonnet *usagePeriod    `json:"seven_day_sonnet"`
	ExtraUsage     *usageExtraData `json:"extra_usage"`
}

type usagePeriod struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type usageExtraData struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *int     `json:"monthly_limit"`
	UsedCredits  *int     `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

// handleUsageCommandAPI fetches CC usage from the Anthropic OAuth API and sends the result to TG.
func handleUsageCommandAPI(c tele.Context, bot *tele.Bot, existingMsg *tele.Message) error {
	var msg *tele.Message
	if existingMsg != nil {
		msg = existingMsg
	} else {
		var err error
		msg, err = retrySend(bot, c.Chat(), "⏳ Fetching CC usage...", tele.ModeHTML)
		if err != nil {
			return err
		}
	}
	formatted, apiErr := fetchUsageFormatted()
	if apiErr != nil {
		logger.Error(fmt.Sprintf("handleUsageCommand: %v", apiErr))
		retryEdit(bot, msg, fmt.Sprintf("❌ %s", apiErr.Error()), tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: usage fetched len=%d", len(formatted)))
	retryEdit(bot, msg, formatted, tele.ModeHTML)
	logger.Info("handleUsageCommand: done")
	return nil
}

// fetchUsageFormatted reads the OAuth token, calls the usage API (with 60s cache), and returns formatted HTML.
func fetchUsageFormatted() (string, error) {
	// Check cache
	cacheFile := filepath.Join(os.TempDir(), "tg-cli", "usage.json")
	if usageCache != nil && time.Since(usageCache.fetchedAt) < 60*time.Second {
		return formatUsageResponse(usageCache.data)
	}
	// Read OAuth token from ~/.claude/.credentials.json
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home dir: %w", err)
	}
	credsPath := filepath.Join(home, ".claude", ".credentials.json")
	credsData, err := os.ReadFile(credsPath)
	if err != nil {
		return "", fmt.Errorf("cannot read credentials file: %w", err)
	}
	var creds map[string]interface{}
	if err := json.Unmarshal(credsData, &creds); err != nil {
		return "", fmt.Errorf("cannot parse credentials: %w", err)
	}
	token, _ := creds["accessToken"].(string)
	if token == "" {
		// Try nested structure: claudeAiOauth.accessToken
		if oauth, ok := creds["claudeAiOauth"].(map[string]interface{}); ok {
			token, _ = oauth["accessToken"].(string)
		}
	}
	if token == "" {
		return "", fmt.Errorf("access token not found in credentials")
	}
	// Call usage API
	req, err := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}
	// Cache to file
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err == nil {
		os.WriteFile(cacheFile, body, 0600)
	}
	usageCache = &usageCacheEntry{data: body, fetchedAt: time.Now()}
	return formatUsageResponse(body)
}

// formatUsageResponse parses the usage API JSON and returns a TG HTML message.
func formatUsageResponse(data []byte) (string, error) {
	var u usageAPIResponse
	if err := json.Unmarshal(data, &u); err != nil {
		return "", fmt.Errorf("parse usage response: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("📊 CC Usage\n")
	if u.FiveHour != nil {
		pct := int(u.FiveHour.Utilization)
		resetTime := parseResetTime(u.FiveHour.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent session: %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if u.SevenDay != nil {
		pct := int(u.SevenDay.Utilization)
		resetTime := parseResetTime(u.SevenDay.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent week (all models): %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if u.SevenDaySonnet != nil {
		pct := int(u.SevenDaySonnet.Utilization)
		resetTime := parseResetTime(u.SevenDaySonnet.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent week (Sonnet only): %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if u.ExtraUsage != nil {
		sb.WriteString("\nExtra usage\n")
		if !u.ExtraUsage.IsEnabled {
			sb.WriteString("Extra usage not enabled • /extra-usage to enable\n")
		} else if u.ExtraUsage.UsedCredits != nil && u.ExtraUsage.MonthlyLimit != nil {
			used := float64(*u.ExtraUsage.UsedCredits) / 100.0
			limit := float64(*u.ExtraUsage.MonthlyLimit) / 100.0
			sb.WriteString(fmt.Sprintf("$%.2f / $%.2f used\n", used, limit))
		}
	}
	result := strings.TrimRight(sb.String(), "\n")
	if result == "📊 CC Usage" {
		return "", fmt.Errorf("no usage data in response")
	}
	return result, nil
}

// parseResetTime formats a reset timestamp for display, always including day and timezone.
func parseResetTime(ts string, includeDay bool) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Try without timezone suffix
		t, err = time.Parse("2006-01-02T15:04:05Z", ts)
		if err != nil {
			return ts
		}
	}
	t = t.Local()
	tz := getIANATimezone()
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("3pm") + " (" + tz + ")"
	}
	return t.Format("Jan 2, 3pm") + " (" + tz + ")"
}

// getIANATimezone returns the IANA timezone name (e.g., "Asia/Shanghai").
func getIANATimezone() string {
	link, err := os.Readlink("/etc/localtime")
	if err == nil {
		if idx := strings.Index(link, "zoneinfo/"); idx >= 0 {
			return link[idx+len("zoneinfo/"):]
		}
	}
	zone, _ := time.Now().Zone()
	return zone
}

// waitForPaneContent polls the tmux pane until the needle string appears or timeout is reached.
func waitForPaneContent(target injector.TmuxTarget, needle string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		content, err := injector.CapturePane(target)
		if err == nil && strings.Contains(content, needle) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// handleUsageCommandTmux launches a temporary CC session, runs /usage, and sends the result to TG.
func handleUsageCommandTmux(c tele.Context, bot *tele.Bot, existingMsg *tele.Message) error {
	var msg *tele.Message
	if existingMsg != nil {
		msg = existingMsg
	} else {
		var err error
		msg, err = retrySend(bot, c.Chat(), "⏳ Fetching CC usage...", tele.ModeHTML)
		if err != nil {
			return err
		}
	}
	sessionName := fmt.Sprintf("tg-cli-usage-%d", time.Now().UnixMilli())
	configDir := config.GetConfigDir()
	cmd := exec.Command("tmux", "new-session", "-d", "-s", sessionName, "-c", configDir, "-x", "120", "-y", "40")
	if err := cmd.Run(); err != nil {
		logger.Error(fmt.Sprintf("handleUsageCommand: failed to create temp session err=%v", err))
		retryEdit(bot, msg, "❌ Failed to create temp session", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: temp session created session=%s", sessionName))
	defer exec.Command("tmux", "kill-session", "-t", sessionName).Run()
	target, _ := injector.ParseTarget(sessionName)
	logger.Info(fmt.Sprintf("handleUsageCommand: starting claude session=%s", sessionName))
	injector.SendKeys(target, "claude", "Enter")
	if !waitForPaneContent(target, "❯", 30*time.Second) {
		logger.Error("handleUsageCommand: CC failed to initialize (timeout waiting for ❯)")
		retryEdit(bot, msg, "❌ CC failed to initialize", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: CC ready session=%s", sessionName))
	logger.Info("handleUsageCommand: injecting /usage")
	injector.SendKeys(target, "/usage", "Enter")
	if !waitForPaneContent(target, "used", 10*time.Second) {
		logger.Error("handleUsageCommand: failed to get usage data (timeout waiting for 'used')")
		retryEdit(bot, msg, "❌ Failed to get usage data", tele.ModeHTML)
		return nil
	}
	logger.Info("handleUsageCommand: usage output detected")
	time.Sleep(1 * time.Second)
	content, err := injector.CapturePane(target)
	if err != nil {
		logger.Error(fmt.Sprintf("handleUsageCommand: failed to capture pane err=%v", err))
		retryEdit(bot, msg, "❌ Failed to capture usage data", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: pane captured len=%d", len(content)))
	formatted := parseUsageOutput(content)
	if formatted == "" {
		logger.Error("handleUsageCommand: failed to parse usage data (empty result)")
		retryEdit(bot, msg, "❌ Failed to parse usage data", tele.ModeHTML)
		return nil
	}
	logger.Info(fmt.Sprintf("handleUsageCommand: parsed output len=%d", len(formatted)))
	formatted = markdown.EscapeHTML(formatted)
	chunks := splitBody(formatted, 4000)
	if len(chunks) <= 1 {
		retryEdit(bot, msg, formatted, tele.ModeHTML)
	} else {
		kb := buildPageKeyboard(1, len(chunks))
		retryEdit(bot, msg, chunks[0]+fmt.Sprintf("\n\n📄 1/%d", len(chunks)), kb, tele.ModeHTML)
		pages.store(msg.ID, "", &pageEntry{chunks: chunks, permRows: []tele.Row{}})
	}
	logger.Info("handleUsageCommand: done")
	return nil
}

// parseUsageOutput extracts relevant usage lines from raw CC pane output.
func parseUsageOutput(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string
	result = append(result, "📊 CC Usage\n")
	var currentSection string
	re := regexp.MustCompile(`(\d+%\s+used)`)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Settings:") ||
			trimmed == "Esc to cancel" || strings.HasPrefix(trimmed, "❯") ||
			strings.HasPrefix(trimmed, "╭") || strings.HasPrefix(trimmed, "╰") {
			continue
		}
		if !strings.Contains(trimmed, "% used") && !strings.HasPrefix(trimmed, "Resets") &&
			!strings.HasPrefix(trimmed, "█") && !strings.HasPrefix(trimmed, "Extra") &&
			!strings.HasPrefix(trimmed, "▌") && len(trimmed) > 5 {
			if strings.HasPrefix(trimmed, "Current") || strings.HasPrefix(trimmed, "Extra") {
				currentSection = trimmed
			}
			continue
		}
		if strings.Contains(trimmed, "% used") {
			if m := re.FindString(trimmed); m != "" {
				if currentSection != "" {
					result = append(result, fmt.Sprintf("%s: %s", currentSection, m))
				}
			}
		}
		if strings.HasPrefix(trimmed, "Resets") {
			result = append(result, fmt.Sprintf("⏰ %s\n", trimmed))
		}
		if strings.HasPrefix(trimmed, "Extra usage") {
			result = append(result, trimmed)
		}
	}
	return strings.Join(result, "\n")
}

// handleUsageCommand shows the current usage source and inline buttons to switch and fetch.
func handleUsageCommand(c tele.Context, bot *tele.Bot) error {
	sel := &tele.ReplyMarkup{}
	sel.Inline(sel.Row(
		sel.Data("📟 tmux", "usage_src", "tmux"),
		sel.Data("🌐 api", "usage_src", "api"),
	))
	_, err := retrySend(bot, c.Chat(), "📊 Select usage source:", sel, tele.ModeHTML)
	return err
}

func buildVoiceText(cfg config.AppConfig) string {
	engine := cfg.VoiceEngine
	if engine == "" {
		engine = "whisper"
	}
	lang := cfg.Language
	if lang == "" {
		lang = "auto"
	}
	text := fmt.Sprintf("🎙 Voice Settings\nEngine: %s", engine)
	if engine == "whisper" {
		model := currentWhisperModelName()
		if model == "" {
			model = "none"
		}
		text += fmt.Sprintf("\nModel: %s", model)
	}
	text += fmt.Sprintf("\nLanguage: %s", lang)
	return text
}

func buildVoiceMenu(engine string) *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	btnWhisper := menu.Data("🔊 whisper", "voice", "engine:whisper")
	btnSenseVoice := menu.Data("🔊 sensevoice", "voice", "engine:sensevoice")
	btnLangAuto := menu.Data("🌐 auto", "voice", "lang:auto")
	btnLangZh := menu.Data("🇨🇳 zh", "voice", "lang:zh")
	btnLangEn := menu.Data("🇺🇸 en", "voice", "lang:en")
	btnLangJa := menu.Data("🇯🇵 ja", "voice", "lang:ja")
	rows := []tele.Row{
		menu.Row(btnWhisper, btnSenseVoice),
	}
	if engine == "" || engine == "whisper" {
		rows = append(rows,
			menu.Row(
				menu.Data("tiny", "voice", "model:tiny"),
				menu.Data("base", "voice", "model:base"),
				menu.Data("small", "voice", "model:small"),
			),
			menu.Row(
				menu.Data("medium", "voice", "model:medium"),
				menu.Data("turbo", "voice", "model:large-v3-turbo"),
				menu.Data("large", "voice", "model:large-v3"),
			),
		)
	}
	rows = append(rows, menu.Row(btnLangAuto, btnLangZh, btnLangEn, btnLangJa))
	menu.Inline(rows...)
	return menu
}


// flushInjectQueue merges all queued items for a target and injects as one combined message.
// Handles three states: idle → inject, AskQ → answer, PermReq → skip (keep queue).
func flushInjectQueue(bot *tele.Bot, tmuxTarget string) {
	if !injectQueue.hasItems(tmuxTarget) {
		return
	}
	// Check PermissionRequest — if pending, do NOT flush (keep queue for later)
	if _, ok := pendingPerms.findByTmuxTarget(tmuxTarget); ok {
		logger.Info(fmt.Sprintf("flushInjectQueue: PermissionRequest pending, keeping queue for target=%s", tmuxTarget))
		return
	}
	// Capture notify message ID and inject ID before flush clears them
	notifyMsgID, hasNotify := injectQueue.getNotifyMsg(tmuxTarget)
	injectID := injectQueue.getInjectID(tmuxTarget)
	items := injectQueue.flush(tmuxTarget)
	if len(items) == 0 {
		return
	}
	// Merge all items into one text
	var texts []string
	for _, item := range items {
		texts = append(texts, item.Text)
	}
	merged := strings.Join(texts, "\n")
	logger.Info(fmt.Sprintf("flushInjectQueue: merging %d items for target=%s merged_len=%d", len(items), tmuxTarget, len(merged)))
	// Resolve chat for TG notification updates
	chat, _, topicID := resolveChat(tmuxTarget)
	if injectID == "" {
		injectID = fmt.Sprintf("%x", time.Now().UnixNano()%0xFFFFFF)
	}
	logger.Info(fmt.Sprintf("flushInjectQueue: [%s] starting flush for target=%s items=%d", injectID, tmuxTarget, len(items)))
	// Build message list for notifications (with delimiters)
	msgContent := "──────\n" + strings.Join(texts, "\n") + "\n──────"
	// Inject the merged text in a goroutine to avoid blocking the hook handler
	go func(target, text, id, msgList string, itemCount int, notifyID int, hasNotifyMsg bool, chat *tele.Chat, topicID int) {
		time.Sleep(3 * time.Second)
		// Ensure hook state is idle before injecting (Stop already set it, but be explicit)
		hookRunningState.setIdle(target)
		// Register confirmation channel BEFORE inject to avoid race with UserPromptSubmit
		ch := injectConfirm.register(target)
		if err := safeInjectText(bot, target, text); err != nil {
			logger.Error(fmt.Sprintf("flushInjectQueue: [%s] inject failed: target=%s err=%v", id, target, err))
			injectConfirm.cancel(target)
			if hasNotifyMsg && chat != nil {
				editMsg := &tele.Message{ID: notifyID, Chat: chat}
				retryEdit(bot, editMsg, fmt.Sprintf("❌ Inject failed [%s] (%d)\n📟 %s\n%s", id, itemCount, notify.FormatPaneID(target), msgList))
			}
			return
		}
		// Wait for UserPromptSubmit confirmation
		select {
		case <-ch:
			logger.Info(fmt.Sprintf("flushInjectQueue: [%s] inject confirmed: target=%s", id, target))
			// Update TG notification to show success
			if hasNotifyMsg && chat != nil {
				editMsg := &tele.Message{ID: notifyID, Chat: chat}
				retryEdit(bot, editMsg, fmt.Sprintf("✅ Injected [%s] (%d)\n📟 %s\n%s", id, itemCount, notify.FormatPaneID(target), msgList))
			}
		case <-time.After(30 * time.Second):
			logger.Error(fmt.Sprintf("flushInjectQueue: [%s] inject verification timeout: target=%s", id, target))
			injectConfirm.cancel(target)
			// Still mark as injected (text was sent), but warn about no confirmation
			if hasNotifyMsg && chat != nil {
				editMsg := &tele.Message{ID: notifyID, Chat: chat}
				retryEdit(bot, editMsg, fmt.Sprintf("⚠️ Injected [%s] (%d) — no confirmation\n📟 %s\n%s", id, itemCount, notify.FormatPaneID(target), msgList))
			}
		}
	}(tmuxTarget, merged, injectID, msgContent, len(items), notifyMsgID, hasNotify, chat, topicID)
}

// safeInjectText checks for pending AskUserQuestion/PermissionRequest on the target pane.
// If AskUserQuestion is pending, answers it with the text and returns. Otherwise injects text directly.
func safeInjectText(bot *tele.Bot, tmuxTarget string, text string) error {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return err
	}
	// PRE-INJECT: check if CC is busy (hook-based + pane title fallback). If busy, queue and return.
	if isSessionBusy(tmuxTarget) {
		// Check if there's a pending AskUserQuestion — if so, answer it directly (don't queue)
		if _, _, ok := toolNotifs.findByTmuxTarget(tmuxTarget); ok {
			// Fall through to AskUserQuestion handling below
		} else {
			chat, chatIDStr, topicID := resolveChat(tmuxTarget)
			chatIDInt, _ := strconv.ParseInt(chatIDStr, 10, 64)
			injectQueue.enqueue(tmuxTarget, injectItem{Text: text, ChatID: chatIDInt, TopicID: topicID})
			count := injectQueue.itemCount(tmuxTarget)
			logger.Info(fmt.Sprintf("safeInjectText: CC busy, queued for target=%s count=%d text=%s", tmuxTarget, count, truncateStr(text, 200)))
			if chat != nil {
				// Show all queued messages with inject ID and delimiters
				allTexts := injectQueue.getTexts(tmuxTarget)
				queueID := injectQueue.getInjectID(tmuxTarget)
				notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n──────\n%s\n──────", queueID, count, notify.FormatPaneID(tmuxTarget), strings.Join(allTexts, "\n"))
				var sendOpts []interface{}
				if topicID > 0 {
					sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
				}
				if existingMsgID, ok := injectQueue.getNotifyMsg(tmuxTarget); ok {
					editMsg := &tele.Message{ID: existingMsgID, Chat: chat}
					retryEdit(bot, editMsg, notifyText)
				} else {
					sent, _ := retrySend(bot, chat, notifyText, sendOpts...)
					if sent != nil {
						injectQueue.setNotifyMsg(tmuxTarget, sent.ID)
					}
				}
			}
			return nil
		}
	}
	// Answer pending AskUserQuestion with the text (same as normal custom reply)
	for {
		msgID, entry, ok := toolNotifs.findByTmuxTarget(tmuxTarget)
		if !ok {
			break
		}
		uuid, uuidOk := pendingFiles.get(msgID)
		if !uuidOk {
			toolNotifs.markResolved(msgID)
			continue
		}
		if handleStalePending(msgID, uuid, bot) {
			continue
		}
		path := filepath.Join(pendingDir(), uuid+".json")
		pf, pfErr := readPendingFile(path)
		if pfErr != nil {
			toolNotifs.markResolved(msgID)
			continue
		}
		answers := make(map[string]string)
		if len(entry.questions) > 0 {
			answers[entry.questions[0].questionText] = text
		}
		ccOutput := buildAskCCOutput(pf.Payload, answers)
		if writeErr := writePendingAnswer(uuid, ccOutput); writeErr != nil {
			logger.Error(fmt.Sprintf("safeInjectText: failed to write answer: %v", writeErr))
		}
		toolNotifs.markResolved(msgID)
		editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: entry.chatID}}
		retryEdit(bot, editMsg, entry.msgText, buildFrozenMarkup(entry, "✅ Custom reply"), tele.ModeHTML)
		logger.Info(fmt.Sprintf("safeInjectText: answered AskUserQuestion msg_id=%d uuid=%s text=%s", msgID, uuid, truncateStr(text, 200)))
		return nil
	}
	// PermissionRequest pending — queue instead of injecting
	if _, ok := pendingPerms.findByTmuxTarget(tmuxTarget); ok {
		chat, chatIDStr, topicID := resolveChat(tmuxTarget)
		chatIDInt, _ := strconv.ParseInt(chatIDStr, 10, 64)
		injectQueue.enqueue(tmuxTarget, injectItem{Text: text, ChatID: chatIDInt, TopicID: topicID})
		count := injectQueue.itemCount(tmuxTarget)
		logger.Info(fmt.Sprintf("safeInjectText: PermissionRequest pending, queued for target=%s count=%d text=%s", tmuxTarget, count, truncateStr(text, 200)))
		if chat != nil {
			allTexts := injectQueue.getTexts(tmuxTarget)
			queueID := injectQueue.getInjectID(tmuxTarget)
			notifyText := fmt.Sprintf("⏳ Queued [%s] (%d)\n📟 %s\n🔒 PermissionRequest pending\n──────\n%s\n──────", queueID, count, notify.FormatPaneID(tmuxTarget), strings.Join(allTexts, "\n"))
			var sendOpts []interface{}
			if topicID > 0 {
				sendOpts = append(sendOpts, &tele.SendOptions{ThreadID: topicID})
			}
			if existingMsgID, ok := injectQueue.getNotifyMsg(tmuxTarget); ok {
				editMsg := &tele.Message{ID: existingMsgID, Chat: chat}
				retryEdit(bot, editMsg, notifyText)
			} else {
				sent, _ := retrySend(bot, chat, notifyText, sendOpts...)
				if sent != nil {
					injectQueue.setNotifyMsg(tmuxTarget, sent.ID)
				}
			}
		}
		return nil
	}
	return injector.InjectText(target, text)
}

// handleVoiceCommand handles /bot_voice — shows voice config and allows engine/language switch.
func handleVoiceCommand(c tele.Context) error {
	cfg, err := config.LoadAppConfig()
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
	}
	engine := cfg.VoiceEngine
	if engine == "" {
		engine = "whisper"
	}
	text := buildVoiceText(cfg)
	menu := buildVoiceMenu(engine)
	_, err = retrySend(c.Bot(), c.Chat(), text, menu, tele.ModeHTML)
	return err
}

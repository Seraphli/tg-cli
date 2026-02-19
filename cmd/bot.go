package cmd

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/notify"
	"github.com/Seraphli/tg-cli/internal/pairing"
	"github.com/Seraphli/tg-cli/internal/voice"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	tele "gopkg.in/telebot.v3"
)

var BotCmd = &cobra.Command{
	Use:   "bot",
	Short: "Start the Telegram bot with hook HTTP server",
	Run:   runBot,
}

var Version string

var (
	debugFlag bool
	portFlag  int
)

func init() {
	BotCmd.Flags().BoolVar(&debugFlag, "debug", false, "Enable debug mode")
	BotCmd.Flags().IntVar(&portFlag, "port", 0, "HTTP server port (overrides config)")
}

type customCmd struct {
	desc   string
	ccName string
}

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

type pageCacheStore struct {
	mu       sync.RWMutex
	entries  map[int]*pageEntry
	sessions map[string][]int // sessionID → []messageID
}

type pageEntry struct {
	chunks     []string
	event      string
	project    string
	tmuxTarget string
	permRows   []tele.Row // non-nil for permission messages
	chatID     int64
}

var pages = &pageCacheStore{
	entries:  make(map[int]*pageEntry),
	sessions: make(map[string][]int),
}


var ccBuiltinCommands = map[string]string{
	"clear":          "Clear conversation history",
	"compact":        "Compact conversation",
	"config":         "Open config",
	"context":        "Visualize context usage",
	"copy":           "Copy last response to clipboard",
	"cost":           "Show token usage stats",
	"debug":          "Debug current session",
	"doctor":         "Check installation health",
	"exit":           "Exit REPL",
	"export":         "Export conversation to file",
	"fast":           "Toggle fast mode",
	"help":           "Show help",
	"init":           "Initialize project CLAUDE.md",
	"mcp":            "Manage MCP servers",
	"memory":         "Edit CLAUDE.md memory",
	"model":          "Switch AI model",
	"permissions":    "View/update permissions",
	"plan":           "Enter plan mode",
	"rename":         "Rename current session",
	"resume":         "Resume a conversation",
	"rewind":         "Rewind conversation",
	"stats":          "Show usage stats",
	"status":         "Show status",
	"statusline":     "Configure status line",
	"tasks":          "List background tasks",
	"teleport":       "Resume remote session",
	"theme":          "Change color theme",
	"todos":          "List TODO items",
	"usage":          "Show plan usage limits",
	"vim":            "Toggle vim mode",
	"terminal_setup": "Configure terminal",
}

func (pc *pageCacheStore) store(msgID int, sessionID string, entry *pageEntry) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.entries[msgID] = entry
	if sessionID != "" {
		pc.sessions[sessionID] = append(pc.sessions[sessionID], msgID)
	}
}

func (pc *pageCacheStore) get(msgID int) (*pageEntry, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	e, ok := pc.entries[msgID]
	return e, ok
}

func (pc *pageCacheStore) cleanupSession(sessionID string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for _, msgID := range pc.sessions[sessionID] {
		delete(pc.entries, msgID)
	}
	delete(pc.sessions, sessionID)
}

type permDecision struct {
	Behavior           string          `json:"behavior"`
	Message            string          `json:"message,omitempty"`
	UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
}

type pendingPermStore struct {
	mu          sync.RWMutex
	entries     map[int]chan permDecision
	targets     map[int]string
	suggestions map[int]json.RawMessage
	msgTexts    map[int]string
	chatIDs     map[int]int64
}

var pendingPerms = &pendingPermStore{
	entries:     make(map[int]chan permDecision),
	targets:     make(map[int]string),
	suggestions: make(map[int]json.RawMessage),
	msgTexts:    make(map[int]string),
	chatIDs:     make(map[int]int64),
}

func (ps *pendingPermStore) create(msgID int, tmuxTarget string, suggestionsJSON json.RawMessage, msgText string, chatID int64) chan permDecision {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ch := make(chan permDecision, 1)
	ps.entries[msgID] = ch
	ps.targets[msgID] = tmuxTarget
	ps.suggestions[msgID] = suggestionsJSON
	ps.msgTexts[msgID] = msgText
	ps.chatIDs[msgID] = chatID
	return ch
}

func (ps *pendingPermStore) resolve(msgID int, d permDecision) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ch, ok := ps.entries[msgID]
	if !ok {
		return false
	}
	ch <- d
	delete(ps.entries, msgID)
	return true
}

func (ps *pendingPermStore) getTarget(msgID int) (string, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	t, ok := ps.targets[msgID]
	return t, ok
}

func (ps *pendingPermStore) getSuggestions(msgID int) json.RawMessage {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.suggestions[msgID]
}

func (ps *pendingPermStore) getMsgText(msgID int) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.msgTexts[msgID]
}

func (ps *pendingPermStore) getChatID(msgID int) int64 {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.chatIDs[msgID]
}

func (ps *pendingPermStore) cleanup(msgID int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.entries, msgID)
	delete(ps.targets, msgID)
	delete(ps.suggestions, msgID)
	delete(ps.msgTexts, msgID)
	delete(ps.chatIDs, msgID)
}

type questionMeta struct {
	questionText    string
	header          string
	numOptions      int
	optionLabels    []string
	multiSelect     bool
	selectedOptions map[int]bool
	selectedOption  int
}

type toolNotifyEntry struct {
	tmuxTarget string
	toolName   string
	questions  []questionMeta
	chatID     int64
	msgText    string
}

type toolNotifyStore struct {
	mu      sync.RWMutex
	entries map[int]*toolNotifyEntry
}

var toolNotifs = &toolNotifyStore{
	entries: make(map[int]*toolNotifyEntry),
}

type pendingAskEntry struct {
	ch chan map[string]string
}

type pendingAskStore struct {
	mu      sync.Mutex
	entries map[int]*pendingAskEntry
}

var pendingAsks = &pendingAskStore{entries: make(map[int]*pendingAskEntry)}

func (s *pendingAskStore) create(msgID int) chan map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan map[string]string, 1)
	s.entries[msgID] = &pendingAskEntry{ch: ch}
	return ch
}

func (s *pendingAskStore) resolve(msgID int, answers map[string]string) bool {
	s.mu.Lock()
	entry, ok := s.entries[msgID]
	delete(s.entries, msgID)
	s.mu.Unlock()
	if !ok {
		return false
	}
	entry.ch <- answers
	return true
}

func (s *pendingAskStore) cleanup(msgID int) {
	s.mu.Lock()
	delete(s.entries, msgID)
	s.mu.Unlock()
}

type sessionCountStore struct {
	mu     sync.Mutex
	counts map[string]int
	locks  map[string]*sync.Mutex
}

var sessionCounts = &sessionCountStore{
	counts: make(map[string]int),
	locks:  make(map[string]*sync.Mutex),
}

type reactionEntry struct {
	chatID int64
	msgID  int
}

type reactionTrackerStore struct {
	mu      sync.Mutex
	entries map[string][]reactionEntry
}

var reactionTracker = &reactionTrackerStore{
	entries: make(map[string][]reactionEntry),
}

func (s *sessionCountStore) getLock(sessionID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[sessionID] == nil {
		s.locks[sessionID] = &sync.Mutex{}
	}
	return s.locks[sessionID]
}

func (s *sessionCountStore) cleanup(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counts, sessionID)
	delete(s.locks, sessionID)
}

func (rt *reactionTrackerStore) record(tmuxTarget string, chatID int64, msgID int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.entries[tmuxTarget] = append(rt.entries[tmuxTarget], reactionEntry{chatID: chatID, msgID: msgID})
	logger.Debug(fmt.Sprintf("Reaction recorded: target=%s msg_id=%d", tmuxTarget, msgID))
}

func (rt *reactionTrackerStore) clearAndRemove(bot *tele.Bot, tmuxTarget string) {
	rt.mu.Lock()
	rEntries := rt.entries[tmuxTarget]
	delete(rt.entries, tmuxTarget)
	rt.mu.Unlock()
	if len(rEntries) > 0 {
		logger.Debug(fmt.Sprintf("Clearing %d reactions for target %s", len(rEntries), tmuxTarget))
	}
	for _, e := range rEntries {
		bot.Raw("setMessageReaction", map[string]interface{}{
			"chat_id":    e.chatID,
			"message_id": e.msgID,
			"reaction":   []interface{}{},
		})
	}
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

func (ts *toolNotifyStore) store(msgID int, entry *toolNotifyEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.entries[msgID] = entry
}

func (ts *toolNotifyStore) get(msgID int) (*toolNotifyEntry, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	e, ok := ts.entries[msgID]
	return e, ok
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

func runBot(cmd *cobra.Command, args []string) {
	if debugFlag {
		logger.SetDebugMode(true)
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load credentials: %v\n", err)
		os.Exit(1)
	}
	if creds.BotToken == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprintln(os.Stderr, "Bot token not found. Run interactively or set botToken in ~/.tg-cli/credentials.json")
			os.Exit(1)
		}
		fmt.Print("Enter your Telegram bot token (from @BotFather): ")
		reader := bufio.NewReader(os.Stdin)
		token, _ := reader.ReadString('\n')
		token = strings.TrimSpace(token)
		if token == "" {
			fmt.Fprintln(os.Stderr, "No token provided.")
			os.Exit(1)
		}
		creds.BotToken = token
		if err := config.SaveCredentials(creds); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save credentials: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Bot token saved.")
	}
	port := portFlag
	if port == 0 {
		port = creds.Port
	}
	if port == 0 {
		port = 12500
	}
	pref := tele.Settings{
		Token:  creds.BotToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	bot, err := tele.NewBot(pref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create bot: %v\n", err)
		os.Exit(1)
	}
	// Build command list for Telegram menu
	var commands []tele.Command
	// Bot's own commands
	commands = append(commands,
		tele.Command{Text: "bot_start", Description: "Show welcome message"},
		tele.Command{Text: "bot_pair", Description: "Pair this chat with the bot"},
		tele.Command{Text: "bot_status", Description: "Check bot and pairing status"},
		tele.Command{Text: "bot_perm_default", Description: "Switch to default mode"},
		tele.Command{Text: "bot_perm_plan", Description: "Switch to plan mode"},
		tele.Command{Text: "bot_perm_auto", Description: "Switch to auto-edit mode"},
		tele.Command{Text: "bot_perm_bypass", Description: "Switch to full-auto (bypass) mode"},
		tele.Command{Text: "bot_perm_status", Description: "Show current pane content"},
		tele.Command{Text: "bot_capture", Description: "Capture tmux pane content"},
		tele.Command{Text: "bot_routes", Description: "Show route bindings"},
		tele.Command{Text: "bot_bind", Description: "Bind a tmux session to this chat"},
		tele.Command{Text: "bot_unbind", Description: "Unbind a tmux session from this chat"},
	)
	// CC built-in commands
	for name, desc := range ccBuiltinCommands {
		commands = append(commands, tele.Command{Text: name, Description: desc})
	}
	// CC custom commands
	customCmds := scanCustomCommands()
	for name, cmd := range customCmds {
		commands = append(commands, tele.Command{Text: name, Description: cmd.desc})
	}
	bot.SetCommands(commands)
	// Build TG→CC name mapping
	ccCommandMap := make(map[string]string)
	for tgName := range ccBuiltinCommands {
		ccName := tgName
		if tgName == "terminal_setup" {
			ccName = "terminal-setup"
		}
		ccCommandMap[tgName] = ccName
	}
	for tgName, cmd := range customCmds {
		ccCommandMap[tgName] = cmd.ccName
	}
	// Register CC command handlers
	for tgName, ccName := range ccCommandMap {
		tg, cc := tgName, ccName
		bot.Handle("/"+tg, func(c tele.Context) error {
			if c.Message().ReplyTo == nil {
				if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
					creds, _ := config.LoadCredentials()
					var targets []string
					for t, chatID := range creds.RouteMap {
						if chatID == c.Chat().ID {
							targets = append(targets, t)
						}
					}
					if len(targets) == 1 {
						target, err := injector.ParseTarget(targets[0])
						if err != nil || !injector.SessionExists(target) {
							return c.Reply("❌ tmux session not found.")
						}
						text := "/" + cc
						if payload := strings.TrimSpace(c.Message().Payload); payload != "" {
							text += " " + payload
						}
						if err := injector.InjectText(target, text); err != nil {
							return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
						}
						logger.Info(fmt.Sprintf("Group quick reply (command): target=%s text=%s", targets[0], truncateStr(text, 200)))
						bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
							Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
						})
						reactionTracker.record(targets[0], c.Chat().ID, c.Message().ID)
						return nil
					}
					if len(targets) > 1 {
						return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
					}
				}
				return c.Send("💡 Please reply to a notification message to target a session.")
			}
			targetPtr, err := extractTmuxTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Send("❌ No tmux session info found in the original message.")
			}
			target := *targetPtr
			if !injector.SessionExists(target) {
				return c.Send("❌ tmux session not found. The Claude Code session may have ended.")
			}
			text := "/" + cc
			if payload := strings.TrimSpace(c.Message().Payload); payload != "" {
				text += " " + payload
			}
			if err := injector.InjectText(target, text); err != nil {
				return c.Send(fmt.Sprintf("❌ Injection failed: %v", err))
			}
			if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
				Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
			}); err == nil {
				tmuxStr := injector.FormatTarget(target)
				reactionTracker.record(tmuxStr, c.Chat().ID, c.Message().ID)
			}
			return nil
		})
	}
	bot.Handle("/start", func(c tele.Context) error {
		return c.Send("tg-cli bot is running. Use /bot_pair to pair this chat.")
	})
	bot.Handle("/bot_pair", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if pairing.IsAllowed(userID) || pairing.IsAllowed(chatID) {
			return c.Send("Already paired.")
		}
		code := pairing.CreatePairingRequest(userID, chatID)
		return c.Send(fmt.Sprintf("Pairing code: %s\n\nEnter this code in the bot terminal to approve.\n\nCode expires in 10 minutes.", code))
	})
	bot.Handle("/status", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		return c.Send("Bot is running and paired.")
	})
	bot.Handle("/bot_routes", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		if !pairing.IsAllowed(userID) {
			return c.Send("❌ Not paired. Use /bot_pair first.")
		}
		creds, _ := config.LoadCredentials()
		if len(creds.RouteMap) == 0 {
			return c.Send("No active route bindings.")
		}
		var lines []string
		for tmux, chatID := range creds.RouteMap {
			chatName := fmt.Sprintf("%d", chatID)
			if chat, err := bot.ChatByID(chatID); err == nil && chat.Title != "" {
				chatName = chat.Title
			}
			lines = append(lines, fmt.Sprintf("📟 %s → %s", tmux, chatName))
		}
		return c.Send("🗺 Route bindings:\n" + strings.Join(lines, "\n"))
	})
	bot.Handle("/bot_bind", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		if !pairing.IsAllowed(userID) {
			return c.Reply("❌ Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			return c.Reply("❌ Reply to a notification message with /bot_bind to bind that session to this chat.")
		}
		target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info (📟) found in the replied message.")
		}
		tmuxStr := injector.FormatTarget(*target)
		if tmuxStr == "" {
			return c.Reply("❌ Empty tmux target, cannot bind.")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
		}
		creds.RouteMap[tmuxStr] = c.Chat().ID
		if err := config.SaveCredentials(creds); err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to save binding: %v", err))
		}
		logger.Info(fmt.Sprintf("Route bound: tmux=%s → chat=%d by user=%s", tmuxStr, c.Chat().ID, userID))
		return c.Reply(fmt.Sprintf("✅ Bound session to this chat.\n📟 %s", tmuxStr))
	})
	bot.Handle("/bot_unbind", func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		if !pairing.IsAllowed(userID) {
			return c.Reply("❌ Not paired.")
		}
		if c.Message().ReplyTo == nil {
			return c.Reply("❌ Reply to a notification message with /bot_unbind to unbind that session.")
		}
		target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info (📟) found in the replied message.")
		}
		tmuxStr := injector.FormatTarget(*target)
		creds, err := config.LoadCredentials()
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to load config: %v", err))
		}
		if _, ok := creds.RouteMap[tmuxStr]; !ok {
			return c.Reply("❌ This session is not bound to any chat.")
		}
		delete(creds.RouteMap, tmuxStr)
		if err := config.SaveCredentials(creds); err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to save: %v", err))
		}
		logger.Info(fmt.Sprintf("Route unbound: tmux=%s by user=%s", tmuxStr, userID))
		return c.Reply(fmt.Sprintf("✅ Unbound session. Messages will go to default chat.\n📟 %s", tmuxStr))
	})
	bot.Handle(tele.OnText, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
				creds, _ := config.LoadCredentials()
				var targets []string
				for t, cid := range creds.RouteMap {
					if cid == c.Chat().ID {
						targets = append(targets, t)
					}
				}
				if len(targets) == 0 {
					return nil
				}
				if len(targets) > 1 {
					return c.Reply("❌ Multiple sessions bound to this group. Reply to a specific notification.")
				}
				target, err := injector.ParseTarget(targets[0])
				if err != nil || !injector.SessionExists(target) {
					return c.Reply("❌ tmux session not found.")
				}
				// Check for bot commands before injecting as text
				if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
					return handlePermCommand(c, target)
				}
				if c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@") {
					return handleCaptureCommand(c, target)
				}
				if err := injector.InjectText(target, c.Message().Text); err != nil {
					return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
				}
				logger.Info(fmt.Sprintf("Group quick reply: target=%s text=%s", targets[0], truncateStr(c.Message().Text, 200)))
				bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
					Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
				})
				reactionTracker.record(targets[0], c.Chat().ID, c.Message().ID)
				return nil
			}
			return nil
		}
		// Permission mode switching commands
		if strings.HasPrefix(c.Message().Text, "/bot_perm_") {
			targetPtr, err := extractTmuxTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info found.")
			}
			target := *targetPtr
			if !injector.SessionExists(target) {
				return c.Reply("❌ tmux session not found.")
			}
			return handlePermCommand(c, target)
		}
		if (c.Message().Text == "/bot_capture" || strings.HasPrefix(c.Message().Text, "/bot_capture@")) && c.Message().ReplyTo != nil {
			targetPtr, err := extractTmuxTarget(c.Message().ReplyTo.Text)
			if err != nil {
				return c.Reply("❌ No tmux session info found.")
			}
			target := *targetPtr
			if !injector.SessionExists(target) {
				return c.Reply("❌ tmux session not found.")
			}
			return handleCaptureCommand(c, target)
		}
		if replyTo := c.Message().ReplyTo; replyTo != nil {
			if _, ok := pendingPerms.getTarget(replyTo.ID); ok {
				pendingPerms.resolve(replyTo.ID, permDecision{
					Behavior: "deny",
					Message:  "User provided custom input: " + c.Message().Text,
				})
				editMsg := &tele.Message{ID: replyTo.ID, Chat: &tele.Chat{ID: c.Chat().ID}}
				bot.Edit(editMsg, replyTo.Text)
				targetPtr, err := extractTmuxTarget(replyTo.Text)
				if err == nil && targetPtr != nil {
					target := *targetPtr
					if injector.SessionExists(target) {
						injector.InjectText(target, c.Message().Text)
					}
					logger.Info(fmt.Sprintf("Permission denied via text reply, text injected: msg_id=%d target=%s text=%s", replyTo.ID, injector.FormatTarget(target), truncateStr(c.Message().Text, 200)))
					if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
						Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
					}); err == nil {
						tmuxStr := injector.FormatTarget(target)
						reactionTracker.record(tmuxStr, c.Chat().ID, c.Message().ID)
					}
				}
				return nil
			}
			if entry, ok := toolNotifs.get(replyTo.ID); ok {
				target, err := injector.ParseTarget(entry.tmuxTarget)
				if err != nil || !injector.SessionExists(target) {
					return c.Reply("❌ tmux session not found.")
				}
				switch entry.toolName {
				case "AskUserQuestion":
					pendingAsks.mu.Lock()
					_, isPending := pendingAsks.entries[replyTo.ID]
					pendingAsks.mu.Unlock()
					if isPending {
						answers := make(map[string]string)
						if len(entry.questions) > 0 {
							answers[entry.questions[0].questionText] = c.Message().Text
						}
						pendingAsks.resolve(replyTo.ID, answers)
						logger.Info(fmt.Sprintf("AskUserQuestion custom text via reply: msg_id=%d text=%s", replyTo.ID, truncateStr(c.Message().Text, 200)))
					} else {
						numOptions := 0
						if len(entry.questions) > 0 {
							numOptions = entry.questions[0].numOptions
						}
						for i := 0; i < numOptions; i++ {
							injector.SendKeys(target, "Down")
							time.Sleep(100 * time.Millisecond)
						}
						time.Sleep(100 * time.Millisecond)
						injector.SendKeys(target, "Enter")
						time.Sleep(1000 * time.Millisecond)
						injector.InjectText(target, c.Message().Text)
					}
				}
				logger.Info(fmt.Sprintf("Tool text reply: tool=%s msg_id=%d target=%s text=%s", entry.toolName, replyTo.ID, entry.tmuxTarget, truncateStr(c.Message().Text, 200)))
				if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
					Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
				}); err == nil {
					reactionTracker.record(entry.tmuxTarget, c.Chat().ID, c.Message().ID)
				}
				return nil
			}
		}
		targetPtr, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info found in the original message.")
		}
		target := *targetPtr
		if !injector.SessionExists(target) {
			return c.Reply("❌ tmux session not found. The Claude Code session may have ended.")
		}
		if err := injector.InjectText(target, c.Message().Text); err != nil {
			logger.Error(fmt.Sprintf("Injection failed: %v", err))
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Injected text to %s text=%s", injector.FormatTarget(target), truncateStr(c.Message().Text, 200)))
		if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		}); err != nil {
			logger.Debug(fmt.Sprintf("React failed: %v, falling back to reply", err))
			return c.Reply("✅")
		} else {
			tmuxStr := injector.FormatTarget(target)
			reactionTracker.record(tmuxStr, c.Chat().ID, c.Message().ID)
		}
		return nil
	})
	bot.Handle(tele.OnVoice, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /bot_pair first.")
		}
		if c.Message().ReplyTo == nil {
			if c.Chat().Type == "group" || c.Chat().Type == "supergroup" {
				creds, _ := config.LoadCredentials()
				var targets []string
				for t, chatID := range creds.RouteMap {
					if chatID == c.Chat().ID {
						targets = append(targets, t)
					}
				}
				if len(targets) == 0 {
					return nil
				}
				if len(targets) > 1 {
					return c.Reply("❌ Multiple sessions bound. Reply to a specific notification.")
				}
				file, err := bot.FileByID(c.Message().Voice.FileID)
				if err != nil {
					return c.Reply(fmt.Sprintf("❌ Failed to get voice file: %v", err))
				}
				tmpFile := filepath.Join(os.TempDir(), "tg-cli-voice-"+c.Message().Voice.FileID+".ogg")
				defer os.Remove(tmpFile)
				if err := bot.Download(&file, tmpFile); err != nil {
					return c.Reply(fmt.Sprintf("❌ Failed to download voice: %v", err))
				}
				text, err := voice.Transcribe(tmpFile)
				if err != nil || text == "" {
					return c.Reply("❌ Transcription failed or empty.")
				}
				target, err := injector.ParseTarget(targets[0])
				if err != nil || !injector.SessionExists(target) {
					return c.Reply("❌ tmux session not found.")
				}
				if err := injector.InjectText(target, text); err != nil {
					return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
				}
				logger.Info(fmt.Sprintf("Group voice quick reply: target=%s text=%s", targets[0], truncateStr(text, 200)))
				sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
				if sentMsg != nil {
					bot.React(c.Message().Chat, sentMsg, tele.ReactionOptions{
						Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
					})
					reactionTracker.record(targets[0], c.Chat().ID, sentMsg.ID)
				}
				return nil
			}
			return nil
		}
		file, err := bot.FileByID(c.Message().Voice.FileID)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to get voice file: %v", err))
		}
		tmpFile := filepath.Join(os.TempDir(), "tg-cli-voice-"+c.Message().Voice.FileID+".ogg")
		defer os.Remove(tmpFile)
		if err := bot.Download(&file, tmpFile); err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to download voice: %v", err))
		}
		text, err := voice.Transcribe(tmpFile)
		if err != nil {
			return c.Reply(fmt.Sprintf("❌ Transcription failed: %v", err))
		}
		if text == "" {
			return c.Reply("❌ Transcription produced empty text.")
		}
		if replyTo := c.Message().ReplyTo; replyTo != nil {
			if entry, ok := toolNotifs.get(replyTo.ID); ok {
				switch entry.toolName {
				case "AskUserQuestion":
					answers := make(map[string]string)
					if len(entry.questions) > 0 {
						answers[entry.questions[0].questionText] = text
					}
					if !pendingAsks.resolve(replyTo.ID, answers) {
						logger.Info(fmt.Sprintf("AskUserQuestion voice reply: pendingAsk not found (already resolved/expired): msg_id=%d", replyTo.ID))
						return c.Reply("❌ Question already answered or expired.")
					}
					logger.Info(fmt.Sprintf("AskUserQuestion custom voice via reply: msg_id=%d text=%s", replyTo.ID, truncateStr(text, 200)))
					editChat := &tele.Chat{ID: entry.chatID}
					editMsg := &tele.Message{ID: replyTo.ID, Chat: editChat}
					bot.Edit(editMsg, entry.msgText)
					sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
					if sentMsg != nil {
						bot.React(c.Message().Chat, sentMsg, tele.ReactionOptions{
							Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
						})
						reactionTracker.record(entry.tmuxTarget, c.Chat().ID, sentMsg.ID)
					}
					return nil
				}
			}
		}
		targetPtr, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info found in the original message.")
		}
		target := *targetPtr
		if !injector.SessionExists(target) {
			return c.Reply("❌ tmux session not found. The Claude Code session may have ended.")
		}
		if err := injector.InjectText(target, text); err != nil {
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Injected voice transcription to %s text=%s", injector.FormatTarget(target), truncateStr(text, 200)))
		sentMsg, _ := bot.Reply(c.Message(), fmt.Sprintf("🎙️ %s", text))
		if sentMsg != nil {
			if err := bot.React(c.Message().Chat, sentMsg, tele.ReactionOptions{
				Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
			}); err != nil {
				logger.Debug(fmt.Sprintf("React failed: %v", err))
			} else {
				tmuxStr := injector.FormatTarget(target)
				reactionTracker.record(tmuxStr, c.Chat().ID, sentMsg.ID)
			}
		}
		return nil
	})
	bot.Handle(&tele.InlineButton{Unique: "p"}, func(c tele.Context) error {
		pageNum, err := strconv.Atoi(c.Data())
		if err != nil {
			return c.Respond()
		}
		entry, ok := pages.get(c.Message().ID)
		if !ok {
			return c.Respond(&tele.CallbackResponse{Text: "Page expired"})
		}
		if pageNum < 1 || pageNum > len(entry.chunks) {
			return c.Respond()
		}
		var text string
		if entry.permRows != nil {
			// Permission message: chunks are raw text fragments
			text = entry.chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.chunks))
		} else {
			text = notify.BuildNotificationText(notify.NotificationData{
				Event:      entry.event,
				Project:    entry.project,
				Body:       entry.chunks[pageNum-1],
				TmuxTarget: entry.tmuxTarget,
				Page:       pageNum,
				TotalPages: len(entry.chunks),
			})
		}
		kb := buildPageKeyboardWithExtra(pageNum, len(entry.chunks), entry.permRows)
		_, err = bot.Edit(c.Message(), text, kb)
		if err != nil {
			logger.Debug(fmt.Sprintf("edit page error: %v", err))
		}
		return c.Respond()
	})
	bot.Handle(&tele.InlineButton{Unique: "perm"}, func(c tele.Context) error {
		decision := c.Data()
		_, err := resolvePermission(c.Message().ID, decision, nil)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Expired or invalid"})
		}
		logger.Info(fmt.Sprintf("Permission resolved via TG button: msg_id=%d decision=%s", c.Message().ID, decision))
		bot.Edit(c.Message(), c.Message().Text)
		displayText := decision
		if strings.HasPrefix(decision, "s") {
			displayText = "Always Allow"
		}
		if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		}); err == nil {
			targetPtr, err := extractTmuxTarget(c.Message().Text)
			if err == nil && targetPtr != nil {
				target := *targetPtr
				tmuxStr := injector.FormatTarget(target)
				reactionTracker.record(tmuxStr, c.Chat().ID, c.Message().ID)
			}
		}
		return c.Respond(&tele.CallbackResponse{Text: "✅ " + displayText})
	})
	bot.Handle(&tele.InlineButton{Unique: "tool"}, func(c tele.Context) error {
		parts := strings.SplitN(c.Data(), "|", 2)
		if len(parts) < 2 {
			return c.Respond(&tele.CallbackResponse{Text: "Invalid data"})
		}
		toolName := parts[0]
		switch toolName {
		case "AskUserQuestion":
			entry, ok := toolNotifs.get(c.Message().ID)
			if !ok {
				return c.Respond(&tele.CallbackResponse{Text: "Expired"})
			}
			if parts[1] == "chat" {
				answers := map[string]string{"__chat": "true"}
				if !pendingAsks.resolve(c.Message().ID, answers) {
					target, _ := injector.ParseTarget(entry.tmuxTarget)
					numOptions := 0
					if len(entry.questions) > 0 {
						numOptions = entry.questions[0].numOptions
					}
					for i := 0; i < numOptions+1; i++ {
						injector.SendKeys(target, "Down")
						time.Sleep(100 * time.Millisecond)
					}
					injector.SendKeys(target, "Enter")
				}
				logger.Info(fmt.Sprintf("AskUserQuestion 'Chat about this' selected: msg_id=%d", c.Message().ID))
				return c.Respond(&tele.CallbackResponse{Text: "Chat mode"})
			} else if parts[1] == "submit" {
				answers := buildAnswers(entry)
				if !pendingAsks.resolve(c.Message().ID, answers) {
					return c.Respond(&tele.CallbackResponse{Text: "Already submitted"})
				}
				logger.Info(fmt.Sprintf("AskUserQuestion submitted: msg_id=%d answers=%v", c.Message().ID, answers))
				return c.Respond(&tele.CallbackResponse{Text: "✅ Submitted"})
			} else {
				split := strings.SplitN(parts[1], ":", 2)
				qIdx, _ := strconv.Atoi(split[0])
				optIdx, _ := strconv.Atoi(split[1])
				if qIdx >= len(entry.questions) {
					return c.Respond(&tele.CallbackResponse{Text: "Invalid question"})
				}
				qm := &entry.questions[qIdx]
				if qm.multiSelect {
					qm.selectedOptions[optIdx] = !qm.selectedOptions[optIdx]
					logger.Info(fmt.Sprintf("AskUserQuestion multiSelect toggle: msg_id=%d q=%d opt=%d state=%v label=%s", c.Message().ID, qIdx, optIdx, qm.selectedOptions[optIdx], qm.optionLabels[optIdx]))
					newMarkup := rebuildAskMarkup(entry)
					bot.Edit(c.Message(), c.Message().Text, newMarkup)
					return c.Respond(&tele.CallbackResponse{Text: "Toggled"})
				} else {
					qm.selectedOption = optIdx
					hasSubmit := len(entry.questions) > 1
					for _, q := range entry.questions {
						if q.multiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						answers := buildAnswers(entry)
						if !pendingAsks.resolve(c.Message().ID, answers) {
							return c.Respond(&tele.CallbackResponse{Text: "Already submitted"})
						}
						logger.Info(fmt.Sprintf("AskUserQuestion auto-resolved: msg_id=%d answers=%v", c.Message().ID, answers))
						// Remove buttons after auto-resolve
						bot.Edit(c.Message(), c.Message().Text)
						return c.Respond(&tele.CallbackResponse{Text: "✅ Selected"})
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected: msg_id=%d q=%d opt=%d label=%s", c.Message().ID, qIdx, optIdx, qm.optionLabels[optIdx]))
						newMarkup := rebuildAskMarkup(entry)
						bot.Edit(c.Message(), c.Message().Text, newMarkup)
						return c.Respond(&tele.CallbackResponse{Text: "Selected"})
					}
				}
			}
		}
		if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		}); err == nil {
			if entry, ok := toolNotifs.get(c.Message().ID); ok {
				reactionTracker.record(entry.tmuxTarget, c.Chat().ID, c.Message().ID)
			}
		}
		return c.Respond()
	})
	mux := http.NewServeMux()
	// hookPayload represents the CC payload enriched by hook.go
	type hookPayload struct {
		HookEventName  string          `json:"hook_event_name"`
		SessionID      string          `json:"session_id"`
		CWD            string          `json:"cwd"`
		TranscriptPath string          `json:"transcript_path"`
		ToolName       string          `json:"tool_name"`
		ToolInput      json.RawMessage `json:"tool_input"`
		PermSuggestions json.RawMessage `json:"permission_suggestions"`
		TmuxTarget     string          `json:"tmux_target"`
		Project        string          `json:"project"`
	}
	parseHookPayload := func(r *http.Request) (*hookPayload, []byte, error) {
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
	resolveChat := func(tmuxTarget string) (*tele.Chat, string) {
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
	mux.HandleFunc("/hook/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		event := strings.TrimPrefix(r.URL.Path, "/hook/")
		p, raw, err := parseHookPayload(r)
		if err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		logger.Info(fmt.Sprintf("Raw hook payload [%s]: %s", event, string(raw)))
		chat, chatID := resolveChat(p.TmuxTarget)
		switch event {
		case "SessionStart":
			if chat == nil || p.TmuxTarget == "" {
				w.WriteHeader(200)
				return
			}
			text := notify.BuildNotificationText(notify.NotificationData{
				Event: "SessionStart", Project: p.Project, TmuxTarget: p.TmuxTarget,
			})
			bot.Send(chat, text)
			logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionStart [%s] tmux=%s", chatID, p.Project, p.TmuxTarget))
		case "SessionEnd":
			if chat != nil {
				text := notify.BuildNotificationText(notify.NotificationData{
					Event: "SessionEnd", Project: p.Project, TmuxTarget: p.TmuxTarget,
				})
				bot.Send(chat, text)
				logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionEnd [%s] tmux=%s", chatID, p.Project, p.TmuxTarget))
			}
			pages.cleanupSession(p.SessionID)
			sessionCounts.cleanup(p.SessionID)
			logger.Info(fmt.Sprintf("Cleaned up session %s", p.SessionID))
		case "UserPromptSubmit":
			if p.SessionID != "" && p.TranscriptPath != "" {
				lock := sessionCounts.getLock(p.SessionID)
				lock.Lock()
				texts := readAssistantTexts(p.TranscriptPath)
				sessionCounts.counts[p.SessionID] = len(texts)
				lock.Unlock()
				logger.Debug(fmt.Sprintf("UserPromptSubmit position: session=%s count=%d", p.SessionID, len(texts)))
			}
			if p.TmuxTarget != "" {
				reactionTracker.clearAndRemove(bot, p.TmuxTarget)
				logger.Debug(fmt.Sprintf("Cleared reactions for tmux target: %s", p.TmuxTarget))
			}
		case "Stop":
			if chat != nil {
				body := processTranscriptUpdates(p.SessionID, p.TranscriptPath)
				sendEventNotification(bot, chat, chatID, p.SessionID, "Stop", p.Project, p.TmuxTarget, body)
			}
		case "PreToolUse":
			toolName := p.ToolName
			if toolName == "AskUserQuestion" {
				// AskUserQuestion: just send Update notification, don't block.
				// Answers will be handled by PermissionRequest handler.
				if chat != nil {
					if updateBody := processTranscriptUpdates(p.SessionID, p.TranscriptPath); updateBody != "" {
						sendEventNotification(bot, chat, chatID, p.SessionID, "PreToolUse", p.Project, p.TmuxTarget, updateBody)
					}
				}
				w.WriteHeader(200)
				return
			}
			// Non-AskUserQuestion PreToolUse: just send intermediate notification
			if chat != nil {
				body := processTranscriptUpdates(p.SessionID, p.TranscriptPath)
				if body != "" {
					sendEventNotification(bot, chat, chatID, p.SessionID, "PreToolUse", p.Project, p.TmuxTarget, body)
				}
			}
		case "PermissionRequest":
			toolName := p.ToolName
			if toolName == "AskUserQuestion" {
				// --- AskUserQuestion: blocking, wait for TG answer ---
				if chat == nil {
					// No chat paired, auto-allow with current tool_input
					var toolInput map[string]interface{}
					json.Unmarshal(p.ToolInput, &toolInput)
					output := map[string]interface{}{
						"hookSpecificOutput": map[string]interface{}{
							"hookEventName": "PermissionRequest",
							"decision": map[string]interface{}{
								"behavior":     "allow",
								"updatedInput": toolInput,
							},
						},
					}
					outJSON, _ := json.Marshal(output)
					w.Header().Set("Content-Type", "application/json")
					w.Write(outJSON)
					return
				}
				// Parse questions from tool_input
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
				json.Unmarshal(p.ToolInput, &askInput)
				if len(askInput.Questions) == 0 {
					var toolInput map[string]interface{}
					json.Unmarshal(p.ToolInput, &toolInput)
					output := map[string]interface{}{
						"hookSpecificOutput": map[string]interface{}{
							"hookEventName": "PermissionRequest",
							"decision": map[string]interface{}{
								"behavior":     "allow",
								"updatedInput": toolInput,
							},
						},
					}
					outJSON, _ := json.Marshal(output)
					w.Header().Set("Content-Type", "application/json")
					w.Write(outJSON)
					return
				}
				var qMetas []questionMeta
				var questionEntries []notify.QuestionEntry
				for _, q := range askInput.Questions {
					var opts []notify.QuestionOption
					var labels []string
					for _, o := range q.Options {
						opts = append(opts, notify.QuestionOption{Label: o.Label, Description: o.Description})
						labels = append(labels, o.Label)
					}
					qMetas = append(qMetas, questionMeta{
						questionText: q.Question, header: q.Header,
						numOptions: len(q.Options), optionLabels: labels,
						multiSelect: q.MultiSelect, selectedOptions: make(map[int]bool),
						selectedOption: -1,
					})
					questionEntries = append(questionEntries, notify.QuestionEntry{
						Header: q.Header, Question: q.Question, Options: opts, MultiSelect: q.MultiSelect,
					})
				}
				text := notify.BuildQuestionText(notify.QuestionData{
					Project: p.Project, TmuxTarget: p.TmuxTarget, Questions: questionEntries,
				})
				markup := &tele.ReplyMarkup{}
				var rows []tele.Row
				hasSubmit := len(askInput.Questions) > 1
				for _, q := range askInput.Questions {
					if q.MultiSelect {
						hasSubmit = true
					}
				}
				if len(askInput.Questions) == 1 && !askInput.Questions[0].MultiSelect {
					q := askInput.Questions[0]
					var buttons []tele.Btn
					for i, o := range q.Options {
						buttons = append(buttons, markup.Data(o.Label, "tool", fmt.Sprintf("AskUserQuestion|0:%d", i)))
					}
					for i := 0; i < len(buttons); i += 2 {
						if i+1 < len(buttons) {
							rows = append(rows, markup.Row(buttons[i], buttons[i+1]))
						} else {
							rows = append(rows, markup.Row(buttons[i]))
						}
					}
					chatBtn := markup.Data("💬 Chat about this", "tool", "AskUserQuestion|chat")
					rows = append(rows, markup.Row(chatBtn))
				} else {
					for qIdx, q := range askInput.Questions {
						for optIdx, o := range q.Options {
							label := o.Label
							if len(askInput.Questions) > 1 {
								label = fmt.Sprintf("Q%d: %s", qIdx+1, o.Label)
							}
							rows = append(rows, markup.Row(markup.Data(label, "tool", fmt.Sprintf("AskUserQuestion|%d:%d", qIdx, optIdx))))
						}
					}
					if hasSubmit {
						rows = append(rows, markup.Row(markup.Data("📤 Submit", "tool", "AskUserQuestion|submit")))
					}
					rows = append(rows, markup.Row(markup.Data("💬 Chat about this", "tool", "AskUserQuestion|chat")))
				}
				markup.Inline(rows...)
				sent, err := bot.Send(chat, text, markup)
				if err != nil {
					logger.Error(fmt.Sprintf("Failed to send AskUserQuestion: %v", err))
					w.WriteHeader(200)
					return
				}
				chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
				toolNotifs.store(sent.ID, &toolNotifyEntry{
					tmuxTarget: p.TmuxTarget, toolName: "AskUserQuestion",
					questions: qMetas, chatID: chatIDInt, msgText: text,
				})
				logger.Info(fmt.Sprintf("TG question message sent full_text:\n%s", text))
				var qSummaries []string
				for _, q := range askInput.Questions {
					var labels []string
					for _, o := range q.Options {
						labels = append(labels, o.Label)
					}
					qSummaries = append(qSummaries, fmt.Sprintf("%s:[%s]", q.Header, strings.Join(labels, ",")))
				}
				contentSummary := strings.Join(qSummaries, " | ")
				ch := pendingAsks.create(sent.ID)
				logger.Info(fmt.Sprintf("AskUserQuestion sent: msg_id=%d questions=%d tmux=%s content=%s", sent.ID, len(askInput.Questions), p.TmuxTarget, contentSummary))
				// Block until answered
				select {
				case answers := <-ch:
					pendingAsks.cleanup(sent.ID)
					var ti map[string]interface{}
					json.Unmarshal(p.ToolInput, &ti)
					questions := ti["questions"]
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
					outJSON, _ := json.Marshal(output)
					logger.Info(fmt.Sprintf("AskUserQuestion hookOutput to CC: %s", string(outJSON)))
					w.Header().Set("Content-Type", "application/json")
					w.Write(outJSON)
				case <-r.Context().Done():
					pendingAsks.cleanup(sent.ID)
					logger.Info(fmt.Sprintf("AskUserQuestion client disconnected: msg_id=%d", sent.ID))
					w.WriteHeader(200)
				}
				return
			}
			if chat == nil {
				w.WriteHeader(200)
				return
			}
			logger.Info(fmt.Sprintf("Permission request: tool=%s project=%s", toolName, p.Project))
			// Send intermediate text before permission message
			if updateBody := processTranscriptUpdates(p.SessionID, p.TranscriptPath); updateBody != "" {
				sendEventNotification(bot, chat, chatID, p.SessionID, "PreToolUse", p.Project, p.TmuxTarget, updateBody)
			}
			var toolInput map[string]interface{}
			json.Unmarshal(p.ToolInput, &toolInput)
			logger.Info(fmt.Sprintf("Permission payload: toolInput=%s suggestions=%s", string(p.ToolInput), string(p.PermSuggestions)))
			text := notify.BuildPermissionText(notify.PermissionData{
				Project: p.Project, TmuxTarget: p.TmuxTarget,
				ToolName: toolName, ToolInput: toolInput,
			})
			markup := &tele.ReplyMarkup{}
			row1 := []tele.Btn{
				markup.Data("✅ Allow", "perm", "allow"),
				markup.Data("❌ Deny", "perm", "deny"),
			}
			var suggestions []json.RawMessage
			json.Unmarshal(p.PermSuggestions, &suggestions)
			var row2 []tele.Btn
			for i, s := range suggestions {
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
				row2 = append(row2, markup.Data(label, "perm", fmt.Sprintf("s%d", i)))
			}
			// Build permission button rows for reuse in pagination
			var permBtnRows []tele.Row
			permBtnRows = append(permBtnRows, row1)
			if len(row2) > 0 {
				permBtnRows = append(permBtnRows, row2)
			}
			// Split permission text into pages if too long
			permChunks := splitBody(text, 3900)
			if len(permChunks) <= 1 {
				// Single page — just attach permission buttons
				if len(row2) > 0 {
					markup.Inline(markup.Row(row1...), markup.Row(row2...))
				} else {
					markup.Inline(markup.Row(row1...))
				}
			} else {
				// Multi-page — permission buttons + page navigation
				text = permChunks[0] + fmt.Sprintf("\n\n📄 1/%d", len(permChunks))
				kb := buildPageKeyboardWithExtra(1, len(permChunks), permBtnRows)
				markup = kb
			}
			sent, err := bot.Send(chat, text, markup)
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send permission message: %v", err))
				w.WriteHeader(200)
				return
			}
			chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
			if len(permChunks) > 1 {
				pages.store(sent.ID, p.SessionID, &pageEntry{
					chunks:     permChunks,
					event:      "PermissionRequest",
					project:    p.Project,
					tmuxTarget: p.TmuxTarget,
					permRows:   permBtnRows,
				chatID:     chatIDInt,
				})
			}
			logger.Info(fmt.Sprintf("Permission request sent: tool=%s project=%s tmux=%s (msg_id=%d pages=%d)", toolName, p.Project, p.TmuxTarget, sent.ID, len(permChunks)))
			logger.Info(fmt.Sprintf("TG permission message sent full_text:\n%s", text))
			suggestionsRaw, _ := json.Marshal(suggestions)
			ch := pendingPerms.create(sent.ID, p.TmuxTarget, suggestionsRaw, text, chatIDInt)
			// Block until resolved
			select {
			case d := <-ch:
				pendingPerms.cleanup(sent.ID)
				logger.Info(fmt.Sprintf("Permission resolved: msg_id=%d behavior=%s", sent.ID, d.Behavior))
				// Construct hookSpecificOutput for CC
				output := map[string]interface{}{
					"hookSpecificOutput": map[string]interface{}{
						"hookEventName": "PermissionRequest",
						"decision": map[string]interface{}{
							"behavior": d.Behavior,
						},
					},
				}
				if d.Message != "" {
					output["hookSpecificOutput"].(map[string]interface{})["decision"].(map[string]interface{})["message"] = d.Message
				}
				if len(d.UpdatedPermissions) > 0 {
					output["hookSpecificOutput"].(map[string]interface{})["decision"].(map[string]interface{})["updatedPermissions"] = d.UpdatedPermissions
				}
				outJSON, _ := json.Marshal(output)
				logger.Info(fmt.Sprintf("PermissionRequest hookOutput to CC: %s", string(outJSON)))
				w.Header().Set("Content-Type", "application/json")
				w.Write(outJSON)
			case <-r.Context().Done():
				pendingPerms.cleanup(sent.ID)
				logger.Info(fmt.Sprintf("Permission client disconnected: msg_id=%d", sent.ID))
				w.WriteHeader(200)
			}
			return
		default:
			// Unknown event — send notification if possible
			if chat != nil {
				body := processTranscriptUpdates(p.SessionID, p.TranscriptPath)
				sendEventNotification(bot, chat, chatID, p.SessionID, event, p.Project, p.TmuxTarget, body)
			}
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		msgIDStr := r.URL.Query().Get("msg_id")
		pageStr := r.URL.Query().Get("page")
		msgID, err := strconv.Atoi(msgIDStr)
		if err != nil {
			http.Error(w, "invalid msg_id", 400)
			return
		}
		pageNum, err := strconv.Atoi(pageStr)
		if err != nil {
			http.Error(w, "invalid page", 400)
			return
		}
		entry, ok := pages.get(msgID)
		if !ok {
			http.Error(w, "page entry not found", 404)
			return
		}
		if pageNum < 1 || pageNum > len(entry.chunks) {
			http.Error(w, "page out of range", 400)
			return
		}
		chat := &tele.Chat{ID: entry.chatID}
		var text string
		if entry.permRows != nil {
			text = entry.chunks[pageNum-1] + fmt.Sprintf("\n\n📄 %d/%d", pageNum, len(entry.chunks))
		} else {
			text = notify.BuildNotificationText(notify.NotificationData{
				Event:      entry.event,
				Project:    entry.project,
				Body:       entry.chunks[pageNum-1],
				TmuxTarget: entry.tmuxTarget,
				Page:       pageNum,
				TotalPages: len(entry.chunks),
			})
		}
		kb := buildPageKeyboardWithExtra(pageNum, len(entry.chunks), entry.permRows)
		editMsg := &tele.Message{ID: msgID, Chat: chat}
		_, err = bot.Edit(editMsg, text, kb)
		if err != nil {
			logger.Error(fmt.Sprintf("Callback edit failed: %v", err))
			http.Error(w, "edit failed: "+err.Error(), 500)
			return
		}
		logger.Info(fmt.Sprintf("Callback page turn: msg_id=%d page=%d/%d", msgID, pageNum, len(entry.chunks)))
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/permission/decide", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		decision := r.URL.Query().Get("decision")
		d, err := resolvePermission(msgID, decision, nil)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		logger.Info(fmt.Sprintf("Permission resolved via API: msg_id=%d decision=%s", msgID, decision))
		msgText := pendingPerms.getMsgText(msgID)
		permChatID := pendingPerms.getChatID(msgID)
		if permChatID != 0 && msgText != "" {
			editMsg := &tele.Message{ID: msgID, Chat: &tele.Chat{ID: permChatID}}
			bot.Edit(editMsg, msgText)
		}
		respJSON, _ := json.Marshal(d)
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	})
	mux.HandleFunc("/tool/respond", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		tool := r.URL.Query().Get("tool")
		action := r.URL.Query().Get("action")
		switch tool {
		case "AskUserQuestion":
			if action == "text" {
				value := r.URL.Query().Get("value")
				entry, ok := toolNotifs.get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				answers := make(map[string]string)
				if len(entry.questions) > 0 {
					answers[entry.questions[0].questionText] = value
				}
				if !pendingAsks.resolve(msgID, answers) {
					http.Error(w, "no pending ask", 404)
					return
				}
				logger.Info(fmt.Sprintf("AskUserQuestion text via API: msg_id=%d text=%s", msgID, truncateStr(value, 200)))
				editChat := &tele.Chat{ID: entry.chatID}
				editMsg := &tele.Message{ID: msgID, Chat: editChat}
				bot.Edit(editMsg, entry.msgText)
			} else if action == "submit" {
				entry, ok := toolNotifs.get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				answers := buildAnswers(entry)
				if !pendingAsks.resolve(msgID, answers) {
					http.Error(w, "no pending ask", 404)
					return
				}
				logger.Info(fmt.Sprintf("AskUserQuestion submitted via API: msg_id=%d answers=%v", msgID, answers))
				editChat := &tele.Chat{ID: entry.chatID}
				editMsg := &tele.Message{ID: msgID, Chat: editChat}
				bot.Edit(editMsg, entry.msgText)
			} else {
				qIdx, _ := strconv.Atoi(r.URL.Query().Get("question"))
				optIdx, _ := strconv.Atoi(r.URL.Query().Get("option"))
				entry, ok := toolNotifs.get(msgID)
				if !ok {
					http.Error(w, "not found", 404)
					return
				}
				if qIdx >= len(entry.questions) {
					http.Error(w, "invalid question index", 400)
					return
				}
				qm := &entry.questions[qIdx]
				if qm.multiSelect {
					qm.selectedOptions[optIdx] = !qm.selectedOptions[optIdx]
					logger.Info(fmt.Sprintf("AskUserQuestion option toggled via API: msg_id=%d q=%d opt=%d state=%v label=%s", msgID, qIdx, optIdx, qm.selectedOptions[optIdx], qm.optionLabels[optIdx]))
					newMarkup := rebuildAskMarkup(entry)
					editChat := &tele.Chat{ID: entry.chatID}
					editMsg := &tele.Message{ID: msgID, Chat: editChat}
					bot.Edit(editMsg, entry.msgText, newMarkup)
				} else {
					qm.selectedOption = optIdx
					hasSubmit := len(entry.questions) > 1
					for _, q := range entry.questions {
						if q.multiSelect {
							hasSubmit = true
						}
					}
					if !hasSubmit {
						answers := buildAnswers(entry)
						if !pendingAsks.resolve(msgID, answers) {
							http.Error(w, "no pending ask", 404)
							return
						}
						logger.Info(fmt.Sprintf("AskUserQuestion auto-resolved via API: msg_id=%d q=%d opt=%d label=%s answers=%v", msgID, qIdx, optIdx, qm.optionLabels[optIdx], answers))
						editChat := &tele.Chat{ID: entry.chatID}
						editMsg := &tele.Message{ID: msgID, Chat: editChat}
						bot.Edit(editMsg, entry.msgText)
					} else {
						logger.Info(fmt.Sprintf("AskUserQuestion option selected via API: msg_id=%d q=%d opt=%d label=%s", msgID, qIdx, optIdx, qm.optionLabels[optIdx]))
						newMarkup := rebuildAskMarkup(entry)
						editChat := &tele.Chat{ID: entry.chatID}
						editMsg := &tele.Message{ID: msgID, Chat: editChat}
						bot.Edit(editMsg, entry.msgText, newMarkup)
					}
				}
			}
		default:
			http.Error(w, "unsupported tool", 400)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/route/bind", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			TmuxTarget string `json:"tmux_target"`
			ChatID     int64  `json:"chat_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		creds.RouteMap[req.TmuxTarget] = req.ChatID
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route bound via API: tmux=%s → chat=%d", req.TmuxTarget, req.ChatID))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/route/unbind", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			TmuxTarget string `json:"tmux_target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		delete(creds.RouteMap, req.TmuxTarget)
		if err := config.SaveCredentials(creds); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info(fmt.Sprintf("Route unbound via API: tmux=%s", req.TmuxTarget))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/route/list", func(w http.ResponseWriter, r *http.Request) {
		creds, err := config.LoadCredentials()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"routes": creds.RouteMap})
	})
	mux.HandleFunc("/inject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Target string `json:"target"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		target, err := injector.ParseTarget(req.Target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !injector.SessionExists(target) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		logger.Info(fmt.Sprintf("Inject API: target=%s text=%s", injector.FormatTarget(target), truncateStr(req.Text, 200)))
		if err := injector.InjectText(target, req.Text); err != nil {
			logger.Error(fmt.Sprintf("Inject API failed: %v", err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/capture", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			http.Error(w, "target required", http.StatusBadRequest)
			return
		}
		t, err := injector.ParseTarget(target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !injector.SessionExists(t) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		content, err := injector.CapturePane(t)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"content": content})
	})
	mux.HandleFunc("/perm/switch", func(w http.ResponseWriter, r *http.Request) {
		targetStr := r.URL.Query().Get("target")
		mode := r.URL.Query().Get("mode")
		if targetStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "target required"})
			return
		}
		if mode == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "mode required"})
			return
		}
		t, err := injector.ParseTarget(targetStr)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		if !injector.SessionExists(t) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "session not found"})
			return
		}
		logger.Info(fmt.Sprintf("Perm switch API: target=%s mode=%s", injector.FormatTarget(t), mode))
		finalMode, err := switchPermMode(t, mode)
		if err != nil {
			logger.Info(fmt.Sprintf("Perm switch API failed: %v", err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "mode": finalMode})
	})
	mux.HandleFunc("/perm/status", func(w http.ResponseWriter, r *http.Request) {
		targetStr := r.URL.Query().Get("target")
		if targetStr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "target required"})
			return
		}
		t, err := injector.ParseTarget(targetStr)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		if !injector.SessionExists(t) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "session not found"})
			return
		}
		mode, content, err := detectPermMode(t)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "mode": mode, "content": content})
	})
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{Addr: addr, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("Received shutdown signal, stopping...")
		bot.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	go func() {
		logger.Info(fmt.Sprintf("Hook HTTP server listening on %s", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(fmt.Sprintf("HTTP server error: %v", err))
		}
	}()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		go func() {
			reader := bufio.NewReader(os.Stdin)
			for {
				input, err := reader.ReadString('\n')
				if err != nil {
					break
				}
				input = strings.TrimSpace(input)
				if input == "" {
					continue
				}
				if pairing.ApprovePairingByCode(input) {
					fmt.Printf("Pairing approved for code: %s\n", input)
				} else {
					pending := pairing.ListPending()
					if len(pending) > 0 {
						fmt.Println("Pending pairing requests:")
						for _, p := range pending {
							fmt.Printf("  Code: %s, User: %s\n", p.Code, p.UserID)
						}
					} else {
						fmt.Printf("Unknown input: %s\n", input)
					}
				}
			}
		}()
	}
	binaryMD5 := "unknown"
	if exePath, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(exePath); err == nil {
			h := md5.Sum(data)
			binaryMD5 = hex.EncodeToString(h[:])
		}
	}
	logger.Info(fmt.Sprintf("Starting tg-cli bot... version=%s binary_md5=%s", Version, binaryMD5))
	bot.Start()
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

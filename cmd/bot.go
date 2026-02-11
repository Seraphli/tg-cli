package cmd

import (
	"bufio"
	"context"
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

var (
	debugFlag bool
	portFlag  int
)

func init() {
	BotCmd.Flags().BoolVar(&debugFlag, "debug", false, "Enable debug mode")
	BotCmd.Flags().IntVar(&portFlag, "port", 0, "HTTP server port (overrides config)")
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
}

var pages = &pageCacheStore{
	entries:  make(map[int]*pageEntry),
	sessions: make(map[string][]int),
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
}

var pendingPerms = &pendingPermStore{
	entries:     make(map[int]chan permDecision),
	targets:     make(map[int]string),
	suggestions: make(map[int]json.RawMessage),
}

func (ps *pendingPermStore) create(msgID int, tmuxTarget string, suggestionsJSON json.RawMessage) chan permDecision {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ch := make(chan permDecision, 1)
	ps.entries[msgID] = ch
	ps.targets[msgID] = tmuxTarget
	ps.suggestions[msgID] = suggestionsJSON
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

func (ps *pendingPermStore) cleanup(msgID int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.entries, msgID)
	delete(ps.targets, msgID)
	delete(ps.suggestions, msgID)
}

type toolNotifyEntry struct {
	tmuxTarget string
	toolName   string
	numOptions int
}

type toolNotifyStore struct {
	mu      sync.RWMutex
	entries map[int]*toolNotifyEntry
}

var toolNotifs = &toolNotifyStore{
	entries: make(map[int]*toolNotifyEntry),
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
	markup := &tele.ReplyMarkup{}
	var buttons []tele.Btn
	if currentPage > 1 {
		buttons = append(buttons, markup.Data("◀️", "p", fmt.Sprintf("%d", currentPage-1)))
	}
	buttons = append(buttons, markup.Data(fmt.Sprintf("%d/%d", currentPage, totalPages), "p", fmt.Sprintf("%d", currentPage)))
	if currentPage < totalPages {
		buttons = append(buttons, markup.Data("▶️", "p", fmt.Sprintf("%d", currentPage+1)))
	}
	markup.Inline(markup.Row(buttons...))
	return markup
}

// extractTmuxTarget extracts tmux target from notification text.
func extractTmuxTarget(text string) (injector.TmuxTarget, error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "📟 ") {
			raw := strings.TrimPrefix(line, "📟 ")
			return injector.ParseTarget(raw)
		}
	}
	return injector.TmuxTarget{}, fmt.Errorf("no tmux target found")
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
	bot.Handle("/start", func(c tele.Context) error {
		return c.Send("tg-cli bot is running. Use /pair to pair this chat.")
	})
	bot.Handle("/pair", func(c tele.Context) error {
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
			return c.Send("Not paired. Use /pair first.")
		}
		return c.Send("Bot is running and paired.")
	})
	bot.Handle(tele.OnText, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /pair first.")
		}
		if c.Message().ReplyTo == nil {
			return nil
		}
		if replyTo := c.Message().ReplyTo; replyTo != nil {
			if _, ok := pendingPerms.getTarget(replyTo.ID); ok {
				pendingPerms.resolve(replyTo.ID, permDecision{
					Behavior: "deny",
					Message:  "User provided custom input: " + c.Message().Text,
				})
				target, err := extractTmuxTarget(replyTo.Text)
				if err == nil && injector.SessionExists(target) {
					injector.InjectText(target, c.Message().Text)
				}
				logger.Info(fmt.Sprintf("Permission denied via text reply, text injected: msg_id=%d", replyTo.ID))
				return bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
					Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
				})
			}
			if entry, ok := toolNotifs.get(replyTo.ID); ok {
				target, err := injector.ParseTarget(entry.tmuxTarget)
				if err != nil || !injector.SessionExists(target) {
					return c.Reply("❌ tmux session not found.")
				}
				switch entry.toolName {
				case "AskUserQuestion":
					for i := 0; i < entry.numOptions; i++ {
						injector.SendKeys(target, "Down")
						time.Sleep(100 * time.Millisecond)
					}
					time.Sleep(100 * time.Millisecond)
					injector.SendKeys(target, "Enter")
					time.Sleep(500 * time.Millisecond)
					injector.InjectText(target, c.Message().Text)
				}
				logger.Info(fmt.Sprintf("Tool text reply: tool=%s msg_id=%d", entry.toolName, replyTo.ID))
				return bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
					Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
				})
			}
		}
		target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info found in the original message.")
		}
		if !injector.SessionExists(target) {
			return c.Reply("❌ tmux session not found. The Claude Code session may have ended.")
		}
		if err := injector.InjectText(target, c.Message().Text); err != nil {
			logger.Error(fmt.Sprintf("Injection failed: %v", err))
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Injected text to %s", injector.FormatTarget(target)))
		if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		}); err != nil {
			logger.Debug(fmt.Sprintf("React failed: %v, falling back to reply", err))
			return c.Reply("✅")
		}
		return nil
	})
	bot.Handle(tele.OnVoice, func(c tele.Context) error {
		userID := strconv.FormatInt(c.Sender().ID, 10)
		chatID := strconv.FormatInt(c.Chat().ID, 10)
		if !pairing.IsAllowed(userID) && !pairing.IsAllowed(chatID) {
			return c.Send("Not paired. Use /pair first.")
		}
		if c.Message().ReplyTo == nil {
			return nil
		}
		target, err := extractTmuxTarget(c.Message().ReplyTo.Text)
		if err != nil {
			return c.Reply("❌ No tmux session info found in the original message.")
		}
		if !injector.SessionExists(target) {
			return c.Reply("❌ tmux session not found. The Claude Code session may have ended.")
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
		if err := injector.InjectText(target, text); err != nil {
			return c.Reply(fmt.Sprintf("❌ Injection failed: %v", err))
		}
		logger.Info(fmt.Sprintf("Injected voice transcription to %s", injector.FormatTarget(target)))
		if err := bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		}); err != nil {
			logger.Debug(fmt.Sprintf("React failed: %v, falling back to reply", err))
			return c.Reply("✅")
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
		text := notify.BuildNotificationText(notify.NotificationData{
			Event:      entry.event,
			Project:    entry.project,
			Body:       entry.chunks[pageNum-1],
			TmuxTarget: entry.tmuxTarget,
			Page:       pageNum,
			TotalPages: len(entry.chunks),
		})
		kb := buildPageKeyboard(pageNum, len(entry.chunks))
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
		return c.Respond(&tele.CallbackResponse{Text: "✅ " + decision})
	})
	bot.Handle(&tele.InlineButton{Unique: "tool"}, func(c tele.Context) error {
		parts := strings.SplitN(c.Data(), "|", 2)
		if len(parts) < 2 {
			return c.Respond(&tele.CallbackResponse{Text: "Invalid data"})
		}
		toolName := parts[0]
		switch toolName {
		case "AskUserQuestion":
			optIdx, _ := strconv.Atoi(parts[1])
			if err := selectToolOption(c.Message().ID, optIdx); err != nil {
				return c.Respond(&tele.CallbackResponse{Text: err.Error()})
			}
			logger.Info(fmt.Sprintf("AskUserQuestion option selected via TG: msg_id=%d option=%d", c.Message().ID, optIdx))
		}
		bot.React(c.Message().Chat, c.Message(), tele.ReactionOptions{
			Reactions: []tele.Reaction{{Type: "emoji", Emoji: "✍"}},
		})
		return c.Respond()
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		var msg struct {
			Event      string `json:"event"`
			SessionID  string `json:"sessionId"`
			Project    string `json:"project"`
			Body       string `json:"body"`
			TmuxTarget string `json:"tmuxTarget"`
			ToolName   string `json:"toolName"`
			ToolInput  string `json:"toolInput"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, "Invalid JSON", 400)
			return
		}
		bodyPreview := msg.Body
		if len(bodyPreview) > 200 {
			bodyPreview = bodyPreview[:200]
		}
		logger.Debug(fmt.Sprintf("Received hook: %s [%s] body=%s", msg.Event, msg.Project, bodyPreview))
		chatID := pairing.GetDefaultChatID()
		if chatID == "" {
			logger.Error("No paired chat to send notifications to")
			w.WriteHeader(200)
			w.Write([]byte("No paired chat"))
			return
		}
		chat := &tele.Chat{ID: int64(0)}
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		chat.ID = chatIDInt
		switch msg.Event {
		case "SessionStart":
			if msg.TmuxTarget == "" {
				w.WriteHeader(200)
				w.Write([]byte("OK"))
				return
			}
			text := notify.BuildNotificationText(notify.NotificationData{
				Event:      "SessionStart",
				Project:    msg.Project,
				TmuxTarget: msg.TmuxTarget,
			})
			_, err = bot.Send(chat, text)
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
				http.Error(w, "Send failed", 500)
				return
			}
			logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s]", chatID, msg.Event, msg.Project))
		case "SessionEnd":
			text := notify.BuildNotificationText(notify.NotificationData{
				Event:      "SessionEnd",
				Project:    msg.Project,
				TmuxTarget: msg.TmuxTarget,
			})
			_, err = bot.Send(chat, text)
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
			} else {
				logger.Info(fmt.Sprintf("Notification sent to chat %s: SessionEnd [%s]", chatID, msg.Project))
			}
			pages.cleanupSession(msg.SessionID)
			logger.Info(fmt.Sprintf("Cleaned up session %s", msg.SessionID))
		case "AskUserQuestion":
			var toolInput struct {
				Questions []struct {
					Question string `json:"question"`
					Header   string `json:"header"`
					Options  []struct {
						Label       string `json:"label"`
						Description string `json:"description"`
					} `json:"options"`
					MultiSelect bool `json:"multiSelect"`
				} `json:"questions"`
			}
			json.Unmarshal([]byte(msg.ToolInput), &toolInput)
			if len(toolInput.Questions) == 0 {
				w.WriteHeader(200)
				return
			}
			q := toolInput.Questions[0]
			var optLabels []string
			for _, o := range q.Options {
				optLabels = append(optLabels, o.Label)
			}
			text := notify.BuildQuestionText(notify.QuestionData{
				Project:    msg.Project,
				TmuxTarget: msg.TmuxTarget,
				Question:   q.Question,
				Options:    optLabels,
			})
			markup := &tele.ReplyMarkup{}
			var buttons []tele.Btn
			for i, o := range q.Options {
				buttons = append(buttons, markup.Data(o.Label, "tool", fmt.Sprintf("AskUserQuestion|%d", i)))
			}
			var rows []tele.Row
			for i := 0; i < len(buttons); i += 2 {
				if i+1 < len(buttons) {
					rows = append(rows, markup.Row(buttons[i], buttons[i+1]))
				} else {
					rows = append(rows, markup.Row(buttons[i]))
				}
			}
			markup.Inline(rows...)
			sent, err := bot.Send(chat, text, markup)
			if err != nil {
				logger.Error(fmt.Sprintf("Failed to send question: %v", err))
				http.Error(w, "Send failed", 500)
				return
			}
			toolNotifs.store(sent.ID, &toolNotifyEntry{
				tmuxTarget: msg.TmuxTarget,
				toolName:   "AskUserQuestion",
				numOptions: len(q.Options),
			})
			logger.Info(fmt.Sprintf("AskUserQuestion sent: msg_id=%d options=%d", sent.ID, len(q.Options)))
		default:
			headerLen := notify.HeaderLen(notify.NotificationData{
				Event:      msg.Event,
				Project:    msg.Project,
				TmuxTarget: msg.TmuxTarget,
			})
			maxBodyRunes := 4000 - headerLen - 100
			chunks := splitBody(msg.Body, maxBodyRunes)
			if len(chunks) <= 1 {
				text := notify.BuildNotificationText(notify.NotificationData{
					Event:      msg.Event,
					Project:    msg.Project,
					Body:       msg.Body,
					TmuxTarget: msg.TmuxTarget,
				})
				_, err = bot.Send(chat, text)
				if err != nil {
					logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
					http.Error(w, "Send failed", 500)
					return
				}
				logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s]", chatID, msg.Event, msg.Project))
			} else {
				text := notify.BuildNotificationText(notify.NotificationData{
					Event:      msg.Event,
					Project:    msg.Project,
					Body:       chunks[0],
					TmuxTarget: msg.TmuxTarget,
					Page:       1,
					TotalPages: len(chunks),
				})
				kb := buildPageKeyboard(1, len(chunks))
				sent, err := bot.Send(chat, text, kb)
				if err != nil {
					logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
					http.Error(w, "Send failed", 500)
					return
				}
				pages.store(sent.ID, msg.SessionID, &pageEntry{
					chunks:     chunks,
					event:      msg.Event,
					project:    msg.Project,
					tmuxTarget: msg.TmuxTarget,
				})
				logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s] (%d pages, msg_id=%d)", chatID, msg.Event, msg.Project, len(chunks), sent.ID))
			}
		}
		w.WriteHeader(200)
		w.Write([]byte("OK"))
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
		chatID := pairing.GetDefaultChatID()
		if chatID == "" {
			http.Error(w, "no paired chat", 400)
			return
		}
		chat := &tele.Chat{ID: int64(0)}
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		chat.ID = chatIDInt
		text := notify.BuildNotificationText(notify.NotificationData{
			Event:      entry.event,
			Project:    entry.project,
			Body:       entry.chunks[pageNum-1],
			TmuxTarget: entry.tmuxTarget,
			Page:       pageNum,
			TotalPages: len(entry.chunks),
		})
		kb := buildPageKeyboard(pageNum, len(entry.chunks))
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
	mux.HandleFunc("/permission", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			Event       string `json:"event"`
			ToolName    string `json:"toolName"`
			ToolInput   string `json:"toolInput"`
			Suggestions string `json:"suggestions"`
			Project     string `json:"project"`
			TmuxTarget  string `json:"tmuxTarget"`
		}
		if json.Unmarshal(body, &msg) != nil {
			http.Error(w, "Invalid JSON", 400)
			return
		}
		logger.Debug(fmt.Sprintf("Permission request: tool=%s project=%s", msg.ToolName, msg.Project))
		chatID := pairing.GetDefaultChatID()
		if chatID == "" {
			w.WriteHeader(200)
			return
		}
		chat := &tele.Chat{ID: 0}
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		chat.ID = chatIDInt
		var toolInput map[string]interface{}
		json.Unmarshal([]byte(msg.ToolInput), &toolInput)
		text := notify.BuildPermissionText(notify.PermissionData{
			Project:    msg.Project,
			TmuxTarget: msg.TmuxTarget,
			ToolName:   msg.ToolName,
			ToolInput:  toolInput,
		})
		markup := &tele.ReplyMarkup{}
		row1 := []tele.Btn{
			markup.Data("✅ Allow", "perm", "allow"),
			markup.Data("❌ Deny", "perm", "deny"),
		}
		var suggestions []json.RawMessage
		json.Unmarshal([]byte(msg.Suggestions), &suggestions)
		var row2 []tele.Btn
		for i, s := range suggestions {
			var sug struct {
				Type string `json:"type"`
				Tool string `json:"tool"`
			}
			json.Unmarshal(s, &sug)
			label := "🔓 Always Allow"
			if sug.Tool != "" {
				label += " " + sug.Tool
			}
			row2 = append(row2, markup.Data(label, "perm", fmt.Sprintf("s%d", i)))
		}
		if len(row2) > 0 {
			markup.Inline(markup.Row(row1...), markup.Row(row2...))
		} else {
			markup.Inline(markup.Row(row1...))
		}
		sent, err := bot.Send(chat, text, markup)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send permission message: %v", err))
			w.WriteHeader(200)
			return
		}
		logger.Info(fmt.Sprintf("Permission request sent: tool=%s (msg_id=%d)", msg.ToolName, sent.ID))
		suggestionsRaw, _ := json.Marshal(suggestions)
		ch := pendingPerms.create(sent.ID, msg.TmuxTarget, suggestionsRaw)
		select {
		case d := <-ch:
			pendingPerms.cleanup(sent.ID)
			logger.Info(fmt.Sprintf("Permission resolved: msg_id=%d behavior=%s", sent.ID, d.Behavior))
			respJSON, _ := json.Marshal(d)
			w.Header().Set("Content-Type", "application/json")
			w.Write(respJSON)
		case <-time.After(110 * time.Second):
			pendingPerms.cleanup(sent.ID)
			logger.Info(fmt.Sprintf("Permission timed out: msg_id=%d", sent.ID))
			w.WriteHeader(200)
		}
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
		respJSON, _ := json.Marshal(d)
		w.Header().Set("Content-Type", "application/json")
		w.Write(respJSON)
	})
	mux.HandleFunc("/tool/respond", func(w http.ResponseWriter, r *http.Request) {
		msgID, _ := strconv.Atoi(r.URL.Query().Get("msg_id"))
		tool := r.URL.Query().Get("tool")
		switch tool {
		case "AskUserQuestion":
			optIdx, _ := strconv.Atoi(r.URL.Query().Get("option"))
			if err := selectToolOption(msgID, optIdx); err != nil {
				http.Error(w, err.Error(), 404)
				return
			}
			logger.Info(fmt.Sprintf("AskUserQuestion option selected via API: msg_id=%d option=%d", msgID, optIdx))
		default:
			http.Error(w, "unsupported tool", 400)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("OK"))
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
	logger.Info("Starting tg-cli bot...")
	bot.Start()
}

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
		text := notify.BuildNotificationText(notify.NotificationData{
			Event:      msg.Event,
			Project:    msg.Project,
			Body:       msg.Body,
			TmuxTarget: msg.TmuxTarget,
		})
		chatIDInt, _ := strconv.ParseInt(chatID, 10, 64)
		_, err = bot.Send(&tele.Chat{ID: chatIDInt}, text)
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to send notification: %v", err))
			http.Error(w, "Send failed", 500)
			return
		}
		logger.Info(fmt.Sprintf("Notification sent to chat %s: %s [%s]", chatID, msg.Event, msg.Project))
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

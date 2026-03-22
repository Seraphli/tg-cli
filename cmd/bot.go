package cmd

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
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
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/pairing"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	tele "gopkg.in/telebot.v3"
)

func startTypingLoop(ctx context.Context, bot *tele.Bot) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			creds, err := config.LoadCredentials()
			if err != nil {
				continue
			}
			anyUnboundRunning := false
			sentChats := make(map[int64]bool)
			for _, info := range sessionState.all() {
				if !isSessionRunning(info.tmuxTarget) {
					continue
				}
				// Check name route map
				if info.name != "" {
					if route, ok := creds.NameRouteMap[info.name]; ok {
						key := route.ChatID*1000 + int64(route.TopicID)
						if !sentChats[key] {
							if route.TopicID > 0 {
								bot.Notify(&tele.Chat{ID: route.ChatID}, tele.Typing, route.TopicID)
							} else {
								bot.Notify(&tele.Chat{ID: route.ChatID}, tele.Typing)
							}
							sentChats[key] = true
						}
						continue
					}
				}
				anyUnboundRunning = true
			}
			if anyUnboundRunning {
				defaultChatIDStr := pairing.GetDefaultChatID()
				if defaultChatIDStr != "" {
					chatID, _ := strconv.ParseInt(defaultChatIDStr, 10, 64)
					if chatID != 0 && !sentChats[chatID] {
						bot.Notify(&tele.Chat{ID: chatID}, tele.Typing)
					}
				}
			}
		}
	}
}


var BotCmd = &cobra.Command{
	Use:   "bot",
	Short: "Start the Telegram bot with hook HTTP server",
	Run:   runBot,
}

var Version string

var (
	debugFlag      bool
	portFlag       int
	tmuxServerFlag string
)

func init() {
	BotCmd.Flags().BoolVar(&debugFlag, "debug", false, "Enable debug mode")
	BotCmd.Flags().IntVar(&portFlag, "port", 0, "HTTP server port (overrides config)")
	BotCmd.Flags().StringVar(&tmuxServerFlag, "tmux-server", "", "tmux server socket name (-L flag)")
}

func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for hook and pending paths (CC hooks don't send auth)
		if strings.HasPrefix(r.URL.Path, "/hook/") || strings.HasPrefix(r.URL.Path, "/pending/") {
			next.ServeHTTP(w, r)
			return
		}
		// Skip auth for localhost requests
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host == "127.0.0.1" || host == "::1" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func runBot(cmd *cobra.Command, args []string) {
	if tmuxServerFlag != "" {
		injector.ServerName = tmuxServerFlag
	}
	logPath := filepath.Join(config.GetConfigDir(), "bot.log")
	logger.Init(logPath, debugFlag)
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
		tele.Command{Text: "bot_escape", Description: "Send Escape to interrupt Claude"},
		tele.Command{Text: "bot_routes", Description: "Show route bindings"},
		tele.Command{Text: "bot_bind", Description: "Bind agent name to this chat/topic"},
		tele.Command{Text: "bot_unbind", Description: "Unbind an agent name route"},
		tele.Command{Text: "bot_name", Description: "Set agent name for a session"},
		tele.Command{Text: "bot_names", Description: "List and name active sessions"},
		tele.Command{Text: "bot_cwd", Description: "Configure CWD source (tmux/payload)"},
		tele.Command{Text: "resume", Description: "Resume a previous Claude Code session"},
		tele.Command{Text: "bot_verbose", Description: "Toggle tool notifications on/off"},
		tele.Command{Text: "bot_tools", Description: "Configure which tools send notifications"},
		tele.Command{Text: "bot_new", Description: "Launch new Claude Code session"},
		tele.Command{Text: "bot_usage", Description: "Show CC usage limits"},
		tele.Command{Text: "bot_merge", Description: "Merge multiple messages before sending"},
		tele.Command{Text: "bot_voice", Description: "Voice transcription settings"},
		tele.Command{Text: "bot_cron", Description: "Manage cron scheduled tasks"},
		tele.Command{Text: "bot_mailbox", Description: "Bind/unbind mailbox group"},
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
	// Register known slash commands for markdown renderer
	cmds := make(map[string]bool)
	for k := range ccBuiltinCommands {
		cmds[k] = true
	}
	for c := range customCmds {
		cmds[c] = true
	}
	markdown.SlashCommands = cmds
	bot.SetCommands(commands)
	// Register all Telegram handlers
	registerTGHandlers(bot, &creds)
	// Scan pending directory to rebuild in-memory state after restart
	scanPendingDir(bot, &creds)
	// Restore persisted sessions and clean up stale routes
	sessionState.loadFromFile()
	sessionState.validateAlive()
	cleanStaleRoutes(bot)
	cronJobs.load()
	mailbox.load()
	injectQueue.load()
	// Setup HTTP server
	mux := http.NewServeMux()
	registerHTTPHooks(mux, bot, &creds, port)
	registerHTTPAPI(mux, bot, &creds)
	registerMailboxAPI(mux, bot, &creds)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	handler := http.Handler(mux)
	if creds.APIToken != "" {
		handler = authMiddleware(creds.APIToken, mux)
	}
	srv := &http.Server{Addr: addr, Handler: handler}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go startTypingLoop(typingCtx, bot)
	go startCronLoop(typingCtx, bot)
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

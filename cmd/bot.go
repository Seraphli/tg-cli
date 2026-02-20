package cmd

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/pairing"
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
		tele.Command{Text: "bot_escape", Description: "Send Escape to interrupt Claude"},
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
	// Register all Telegram handlers
	registerTGHandlers(bot, &creds)
	// Scan pending directory to rebuild in-memory state after restart
	scanPendingDir(bot, &creds)
	// Setup HTTP server
	mux := http.NewServeMux()
	registerHTTPHooks(mux, bot, &creds, port)
	registerHTTPAPI(mux, bot, &creds)
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

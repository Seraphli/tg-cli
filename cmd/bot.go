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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Seraphli/tg-cli/cmd/api"
	"github.com/Seraphli/tg-cli/cmd/handlers"
	"github.com/Seraphli/tg-cli/cmd/helpers"
	"github.com/Seraphli/tg-cli/cmd/hooks"
	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
	"github.com/Seraphli/tg-cli/internal/markdown"
	"github.com/Seraphli/tg-cli/internal/pairing"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	tele "gopkg.in/telebot.v3"
)

func startTypingLoop(ctx context.Context, bs *BotState) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var cancelPrev context.CancelFunc
	for {
		select {
		case <-ctx.Done():
			if cancelPrev != nil {
				cancelPrev()
			}
			return
		case <-ticker.C:
			// Cancel previous batch (don't wait for slow HTTP)
			if cancelPrev != nil {
				cancelPrev()
			}
			tickCtx, cancel := context.WithCancel(ctx)
			cancelPrev = cancel
			creds, err := config.LoadCredentials()
			if err != nil {
				continue
			}
			var sentChats sync.Map
			for _, info := range bs.SessionState.All() {
				go func(tickCtx context.Context, info stores.SessionInfo) {
					title := helpers.GetPaneTitle(info.TmuxTarget)
					paneRunning := helpers.IsSessionRunning(info.TmuxTarget)
					if !paneRunning {
						typingLog("tick: target=%s title=%q paneRunning=false sent=false", info.TmuxTarget, title)
						if bs.InjectQueue.HasItems(info.TmuxTarget) {
							go flushInjectQueue(bs, info.TmuxTarget)
						}
						return
					}
					// Check if this tick was cancelled (next tick arrived)
					if tickCtx.Err() != nil {
						typingLog("tick: target=%s title=%q paneRunning=true cancelled=true", info.TmuxTarget, title)
						return
					}
					typingLog("tick: target=%s title=%q paneRunning=true sending=true", info.TmuxTarget, title)
					key := sendTypingForTarget(bs.Bot, info, &creds, nil)
					if key != 0 {
						sentChats.Store(key, true)
					}
				}(tickCtx, info)
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
	initTypingLog(config.GetConfigDir())
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
	configDir := config.GetConfigDir()
	// Create BotState with all stores
	bs := &BotState{
		Bot:             bot,
		Creds:           &creds,
		Pages:           stores.NewPageCacheStore(),
		PendingPerms:    stores.NewPendingPermStore(),
		ToolNotifs:      stores.NewToolNotifyStore(),
		PendingFiles:    stores.NewPendingFileStore(),
		SessionState:    stores.NewSessionStateStore(configDir),
		SessionCounts:   stores.NewSessionCountStore(),
		MergeBuffers:    stores.NewMergeBufferStore(configDir),
		InjectQueue:     stores.NewInjectQueueStore(configDir),
		InjectConfirm:   stores.NewInjectConfirmStore(),
		CronJobs:        stores.NewCronJobStore(configDir),
		ReactionTracker: stores.NewReactionTrackerStore(),
		HookRunning:     stores.NewHookRunningStateStore(),
		StopCooldown:    stores.NewStopCooldownStore(),
		SessionWatch:    stores.NewSessionWatchStore(),
		ToolUseMsgs:     stores.NewToolUseMsgStore(),
	}
	bs.SessionState.GetPaneCWD = helpers.GetPaneCWD
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
		tele.Command{Text: "stop", Description: "Send Escape to interrupt Claude"},
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
		tele.Command{Text: "cu", Description: "Check for CC version updates"},
		tele.Command{Text: "check_update", Description: "Check for CC version updates"},
	)
	// CC built-in commands
	for name, desc := range stores.CCBuiltinCommands {
		commands = append(commands, tele.Command{Text: name, Description: desc})
	}
	// CC custom commands
	customCmds := handlers.ScanCustomCommands()
	for name, cmd := range customCmds {
		commands = append(commands, tele.Command{Text: name, Description: cmd.Desc})
	}
	// Register known slash commands for markdown renderer
	cmds := make(map[string]bool)
	for k := range stores.CCBuiltinCommands {
		cmds[k] = true
	}
	for c := range customCmds {
		cmds[c] = true
	}
	markdown.SlashCommands = cmds
	bot.SetCommands(commands)
	// Register all Telegram handlers
	handlers.Register(bs)
	// Scan pending directory to rebuild in-memory state after restart
	hookCB := hooks.Callbacks{
		ResolveChat:              resolveChat,
		ProcessTranscriptUpdates: processTranscriptUpdates,
		SendEventNotification:    sendEventNotification,
		TypingLog:                typingLog,
		FlushInjectQueue:         flushInjectQueue,
		CheckSessionVersion:      checkSessionVersion,
	}
	hooks.ScanPendingDir(bs, hookCB, func(bsArg *BotState) { handlers.ScanLaunchDir(bsArg) })
	// Restore persisted sessions and clean up stale routes
	bs.SessionState.LoadFromFile()
	bs.SessionState.ValidateAlive()
	cleanStaleRoutes(bs)
	bs.CronJobs.Load()
	mailbox.load()
	bs.InjectQueue.Load()
	bs.MergeBuffers.Load()
	// Setup HTTP server
	mux := http.NewServeMux()
	hooks.Register(mux, bs, port, hookCB)
	api.Register(mux, bs)
	registerMailboxAPI(mux, bs)
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
	go startTypingLoop(typingCtx, bs)
	go startCronLoop(typingCtx, bs)
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

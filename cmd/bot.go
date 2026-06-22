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
	"sort"
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
						sid, _ := bs.SessionState.FindByTarget(info.TmuxTarget)
						bs.SessionEvents.DispatchAsync(sid, "flush:ticker", func() error {
							flushInjectQueue(bs, info.TmuxTarget)
							return nil
						})
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
		Client: &http.Client{Timeout: 10 * time.Minute},
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
		Port:            port,
		ConfigDir:       configDir,
		Pages:         stores.NewPageCacheStore(),
		SessionState:  stores.NewSessionStateStore(configDir),
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
		CommandStats:    stores.NewCommandStatsStore(configDir),
		SessionEvents:   stores.NewSessionEventStore(),
		AtChannels:      stores.NewAtChannelStore(configDir),
		CompactTools:    stores.NewCompactToolStore(),
		Streams:         stores.NewStreamStore(),
		PendingWait:     stores.NewPendingWaitStore(),
		NotifOpQueue:    stores.NewNotifOpQueue(),
	}
	defer bs.NotifOpQueue.Close()
	bs.SessionState.GetPaneCWD = helpers.GetPaneCWD
	if err := bs.CommandStats.LoadFromDisk(); err != nil {
		logger.Error(fmt.Sprintf("Failed to load command stats: %v", err))
	}
	// Define base bot commands in registration order (used as tiebreak and default).
	// New entries for /t, /reload, /r, /u, /p are included here.
	baseBotCommands := []tele.Command{
		{Text: "bot_start", Description: "Show welcome message"},
		{Text: "bot_pair", Description: "Pair this chat with the bot"},
		{Text: "bot_settings", Description: "Bot settings and management"},
		{Text: "bot_capture", Description: "Capture tmux pane content"},
		{Text: "p", Description: "Capture tmux pane content"},
		{Text: "bot_escape", Description: "Send Escape to interrupt Claude"},
		{Text: "stop", Description: "Send Escape to interrupt Claude"},
		{Text: "t", Description: "Send Escape to interrupt Claude"},
		{Text: "reload", Description: "Reload CC session (exit and resume)"},
		{Text: "r", Description: "Reload CC session (exit and resume)"},
		{Text: "resume", Description: "Resume a previous Claude Code session"},
		{Text: "bot_new", Description: "Launch new Claude Code session"},
		{Text: "bot_usage", Description: "Show CC usage limits"},
		{Text: "u", Description: "Show CC usage limits"},
		{Text: "bot_merge", Description: "Merge multiple messages before sending"},
		{Text: "m", Description: "Merge multiple messages before sending"},
		{Text: "cu", Description: "Check for CC version updates"},
		{Text: "check_update", Description: "Check for CC version updates"},
		{Text: "bot_at", Description: "Open @ channel with another session"},
	}
	pinnedCommands := []string{"u", "t", "r", "p", "m"}
	customCmds := handlers.ScanCustomCommands()
	buildCommands := func() []tele.Command {
		return buildSortedCommands(baseBotCommands, pinnedCommands, bs.CommandStats.GetAll(), customCmds)
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
	bot.SetCommands(buildCommands())
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
		StreamFlush:              streamFlush,
	}
	hooks.ScanPendingDir(bs, hookCB, func(bsArg *BotState) { handlers.ScanLaunchDir(bsArg) })
	// Restore persisted sessions and clean up stale routes
	bs.SessionState.LoadFromFile()
	bs.SessionState.ValidateAlive()
	cleanStaleRoutes(bs)
	bs.CronJobs.Load()
	mailbox.load()
	bs.InjectQueue.Load()
	bs.AtChannels.Load()
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
	// Periodic ticker: re-sort TG command menu based on usage stats when there are new counts
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !bs.CommandStats.IsDirty() {
					continue
				}
				if err := bs.CommandStats.SaveToDisk(); err != nil {
					logger.Error(fmt.Sprintf("Failed to save command stats: %v", err))
				}
				bot.SetCommands(buildCommands())
				logger.Info("Command menu re-sorted based on usage stats")
			}
		}
	}()
	// Periodic ticker: sweep undelivered terminal wait entries (Resolved but never collected by a
	// reconnecting hook) so PendingWaitStore does not leak entries until restart.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, uuid := range bs.PendingWait.SweepUndelivered(60) {
					bs.PendingWait.Remove(uuid)
					logger.Info(fmt.Sprintf("Swept undelivered pending wait entry: uuid=%s", uuid))
				}
			}
		}
	}()
	defer stop()
	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go startTypingLoop(typingCtx, bs)
	go startCronLoop(typingCtx, bs)
	go startStreamLoop(typingCtx, bs)
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

// buildSortedCommands assembles the TG command menu with pinned entries first,
// then base commands sorted by usage count (desc, tiebreak by original registration order),
// then CC built-in commands, then CC custom commands.
func buildSortedCommands(base []tele.Command, pinned []string, counts map[string]int, customCmds map[string]stores.CustomCmd) []tele.Command {
	baseIndex := make(map[string]int, len(base))
	for i, cmd := range base {
		baseIndex[cmd.Text] = i
	}
	used := make(map[string]bool)
	var result []tele.Command
	for _, name := range pinned {
		if i, ok := baseIndex[name]; ok && !used[name] {
			result = append(result, base[i])
			used[name] = true
		}
	}
	type rankEntry struct {
		cmd   tele.Command
		count int
		index int
	}
	var remaining []rankEntry
	for i, cmd := range base {
		if used[cmd.Text] {
			continue
		}
		remaining = append(remaining, rankEntry{cmd: cmd, count: counts[cmd.Text], index: i})
	}
	sort.SliceStable(remaining, func(a, b int) bool {
		if remaining[a].count != remaining[b].count {
			return remaining[a].count > remaining[b].count
		}
		return remaining[a].index < remaining[b].index
	})
	for _, r := range remaining {
		result = append(result, r.cmd)
	}
	for name, desc := range stores.CCBuiltinCommands {
		result = append(result, tele.Command{Text: name, Description: desc})
	}
	for name, cmd := range customCmds {
		result = append(result, tele.Command{Text: name, Description: cmd.Desc})
	}
	return result
}

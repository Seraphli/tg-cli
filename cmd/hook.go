package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var HookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Hook command called by Claude Code (reads stdin payload)",
	Run:   runHook,
}

var hookPortFlag int
var hookBackendFlag string

func init() {
	HookCmd.Flags().IntVar(&hookPortFlag, "port", 0, "HTTP server port")
	HookCmd.Flags().StringVar(&hookBackendFlag, "backend", "cc", "Backend CLI type (cc or codex)")
}

// hookLog appends a debug line to bot.log.
func hookLog(format string, args ...interface{}) {
	logPath := filepath.Join(config.GetConfigDir(), "bot.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().Format("2006-01-02T15:04:05-07:00")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(f, "[%s] [PID=%d] [HOOK] %s\n", ts, os.Getpid(), msg)
}

func hookExit(code int, reason string) {
	hookLog("exit %d: %s", code, reason)
	os.Exit(code)
}

func detectTmuxTarget() string {
	tmuxPane := os.Getenv("TMUX_PANE")
	if tmuxPane == "" {
		return ""
	}
	tmuxEnv := os.Getenv("TMUX")
	if tmuxEnv != "" {
		parts := strings.SplitN(tmuxEnv, ",", 2)
		return tmuxPane + "@" + parts[0]
	}
	return tmuxPane
}

// generateUUID generates a random hex UUID using crypto/rand.
func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ndjsonFrame is the parsed structure of each line from the /pending/connect stream.
type ndjsonFrame struct {
	Type    string          `json:"type"`
	Output  json.RawMessage `json:"output"`
	MsgID   int             `json:"msg_id"`
	ChatID  int64           `json:"chat_id"`
	TopicID int             `json:"topic_id"`
}

// postWithUpgradeRetry posts to url with body, retrying on ECONNREFUSED if an upgrade is active.
// Retries every 2s up to a 25s cap. Returns the first successful response or the last error.
func postWithUpgradeRetry(ctx context.Context, client *http.Client, url string, body []byte) (*http.Response, error) {
	const retryCap = 25 * time.Second
	start := time.Now()
	for {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		if !errors.Is(err, syscall.ECONNREFUSED) || !config.UpgradeFlagActive() {
			return nil, err
		}
		if time.Since(start) >= retryCap {
			return nil, err
		}
		time.Sleep(2 * time.Second)
	}
}

func runHook(cmd *cobra.Command, args []string) {
	// Skip all hook processing for cron-spawned Claude sessions
	if os.Getenv("TG_CLI_CRON") == "1" {
		os.Exit(0)
	}
	hookLog("version=%s", Version)
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		hookExit(1, fmt.Sprintf("stdin read error: %v", err))
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		hookExit(1, "empty stdin")
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		hookExit(1, fmt.Sprintf("JSON parse error: %v", err))
	}
	hookLog("CC stdin payload: %s", raw)
	// Extract event name
	event, _ := payload["hook_event_name"].(string)
	if event == "" {
		hookExit(1, "no hook_event_name in payload")
	}
	hookLog("event=%s", event)
	// Add computed fields (CC doesn't include these)
	payload["tmux_target"] = detectTmuxTarget()
	if cwd, ok := payload["cwd"].(string); ok && cwd != "" {
		payload["project"] = filepath.Base(cwd)
	}
	payload["backend"] = hookBackendFlag
	// Determine port
	port := hookPortFlag
	if port == 0 {
		creds, _ := config.LoadCredentials()
		port = creds.Port
	}
	if port == 0 {
		port = 12500
	}
	// Marshal enriched payload
	enrichedJSON, _ := json.Marshal(payload)

	// PermissionRequest: use streaming long-connection to bot
	if event == "PermissionRequest" {
		uuid := generateUUID()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Watch for parent process change (CC died → hook orphaned)
		origPPID := os.Getppid()
		go func() {
			for {
				time.Sleep(500 * time.Millisecond)
				if ctx.Err() != nil {
					return
				}
				if os.Getppid() != origPPID {
					hookLog("parent process changed: orig=%d current=%d (CC exited)", origPPID, os.Getppid())
					cancel()
					return
				}
			}
		}()

		// Signal handler: best-effort cancel then exit
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			sig := <-sigCh
			hookLog("received signal: %v", sig)
			cancelClient := &http.Client{Timeout: 2 * time.Second}
			cancelURL := fmt.Sprintf("http://127.0.0.1:%d/pending/cancel?uuid=%s", port, uuid)
			cancelClient.Post(cancelURL, "", nil)
			cancel()
			hookExit(0, "signal cleanup")
		}()

		// Reconnect state (populated after first registered frame)
		var msgID int
		var chatID int64
		var topicID int

		// Streaming connect loop with upgrade retry
		const retryCap = 25 * time.Second
		start := time.Now()
		connectClient := &http.Client{Timeout: 0}
		for {
			if ctx.Err() != nil {
				hookExit(0, "parent exited")
			}
			connectURL := fmt.Sprintf("http://127.0.0.1:%d/pending/connect?uuid=%s", port, uuid)
			if msgID != 0 {
				connectURL = fmt.Sprintf("%s&msg_id=%d&chat_id=%d&topic_id=%d", connectURL, msgID, chatID, topicID)
			}
			req, reqErr := http.NewRequestWithContext(ctx, "POST", connectURL, bytes.NewReader(enrichedJSON))
			if reqErr != nil {
				hookExit(1, fmt.Sprintf("build request error: %v", reqErr))
			}
			req.Header.Set("Content-Type", "application/json")
			resp, respErr := connectClient.Do(req)
			if respErr != nil {
				if ctx.Err() != nil {
					hookExit(0, "parent exited")
				}
				if errors.Is(respErr, syscall.ECONNREFUSED) && time.Since(start) < retryCap {
					hookLog("connect ECONNREFUSED, retrying: %v", respErr)
					time.Sleep(2 * time.Second)
					continue
				}
				hookExit(1, fmt.Sprintf("connect error: %v", respErr))
			}

			// Read ndjson frames line by line
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MB buffer for large payloads
			terminal := false
			for scanner.Scan() {
				line := scanner.Bytes()
				if len(line) == 0 {
					continue
				}
				var frame ndjsonFrame
				if err := json.Unmarshal(line, &frame); err != nil {
					hookLog("frame parse error: %v line=%s", err, string(line))
					continue
				}
				switch frame.Type {
				case "registered":
					msgID = frame.MsgID
					chatID = frame.ChatID
					topicID = frame.TopicID
					hookLog("registered: uuid=%s msg_id=%d", uuid, msgID)
				case "answer":
					hookLog("answered: %s", string(frame.Output))
					resp.Body.Close()
					fmt.Print(string(frame.Output))
					hookExit(0, "answered")
				case "cancel":
					hookLog("cancelled by bot (session continued in TUI)")
					resp.Body.Close()
					hookExit(0, "cancelled")
				default:
					hookLog("unknown frame type: %s", frame.Type)
				}
			}
			resp.Body.Close()
			if scanErr := scanner.Err(); scanErr != nil {
				hookLog("scanner error: %v", scanErr)
			}
			if terminal {
				break
			}
			if ctx.Err() != nil {
				hookExit(0, "parent exited")
			}
			// Stream dropped (EOF without terminal) — reconnect if CC is alive
			start = time.Now()
			hookLog("stream dropped, reconnecting")
			time.Sleep(2 * time.Second)
			continue
		}
		hookExit(0, "done")
	}

	// Other events: POST with upgrade retry
	url := fmt.Sprintf("http://127.0.0.1:%d/hook/%s", port, event)
	hookLog("POST %s body: %s", url, string(enrichedJSON))
	client := &http.Client{Timeout: 0}
	resp, err := postWithUpgradeRetry(context.Background(), client, url, enrichedJSON)
	if err != nil {
		hookExit(1, fmt.Sprintf("HTTP error: %v", err))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	hookLog("bot response (%d): %s", resp.StatusCode, string(respBody))
	if resp.StatusCode != 200 {
		hookExit(1, fmt.Sprintf("HTTP status %d", resp.StatusCode))
	}
	// If response is JSON hook output, print to stdout for CC
	respStr := strings.TrimSpace(string(respBody))
	if len(respStr) > 0 && respStr[0] == '{' {
		hookLog("stdout to CC: %s", respStr)
		fmt.Print(respStr)
	}
	hookExit(0, fmt.Sprintf("event=%s", event))
}

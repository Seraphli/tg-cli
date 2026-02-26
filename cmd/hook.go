package cmd

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

func init() {
	HookCmd.Flags().IntVar(&hookPortFlag, "port", 0, "HTTP server port")
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

// generateUUID generates a random hex UUID using crypto/rand
func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// PendingFileHook is a local copy of PendingFile struct for hook.go process
type PendingFileHook struct {
	UUID      string          `json:"uuid"`
	Event     string          `json:"event"`
	ToolName  string          `json:"tool_name"`
	Status    string          `json:"status"`
	Payload   json.RawMessage `json:"payload"`
	TgMsgID   int             `json:"tg_msg_id"`
	TgChatID  int64           `json:"tg_chat_id"`
	SessionID string          `json:"session_id"`
	CCOutput  json.RawMessage `json:"cc_output"`
	CreatedAt time.Time       `json:"created_at"`
	HookPID   int             `json:"hook_pid"`
}

// writePendingFileHook atomically writes a pending file (hook.go local version)
func writePendingFileHook(path string, pf *PendingFileHook) error {
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

func runHook(cmd *cobra.Command, args []string) {
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

	// PermissionRequest: use file-based communication instead of blocking HTTP
	toolName, _ := payload["tool_name"].(string)
	if event == "PermissionRequest" {
		uuid := generateUUID()
		dir := filepath.Join("/tmp", filepath.Base(config.GetConfigDir()), "pending")
		os.MkdirAll(dir, 0755)
		pendingPath := filepath.Join(dir, uuid+".json")

		// 1. Write pending file
		pf := PendingFileHook{
			UUID:      uuid,
			Event:     event,
			ToolName:  toolName,
			Status:    "pending",
			Payload:   enrichedJSON,
			CreatedAt: time.Now(),
			HookPID:   os.Getpid(),
		}
		if err := writePendingFileHook(pendingPath, &pf); err != nil {
			hookExit(1, fmt.Sprintf("write pending file error: %v", err))
		}
		hookLog("pending file created: %s", pendingPath)

		// 2. Signal handler for cleanup
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			sig := <-sigCh
			hookLog("received signal: %v (ppid=%d)", sig, os.Getppid())
			cancelClient := &http.Client{Timeout: 2 * time.Second}
			cancelURL := fmt.Sprintf("http://127.0.0.1:%d/pending/cancel?uuid=%s", port, uuid)
			hookLog("POST %s (cancel)", cancelURL)
			cancelClient.Post(cancelURL, "", nil)
			os.Remove(pendingPath)
			hookExit(0, "signal cleanup")
		}()

		// 3. Notify bot (fire-and-forget, 5s timeout)
		notifyClient := &http.Client{Timeout: 5 * time.Second}
		notifyURL := fmt.Sprintf("http://127.0.0.1:%d/pending/notify?uuid=%s", port, uuid)
		hookLog("POST %s (fire-and-forget)", notifyURL)
		notifyClient.Post(notifyURL, "application/json", bytes.NewReader(enrichedJSON))

		// 4. Poll for status=answered
		hookLog("polling for answer... (ppid=%d)", os.Getppid())
		for {
			hookLog("poll tick: ppid=%d", os.Getppid())
			data, err := os.ReadFile(pendingPath)
			if err == nil && len(data) > 0 {
				var pf PendingFileHook
				if json.Unmarshal(data, &pf) == nil {
					if pf.Status == "answered" && pf.CCOutput != nil {
						hookLog("answered: %s", string(pf.CCOutput))
						fmt.Print(string(pf.CCOutput))
						os.Remove(pendingPath)
						hookExit(0, "answered")
					}
					if pf.Status == "cancelled" {
						hookLog("cancelled by bot (session continued in TUI)")
						os.Remove(pendingPath)
						hookExit(0, "cancelled")
					}
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Other events: use existing HTTP POST
	url := fmt.Sprintf("http://127.0.0.1:%d/hook/%s", port, event)
	hookLog("POST %s body: %s", url, string(enrichedJSON))
	client := &http.Client{Timeout: 0}
	resp, err := client.Post(url, "application/json", bytes.NewReader(enrichedJSON))
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

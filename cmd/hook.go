package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	url := fmt.Sprintf("http://127.0.0.1:%d/hook/%s", port, event)
	hookLog("POST %s body: %s", url, string(enrichedJSON))
	// POST to /hook/{event}
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

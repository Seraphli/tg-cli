package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var tmuxHookEvent string
var tmuxHookSession string
var tmuxHookPane string
var tmuxHookPort int
var tmuxHookHost string
var tmuxHookToken string

// TmuxHookCmd handles tmux lifecycle events by forwarding them to the bot HTTP API.
var TmuxHookCmd = &cobra.Command{
	Use:    "tmux-hook",
	Short:  "Handle tmux lifecycle events (called by tmux hooks)",
	Hidden: true,
	Run:    runTmuxHook,
}

func init() {
	TmuxHookCmd.Flags().StringVar(&tmuxHookEvent, "event", "", "Event name (e.g., pane-died)")
	TmuxHookCmd.Flags().StringVar(&tmuxHookSession, "session", "", "Session name")
	TmuxHookCmd.Flags().StringVar(&tmuxHookPane, "pane", "", "Pane ID (e.g., %0)")
	TmuxHookCmd.Flags().IntVar(&tmuxHookPort, "port", 0, "Bot HTTP port")
	TmuxHookCmd.Flags().StringVar(&tmuxHookHost, "host", "", "Bot API host URL")
	TmuxHookCmd.Flags().StringVar(&tmuxHookToken, "token", "", "API authentication token")
}

func runTmuxHook(cmd *cobra.Command, args []string) {
	if tmuxHookEvent == "" {
		fmt.Fprintln(os.Stderr, "Error: --event required")
		os.Exit(1)
	}
	port := tmuxHookPort
	if port == 0 {
		creds, err := config.LoadCredentials()
		if err == nil && creds.Port != 0 {
			port = creds.Port
		}
	}
	if port == 0 {
		port = 12500
	}
	body, _ := json.Marshal(map[string]string{
		"event":   tmuxHookEvent,
		"session": tmuxHookSession,
		"pane":    tmuxHookPane,
	})
	url := buildAPIURL(tmuxHookHost, port, "/tmux/event")
	resp, err := apiRequest("POST", url, bytes.NewReader(body), tmuxHookToken)
	if err != nil {
		os.Exit(0) // Silently exit if bot not running
	}
	defer resp.Body.Close()
}

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var TmuxManageCmd = &cobra.Command{
	Use:   "tmux",
	Short: "Manage tmux sessions and panes",
}

var (
	tmuxCmdPort    int
	tmuxKillTarget string
	tmuxHost       string
	tmuxToken      string
)

var tmuxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all tmux panes",
	Run:   runTmuxList,
}

var tmuxKillCmd = &cobra.Command{
	Use:   "kill",
	Short: "Kill a tmux pane",
	Run:   runTmuxKill,
}

func init() {
	TmuxManageCmd.PersistentFlags().StringVar(&tmuxHost, "host", "", "Bot API host URL")
	TmuxManageCmd.PersistentFlags().StringVar(&tmuxToken, "token", "", "API authentication token")
	tmuxListCmd.Flags().IntVar(&tmuxCmdPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	tmuxKillCmd.Flags().IntVar(&tmuxCmdPort, "port", 0, "Bot HTTP port (default: from config or 12500)")
	tmuxKillCmd.Flags().StringVar(&tmuxKillTarget, "target", "", "Pane target (e.g., %0 or session:window.pane) (required)")
	TmuxManageCmd.AddCommand(tmuxListCmd)
	TmuxManageCmd.AddCommand(tmuxKillCmd)
}

func getTmuxCmdPort() int {
	if tmuxCmdPort != 0 {
		return tmuxCmdPort
	}
	creds, err := config.LoadCredentials()
	if err == nil && creds.Port != 0 {
		return creds.Port
	}
	return 12500
}

func runTmuxList(cmd *cobra.Command, args []string) {
	port := getTmuxCmdPort()
	url := buildAPIURL(tmuxHost, port, "/tmux/list")
	resp, err := apiRequest("GET", url, nil, tmuxToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Panes []struct {
			Target      string `json:"target"`
			PaneID      string `json:"pane_id"`
			CWD         string `json:"cwd"`
			PID         int    `json:"pid"`
			Command     string `json:"command"`
			SessionName string `json:"session_name"`
			AgentName   string `json:"agent_name"`
		} `json:"panes"`
	}
	json.Unmarshal(body, &result)
	if len(result.Panes) == 0 {
		fmt.Println("No tmux panes found.")
		return
	}
	// Print table header
	fmt.Printf("%-28s %-6s %-14s %-20s %s\n", "TARGET", "PANE", "AGENT", "COMMAND", "CWD")
	fmt.Println(strings.Repeat("-", 90))
	for _, p := range result.Panes {
		agent := p.AgentName
		if agent == "" {
			agent = "-"
		}
		fmt.Printf("%-28s %-6s %-14s %-20s %s\n", p.Target, p.PaneID, agent, p.Command, p.CWD)
	}
}

func runTmuxKill(cmd *cobra.Command, args []string) {
	if tmuxKillTarget == "" {
		fmt.Fprintln(os.Stderr, "Error: --target is required")
		os.Exit(1)
	}
	port := getTmuxCmdPort()
	data, _ := json.Marshal(struct {
		Target string `json:"target"`
	}{tmuxKillTarget})
	url := buildAPIURL(tmuxHost, port, "/tmux/kill")
	resp, err := apiRequest("POST", url, bytes.NewReader(data), tmuxToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot connect to bot: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n", string(respBody))
		os.Exit(1)
	}
	fmt.Printf("Pane %s killed.\n", tmuxKillTarget)
}

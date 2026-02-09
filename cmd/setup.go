package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

var SetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Install hooks into ~/.claude/settings.json",
	Run:   runSetup,
}

var setupPortFlag int

func init() {
	SetupCmd.Flags().IntVar(&setupPortFlag, "port", 0, "HTTP server port (overrides config)")
}

func runSetup(cmd *cobra.Command, args []string) {
	creds, err := config.LoadCredentials()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load credentials: %v\n", err)
		os.Exit(1)
	}
	port := setupPortFlag
	if port == 0 {
		port = creds.Port
	}
	if port == 0 {
		port = 12500
	}
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get executable path: %v\n", err)
		os.Exit(1)
	}
	hookCommand := fmt.Sprintf("TG_CLI_PORT=%d %s hook", port, exePath)
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	var settings map[string]interface{}
	if _, err := os.Stat(settingsPath); err == nil {
		backupPath := settingsPath + ".backup"
		data, _ := os.ReadFile(settingsPath)
		os.WriteFile(backupPath, data, 0644)
		fmt.Printf("Backed up settings to %s\n", backupPath)
		json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}
	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
	}
	hookEntry := map[string]interface{}{
		"matcher": "",
		"hooks": []map[string]interface{}{
			{
				"type":    "command",
				"command": hookCommand,
				"timeout": 5,
			},
		},
	}
	for _, event := range []string{"Stop", "SubagentStop"} {
		existing, ok := hooks[event].([]interface{})
		if !ok {
			existing = []interface{}{}
		}
		var filtered []interface{}
		for _, h := range existing {
			hJSON, _ := json.Marshal(h)
			if !strings.Contains(string(hJSON), "tg-cli") {
				filtered = append(filtered, h)
			}
		}
		filtered = append(filtered, hookEntry)
		hooks[event] = filtered
	}
	settings["hooks"] = hooks
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal settings: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write settings: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Hooks installed to ~/.claude/settings.json")
	fmt.Printf("Hook command: %s\n", hookCommand)
}

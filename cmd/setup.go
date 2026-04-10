package cmd

import (
	_ "embed"
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

// availableTools is the list of tools that can trigger TG notifications.
var availableTools = []string{
	"Edit", "Write", "Bash", "Read", "Glob", "Grep",
	"Agent", "WebFetch", "WebSearch", "MCP", "Skill",
	"TaskCreate", "TaskUpdate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
	"NotebookEdit", "EnterPlanMode", "ExitPlanMode",
	"EnterWorktree", "ExitWorktree", "Other",
}

//go:embed config/cc.json
var ccConfigJSON []byte

//go:embed config/codex.json
var codexConfigJSON []byte

//go:embed commands/tg-cli/cron.md
var cronSkillDoc []byte

//go:embed commands/tg-cli/agent.md
var agentSkillDoc []byte

var SetupCmd = &cobra.Command{
	Use:   "install",
	Short: "Install hooks and skill docs",
	Run:   runSetup,
}

var setupPortFlag int
var setupUninstallFlag bool
var setupSettingsFlag string

func init() {
	SetupCmd.Flags().IntVar(&setupPortFlag, "port", 0, "HTTP server port (overrides config)")
	SetupCmd.Flags().BoolVar(&setupUninstallFlag, "uninstall", false, "Remove hooks for this instance")
	SetupCmd.Flags().StringVar(&setupSettingsFlag, "settings", "", "Target settings file path (default: ~/.claude/settings.json)")
	SetupCmd.Flags().Bool("skip-tmux", false, "Skip tmux hook registration")
	SetupCmd.Flags().String("tmux-server", "", "Tmux server socket name (for -L flag)")
	SetupCmd.Flags().String("tmux-conf", "", "Custom tmux.conf path (default: ~/.tmux.conf)")
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
	home, _ := os.UserHomeDir()
	var hookBin string
	if config.ConfigDir != "" {
		// Custom instance: use the binary installed under that config dir
		hookBin = filepath.Join(config.ConfigDir, "bin", "tg-cli")
	} else {
		// Default instance: use the service binary
		hookBin = installBinPath()
	}
	if _, err := os.Stat(hookBin); err != nil {
		// Fallback to current executable if service binary not found
		hookBin, _ = os.Executable()
	}
	hookCommand := fmt.Sprintf("%s hook --port %d", hookBin, port)
	if config.ConfigDir != "" {
		hookCommand = fmt.Sprintf("%s --config-dir %s hook --port %d", hookBin, config.ConfigDir, port)
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if setupSettingsFlag != "" {
		settingsPath = setupSettingsFlag
	}
	// Require --settings when --config-dir is non-default to prevent polluting production hooks
	defaultSettingsPath := filepath.Join(home, ".claude", "settings.json")
	if settingsPath == defaultSettingsPath && config.ConfigDir != "" && config.ConfigDir != filepath.Join(home, ".tg-cli") {
		fmt.Fprintf(os.Stderr, "Error: --config-dir %s requires --settings to specify the target CC settings file.\n", config.ConfigDir)
		fmt.Fprintf(os.Stderr, "Without --settings, hooks would be written to production %s with test parameters.\n", defaultSettingsPath)
		os.Exit(1)
	}
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
	// Parse CC hooks config template
	ccTemplate := strings.ReplaceAll(string(ccConfigJSON), "HOOK_CMD", hookCommand)
	var ccConfig map[string]interface{}
	json.Unmarshal([]byte(ccTemplate), &ccConfig)
	// Merge hooks
	if ccHooks, ok := ccConfig["hooks"].(map[string]interface{}); ok {
		for event, ourEntries := range ccHooks {
			existing, _ := hooks[event].([]interface{})
			filtered := []interface{}{}
			for _, h := range existing {
				hJSON, _ := json.Marshal(h)
				hStr := string(hJSON)
				if !strings.Contains(hStr, "tg-cli") {
					filtered = append(filtered, h)
					continue
				}
				if config.ConfigDir != "" {
					if strings.Contains(hStr, "--config-dir "+config.ConfigDir) {
						continue
					}
				} else {
					if !strings.Contains(hStr, "--config-dir") {
						continue
					}
				}
				filtered = append(filtered, h)
			}
			if !setupUninstallFlag {
				if ourList, ok := ourEntries.([]interface{}); ok {
					filtered = append(filtered, ourList...)
				}
			}
			hooks[event] = filtered
		}
	}
	settings["hooks"] = hooks
	// Manage permissions
	if ccPerms, ok := ccConfig["permissions"].(map[string]interface{}); ok {
		if allowPerms, ok := ccPerms["allow"].([]interface{}); ok && len(allowPerms) > 0 {
			perms, _ := settings["permissions"].(map[string]interface{})
			if perms == nil {
				perms = make(map[string]interface{})
			}
			if setupUninstallFlag {
				// Remove tg-cli permissions
				if existing, ok := perms["allow"].([]interface{}); ok {
					filtered := []interface{}{}
					for _, p := range existing {
						ps, _ := p.(string)
						isTgCli := false
						for _, allowed := range allowPerms {
							as, _ := allowed.(string)
							if ps == as {
								isTgCli = true
								break
							}
						}
						if !isTgCli {
							filtered = append(filtered, p)
						}
					}
					if len(filtered) > 0 {
						perms["allow"] = filtered
					} else {
						delete(perms, "allow")
					}
				}
				if len(perms) == 0 {
					delete(settings, "permissions")
				} else {
					settings["permissions"] = perms
				}
			} else {
				// Add tg-cli permissions (idempotent)
				existing, _ := perms["allow"].([]interface{})
				for _, allowed := range allowPerms {
					as, _ := allowed.(string)
					found := false
					for _, p := range existing {
						if ps, _ := p.(string); ps == as {
							found = true
							break
						}
					}
					if !found {
						existing = append(existing, as)
					}
				}
				perms["allow"] = existing
				settings["permissions"] = perms
			}
		}
	}
	// Register statusLine command so CC statusbar shows context window usage
	var statusLineCmd string
	if config.ConfigDir != "" {
		statusLineCmd = fmt.Sprintf("%s --config-dir %s statusline", hookBin, config.ConfigDir)
	} else {
		statusLineCmd = fmt.Sprintf("%s statusline", hookBin)
	}
	if setupUninstallFlag {
		// Remove tg-cli statusline from statusLine config
		if existing, ok := settings["statusLine"].(map[string]interface{}); ok {
			if cmd, ok := existing["command"].(string); ok {
				prefix := statusLineCmd + " | "
				if strings.HasPrefix(cmd, prefix) {
					existing["command"] = strings.TrimPrefix(cmd, prefix)
					settings["statusLine"] = existing
				} else if cmd == statusLineCmd {
					delete(settings, "statusLine")
				}
			}
		}
	} else {
		existing, ok := settings["statusLine"].(map[string]interface{})
		if !ok {
			// No existing statusLine — set ours directly
			settings["statusLine"] = map[string]interface{}{
				"type":    "command",
				"command": statusLineCmd,
			}
		} else if cmd, ok := existing["command"].(string); ok {
			if !strings.Contains(cmd, "tg-cli statusline") {
				// Prepend our command and pipe to existing
				existing["command"] = statusLineCmd + " | " + cmd
				settings["statusLine"] = existing
			}
			// If already contains tg-cli statusline: skip (idempotent)
		}
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal settings: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write settings: %v\n", err)
		os.Exit(1)
	}
	// Configure tool notifications (skip during uninstall)
	if !setupUninstallFlag {
		configureToolNotifications()
	}
	instanceDesc := "default"
	if config.ConfigDir != "" {
		instanceDesc = config.ConfigDir
	}
	if setupUninstallFlag {
		fmt.Printf("Hooks uninstalled from %s\n", settingsPath)
		fmt.Printf("Removed hooks for instance: %s\n", instanceDesc)
		// Unregister MCP server
		mcpRm := exec.Command("claude", "mcp", "remove", "tg-cli", "-s", "user")
		if err := mcpRm.Run(); err != nil {
			fmt.Printf("MCP unregistration skipped (claude CLI not found or failed): %v\n", err)
		} else {
			fmt.Println("MCP server unregistered.")
		}
	} else {
		fmt.Printf("Hooks installed to %s\n", settingsPath)
		fmt.Printf("Hook command: %s\n", hookCommand)
		// Register MCP server
		mcpArgs := []string{"mcp", "add", "--scope", "user", "--transport", "stdio", "tg-cli", "--", hookBin, "mcp"}
		if config.ConfigDir != "" {
			mcpArgs = append(mcpArgs, "--config-dir", config.ConfigDir)
		}
		mcpAdd := exec.Command("claude", mcpArgs...)
		if err := mcpAdd.Run(); err != nil {
			fmt.Printf("MCP registration skipped (claude CLI not found or failed): %v\n", err)
		} else {
			fmt.Println("MCP server registered.")
		}
	}
	// Install skill docs
	cmdDir := filepath.Join(home, ".claude", "commands", "tg-cli")
	os.MkdirAll(cmdDir, 0755)
	cronDocPath := filepath.Join(cmdDir, "cron.md")
	if err := os.WriteFile(cronDocPath, cronSkillDoc, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to install skill doc: %v\n", err)
	} else {
		fmt.Printf("Skill doc installed: %s\n", cronDocPath)
	}
	agentDocPath := filepath.Join(cmdDir, "agent.md")
	if err := os.WriteFile(agentDocPath, agentSkillDoc, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to install agent skill doc: %v\n", err)
	} else {
		fmt.Printf("Skill doc installed: %s\n", agentDocPath)
	}
	// Register Codex hooks (write to ~/.codex/hooks.json)
	if !setupUninstallFlag {
		codexHome := os.Getenv("CODEX_HOME")
		if codexHome == "" {
			codexHome = filepath.Join(home, ".codex")
		}
		os.MkdirAll(codexHome, 0755)
		codexHooksPath := filepath.Join(codexHome, "hooks.json")
		// Read template and replace HOOK_CMD placeholder
		codexTemplate := string(codexConfigJSON)
		codexTemplate = strings.ReplaceAll(codexTemplate, "HOOK_CMD", hookCommand)
		// Read existing hooks.json if present
		var codexHooks map[string]interface{}
		if data, err := os.ReadFile(codexHooksPath); err == nil {
			json.Unmarshal(data, &codexHooks)
		}
		if codexHooks == nil {
			codexHooks = make(map[string]interface{})
		}
		// Parse our template
		var ourCodexHooks map[string]interface{}
		json.Unmarshal([]byte(codexTemplate), &ourCodexHooks)
		// Merge: for each event, remove existing tg-cli entries, add ours
		if ourHooksMap, ok := ourCodexHooks["hooks"].(map[string]interface{}); ok {
			existingHooksMap, _ := codexHooks["hooks"].(map[string]interface{})
			if existingHooksMap == nil {
				existingHooksMap = make(map[string]interface{})
			}
			for event, ourEntries := range ourHooksMap {
				existing, _ := existingHooksMap[event].([]interface{})
				// Filter out existing tg-cli entries
				filtered := []interface{}{}
				for _, e := range existing {
					eJSON, _ := json.Marshal(e)
					if !strings.Contains(string(eJSON), "tg-cli") {
						filtered = append(filtered, e)
					}
				}
				// Add our entries
				if ourList, ok := ourEntries.([]interface{}); ok {
					filtered = append(filtered, ourList...)
				}
				existingHooksMap[event] = filtered
			}
			codexHooks["hooks"] = existingHooksMap
		}
		codexData, _ := json.MarshalIndent(codexHooks, "", "  ")
		os.WriteFile(codexHooksPath, codexData, 0644)
		fmt.Printf("Codex hooks installed to %s\n", codexHooksPath)
		// Enable codex_hooks feature flag in config.toml
		codexConfigPath := filepath.Join(codexHome, "config.toml")
		configContent, _ := os.ReadFile(codexConfigPath)
		configStr := string(configContent)
		if !strings.Contains(configStr, "codex_hooks") {
			if strings.Contains(configStr, "[features]") {
				configStr = strings.Replace(configStr, "[features]", "[features]\ncodex_hooks = true", 1)
			} else {
				configStr += "\n[features]\ncodex_hooks = true\n"
			}
			os.WriteFile(codexConfigPath, []byte(configStr), 0644)
			fmt.Println("Codex hooks feature enabled in config.toml")
		}
	} else {
		// Uninstall: remove tg-cli entries from CODEX_HOME/hooks.json
		codexUninstallHome := os.Getenv("CODEX_HOME")
		if codexUninstallHome == "" {
			codexUninstallHome = filepath.Join(home, ".codex")
		}
		codexHooksPath := filepath.Join(codexUninstallHome, "hooks.json")
		if data, err := os.ReadFile(codexHooksPath); err == nil {
			var codexHooks map[string]interface{}
			if json.Unmarshal(data, &codexHooks) == nil {
				if hooksMap, ok := codexHooks["hooks"].(map[string]interface{}); ok {
					for event, entries := range hooksMap {
						if list, ok := entries.([]interface{}); ok {
							filtered := []interface{}{}
							for _, e := range list {
								eJSON, _ := json.Marshal(e)
								if !strings.Contains(string(eJSON), "tg-cli") {
									filtered = append(filtered, e)
								}
							}
							hooksMap[event] = filtered
						}
					}
					codexHooks["hooks"] = hooksMap
					codexData, _ := json.MarshalIndent(codexHooks, "", "  ")
					os.WriteFile(codexHooksPath, codexData, 0644)
					fmt.Printf("Codex hooks uninstalled from %s\n", codexHooksPath)
				}
			}
		}
	}
	skipTmux, _ := cmd.Flags().GetBool("skip-tmux")
	if !skipTmux {
		// Register tmux hooks for session lifecycle events
		hookBinForTmux := installBinPath()
		tmuxServer, _ := cmd.Flags().GetString("tmux-server")
		tmuxConfFlag, _ := cmd.Flags().GetString("tmux-conf")
		tmuxConf := filepath.Join(home, ".tmux.conf")
		if tmuxConfFlag != "" {
			tmuxConf = tmuxConfFlag
		}
		confContent, _ := os.ReadFile(tmuxConf)
		// Remove ALL old tg-cli hook lines from tmux.conf before adding new ones
		var cleanedLines []string
		for _, line := range strings.Split(string(confContent), "\n") {
			if !strings.Contains(line, "tg-cli") {
				cleanedLines = append(cleanedLines, line)
			}
		}
		cleanedConf := strings.TrimRight(strings.Join(cleanedLines, "\n"), "\n")
		os.WriteFile(tmuxConf, []byte(cleanedConf+"\n"), 0644)
		confContent = []byte(cleanedConf)
		for _, event := range []string{"session-created", "session-closed"} {
			hookCmd := fmt.Sprintf("%s tmux-hook --event %s --session '#{hook_session_name}' --port %d", hookBinForTmux, event, port)
			hookShell := fmt.Sprintf("run-shell \"%s\"", hookCmd)
			confLine := fmt.Sprintf("set-hook -g %s '%s'", event, hookShell)
			// Always register runtime hook (overwrite any existing), optionally targeting a specific server
			var tmuxArgs []string
			if tmuxServer != "" {
				tmuxArgs = append(tmuxArgs, "-L", tmuxServer)
			}
			tmuxArgs = append(tmuxArgs, "set-hook", "-g", event, hookShell)
			exec.Command("tmux", tmuxArgs...).Run()
			fmt.Printf("tmux hook registered (%s).\n", event)
			// Always append to tmux.conf (old entries already cleaned above)
			f, err := os.OpenFile(tmuxConf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				f.WriteString("\n# tg-cli tmux lifecycle hook\n" + confLine + "\n")
				f.Close()
				fmt.Printf("tmux hook persisted to %s (%s)\n", tmuxConf, event)
			}
		}
		_ = confContent
	}
}

// configureToolNotifications prompts the user to select which tools trigger TG notifications.
func configureToolNotifications() {
	appCfg, err := config.LoadAppConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load app config: %v\n", err)
		return
	}
	// Build a set of currently selected tools for quick lookup
	currentSet := make(map[string]bool)
	for _, t := range appCfg.ToolNotifyList {
		currentSet[t] = true
	}
	fmt.Println()
	fmt.Println("Tool notification config (select tools to receive TG notifications):")
	// Print tools in a 3-column layout
	for i, tool := range availableTools {
		fmt.Printf("  [%d] %-10s", i+1, tool)
		if (i+1)%3 == 0 {
			fmt.Println()
		}
	}
	if len(availableTools)%3 != 0 {
		fmt.Println()
	}
	// Default to all tools if no selection configured
	if len(appCfg.ToolNotifyList) == 0 {
		appCfg.ToolNotifyList = append([]string{}, availableTools...)
		for _, t := range availableTools {
			currentSet[t] = true
		}
	}
	fmt.Printf("Current: %s\n", strings.Join(appCfg.ToolNotifyList, ", "))
	fmt.Print("Enter numbers (comma-separated), * for all, or press Enter to keep current: ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return
	}
	input := strings.TrimSpace(scanner.Text())
	if input == "" {
		if err := config.SaveAppConfig(appCfg); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save app config: %v\n", err)
		}
		return
	}
	var selected []string
	if input == "*" {
		selected = append(selected, availableTools...)
	} else {
		for _, part := range strings.Split(input, ",") {
			part = strings.TrimSpace(part)
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 || n > len(availableTools) {
				fmt.Fprintf(os.Stderr, "Invalid selection: %q (skipped)\n", part)
				continue
			}
			selected = append(selected, availableTools[n-1])
		}
	}
	appCfg.ToolNotifyList = selected
	if err := config.SaveAppConfig(appCfg); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save app config: %v\n", err)
		return
	}
	if len(selected) > 0 {
		fmt.Printf("Tool notifications configured: %s\n", strings.Join(selected, ", "))
	} else {
		fmt.Println("Tool notifications cleared.")
	}
}

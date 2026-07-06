package cmd

import (
	_ "embed"
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/spf13/cobra"
)

// changedHookEvents compares two hooks maps (event -> entry list) and returns the sorted list of
// event names whose entries differ (added, removed, or modified). Used to report what an upgrade
// actually changed instead of unconditionally claiming "installed".
func changedHookEvents(oldHooks, newHooks map[string]interface{}) []string {
	per := func(h map[string]interface{}) map[string]string {
		out := make(map[string]string)
		for ev, v := range h {
			b, _ := json.Marshal(v)
			out[ev] = string(b)
		}
		return out
	}
	o, n := per(oldHooks), per(newHooks)
	seen := make(map[string]bool)
	var changed []string
	for ev, nv := range n {
		seen[ev] = true
		if o[ev] != nv {
			changed = append(changed, ev)
		}
	}
	for ev := range o {
		if !seen[ev] {
			changed = append(changed, ev)
		}
	}
	sort.Strings(changed)
	return changed
}

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

func applyClaudeSettingsMigrations(settings map[string]interface{}) []string {
	var changed []string
	if v, ok := settings["skipAutoPermissionPrompt"]; !ok || v != true {
		settings["skipAutoPermissionPrompt"] = true
		changed = append(changed, "skipAutoPermissionPrompt")
	}
	return changed
}

func MigrateClaudeSettings(settingsPath string) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}
	var settings map[string]interface{}
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("invalid JSON in %s: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read settings %s: %w", settingsPath, err)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}
	changedKeys := applyClaudeSettingsMigrations(settings)
	if len(changedKeys) == 0 {
		fmt.Println("Claude settings already up-to-date")
		return nil
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		return err
	}
	fmt.Printf("Claude settings: updated %s\n", strings.Join(changedKeys, ", "))
	return nil
}

// mergeCCHooks merges the embedded cc.json hook entries into settings["hooks"], replacing this
// instance's tg-cli hook entries (matched by --config-dir per config.ConfigDir) while preserving
// user-added non-tg-cli entries and other instances' tg-cli entries. Hooks only (not permissions/
// statusLine). hookCommand is substituted for the HOOK_CMD placeholder.
func mergeCCHooks(settings map[string]interface{}, hookCommand string) {
	hooks, ok := settings["hooks"].(map[string]interface{})
	if !ok {
		hooks = make(map[string]interface{})
	}
	ccTemplate := strings.ReplaceAll(string(ccConfigJSON), "HOOK_CMD", hookCommand)
	var ccConfig map[string]interface{}
	json.Unmarshal([]byte(ccTemplate), &ccConfig)
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
			if ourList, ok := ourEntries.([]interface{}); ok {
				filtered = append(filtered, ourList...)
			}
			hooks[event] = filtered
		}
	}
	settings["hooks"] = hooks
}

// InstallCodexHooks registers the tg-cli Codex hooks in CODEX_HOME/hooks.json and
// enables the stable `hooks` feature in config.toml (migrating the legacy experimental
// `codex_hooks` flag). It is idempotent: existing tg-cli hook entries are replaced, and
// the config.toml hooks/codex_hooks line is rewritten in place. home is the user home dir
// (used only when CODEX_HOME is unset); hookCommand is the "<bin> hook --port <port>"
// string substituted for the HOOK_CMD placeholder. Reused by both `tg-cli setup` and
// `tg-cli service upgrade` so the two stay consistent.
func InstallCodexHooks(home, hookCommand string) error {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	if err := os.MkdirAll(codexHome, 0755); err != nil {
		return fmt.Errorf("create codex home: %w", err)
	}
	codexHooksPath := filepath.Join(codexHome, "hooks.json")
	// Read template and replace HOOK_CMD placeholder
	codexTemplate := strings.ReplaceAll(string(codexConfigJSON), "HOOK_CMD", hookCommand)
	// Read existing hooks.json if present
	var codexHooks map[string]interface{}
	if data, err := os.ReadFile(codexHooksPath); err == nil {
		json.Unmarshal(data, &codexHooks)
	}
	if codexHooks == nil {
		codexHooks = make(map[string]interface{})
	}
	// Snapshot the existing hooks before merging so we can report what actually changed.
	oldCodexHooksJSON, _ := json.Marshal(codexHooks["hooks"])
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
	newCodexHooksJSON, _ := json.Marshal(codexHooks["hooks"])
	codexData, _ := json.MarshalIndent(codexHooks, "", "  ")
	if string(oldCodexHooksJSON) == string(newCodexHooksJSON) {
		fmt.Printf("Codex hooks already up-to-date (%s)\n", codexHooksPath)
	} else {
		if err := os.WriteFile(codexHooksPath, codexData, 0644); err != nil {
			return fmt.Errorf("write codex hooks: %w", err)
		}
		var oldCH, newCH map[string]interface{}
		json.Unmarshal(oldCodexHooksJSON, &oldCH)
		json.Unmarshal(newCodexHooksJSON, &newCH)
		if events := changedHookEvents(oldCH, newCH); len(events) > 0 {
			fmt.Printf("Codex hooks updated (%s): %s\n", codexHooksPath, strings.Join(events, ", "))
		} else {
			fmt.Printf("Codex hooks updated (%s)\n", codexHooksPath)
		}
	}
	// Enable the stable `hooks` feature in config.toml. Codex renamed the old
	// experimental `codex_hooks` flag to `hooks` (now stable, default-on).
	// Remove any existing hooks / codex_hooks line at the line level (tolerant
	// of spacing and `= false`), then add `hooks = true` — fully idempotent.
	codexConfigPath := filepath.Join(codexHome, "config.toml")
	configContent, _ := os.ReadFile(codexConfigPath)
	configStr := string(configContent)
	hooksLineRe := regexp.MustCompile(`(?m)^[ \t]*(codex_)?hooks[ \t]*=.*\n?`)
	newConfig := hooksLineRe.ReplaceAllString(configStr, "")
	if strings.Contains(newConfig, "[features]") {
		newConfig = strings.Replace(newConfig, "[features]", "[features]\nhooks = true", 1)
	} else {
		newConfig += "\n[features]\nhooks = true\n"
	}
	if newConfig != configStr {
		if err := os.WriteFile(codexConfigPath, []byte(newConfig), 0644); err != nil {
			return fmt.Errorf("write codex config: %w", err)
		}
		fmt.Println("Codex hooks feature enabled in config.toml")
	}
	return nil
}

// InstallCCHooks syncs the tg-cli CC hooks from the embedded cc.json template into the given
// Claude settings.json, replacing this instance's tg-cli hook entries while preserving user-added
// and other-instance hooks. Idempotent. Reused by `tg-cli setup` (via mergeCCHooks) and
// `tg-cli service upgrade` so the two stay consistent.
func InstallCCHooks(settingsPath, hookCommand string) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return fmt.Errorf("create settings dir: %w", err)
	}
	var settings map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("invalid JSON in %s: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read settings %s: %w", settingsPath, err)
	}
	if settings == nil {
		settings = make(map[string]interface{})
	}
	// Snapshot the hooks before merging (mergeCCHooks mutates settings["hooks"] in place). Compare at
	// the hooks level rather than the whole file: MarshalIndent would reformat unrelated keys, so a
	// full-file byte compare would falsely read as "changed".
	oldHooksJSON, _ := json.Marshal(settings["hooks"])
	mergeCCHooks(settings, hookCommand)
	newHooks, _ := settings["hooks"].(map[string]interface{})
	newHooksJSON, _ := json.Marshal(newHooks)
	if string(oldHooksJSON) == string(newHooksJSON) {
		fmt.Printf("CC hooks already up-to-date (%s)\n", settingsPath)
		return nil
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	var oldHooks map[string]interface{}
	json.Unmarshal(oldHooksJSON, &oldHooks)
	if events := changedHookEvents(oldHooks, newHooks); len(events) > 0 {
		fmt.Printf("CC hooks updated (%s): %s\n", settingsPath, strings.Join(events, ", "))
	} else {
		fmt.Printf("CC hooks updated (%s)\n", settingsPath)
	}
	return nil
}

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
	// Parse CC hooks config template (needed for permissions/statusLine blocks below)
	ccTemplate := strings.ReplaceAll(string(ccConfigJSON), "HOOK_CMD", hookCommand)
	var ccConfig map[string]interface{}
	json.Unmarshal([]byte(ccTemplate), &ccConfig)
	// Merge hooks: install uses shared helper; uninstall removes this instance's tg-cli entries
	if !setupUninstallFlag {
		mergeCCHooks(settings, hookCommand)
	} else {
		hooks, ok := settings["hooks"].(map[string]interface{})
		if !ok {
			hooks = make(map[string]interface{})
		}
		if ccHooks, ok := ccConfig["hooks"].(map[string]interface{}); ok {
			for event := range ccHooks {
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
				hooks[event] = filtered
			}
		}
		settings["hooks"] = hooks
	}
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
	if !setupUninstallFlag {
		applyClaudeSettingsMigrations(settings)
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
	} else {
		fmt.Printf("Hooks installed to %s\n", settingsPath)
		fmt.Printf("Hook command: %s\n", hookCommand)
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
		if err := InstallCodexHooks(home, hookCommand); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: codex hooks setup failed: %v\n", err)
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

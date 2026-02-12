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

// detectTmuxTarget extracts the tmux target from environment variables.
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
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(0)
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		os.Exit(0)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		os.Exit(0)
	}
	event := "unknown"
	if v, ok := payload["hook_event_name"].(string); ok {
		event = v
	} else if v, ok := payload["event"].(string); ok {
		event = v
	}
	sessionID := ""
	if v, ok := payload["session_id"].(string); ok {
		sessionID = v
	}
	cwd := ""
	if v, ok := payload["cwd"].(string); ok {
		cwd = v
	}
	project := "unknown"
	if cwd != "" {
		project = filepath.Base(cwd)
	}
	port := hookPortFlag
	if port == 0 {
		creds, _ := config.LoadCredentials()
		port = creds.Port
	}
	if port == 0 {
		port = 12500
	}
	transcriptPath := ""
	if tp, ok := payload["transcript_path"].(string); ok {
		transcriptPath = tp
	}
	var hookToolName, hookToolInput string
	tmuxTarget := ""
	switch event {
	case "SessionStart":
		tmuxTarget = detectTmuxTarget()
	case "SessionEnd":
		tmuxTarget = detectTmuxTarget()
	case "PermissionRequest":
		tmuxTarget = detectTmuxTarget()
		toolName, _ := payload["tool_name"].(string)
		if toolName == "AskUserQuestion" {
			os.Exit(0)
		}
		toolInputRaw, _ := json.Marshal(payload["tool_input"])
		suggestionsRaw, _ := json.Marshal(payload["permission_suggestions"])
		hookData := map[string]string{
			"event":          "PermissionRequest",
			"toolName":       toolName,
			"toolInput":      string(toolInputRaw),
			"suggestions":    string(suggestionsRaw),
			"project":        project,
			"tmuxTarget":     tmuxTarget,
			"sessionId":      sessionID,
			"transcriptPath": transcriptPath,
		}
		jsonData, _ := json.Marshal(hookData)
		client := &http.Client{Timeout: 115 * time.Second}
		req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/permission", port), bytes.NewReader(jsonData))
		if err != nil {
			os.Exit(0)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			os.Exit(0)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		var decision struct {
			Behavior           string          `json:"behavior"`
			Message            string          `json:"message,omitempty"`
			UpdatedPermissions json.RawMessage `json:"updatedPermissions,omitempty"`
		}
		if json.Unmarshal(respBody, &decision) == nil && decision.Behavior != "" {
			output := map[string]interface{}{
				"hookSpecificOutput": map[string]interface{}{
					"hookEventName": "PermissionRequest",
					"decision": map[string]interface{}{
						"behavior": decision.Behavior,
					},
				},
			}
			if decision.Message != "" {
				output["hookSpecificOutput"].(map[string]interface{})["decision"].(map[string]interface{})["message"] = decision.Message
			}
			if len(decision.UpdatedPermissions) > 0 {
				output["hookSpecificOutput"].(map[string]interface{})["decision"].(map[string]interface{})["updatedPermissions"] = decision.UpdatedPermissions
			}
			outJSON, _ := json.Marshal(output)
			fmt.Print(string(outJSON))
		}
		os.Exit(0)
	case "PreToolUse":
		tmuxTarget = detectTmuxTarget()
		hookToolName, _ = payload["tool_name"].(string)
		if hookToolName == "AskUserQuestion" {
			raw, _ := json.Marshal(payload["tool_input"])
			hookToolInput = string(raw)
		}
	case "UserPromptSubmit":
		tmuxTarget = detectTmuxTarget()
	default:
		tmuxTarget = detectTmuxTarget()
	}
	hookData := map[string]string{
		"event":          event,
		"sessionId":      sessionID,
		"project":        project,
		"body":           "",
		"tmuxTarget":     tmuxTarget,
		"transcriptPath": transcriptPath,
		"toolName":       hookToolName,
		"toolInput":      hookToolInput,
	}
	jsonData, _ := json.Marshal(hookData)
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/hook", port), bytes.NewReader(jsonData))
	if err != nil {
		os.Exit(0)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		os.Exit(0)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	os.Exit(0)
}

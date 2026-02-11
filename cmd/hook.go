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

// countAssistantEntries counts the number of assistant entries in a JSONL transcript.
func countAssistantEntries(transcriptPath string) int {
	content, err := os.ReadFile(transcriptPath)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if typ, _ := entry["type"].(string); typ == "assistant" {
			count++
		}
	}
	return count
}

// extractAssistantBody reads a JSONL transcript and returns the last assistant message text.
func extractAssistantBody(transcriptPath string) string {
	content, err := os.ReadFile(transcriptPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(content), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		typ, _ := entry["type"].(string)
		if typ != "assistant" {
			continue
		}
		msg, _ := entry["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		contentArr, _ := msg["content"].([]interface{})
		if contentArr == nil {
			continue
		}
		var textParts []string
		for _, c := range contentArr {
			cMap, _ := c.(map[string]interface{})
			if cMap == nil {
				continue
			}
			if cType, _ := cMap["type"].(string); cType == "text" {
				if text, ok := cMap["text"].(string); ok {
					textParts = append(textParts, text)
				}
			}
		}
		return strings.Join(textParts, "\n")
	}
	return ""
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
	// Dispatch by event type
	tmuxTarget := ""
	body := ""
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
			"event":       "PermissionRequest",
			"toolName":    toolName,
			"toolInput":   string(toolInputRaw),
			"suggestions": string(suggestionsRaw),
			"project":     project,
			"tmuxTarget":  tmuxTarget,
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
		toolName, _ := payload["tool_name"].(string)
		if toolName != "AskUserQuestion" {
			os.Exit(0)
		}
		tmuxTarget = detectTmuxTarget()
		toolInputRaw, _ := json.Marshal(payload["tool_input"])
		hookData := map[string]string{
			"event":      "AskUserQuestion",
			"toolName":   toolName,
			"toolInput":  string(toolInputRaw),
			"project":    project,
			"tmuxTarget": tmuxTarget,
			"sessionId":  sessionID,
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
	default:
		// Stop: extract transcript body and detect tmux
		tmuxTarget = detectTmuxTarget()
		// Extract last assistant message from transcript with retry.
		// The Stop hook fires before Claude Code finishes writing the assistant
		// entry to the JSONL transcript. We count entries first, then wait for
		// a new one to appear (handles both first and subsequent invocations).
		if transcriptPath, ok := payload["transcript_path"].(string); ok {
			initialCount := countAssistantEntries(transcriptPath)
			for attempt := 0; attempt < 10; attempt++ {
				time.Sleep(200 * time.Millisecond)
				if countAssistantEntries(transcriptPath) > initialCount {
					body = extractAssistantBody(transcriptPath)
					break
				}
			}
			if body == "" {
				body = extractAssistantBody(transcriptPath)
			}
		}
	}
	hookData := map[string]string{
		"event":      event,
		"sessionId":  sessionID,
		"project":    project,
		"body":       body,
		"tmuxTarget": tmuxTarget,
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

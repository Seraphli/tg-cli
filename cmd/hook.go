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
		joined := strings.Join(textParts, "\n")
		if len(joined) > 500 {
			return joined[:500] + "..."
		}
		return joined
	}
	return ""
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
	// Extract last assistant message from transcript with retry.
	// The Stop hook fires before Claude Code finishes writing the assistant
	// entry to the JSONL transcript, so we poll a few times.
	body := ""
	if transcriptPath, ok := payload["transcript_path"].(string); ok {
		for attempt := 0; attempt < 5; attempt++ {
			body = extractAssistantBody(transcriptPath)
			if body != "" {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	port := hookPortFlag
	if port == 0 {
		creds, _ := config.LoadCredentials()
		port = creds.Port
	}
	if port == 0 {
		port = 12500
	}
	hookData := map[string]string{
		"event":     event,
		"sessionId": sessionID,
		"project":   project,
		"body":      body,
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

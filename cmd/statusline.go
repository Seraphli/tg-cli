package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var StatuslineCmd = &cobra.Command{
	Use:   "statusline",
	Short: "Claude Code statusline script — saves context window data",
	RunE:  runStatusline,
}

func runStatusline(cmd *cobra.Command, args []string) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	// Write raw input to stdout for pipeline chaining
	os.Stdout.Write(raw)

	var payload struct {
		SessionID     string          `json:"session_id"`
		Version       string          `json:"version"`
		ContextWindow json.RawMessage `json:"context_window"`
		RateLimits    json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	if payload.SessionID == "" || payload.ContextWindow == nil {
		return nil
	}

	dir := filepath.Join(os.TempDir(), "tg-cli", "context")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	saveData := map[string]interface{}{}
	if payload.ContextWindow != nil {
		json.Unmarshal(payload.ContextWindow, &saveData)
	}
	if payload.Version != "" {
		saveData["cc_version"] = payload.Version
	}
	if len(payload.RateLimits) > 0 && string(payload.RateLimits) != "null" {
		var rl map[string]interface{}
		if json.Unmarshal(payload.RateLimits, &rl) == nil && len(rl) > 0 {
			saveData["rate_limits"] = rl
		}
	}
	out, _ := json.Marshal(saveData)
	return os.WriteFile(filepath.Join(dir, payload.SessionID+".json"), out, 0644)
}

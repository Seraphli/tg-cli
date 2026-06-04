package helpers

import (
	"encoding/json"
	"io"
	"net/http"
)

// HookPayload represents the CC payload enriched by hook.go.
type HookPayload struct {
	HookEventName        string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	CWD                  string          `json:"cwd"`
	TranscriptPath       string          `json:"transcript_path"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	PermSuggestions      json.RawMessage `json:"permission_suggestions"`
	TmuxTarget           string          `json:"tmux_target"`
	Project              string          `json:"project"`
	Source               string          `json:"source"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	AgentID              string          `json:"agent_id"`
	AgentType            string          `json:"agent_type"`
	Backend              string          `json:"backend"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	Prompt               string          `json:"prompt"`
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Index     int    `json:"index"`
	Final     bool   `json:"final"`
	Delta     string `json:"delta"`
}

// ParseHookPayload reads and parses the hook payload from an HTTP request body.
func ParseHookPayload(r *http.Request) (*HookPayload, []byte, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, err
	}
	var p HookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, body, err
	}
	return &p, body, nil
}

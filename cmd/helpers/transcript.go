package helpers

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// TranscriptLogEntry represents a parsed message from a CC or Codex transcript JSONL.
type TranscriptLogEntry struct {
	Type       string `json:"type"`
	Timestamp  string `json:"timestamp"`
	Text       string `json:"text"`
	Tool       string `json:"tool"`
	ToolDetail string `json:"tool_detail"`
}

// ParseCCTranscript parses Claude Code JSONL transcript format.
func ParseCCTranscript(f *os.File, noTools bool, filteredTools map[string]bool, formatToolDetail func(string, map[string]interface{}) string) []TranscriptLogEntry {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var entries []TranscriptLogEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			Type      string          `json:"type"`
			Model     string          `json:"model"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		// Skip progress and synthetic entries
		if raw.Type == "progress" || raw.Model == "<synthetic>" {
			continue
		}
		if raw.Type == "user" {
			var msg struct {
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(raw.Message, &msg) != nil {
				continue
			}
			// Try string content
			var contentStr string
			if json.Unmarshal(msg.Content, &contentStr) == nil {
				entries = append(entries, TranscriptLogEntry{Type: "user", Timestamp: raw.Timestamp, Text: contentStr})
				continue
			}
			// Try array content
			var contentArr []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(msg.Content, &contentArr) != nil {
				continue
			}
			hasToolResult := false
			for _, c := range contentArr {
				if c.Type == "tool_result" {
					hasToolResult = true
					break
				}
			}
			if noTools && hasToolResult {
				var parts []string
				for _, c := range contentArr {
					if c.Type == "text" && c.Text != "" {
						parts = append(parts, c.Text)
					}
				}
				if len(parts) > 0 {
					entries = append(entries, TranscriptLogEntry{Type: "user", Timestamp: raw.Timestamp, Text: strings.Join(parts, "\n")})
				}
				continue
			}
			var parts []string
			for _, c := range contentArr {
				if c.Type == "text" && c.Text != "" {
					parts = append(parts, c.Text)
				}
			}
			if len(parts) > 0 {
				entries = append(entries, TranscriptLogEntry{Type: "user", Timestamp: raw.Timestamp, Text: strings.Join(parts, "\n")})
			}
		} else if raw.Type == "assistant" {
			var msg struct {
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(raw.Message, &msg) != nil {
				continue
			}
			// Parse content blocks as raw to handle both text and tool_use
			var contentBlocks []json.RawMessage
			if json.Unmarshal(msg.Content, &contentBlocks) != nil {
				continue
			}
			var textParts []string
			toolName := ""
			toolDetail := ""
			allFilteredTools := true
			hasAnyBlock := false
			for _, block := range contentBlocks {
				var b struct {
					Type  string                 `json:"type"`
					Text  string                 `json:"text"`
					Name  string                 `json:"name"`
					Input map[string]interface{} `json:"input"`
				}
				if json.Unmarshal(block, &b) != nil {
					continue
				}
				hasAnyBlock = true
				if b.Type == "text" && b.Text != "" {
					textParts = append(textParts, b.Text)
					allFilteredTools = false
				} else if b.Type == "tool_use" {
					if !filteredTools[b.Name] {
						allFilteredTools = false
					}
					// Use first tool_use found for tool/tool_detail fields
					if toolName == "" {
						toolName = b.Name
						toolDetail = formatToolDetail(b.Name, b.Input)
					}
				}
			}
			if !hasAnyBlock {
				continue
			}
			// When no_tools=true: skip if ALL blocks are filtered tools (keep if text or AskUserQuestion)
			if noTools && allFilteredTools && len(textParts) == 0 {
				continue
			}
			if len(textParts) > 0 || toolName != "" {
				entries = append(entries, TranscriptLogEntry{
					Type:       "assistant",
					Timestamp:  raw.Timestamp,
					Text:       strings.Join(textParts, "\n"),
					Tool:       toolName,
					ToolDetail: toolDetail,
				})
			}
		}
	}
	return entries
}

// ParseCodexTranscript parses Codex JSONL transcript format.
// Codex format: {"type":"response_item","timestamp":"...","payload":{"type":"message","role":"user/assistant","content":[{"type":"input_text/output_text","text":"..."}]}}
func ParseCodexTranscript(f *os.File, noTools bool, filteredTools map[string]bool, formatToolDetail func(string, map[string]interface{}) string) []TranscriptLogEntry {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var entries []TranscriptLogEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if raw.Type != "response_item" {
			continue
		}
		var payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type  string                 `json:"type"`
				Text  string                 `json:"text"`
				Name  string                 `json:"name"`
				Input map[string]interface{} `json:"input"`
			} `json:"content"`
		}
		if json.Unmarshal(raw.Payload, &payload) != nil {
			continue
		}
		// Handle function_call (tool invocation)
		if payload.Type == "function_call" {
			var fc struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				CallID    string `json:"call_id"`
			}
			if json.Unmarshal(raw.Payload, &fc) == nil && fc.Name != "" {
				detail := fc.Arguments
				if len(detail) > 200 {
					detail = detail[:200] + "..."
				}
				if noTools && filteredTools[fc.Name] {
					continue
				}
				entries = append(entries, TranscriptLogEntry{
					Type: "assistant", Timestamp: raw.Timestamp,
					Tool: fc.Name, ToolDetail: detail,
				})
			}
			continue
		}
		// Skip function_call_output and reasoning
		if payload.Type == "function_call_output" || payload.Type == "reasoning" {
			continue
		}
		if payload.Type != "message" {
			continue
		}
		if payload.Role != "user" && payload.Role != "assistant" {
			continue
		}
		var textParts []string
		toolName := ""
		toolDetail := ""
		for _, c := range payload.Content {
			if c.Type == "input_text" || c.Type == "output_text" || c.Type == "text" {
				if c.Text != "" {
					textParts = append(textParts, c.Text)
				}
			} else if c.Type == "function_call" {
				if toolName == "" {
					toolName = c.Name
					toolDetail = formatToolDetail(c.Name, c.Input)
				}
			}
		}
		if len(textParts) == 0 && toolName == "" {
			continue
		}
		if noTools && toolName != "" && filteredTools[toolName] && len(textParts) == 0 {
			continue
		}
		entryType := "user"
		if payload.Role == "assistant" {
			entryType = "assistant"
		}
		entries = append(entries, TranscriptLogEntry{
			Type:       entryType,
			Timestamp:  raw.Timestamp,
			Text:       strings.Join(textParts, "\n"),
			Tool:       toolName,
			ToolDetail: toolDetail,
		})
	}
	return entries
}

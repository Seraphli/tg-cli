package helpers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Seraphli/tg-cli/internal/config"
)

// TranscriptLogEntry represents a parsed message from a CC or Codex transcript JSONL.
type TranscriptLogEntry struct {
	Type       string `json:"type"`
	OriginKind string `json:"origin_kind"`
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
			Origin    struct {
				Kind string `json:"kind"`
			} `json:"origin"`
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
				entries = append(entries, TranscriptLogEntry{Type: "user", OriginKind: raw.Origin.Kind, Timestamp: raw.Timestamp, Text: contentStr})
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
					entries = append(entries, TranscriptLogEntry{Type: "user", OriginKind: raw.Origin.Kind, Timestamp: raw.Timestamp, Text: strings.Join(parts, "\n")})
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
				entries = append(entries, TranscriptLogEntry{Type: "user", OriginKind: raw.Origin.Kind, Timestamp: raw.Timestamp, Text: strings.Join(parts, "\n")})
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

// TranscriptRound represents one round of interaction (contiguous user messages + contiguous assistant messages).
type TranscriptRound struct {
	UserTexts      []string
	AssistantTexts []string
}

// ReadLastNRounds reads the last N rounds from a transcript file.
// A round = contiguous block of user messages followed by contiguous block of assistant messages.
func ReadLastNRounds(transcriptPath string, n int, backend string) ([]TranscriptRound, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	noToolsFilter := map[string]bool{
		"Bash": true, "Read": true, "Write": true, "Edit": true,
		"Glob": true, "Grep": true, "Agent": true, "WebFetch": true,
		"WebSearch": true, "NotebookEdit": true,
	}
	noopDetail := func(name string, input map[string]interface{}) string { return "" }
	var entries []TranscriptLogEntry
	if backend == "codex" {
		entries = ParseCodexTranscript(f, true, noToolsFilter, noopDetail)
	} else {
		entries = ParseCCTranscript(f, true, noToolsFilter, noopDetail)
	}
	var rounds []TranscriptRound
	var current TranscriptRound
	lastRole := ""
	ccBackend := backend != "codex"
	for _, e := range entries {
		if e.Text == "" {
			continue
		}
		if e.Type == "user" && isSystemTagContent(e.Text) {
			continue
		}
		role := e.Type
		// A CC user entry opens a new round only if it is a genuine human turn (origin.kind=="human"); a
		// non-human CC user entry (nudge, interrupt) is still appended to the current round, never dropped.
		// codex (no origin field) is unchanged: any user entry opens a round.
		userOpensRound := role == "user" && (!ccBackend || e.OriginKind == "human")
		switch {
		case role == "user" && userOpensRound && lastRole != "user":
			if len(current.UserTexts) > 0 && len(current.AssistantTexts) > 0 {
				rounds = append(rounds, current)
				current = TranscriptRound{}
			} else if len(current.AssistantTexts) > 0 {
				current = TranscriptRound{}
			}
			current.UserTexts = append(current.UserTexts, e.Text)
		case role == "user":
			current.UserTexts = append(current.UserTexts, e.Text)
		case role == "assistant" && lastRole != "assistant":
			current.AssistantTexts = append(current.AssistantTexts, e.Text)
		case role == "assistant" && lastRole == "assistant":
			current.AssistantTexts = append(current.AssistantTexts, e.Text)
		}
		lastRole = role
	}
	if len(current.UserTexts) > 0 && len(current.AssistantTexts) > 0 {
		rounds = append(rounds, current)
	}
	if len(rounds) > n {
		rounds = rounds[len(rounds)-n:]
	}
	return rounds, nil
}

// ReadLastNLines reads the last N text entries from a transcript file.
func ReadLastNLines(transcriptPath string, n int, backend string) ([]TranscriptLogEntry, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	noToolsFilter := map[string]bool{
		"Bash": true, "Read": true, "Write": true, "Edit": true,
		"Glob": true, "Grep": true, "Agent": true, "WebFetch": true,
		"WebSearch": true, "NotebookEdit": true,
	}
	noopDetail := func(name string, input map[string]interface{}) string { return "" }
	var entries []TranscriptLogEntry
	if backend == "codex" {
		entries = ParseCodexTranscript(f, true, noToolsFilter, noopDetail)
	} else {
		entries = ParseCCTranscript(f, true, noToolsFilter, noopDetail)
	}
	var textEntries []TranscriptLogEntry
	for _, e := range entries {
		if e.Text != "" && !(e.Type == "user" && isSystemTagContent(e.Text)) {
			textEntries = append(textEntries, e)
		}
	}
	if len(textEntries) > n {
		textEntries = textEntries[len(textEntries)-n:]
	}
	return textEntries, nil
}

// extractToolParam extracts a representative parameter string from tool input.
func extractToolParam(name string, input map[string]interface{}) string {
	switch name {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			return cmd
		}
	case "Read", "Edit", "Write":
		if fp, ok := input["file_path"].(string); ok {
			return fp
		}
	case "Agent":
		if desc, ok := input["description"].(string); ok {
			return desc
		}
	}
	for _, v := range input {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// FormatToolLine formats a tool call as a single display line with truncation.
func FormatToolLine(sessionName, toolName, param string, targetMax int) string {
	if targetMax <= 0 {
		targetMax = 40
	}
	prefix := fmt.Sprintf("[%s]: 🔧 %s(\"", sessionName, toolName)
	suffix := "\")"
	prefixLen := len([]rune(prefix))
	suffixLen := len([]rune(suffix))
	paramBudget := targetMax - prefixLen - suffixLen
	if paramBudget <= 0 {
		return fmt.Sprintf("[%s]: 🔧 %s", sessionName, toolName)
	}
	paramRunes := []rune(param)
	if len(paramRunes) > paramBudget {
		return prefix + string(paramRunes[:paramBudget-1]) + "…" + suffix
	}
	return prefix + param + suffix
}

// ReadContextBlock reads a transcript and returns formatted context lines.
// rounds specifies number of conversation rounds (0 defaults to 3); lines specifies raw entry count (takes priority over rounds when > 0).
func ReadContextBlock(path string, rounds, lines int, backend, sessionName, displayName string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var entries []TranscriptLogEntry
	if backend == "codex" {
		entries = ParseCodexTranscript(f, false, nil, func(name string, input map[string]interface{}) string {
			return extractToolParam(name, input)
		})
	} else {
		entries = ParseCCTranscript(f, false, nil, func(name string, input map[string]interface{}) string {
			return extractToolParam(name, input)
		})
	}
	var filtered []TranscriptLogEntry
	for _, e := range entries {
		if e.Type == "user" && e.Text != "" && isSystemTagContent(e.Text) {
			continue
		}
		if e.Text == "" && e.Tool == "" {
			continue
		}
		filtered = append(filtered, e)
	}
	var selected []TranscriptLogEntry
	if lines > 0 {
		if len(filtered) > lines {
			selected = filtered[len(filtered)-lines:]
		} else {
			selected = filtered
		}
	} else {
		r := rounds
		if r == 0 {
			r = 3
		}
		type ctxRound struct{ entries []TranscriptLogEntry }
		var rnds []ctxRound
		var current ctxRound
		lastRole := ""
		ccBackend := backend != "codex"
		for _, e := range filtered {
			role := e.Type
			// A user entry opens a new round only if it is a genuine human turn. On the CC path that
			// means origin.kind=="human"; on codex (no origin field) any user entry opens a round, as before.
			if role == "user" && lastRole == "assistant" && (!ccBackend || e.OriginKind == "human") {
				if len(current.entries) > 0 {
					rnds = append(rnds, current)
					current = ctxRound{}
				}
			}
			current.entries = append(current.entries, e)
			lastRole = role
		}
		if len(current.entries) > 0 {
			rnds = append(rnds, current)
		}
		if len(rnds) > r {
			rnds = rnds[len(rnds)-r:]
		}
		for _, rd := range rnds {
			selected = append(selected, rd.entries...)
		}
	}
	cfg, _ := config.LoadAppConfig()
	toolMax := 40
	if cfg.ToolLineMaxRunes > 0 {
		toolMax = cfg.ToolLineMaxRunes
	}
	var output []string
	for _, e := range selected {
		// Default: user speaks to session, assistant speaks back to user
		speaker := displayName
		recipient := sessionName
		if e.Type == "assistant" {
			speaker = sessionName
			recipient = displayName
		}
		if e.Text != "" {
			output = append(output, fmt.Sprintf("[%s → %s]: %s", speaker, recipient, e.Text))
		}
		// Tool use: always attributed to session (no direction marker)
		if e.Tool != "" {
			output = append(output, FormatToolLine(sessionName, e.Tool, e.ToolDetail, toolMax))
		}
	}
	return strings.Join(output, "\n"), nil
}

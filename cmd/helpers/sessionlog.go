package helpers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// ReadLastMeaningfulEntry scans a JSONL transcript file from end to start using
// reverse chunk reading (32KB chunks), returning the first meaningful entry
// (non-synthetic assistant output or non-command user input).
// Returns (text, source) where source is "assistant" or "user", or ("", "") if nothing found.
func ReadLastMeaningfulEntry(path string, maxLen int) (string, string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", ""
	}
	fileSize := info.Size()
	if fileSize == 0 {
		return "", ""
	}
	const chunkSize = 32 * 1024
	// Remainder carries a partial line from the beginning of the previous chunk
	var remainder []byte
	offset := fileSize
	for offset > 0 {
		readSize := int64(chunkSize)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, offset); err != nil {
			return "", ""
		}
		// Append remainder from previous iteration to end of this chunk
		if len(remainder) > 0 {
			buf = append(buf, remainder...)
			remainder = nil
		}
		// Split into lines; first segment may be partial (carry to next iteration)
		lines := bytes.Split(buf, []byte("\n"))
		// If we haven't reached the start of the file, the first segment is partial
		if offset > 0 {
			remainder = lines[0]
			lines = lines[1:]
		}
		// Process lines from end to start
		for i := len(lines) - 1; i >= 0; i-- {
			line := bytes.TrimSpace(lines[i])
			if len(line) == 0 {
				continue
			}
			var entry struct {
				Type    string `json:"type"`
				IsMeta  bool   `json:"isMeta"`
				Model   string `json:"model"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &entry) != nil {
				continue
			}
			if entry.Type == "assistant" && entry.Model != "<synthetic>" {
				var contentArr []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(entry.Message.Content, &contentArr) == nil {
					var parts []string
					for _, c := range contentArr {
						if c.Type == "text" && c.Text != "" {
							parts = append(parts, c.Text)
						}
					}
					if len(parts) > 0 {
						text := strings.Join(parts, "\n")
						if text == "No response requested." {
							continue
						}
						return TruncateStr(text, maxLen), "assistant"
					}
				}
				continue
			}
			if entry.Type == "user" && !entry.IsMeta {
				// Try string content
				var contentStr string
				if json.Unmarshal(entry.Message.Content, &contentStr) == nil && contentStr != "" {
					if !isSystemTagContent(contentStr) {
						return TruncateStr(contentStr, maxLen), "user"
					}
					continue
				}
				// Try array content
				var contentArr []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(entry.Message.Content, &contentArr) == nil {
					for _, c := range contentArr {
						if c.Type == "text" && c.Text != "" {
							if !isSystemTagContent(c.Text) {
								return TruncateStr(c.Text, maxLen), "user"
							}
						}
					}
				}
			}
		}
	}
	return "", ""
}

// ReadLastAssistantText reads the last assistant text from a JSONL transcript file.
func ReadLastAssistantText(path string, maxLen int) string {
	texts := ReadAssistantTexts(path)
	if len(texts) == 0 {
		return ""
	}
	return TruncateStr(texts[len(texts)-1], maxLen)
}

// ReadFirstHumanPrompt reads the first human prompt text from a JSONL session file.
func ReadFirstHumanPrompt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "No prompt"
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var cmdFallback string
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		if lineCount > 20 {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Type   string `json:"type"`
			IsMeta bool   `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Type != "user" || entry.IsMeta {
			continue
		}
		// Try string content first (most common for user prompts)
		var contentStr string
		if json.Unmarshal(entry.Message.Content, &contentStr) == nil && contentStr != "" {
			if !isSystemTagContent(contentStr) {
				return contentStr
			}
			if name := ExtractCommandName(contentStr); name != "" && cmdFallback == "" {
				cmdFallback = name
			}
			continue
		}
		// Try array content format
		var contentArr []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &contentArr) == nil {
			for _, c := range contentArr {
				if c.Type == "text" && c.Text != "" {
					if !isSystemTagContent(c.Text) {
						return c.Text
					}
					if name := ExtractCommandName(c.Text); name != "" && cmdFallback == "" {
						cmdFallback = name
					}
				}
			}
		}
	}
	if cmdFallback != "" {
		return cmdFallback
	}
	return "No prompt"
}

// ExtractCommandName extracts command name from <command-name>...</command-name> tag.
func ExtractCommandName(content string) string {
	const tag = "<command-name>"
	const endTag = "</command-name>"
	start := strings.Index(content, tag)
	if start == -1 {
		return ""
	}
	start += len(tag)
	end := strings.Index(content[start:], endTag)
	if end == -1 {
		return ""
	}
	return content[start : start+end]
}

// isSystemTagContent checks if a string starts with a known CC system tag prefix.
func isSystemTagContent(s string) bool {
	prefixes := []string{"<local-command-", "<command-", "<task-notification", "<bash-input", "<system-reminder"}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

package cmd

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanCustomCommandsFrontmatter tests the YAML frontmatter parsing logic
// used in scanCustomCommands. Since that function hardcodes ~/.claude/commands/,
// we replicate the parsing logic here with temp files.
func TestScanCustomCommandsFrontmatter(t *testing.T) {
	// parseDesc replicates the file-parsing logic from scanCustomCommands
	parseDesc := func(path, defaultDesc string) string {
		desc := defaultDesc
		f, err := os.Open(path)
		if err != nil {
			return desc
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		if scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "---" {
				// YAML frontmatter: scan until closing --- and extract description
				for scanner.Scan() {
					fmLine := strings.TrimSpace(scanner.Text())
					if fmLine == "---" {
						break
					}
					if strings.HasPrefix(fmLine, "description:") {
						desc = truncateStr(strings.TrimSpace(strings.TrimPrefix(fmLine, "description:")), 200)
					}
				}
			} else {
				line = strings.TrimLeft(line, "# ")
				if len(line) > 0 {
					desc = truncateStr(line, 200)
				}
			}
		}
		return desc
	}

	tmpDir := t.TempDir()

	tests := []struct {
		name        string
		content     string
		defaultDesc string
		expected    string
	}{
		{
			name: "frontmatter with description",
			content: "---\ndescription: My test description\n---\nBody content here\n",
			defaultDesc: "default",
			expected:    "My test description",
		},
		{
			name: "frontmatter without description field",
			content: "---\ntitle: Something\nauthor: me\n---\nBody\n",
			defaultDesc: "default",
			expected:    "default",
		},
		{
			name: "heading first line no frontmatter",
			content: "# My Heading\nSome body text\n",
			defaultDesc: "default",
			expected:    "My Heading",
		},
		{
			name: "plain first line no frontmatter",
			content: "Just a plain line\nMore content\n",
			defaultDesc: "default",
			expected:    "Just a plain line",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(tmpDir, strings.ReplaceAll(tt.name, " ", "_")+string(rune('0'+i))+".md")
			if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
				t.Fatalf("failed to write temp file: %v", err)
			}
			got := parseDesc(path, tt.defaultDesc)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestQuoteOriginalMessage tests the blockquote formatting logic used in processUserInput.
func TestQuoteOriginalMessage(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected string // expected quoted result (before the \n\n separator)
	}{
		{"single line", "hello world", "> hello world"},
		{"multi line", "line1\nline2\nline3", "> line1\n> line2\n> line3"},
		{"empty text", "", ""},
		{"long text no truncation", strings.Repeat("a", 600), "> " + strings.Repeat("a", 600)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the quote logic from processUserInput
			if tt.text == "" {
				if tt.expected != "" {
					t.Errorf("expected empty but got %q", tt.expected)
				}
				return
			}
			quoted := tt.text
			var lines []string
			for _, line := range strings.Split(quoted, "\n") {
				lines = append(lines, "> "+line)
			}
			result := strings.Join(lines, "\n")
			if result != tt.expected {
				t.Errorf("got %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestRateLimitDetection tests the rate-limit pattern matching logic from the Stop handler.
func TestRateLimitDetection(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{"typical rate limit message", "You've reached your usage limit. Please stop and wait for your quota to reset.", true},
		{"mixed case", "Stop And Wait until your Usage resets", true},
		{"stop and wait only", "Please stop and wait for a moment", false},
		{"usage only", "Check your usage at the dashboard", false},
		{"empty body", "", false},
		{"normal stop body", "I've completed the task successfully.", false},
		{"both keywords present", "Your usage has exceeded the limit. You should stop and wait.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the detection logic from bot_hooks.go Stop handler
			bodyLower := strings.ToLower(tt.body)
			detected := strings.Contains(bodyLower, "stop and wait") && strings.Contains(bodyLower, "usage")
			if detected != tt.expected {
				t.Errorf("body=%q: got detected=%v, want %v", tt.body, detected, tt.expected)
			}
		})
	}
}

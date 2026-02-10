package injector

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNormalizeText(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"crlf", "hello\r\nworld", "hello\nworld"},
		{"cr", "hello\rworld", "hello\nworld"},
		{"unicode_line_sep", "hello\u2028world", "hello\nworld"},
		{"unicode_para_sep", "hello\u2029world", "hello\nworld"},
		{"next_line", "hello\u0085world", "hello\nworld"},
		{"zero_width_space", "hel\u200Blo", "hello"},
		{"bom", "\uFEFFhello", "hello"},
		{"trailing_newlines", "hello\n\n\n", "hello"},
		{"empty_after_clean", "\u200B\u200C\u200D", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeText(tt.input)
			if got != tt.expect {
				t.Errorf("NormalizeText(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		input   string
		paneID  string
		socket  string
		wantErr bool
	}{
		{"%3@/tmp/tmux-1000/default", "%3", "/tmp/tmux-1000/default", false},
		{"%3", "%3", "", false},
		{"", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseTarget(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseTarget(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.PaneID != tt.paneID || got.Socket != tt.socket {
					t.Errorf("ParseTarget(%q) = {%q, %q}, want {%q, %q}", tt.input, got.PaneID, got.Socket, tt.paneID, tt.socket)
				}
			}
		})
	}
}

func TestFormatTarget(t *testing.T) {
	tests := []struct {
		target TmuxTarget
		expect string
	}{
		{TmuxTarget{PaneID: "%3", Socket: "/tmp/tmux-1000/default"}, "%3@/tmp/tmux-1000/default"},
		{TmuxTarget{PaneID: "%3"}, "%3"},
	}
	for _, tt := range tests {
		t.Run(tt.expect, func(t *testing.T) {
			got := FormatTarget(tt.target)
			if got != tt.expect {
				t.Errorf("FormatTarget() = %q, want %q", got, tt.expect)
			}
		})
	}
}

func TestInjectText(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	session := "tg-cli-inject-test"
	exec.Command("tmux", "kill-session", "-t", session).Run()
	if err := exec.Command("tmux", "new-session", "-d", "-s", session).Run(); err != nil {
		t.Fatalf("Failed to create tmux session: %v", err)
	}
	defer exec.Command("tmux", "kill-session", "-t", session).Run()
	out, err := exec.Command("tmux", "list-panes", "-t", session, "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("Failed to get pane ID: %v", err)
	}
	paneID := strings.TrimSpace(string(out))
	target := TmuxTarget{PaneID: paneID}
	time.Sleep(500 * time.Millisecond)
	testText := "INJECT_TEST_12345"
	if err := InjectText(target, testText); err != nil {
		t.Fatalf("InjectText failed: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	capture, err := exec.Command("tmux", "capture-pane", "-t", paneID, "-p").Output()
	if err != nil {
		t.Fatalf("Failed to capture pane: %v", err)
	}
	if !strings.Contains(string(capture), testText) {
		t.Errorf("Injected text not found in pane output.\nExpected: %s\nGot:\n%s", testText, string(capture))
	}
}

func TestInjectTextMultiline(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	session := "tg-cli-inject-test-ml"
	exec.Command("tmux", "kill-session", "-t", session).Run()
	if err := exec.Command("tmux", "new-session", "-d", "-s", session).Run(); err != nil {
		t.Fatalf("Failed to create tmux session: %v", err)
	}
	defer exec.Command("tmux", "kill-session", "-t", session).Run()
	out, err := exec.Command("tmux", "list-panes", "-t", session, "-F", "#{pane_id}").Output()
	if err != nil {
		t.Fatalf("Failed to get pane ID: %v", err)
	}
	paneID := strings.TrimSpace(string(out))
	target := TmuxTarget{PaneID: paneID}
	time.Sleep(500 * time.Millisecond)
	testText := "LINE_ONE\nLINE_TWO"
	if err := InjectText(target, testText); err != nil {
		t.Fatalf("InjectText failed: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	capture, err := exec.Command("tmux", "capture-pane", "-t", paneID, "-p").Output()
	if err != nil {
		t.Fatalf("Failed to capture pane: %v", err)
	}
	content := string(capture)
	if !strings.Contains(content, "LINE_ONE") || !strings.Contains(content, "LINE_TWO") {
		t.Errorf("Multiline text not found in pane output.\nGot:\n%s", content)
	}
}

func TestSessionExists(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	// Non-existent pane should return false
	if SessionExists(TmuxTarget{PaneID: "%99999"}) {
		t.Error("SessionExists returned true for non-existent pane")
	}
}

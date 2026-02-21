package injector

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type TmuxTarget struct {
	PaneID string // e.g. "%3"
	Socket string // e.g. "/tmp/tmux-1000/default", empty for default
}

// NormalizeText cleans text for tmux injection.
func NormalizeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Remove Unicode line/paragraph separators
	for _, r := range []rune{'\u2028', '\u2029', '\u0085'} {
		text = strings.ReplaceAll(text, string(r), "\n")
	}
	// Remove zero-width characters
	for _, r := range []rune{'\u200B', '\u200C', '\u200D', '\u200E', '\u200F', '\uFEFF'} {
		text = strings.ReplaceAll(text, string(r), "")
	}
	text = strings.TrimRight(text, "\n")
	return text
}

// tmuxCmd builds a tmux command with optional socket flag.
func tmuxCmd(target TmuxTarget, args ...string) *exec.Cmd {
	if target.Socket != "" {
		fullArgs := append([]string{"-S", target.Socket}, args...)
		return exec.Command("tmux", fullArgs...)
	}
	return exec.Command("tmux", args...)
}

// SessionExists checks if the tmux pane still exists.
func SessionExists(target TmuxTarget) bool {
	cmd := tmuxCmd(target, "has-session", "-t", target.PaneID)
	return cmd.Run() == nil
}

// InjectText injects text into a tmux pane using bracketed paste.
func InjectText(target TmuxTarget, text string) error {
	text = NormalizeText(text)
	if text == "" {
		return fmt.Errorf("empty text after normalization")
	}
	// Clear current input
	if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-u").Run(); err != nil {
		return fmt.Errorf("clear input failed: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	// Set buffer
	if err := tmuxCmd(target, "set-buffer", "-b", "tg-cli", "--", text).Run(); err != nil {
		return fmt.Errorf("set-buffer failed: %w", err)
	}
	// Paste with bracketed paste
	if err := tmuxCmd(target, "paste-buffer", "-t", target.PaneID, "-b", "tg-cli", "-r", "-p").Run(); err != nil {
		return fmt.Errorf("paste-buffer failed: %w", err)
	}
	time.Sleep(1000 * time.Millisecond)
	// Submit
	if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-m").Run(); err != nil {
		return fmt.Errorf("submit failed: %w", err)
	}
	return nil
}

// ParseTarget parses a tmux target string like "%3@/tmp/tmux-1000/default".
func ParseTarget(s string) (TmuxTarget, error) {
	if s == "" {
		return TmuxTarget{}, fmt.Errorf("empty tmux target")
	}
	if idx := strings.Index(s, "@"); idx != -1 {
		return TmuxTarget{PaneID: s[:idx], Socket: s[idx+1:]}, nil
	}
	return TmuxTarget{PaneID: s}, nil
}

// FormatTarget formats a TmuxTarget as a string for embedding in messages.
func FormatTarget(t TmuxTarget) string {
	if t.Socket != "" {
		return t.PaneID + "@" + t.Socket
	}
	return t.PaneID
}

// SendKeys sends keys to a tmux pane.
func SendKeys(target TmuxTarget, keys ...string) error {
	args := append([]string{"send-keys", "-t", target.PaneID}, keys...)
	return tmuxCmd(target, args...).Run()
}

// CapturePane captures the content of a tmux pane.
func CapturePane(target TmuxTarget) (string, error) {
	cmd := tmuxCmd(target, "capture-pane", "-t", target.PaneID, "-p", "-S", "-")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("capture-pane failed: %w", err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// GetPaneTitle reads the tmux pane title via #{pane_title} format.
// Idle CC shows "✳ <name>", running CC shows spinner characters.
func GetPaneTitle(target TmuxTarget) (string, error) {
	cmd := tmuxCmd(target, "display-message", "-p", "-t", target.PaneID, "#{pane_title}")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

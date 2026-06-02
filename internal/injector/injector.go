package injector

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ServerName is the tmux server socket name (-L flag). Empty = default server.
var ServerName string

var injectMu sync.Map // key: formatted target string, value: *sync.Mutex

func getInjectLock(target TmuxTarget) *sync.Mutex {
	key := FormatTarget(target)
	v, _ := injectMu.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

const maxSetBufferSize = 16000

// setBuffer sets a tmux buffer by name. For text exceeding maxSetBufferSize bytes,
// it falls back to writing a temp file and using load-buffer to avoid tmux arg-length limits.
func setBuffer(target TmuxTarget, bufName, text string) error {
	if len(text) <= maxSetBufferSize {
		return tmuxCmd(target, "set-buffer", "-b", bufName, "--", text).Run()
	}
	f, err := os.CreateTemp("", "tg-cli-buf-*.txt")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	f.Close()
	return tmuxCmd(target, "load-buffer", "-b", bufName, tmpPath).Run()
}

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

// tmuxCmd builds a tmux command with optional server name (-L) and socket (-S) flags.
func tmuxCmd(target TmuxTarget, args ...string) *exec.Cmd {
	prefix := []string{"-u"}
	if ServerName != "" {
		prefix = append(prefix, "-L", ServerName)
	}
	if target.Socket != "" {
		prefix = append(prefix, "-S", target.Socket)
	}
	return exec.Command("tmux", append(prefix, args...)...)
}

// globalTmuxCmd builds a tmux command with optional server name (-L) flag only (no target socket).
func globalTmuxCmd(args ...string) *exec.Cmd {
	prefix := []string{"-u"}
	if ServerName != "" {
		prefix = append(prefix, "-L", ServerName)
	}
	return exec.Command("tmux", append(prefix, args...)...)
}

// TmuxCmd is the exported wrapper for tmuxCmd, used by external packages
// to build tmux commands targeting a specific pane with the correct server socket.
func TmuxCmd(target TmuxTarget, args ...string) *exec.Cmd {
	return tmuxCmd(target, args...)
}

// GlobalTmuxCmd is the exported wrapper for globalTmuxCmd, used by external packages
// to build tmux commands without a target pane (server-level operations).
func GlobalTmuxCmd(args ...string) *exec.Cmd {
	return globalTmuxCmd(args...)
}

// SessionExists checks if the tmux pane still exists.
func SessionExists(target TmuxTarget) bool {
	cmd := tmuxCmd(target, "has-session", "-t", target.PaneID)
	return cmd.Run() == nil
}

// InjectText injects text into a tmux pane using bracketed paste.
// Uses a per-target mutex to prevent concurrent injections into the same pane.
func InjectText(target TmuxTarget, text string, submit ...bool) error {
	mu := getInjectLock(target)
	mu.Lock()
	defer mu.Unlock()
	text = NormalizeText(text)
	if text == "" {
		return fmt.Errorf("empty text after normalization")
	}
	bufName := fmt.Sprintf("tg-cli-%s", target.PaneID)
	// Exit copy-mode if active
	out, err := tmuxCmd(target, "display-message", "-p", "-t", target.PaneID, "#{pane_mode}").Output()
	if err == nil && strings.TrimSpace(string(out)) == "copy-mode" {
		tmuxCmd(target, "send-keys", "-t", target.PaneID, "q").Run()
		time.Sleep(200 * time.Millisecond)
	}
	// Clear current input
	if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-u").Run(); err != nil {
		return fmt.Errorf("clear input failed: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	// Set buffer
	if err := setBuffer(target, bufName, text); err != nil {
		return fmt.Errorf("set-buffer failed: %w", err)
	}
	// Paste with bracketed paste
	if err := tmuxCmd(target, "paste-buffer", "-t", target.PaneID, "-b", bufName, "-r", "-p").Run(); err != nil {
		return fmt.Errorf("paste-buffer failed: %w", err)
	}
	time.Sleep(1000 * time.Millisecond)
	// Submit unless explicitly disabled
	if len(submit) == 0 || submit[0] {
		if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-m").Run(); err != nil {
			return fmt.Errorf("submit failed: %w", err)
		}
	}
	return nil
}

func InjectTextAppend(target TmuxTarget, text string, submit ...bool) error {
	mu := getInjectLock(target)
	mu.Lock()
	defer mu.Unlock()
	text = NormalizeText(text)
	if text == "" {
		return fmt.Errorf("empty text after normalization")
	}
	bufName := fmt.Sprintf("tg-cli-%s", target.PaneID)
	if err := setBuffer(target, bufName, text); err != nil {
		return fmt.Errorf("set-buffer failed: %w", err)
	}
	if err := tmuxCmd(target, "paste-buffer", "-t", target.PaneID, "-b", bufName, "-r", "-p").Run(); err != nil {
		return fmt.Errorf("paste-buffer failed: %w", err)
	}
	time.Sleep(1000 * time.Millisecond)
	if len(submit) == 0 || submit[0] {
		if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-m").Run(); err != nil {
			return fmt.Errorf("submit failed: %w", err)
		}
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

// CreateSession creates a new tmux session with optional working directory.
func CreateSession(name, workDir string) error {
	args := []string{"new-session", "-d", "-s", name}
	if workDir != "" {
		args = append(args, "-c", workDir)
	}
	return globalTmuxCmd(args...).Run()
}

// CreateWindow creates a new window in an existing tmux session.
// Returns the pane ID of the new window.
func CreateWindow(session, workDir string) (string, error) {
	args := []string{"new-window", "-t", session, "-P", "-F", "#{pane_id}"}
	if workDir != "" {
		args = append(args, "-c", workDir)
	}
	out, err := globalTmuxCmd(args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ListPanes returns pane IDs in a session.
func ListPanes(session string) ([]string, error) {
	out, err := globalTmuxCmd("list-panes", "-t", session, "-F", "#{pane_id}").Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines, nil
}

// NamedSessionExists checks if a tmux session with the given name exists.
func NamedSessionExists(name string) bool {
	return globalTmuxCmd("has-session", "-t", name).Run() == nil
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

// GetPaneCommand returns the current command running in a tmux pane.
func GetPaneCommand(target TmuxTarget) (string, error) {
	cmd := tmuxCmd(target, "display-message", "-t", target.PaneID, "-p", "#{pane_current_command}")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("display-message failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

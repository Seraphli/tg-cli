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

// ErrSubmitAfterPaste is returned when send-keys C-m fails after paste-buffer succeeds.
var ErrSubmitAfterPaste = fmt.Errorf("submit failed after paste")

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

// TargetExists parses a tmux target string and checks if the pane is alive.
// Returns false on parse errors or when the pane does not exist.
func TargetExists(target string) bool {
	t, err := ParseTarget(target)
	return err == nil && SessionExists(t)
}

// InjectText injects text into a tmux pane using bracketed paste.
// Uses a per-target mutex to prevent concurrent injections into the same pane.
func InjectText(target TmuxTarget, text string, submit ...bool) error {
	return injectTextInternal(target, text, len(submit) == 0 || submit[0], nil)
}

// InjectTextDiag behaves like InjectText(target, text, submit) but invokes diag(phase, pane)
// immediately BEFORE the Enter (C-m) key press ("before-enter") and shortly AFTER it
// ("after-enter"), capturing the pane each time. Both captures run under the same per-target
// inject lock as the paste+submit, so the paste->Enter sequence stays atomic — the diagnostics
// introduce no new timing window into the injection itself.
func InjectTextDiag(target TmuxTarget, text string, submit bool, diag func(phase, pane string)) error {
	return injectTextInternal(target, text, submit, diag)
}

func injectTextInternal(target TmuxTarget, text string, submit bool, diag func(phase, pane string)) error {
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
	if submit {
		// Diagnostic: snapshot the pane immediately BEFORE pressing Enter — shows whether the merged
		// text is staged in the input box, or whether an AskUserQuestion popup sits on the pane.
		if diag != nil {
			if pane, capErr := CapturePane(target); capErr == nil {
				diag("before-enter", pane)
			}
		}
		if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-m").Run(); err != nil {
			return fmt.Errorf("%w: %v", ErrSubmitAfterPaste, err)
		}
		// Diagnostic: snapshot the pane shortly AFTER Enter. A short bounded delay (still under the
		// inject lock) lets CC re-render — input cleared on submit, or the popup swallowed the Enter —
		// before we capture, so the 0ms pre-render state isn't what gets logged.
		if diag != nil {
			time.Sleep(400 * time.Millisecond)
			if pane, capErr := CapturePane(target); capErr == nil {
				diag("after-enter", pane)
			}
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

// InjectTextConfirmSubmit clears+pastes text WITHOUT Enter, then polls confirm(pane) under the per-target
// inject lock; on the first confirm==true it presses Enter (still under the lock) and returns
// submitted=true. If confirm never returns true within composeTimeout it presses nothing (the callback
// VETOes the submit) and returns submitted=false. Holding the per-target injectMu across the whole
// clear→paste→confirm→Enter sequence keeps a concurrent InjectText/InjectTextAppend from swapping the
// composer between our paste and our Enter (f29 C: codex slash-command inject confirmation).
func InjectTextConfirmSubmit(target TmuxTarget, text string, confirm func(pane string) bool, composeTimeout, poll time.Duration) (bool, error) {
	mu := getInjectLock(target)
	mu.Lock()
	defer mu.Unlock()
	text = NormalizeText(text)
	if text == "" {
		return false, fmt.Errorf("empty text after normalization")
	}
	bufName := fmt.Sprintf("tg-cli-%s", target.PaneID)
	// Exit copy-mode if active (mirrors injectTextInternal).
	out, err := tmuxCmd(target, "display-message", "-p", "-t", target.PaneID, "#{pane_mode}").Output()
	if err == nil && strings.TrimSpace(string(out)) == "copy-mode" {
		tmuxCmd(target, "send-keys", "-t", target.PaneID, "q").Run()
		time.Sleep(200 * time.Millisecond)
	}
	// Clear current input, then paste WITHOUT Enter (same primitives/timings as injectTextInternal).
	if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-u").Run(); err != nil {
		return false, fmt.Errorf("clear input failed: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := setBuffer(target, bufName, text); err != nil {
		return false, fmt.Errorf("set-buffer failed: %w", err)
	}
	if err := tmuxCmd(target, "paste-buffer", "-t", target.PaneID, "-b", bufName, "-r", "-p").Run(); err != nil {
		return false, fmt.Errorf("paste-buffer failed: %w", err)
	}
	time.Sleep(1000 * time.Millisecond)
	// Compose-confirm poll (under the lock): on the first confirm==true, submit (Enter) and return.
	deadline := time.Now().Add(composeTimeout)
	for {
		if pane, capErr := CapturePane(target); capErr == nil && confirm(pane) {
			if err := tmuxCmd(target, "send-keys", "-t", target.PaneID, "C-m").Run(); err != nil {
				return false, fmt.Errorf("%w: %v", ErrSubmitAfterPaste, err)
			}
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil // composer never showed our text within composeTimeout → VETO the submit
		}
		time.Sleep(poll)
	}
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
	args := []string{"new-session", "-d", "-s", name, "-x", "220", "-y", "50"}
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

// PaneInfo holds the per-pane fields fetched by one batched list-panes call.
type PaneInfo struct {
	Command string // #{pane_current_command}
	PID     string // #{pane_pid}
	Title   string // #{pane_title}
}

// parsePaneList parses the TAB-separated output of `list-panes -a` with the format
// "#{pane_id}\t#{pane_current_command}\t#{pane_pid}\t#{pane_title}" into a map keyed by pane_id.
// pane_title is LAST because titles may contain spaces and non-ASCII; SplitN(line,"\t",4) keeps the
// whole remainder (including spaces) as the title. Splitting on whitespace would corrupt every
// multi-word title, so this must stay a TAB split. Lines with fewer than 4 fields are skipped.
func parsePaneList(out string) map[string]PaneInfo {
	m := make(map[string]PaneInfo)
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		m[parts[0]] = PaneInfo{Command: parts[1], PID: parts[2], Title: parts[3]}
	}
	return m
}

// paneListCmd builds the single `list-panes -a` command for one tmux socket. It is a package-level var
// so a test can replace it to assert ListPanesBatch issues exactly one exec per DISTINCT socket with the
// exact flags and format string. Production always uses this default.
var paneListCmd = func(socket string) *exec.Cmd {
	return tmuxCmd(TmuxTarget{Socket: socket}, "list-panes", "-a", "-F",
		"#{pane_id}\t#{pane_current_command}\t#{pane_pid}\t#{pane_title}")
}

// ListPanesBatch fetches every pane's command, pid and title in ONE `list-panes -a` call per DISTINCT
// tmux socket among targets, returning a map keyed by FormatTarget(target). It replaces the old
// per-pane fan-out (GetPaneTitle + GetPaneCommand + #{pane_pid}) that issued 3-4 tmux execs per
// session per tick. A pane absent from every socket's output is simply not in the map (callers treat
// that as "gone"). A socket whose list-panes call errors contributes no entries.
func ListPanesBatch(targets []TmuxTarget) map[string]PaneInfo {
	sockets := make(map[string]struct{})
	for _, t := range targets {
		sockets[t.Socket] = struct{}{}
	}
	result := make(map[string]PaneInfo)
	for socket := range sockets {
		out, err := paneListCmd(socket).Output()
		if err != nil {
			continue
		}
		for paneID, info := range parsePaneList(string(out)) {
			result[FormatTarget(TmuxTarget{PaneID: paneID, Socket: socket})] = info
		}
	}
	return result
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

package helpers

import (
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/injector"
)

// GetPaneCWD returns the current working directory of the given tmux pane.
func GetPaneCWD(tmuxTarget string) string {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return ""
	}
	out, err := injector.TmuxCmd(target, "display-message", "-t", target.PaneID, "-p", "#{pane_current_path}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// GetPaneTitle returns the title of the given tmux pane.
func GetPaneTitle(tmuxTarget string) string {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return ""
	}
	title, err := injector.GetPaneTitle(target)
	if err != nil {
		return ""
	}
	return title
}

// GetPaneCommand returns the command running in the given tmux pane.
func GetPaneCommand(tmuxTarget string) string {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return ""
	}
	cmd, err := injector.GetPaneCommand(target)
	if err != nil {
		return ""
	}
	return cmd
}

// GetPaneCLICommand returns the full command line of the process running in the tmux pane.
func GetPaneCLICommand(tmuxTarget string) string {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return ""
	}
	pidOut, err := injector.TmuxCmd(target, "display-message", "-p", "-t", target.PaneID, "#{pane_pid}").Output()
	if err != nil {
		return ""
	}
	shellPID := strings.TrimSpace(string(pidOut))
	if shellPID == "" {
		return ""
	}
	cmdOut, err := exec.Command("ps", "--ppid", shellPID, "-o", "cmd", "--no-headers").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(cmdOut))
}

// GetPaneLabel returns a human-readable label (session:window.pane) for the given tmux target.
func GetPaneLabel(tmuxTarget string, formatPaneID func(string) string) string {
	paneID := formatPaneID(tmuxTarget)
	target, parseErr := injector.ParseTarget(tmuxTarget)
	if parseErr != nil {
		return paneID
	}
	out, err := injector.TmuxCmd(target, "display-message", "-t", target.PaneID, "-p", "#{session_name}:#{window_name}.#{pane_index}").Output()
	if err != nil {
		return paneID
	}
	return strings.TrimSpace(string(out))
}

// DetectPermMode captures pane content and detects the current CC permission mode.
// Returns (mode, rawContent, error). Mode is one of: "default", "plan", "auto", "acceptEdits", "bypass", "question".
func DetectPermMode(t injector.TmuxTarget) (string, string, error) {
	content, err := injector.CapturePane(t)
	if err != nil {
		return "", "", err
	}
	// Only check the bottom 5 lines where CC TUI mode indicator appears.
	// Searching full pane causes false positives from conversation content.
	lines := strings.Split(content, "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	bottom := strings.ToLower(strings.Join(lines, "\n"))
	switch {
	case strings.Contains(bottom, "bypass"):
		return "bypass", content, nil
	case strings.Contains(bottom, "plan"):
		return "plan", content, nil
	case strings.Contains(bottom, "auto mode on"):
		return "auto", content, nil
	case strings.Contains(bottom, "accept edits"):
		return "acceptEdits", content, nil
	case strings.Contains(bottom, "options") || strings.Contains(bottom, "answer"):
		// CC is showing an AskUserQuestion dialog
		return "question", content, nil
	default:
		return "default", content, nil
	}
}

// SwitchPermMode cycles BTab until the target mode is reached.
// Returns the final mode name or error if target mode is not available.
func SwitchPermMode(t injector.TmuxTarget, targetMode string) (string, error) {
	startMode, _, err := DetectPermMode(t)
	if err != nil {
		return "", err
	}
	if startMode == targetMode {
		return startMode, nil
	}
	for i := 0; i < 10; i++ {
		injector.SendKeys(t, "BTab")
		time.Sleep(500 * time.Millisecond)
		currentMode, _, err := DetectPermMode(t)
		if err != nil {
			return "", err
		}
		if currentMode == targetMode {
			return currentMode, nil
		}
		// If we've cycled back to the starting mode, target is not available
		if i > 0 && currentMode == startMode {
			return "", &modeNotAvailableError{target: targetMode, current: startMode}
		}
	}
	return "", &modeMaxAttemptsError{target: targetMode}
}

type modeNotAvailableError struct {
	target  string
	current string
}

func (e *modeNotAvailableError) Error() string {
	return "mode \"" + e.target + "\" not available in BTab cycle (cycled back to \"" + e.current + "\")"
}

type modeMaxAttemptsError struct {
	target string
}

func (e *modeMaxAttemptsError) Error() string {
	return "failed to reach mode \"" + e.target + "\" after 10 BTab presses"
}

// ProjectSlug converts an absolute path to a CC project slug by replacing
// all slashes with dashes.
func ProjectSlug(cwd string) string {
	s := strings.ReplaceAll(cwd, "/", "-")
	return strings.ReplaceAll(s, "_", "-")
}

// SessionListEntry holds metadata for a discovered CC session.
type SessionListEntry struct {
	SessionID     string
	Summary       string
	SummarySource string // "assistant" or "user"
	Modified      time.Time
}

// WaitForPaneContent polls the tmux pane until the needle string appears or timeout is reached.
func WaitForPaneContent(target injector.TmuxTarget, needle string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		content, err := injector.CapturePane(target)
		if err == nil && strings.Contains(content, needle) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// ListProjectSessions scans ~/.claude/projects/<slug>/ for session JSONL files,
// returns up to limit entries sorted by mtime descending.
// excludeID is an optional session ID to skip (e.g. the currently active session).
func ListProjectSessions(cwd string, limit int, excludeID string) ([]SessionListEntry, error) {
	home, err := getHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".claude", "projects", ProjectSlug(cwd))
	return listSessionsFromDir(dir, limit, excludeID)
}

// ListProjectSessionsByDir scans a specific directory for session JSONL files,
// returns up to limit entries sorted by mtime descending.
func ListProjectSessionsByDir(dir string, limit int, excludeID string) ([]SessionListEntry, error) {
	return listSessionsFromDir(dir, limit, excludeID)
}

// DetectBackend determines the CLI backend running in the tmux pane.
// Returns "cc", "codex", or "" (unknown/exited).
func DetectBackend(tmuxTarget string) string {
	cmd := GetPaneCommand(tmuxTarget)
	switch cmd {
	case "claude":
		return "cc"
	case "codex":
		return "codex"
	case "node":
		cliCmd := GetPaneCLICommand(tmuxTarget)
		for _, token := range strings.Fields(cliCmd) {
			base := filepath.Base(token)
			if base == "codex" {
				return "codex"
			}
			if base == "claude" {
				return "cc"
			}
		}
	}
	return ""
}

func listSessionsFromDir(dir string, limit int, excludeID string) ([]SessionListEntry, error) {
	entries, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	type fileInfo struct {
		path    string
		name    string
		modTime time.Time
	}
	var files []fileInfo
	for _, e := range entries {
		if e.isDir || !strings.HasSuffix(e.name, ".jsonl") {
			continue
		}
		files = append(files, fileInfo{
			path:    filepath.Join(dir, e.name),
			name:    strings.TrimSuffix(e.name, ".jsonl"),
			modTime: e.modTime,
		})
	}
	// Sort by mtime descending
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j].modTime.After(files[j-1].modTime); j-- {
			files[j], files[j-1] = files[j-1], files[j]
		}
	}
	var result []SessionListEntry
	for _, f := range files {
		if len(result) >= limit {
			break
		}
		if excludeID != "" && f.name == excludeID {
			continue
		}
		summary, source := ReadLastMeaningfulEntry(f.path, 4000)
		if summary == "" {
			continue
		}
		result = append(result, SessionListEntry{
			SessionID:     f.name,
			Summary:       summary,
			SummarySource: source,
			Modified:      f.modTime,
		})
	}
	return result, nil
}

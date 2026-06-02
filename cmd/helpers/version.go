package helpers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
)

// UsageCacheEntry holds cached usage API response.
type UsageCacheEntry struct {
	Data      []byte
	FetchedAt time.Time
}

// UsageAPIResponse matches the Anthropic OAuth usage API response shape.
type UsageAPIResponse struct {
	FiveHour       *UsagePeriod    `json:"five_hour"`
	SevenDay       *UsagePeriod    `json:"seven_day"`
	SevenDaySonnet *UsagePeriod    `json:"seven_day_sonnet"`
	ExtraUsage     *UsageExtraData `json:"extra_usage"`
}

// UsagePeriod holds usage data for a time period.
type UsagePeriod struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// UsageExtraData holds extra usage data.
type UsageExtraData struct {
	IsEnabled    bool     `json:"is_enabled"`
	MonthlyLimit *float64 `json:"monthly_limit"`
	UsedCredits  *float64 `json:"used_credits"`
	Utilization  *float64 `json:"utilization"`
}

// ReadContextUsage reads context usage from the tg-cli context file for a session.
func ReadContextUsage(sessionID string) (usedPct int, usedTokens int, windowSize int, ok bool) {
	const autoCompactPct = 80
	path := filepath.Join(os.TempDir(), "tg-cli", "context", sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, false
	}
	var ctx map[string]interface{}
	if err := json.Unmarshal(data, &ctx); err != nil {
		return 0, 0, 0, false
	}
	size, sizeOk := ctx["context_window_size"].(float64)
	if !sizeOk {
		return 0, 0, 0, false
	}
	currentUsage, cuOk := ctx["current_usage"].(map[string]interface{})
	if !cuOk {
		return 0, 0, 0, false
	}
	inputTokens, _ := currentUsage["input_tokens"].(float64)
	cacheCreation, _ := currentUsage["cache_creation_input_tokens"].(float64)
	cacheRead, _ := currentUsage["cache_read_input_tokens"].(float64)
	used := inputTokens + cacheCreation + cacheRead
	effectiveLimit := size * autoCompactPct / 100
	pct := int(used / effectiveLimit * 100)
	return pct, int(used), int(effectiveLimit), true
}

// ReadSessionCCVersion reads the CC current version from the tmux pane.
// CC TUI shows "current: X.Y.Z · latest: A.B.C" near the bottom when update is available.
// Returns empty if not found.
func ReadSessionCCVersion(tmuxTarget string) string {
	target, err := injector.ParseTarget(tmuxTarget)
	if err != nil {
		return ""
	}
	content, err := injector.CapturePane(target)
	if err != nil {
		return ""
	}
	lines := strings.Split(content, "\n")
	checkLines := lines
	if len(lines) > 5 {
		checkLines = lines[len(lines)-5:]
	}
	re := regexp.MustCompile(`current:\s*([\d.]+)`)
	for _, line := range checkLines {
		if m := re.FindStringSubmatch(line); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// GetInstalledCCVersion returns the version of the installed CC binary.
func GetInstalledCCVersion() string {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return ""
	}
	// Output: "2.1.89 (Claude Code)"
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// FetchUsageFormatted reads the OAuth token, calls the usage API (with 60s cache), and returns formatted HTML.
func FetchUsageFormatted(cache *UsageCacheEntry) (string, *UsageCacheEntry, error) {
	// Check cache
	if cache != nil && time.Since(cache.FetchedAt) < 60*time.Second {
		formatted, err := FormatUsageResponse(cache.Data)
		return formatted, cache, err
	}
	// Read OAuth token from ~/.claude/.credentials.json
	home, err := os.UserHomeDir()
	if err != nil {
		return "", cache, fmt.Errorf("cannot determine home dir: %w", err)
	}
	credsPath := filepath.Join(home, ".claude", ".credentials.json")
	credsData, err := os.ReadFile(credsPath)
	if err != nil {
		return "", cache, fmt.Errorf("cannot read credentials file: %w", err)
	}
	var creds map[string]interface{}
	if err := json.Unmarshal(credsData, &creds); err != nil {
		return "", cache, fmt.Errorf("cannot parse credentials: %w", err)
	}
	token, _ := creds["accessToken"].(string)
	if token == "" {
		// Try nested structure: claudeAiOauth.accessToken
		if oauth, ok := creds["claudeAiOauth"].(map[string]interface{}); ok {
			token, _ = oauth["accessToken"].(string)
		}
	}
	if token == "" {
		return "", cache, fmt.Errorf("access token not found in credentials")
	}
	// Call usage API
	req, err := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return "", cache, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", cache, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()
	var bodyBytes []byte
	bodyBytes, err = readAll(resp.Body)
	if err != nil {
		return "", cache, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", cache, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	// Cache to file
	cacheFile := filepath.Join(os.TempDir(), "tg-cli", "usage.json")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err == nil {
		os.WriteFile(cacheFile, bodyBytes, 0600)
	}
	newCache := &UsageCacheEntry{Data: bodyBytes, FetchedAt: time.Now()}
	formatted, err := FormatUsageResponse(bodyBytes)
	return formatted, newCache, err
}

// FormatUsageResponse parses the usage API JSON and returns a TG HTML message.
func FormatUsageResponse(data []byte) (string, error) {
	var u UsageAPIResponse
	if err := json.Unmarshal(data, &u); err != nil {
		return "", fmt.Errorf("parse usage response: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("📊 CC Usage\n")
	if u.FiveHour != nil {
		pct := int(u.FiveHour.Utilization)
		resetTime := ParseResetTime(u.FiveHour.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent session: %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if u.SevenDay != nil {
		pct := int(u.SevenDay.Utilization)
		resetTime := ParseResetTime(u.SevenDay.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent week (all models): %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if u.SevenDaySonnet != nil {
		pct := int(u.SevenDaySonnet.Utilization)
		resetTime := ParseResetTime(u.SevenDaySonnet.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent week (Sonnet only): %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if u.ExtraUsage != nil {
		sb.WriteString("\nExtra usage\n")
		if !u.ExtraUsage.IsEnabled {
			sb.WriteString("Extra usage not enabled • /extra-usage to enable\n")
		} else if u.ExtraUsage.UsedCredits != nil && u.ExtraUsage.MonthlyLimit != nil {
			used := *u.ExtraUsage.UsedCredits / 100.0
			limit := *u.ExtraUsage.MonthlyLimit / 100.0
			sb.WriteString(fmt.Sprintf("$%.2f / $%.2f used\n", used, limit))
		}
	}
	result := strings.TrimRight(sb.String(), "\n")
	if result == "📊 CC Usage" {
		return "", fmt.Errorf("no usage data in response")
	}
	return result, nil
}

// FetchUsageTmux creates a temporary tmux session, launches CC, runs /usage, and returns formatted output.
func FetchUsageTmux() (string, error) {
	// Clean up stale sessions from previous failed runs
	out, _ := injector.GlobalTmuxCmd("list-sessions", "-F", "#{session_name}").Output()
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(name, "tg-cli-usage-") {
			logger.Info(fmt.Sprintf("FetchUsageTmux: killing stale session %s", name))
			injector.GlobalTmuxCmd("kill-session", "-t", name).Run()
		}
	}
	sessionName := fmt.Sprintf("tg-cli-usage-%d", time.Now().UnixMilli())
	configDir := config.GetConfigDir()
	cmd := injector.GlobalTmuxCmd("new-session", "-d", "-s", sessionName, "-c", configDir, "-x", "120", "-y", "40")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to create temp session: %w", err)
	}
	target, _ := injector.ParseTarget(sessionName)
	injector.SendKeys(target, "claude", "Enter")
	if !WaitForPaneContent(target, "❯", 30*time.Second) {
		dump, _ := injector.CapturePane(target)
		logger.Error(fmt.Sprintf("FetchUsageTmux: CC init timeout, session=%s kept alive for debugging, pane dump:\n%s", sessionName, dump))
		return "", fmt.Errorf("CC failed to initialize (timeout waiting for ❯), session %s kept alive", sessionName)
	}
	injector.SendKeys(target, "/usage", "Enter")
	if !WaitForPaneContent(target, "used", 10*time.Second) {
		dump, _ := injector.CapturePane(target)
		logger.Error(fmt.Sprintf("FetchUsageTmux: /usage timeout, session=%s kept alive for debugging, pane dump:\n%s", sessionName, dump))
		return "", fmt.Errorf("failed to get usage data (timeout waiting for 'used'), session %s kept alive", sessionName)
	}
	time.Sleep(1 * time.Second)
	content, err := injector.CapturePane(target)
	if err != nil {
		logger.Error(fmt.Sprintf("FetchUsageTmux: capture failed, session=%s kept alive", sessionName))
		return "", fmt.Errorf("failed to capture pane: %w, session %s kept alive", err, sessionName)
	}
	formatted := ParseUsageOutput(content)
	if formatted == "" {
		logger.Error(fmt.Sprintf("FetchUsageTmux: parse empty, session=%s kept alive, raw pane:\n%s", sessionName, content))
		return "", fmt.Errorf("failed to parse usage data (empty result), session %s kept alive", sessionName)
	}
	injector.GlobalTmuxCmd("kill-session", "-t", sessionName).Run()
	return formatted, nil
}

// ParseResetTime formats a reset timestamp for display, always including day and timezone.
func ParseResetTime(ts string, includeDay bool) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Try without timezone suffix
		t, err = time.Parse("2006-01-02T15:04:05Z", ts)
		if err != nil {
			return ts
		}
	}
	t = t.Local()
	tz := GetIANATimezone()
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("3pm") + " (" + tz + ")"
	}
	return t.Format("Jan 2, 3pm") + " (" + tz + ")"
}

// GetIANATimezone returns the IANA timezone name (e.g., "Asia/Shanghai").
func GetIANATimezone() string {
	link, err := os.Readlink("/etc/localtime")
	if err == nil {
		if idx := strings.Index(link, "zoneinfo/"); idx >= 0 {
			return link[idx+len("zoneinfo/"):]
		}
	}
	zone, _ := time.Now().Zone()
	return zone
}

// ParseUsageOutput extracts relevant usage lines from raw CC pane output.
func ParseUsageOutput(raw string) string {
	lines := strings.Split(raw, "\n")
	var result []string
	result = append(result, "📊 CC Usage\n")
	var currentSection string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Settings:") ||
			trimmed == "Esc to cancel" || strings.HasPrefix(trimmed, "❯") ||
			strings.HasPrefix(trimmed, "╭") || strings.HasPrefix(trimmed, "╰") {
			continue
		}
		if !strings.Contains(trimmed, "% used") && !strings.HasPrefix(trimmed, "Resets") &&
			!strings.HasPrefix(trimmed, "█") && !strings.HasPrefix(trimmed, "Extra") &&
			!strings.HasPrefix(trimmed, "▌") && len(trimmed) > 5 {
			if strings.HasPrefix(trimmed, "Current") || strings.HasPrefix(trimmed, "Extra") {
				currentSection = trimmed
			}
			continue
		}
		if strings.Contains(trimmed, "% used") {
			if m := findUsagePct(trimmed); m != "" {
				if currentSection != "" {
					result = append(result, fmt.Sprintf("%s: %s", currentSection, m))
				}
			}
		}
		if strings.HasPrefix(trimmed, "Resets") {
			result = append(result, fmt.Sprintf("⏰ %s\n", trimmed))
		}
		if strings.HasPrefix(trimmed, "Extra usage") {
			result = append(result, trimmed)
		}
	}
	return strings.Join(result, "\n")
}

// findUsagePct extracts "X% used" pattern from a string.
func findUsagePct(s string) string {
	// Find pattern like "80% used"
	idx := strings.Index(s, "% used")
	if idx < 0 {
		return ""
	}
	// Walk backwards to find the number
	start := idx
	for start > 0 && (s[start-1] >= '0' && s[start-1] <= '9') {
		start--
	}
	if start == idx {
		return ""
	}
	return s[start : idx+len("% used")]
}

// readAll reads all bytes from a reader (avoids io import cycle).
func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 512)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}

// knownTools is the set of named tool categories. Tools not in this set are classified as "Other".
var knownTools = map[string]bool{
	"Edit": true, "Write": true, "Bash": true, "Read": true,
	"Glob": true, "Grep": true, "Agent": true, "WebFetch": true,
	"WebSearch": true, "MCP": true, "Skill": true,
	"TaskCreate": true, "TaskUpdate": true, "TaskGet": true,
	"TaskList": true, "TaskStop": true, "TaskOutput": true,
	"NotebookEdit": true, "EnterPlanMode": true, "ExitPlanMode": true,
	"EnterWorktree": true, "ExitWorktree": true,
}

// ShouldNotifyTool checks if the user has configured notifications for the given tool name.
func ShouldNotifyTool(toolName string, toolNotifyEnabled *bool, toolNotifyList []string) bool {
	if toolNotifyEnabled != nil && !*toolNotifyEnabled {
		return false
	}
	displayName := toolName
	if strings.HasPrefix(toolName, "mcp__") {
		displayName = "MCP"
	}
	for _, t := range toolNotifyList {
		if t == displayName {
			return true
		}
	}
	// Unknown tools fall through to "Other" category
	if !knownTools[displayName] {
		for _, t := range toolNotifyList {
			if t == "Other" {
				return true
			}
		}
	}
	return false
}

// LogVersion logs a version check result.
func LogVersion(target, current, latest string) {
	logger.Info(fmt.Sprintf("CC version update detected: target=%s current=%s latest=%s", target, current, latest))
}

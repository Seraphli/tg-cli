package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// ErrCredentialUnavailable is returned when credentials are missing or inaccessible.
var ErrCredentialUnavailable = errors.New("credential unavailable")

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

// DynamicTier represents a dynamically detected usage tier from the API response.
type DynamicTier struct {
	Name        string
	Utilization float64
	ResetsAt    string
}

// UsageFullResponse extends UsageAPIResponse with dynamically detected tiers.
type UsageFullResponse struct {
	UsageAPIResponse
	DynamicTiers []DynamicTier
}

// knownUsageKeys lists the well-known top-level keys in the usage API response.
var knownUsageKeys = map[string]bool{
	"five_hour": true, "seven_day": true,
	"seven_day_sonnet": true, "extra_usage": true,
}

// parseFullUsageResponse parses usage API JSON including any dynamic tier keys.
func parseFullUsageResponse(data []byte) (*UsageFullResponse, error) {
	var full UsageFullResponse
	if err := json.Unmarshal(data, &full.UsageAPIResponse); err != nil {
		return nil, fmt.Errorf("parse usage response: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil
	}
	for key, val := range raw {
		if knownUsageKeys[key] {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(val, &fields) != nil {
			continue
		}
		_, hasUtilization := fields["utilization"]
		resetsAtRaw, hasResetsAt := fields["resets_at"]
		if !hasUtilization || !hasResetsAt {
			continue
		}
		var utilization float64
		json.Unmarshal(fields["utilization"], &utilization)
		var resetsAt string
		json.Unmarshal(resetsAtRaw, &resetsAt)
		full.DynamicTiers = append(full.DynamicTiers, DynamicTier{
			Name:        formatTierName(key),
			Utilization: utilization,
			ResetsAt:    resetsAt,
		})
	}
	return &full, nil
}

// formatTierName converts a snake_case key to Title Case for display.
func formatTierName(key string) string {
	words := strings.Split(key, "_")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// readClaudeToken reads the OAuth access token from the credentials file.
// Returns the token, whether it is expired, and any error.
func readClaudeToken(credsPath string) (token string, expired bool, err error) {
	data, err := os.ReadFile(credsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, fmt.Errorf("%w: %s", ErrCredentialUnavailable, err)
		}
		return "", false, fmt.Errorf("cannot read credentials file: %w", err)
	}
	var creds map[string]interface{}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", false, fmt.Errorf("cannot parse credentials: %w", err)
	}
	var expiresAt interface{}
	token, _ = creds["accessToken"].(string)
	if token == "" {
		if oauth, ok := creds["claudeAiOauth"].(map[string]interface{}); ok {
			token, _ = oauth["accessToken"].(string)
			if token != "" {
				expiresAt = oauth["expiresAt"]
			}
		}
	}
	if token == "" {
		expiresAt = nil
		if oauth, ok := creds["claude.ai_oauth"].(map[string]interface{}); ok {
			token, _ = oauth["accessToken"].(string)
			if token != "" {
				expiresAt = oauth["expiresAt"]
			}
		}
	}
	if token == "" {
		return "", false, fmt.Errorf("%w: No Claude OAuth token found", ErrCredentialUnavailable)
	}
	if expiresAt != nil {
		expired = isTokenExpired(expiresAt)
	}
	return token, expired, nil
}

// isTokenExpired checks whether an expiresAt value (unix ms float64 or RFC3339 string) is in the past.
func isTokenExpired(expiresAt interface{}) bool {
	var expTime time.Time
	switch v := expiresAt.(type) {
	case float64:
		if v > 1e12 {
			v = v / 1000
		}
		expTime = time.Unix(int64(v), 0)
	case string:
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return false
		}
		expTime = t
	default:
		return false
	}
	return time.Now().After(expTime)
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

const defaultClaudeAPIURL = "https://api.anthropic.com/api/oauth/usage"

// FetchClaudeUsage fetches Claude usage from the Anthropic OAuth API using the given credentials path.
// Uses in-memory cache; apiURL overrides the default endpoint when non-empty.
func FetchClaudeUsage(cache *UsageCacheEntry, credsPath, apiURL string) (string, *UsageCacheEntry, error) {
	if cache != nil && time.Since(cache.FetchedAt) < 60*time.Second {
		formatted, err := FormatClaudeUsage(cache.Data)
		return formatted, cache, err
	}
	token, expired, err := readClaudeToken(credsPath)
	if err != nil {
		return "", cache, err
	}
	if apiURL == "" {
		apiURL = defaultClaudeAPIURL
	}
	req, err := http.NewRequest("GET", apiURL, nil)
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
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", cache, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized && expired {
			return "", cache, fmt.Errorf("Claude token expired, please re-authenticate")
		}
		return "", cache, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	cacheFile := filepath.Join(os.TempDir(), "tg-cli", "usage.json")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err == nil {
		os.WriteFile(cacheFile, bodyBytes, 0600)
	}
	newCache := &UsageCacheEntry{Data: bodyBytes, FetchedAt: time.Now()}
	formatted, err := FormatClaudeUsage(bodyBytes)
	return formatted, newCache, err
}

// FetchUsageFormatted is a thin wrapper around FetchClaudeUsage using the default credentials path.
func FetchUsageFormatted(cache *UsageCacheEntry) (string, *UsageCacheEntry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", cache, fmt.Errorf("cannot determine home dir: %w", err)
	}
	credsPath := filepath.Join(home, ".claude", ".credentials.json")
	return FetchClaudeUsage(cache, credsPath, "")
}

// FormatClaudeUsage parses the usage API JSON and returns a formatted message.
func FormatClaudeUsage(data []byte) (string, error) {
	full, err := parseFullUsageResponse(data)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("📊 Claude Usage\n")
	if full.FiveHour != nil {
		pct := int(full.FiveHour.Utilization)
		resetTime := ParseResetTime(full.FiveHour.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent session: %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if full.SevenDay != nil {
		pct := int(full.SevenDay.Utilization)
		resetTime := ParseResetTime(full.SevenDay.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent week (all models): %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if full.SevenDaySonnet != nil {
		pct := int(full.SevenDaySonnet.Utilization)
		resetTime := ParseResetTime(full.SevenDaySonnet.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\nCurrent week (Sonnet only): %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	for _, dt := range full.DynamicTiers {
		pct := int(dt.Utilization)
		resetTime := ParseResetTime(dt.ResetsAt, true)
		sb.WriteString(fmt.Sprintf("\n%s: %d%% used\n⏰ Resets %s\n", dt.Name, pct, resetTime))
	}
	if full.ExtraUsage != nil {
		sb.WriteString("\nExtra usage\n")
		if !full.ExtraUsage.IsEnabled {
			sb.WriteString("Extra usage not enabled • /extra-usage to enable\n")
		} else if full.ExtraUsage.UsedCredits != nil && full.ExtraUsage.MonthlyLimit != nil {
			used := *full.ExtraUsage.UsedCredits / 100.0
			limit := *full.ExtraUsage.MonthlyLimit / 100.0
			sb.WriteString(fmt.Sprintf("$%.2f / $%.2f used\n", used, limit))
		}
	}
	result := strings.TrimRight(sb.String(), "\n")
	if result == "📊 Claude Usage" {
		return "", fmt.Errorf("no usage data in response")
	}
	return result, nil
}

// FormatUsageResponse is a backward-compatibility alias for FormatClaudeUsage.
func FormatUsageResponse(data []byte) (string, error) {
	return FormatClaudeUsage(data)
}

const defaultCodexAPIURL = "https://chatgpt.com/backend-api/wham/usage"

// CodexWindow holds usage data for a single rate-limit window.
type CodexWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// CodexRateLimit holds the primary and secondary usage windows for Codex.
type CodexRateLimit struct {
	PrimaryWindow   *CodexWindow `json:"primary_window"`
	SecondaryWindow *CodexWindow `json:"secondary_window"`
}

// CodexUsageResponse is the top-level Codex usage API response.
type CodexUsageResponse struct {
	RateLimit CodexRateLimit `json:"rate_limit"`
}

// readCodexToken reads the Codex access token and account ID from auth.json.
func readCodexToken(codexHome string) (accessToken, accountID string, err error) {
	if codexHome == "" {
		codexHome = os.Getenv("CODEX_HOME")
	}
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		codexHome = filepath.Join(home, ".codex")
	}
	authPath := filepath.Join(codexHome, "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("%w: %s", ErrCredentialUnavailable, err)
		}
		return "", "", fmt.Errorf("cannot read Codex auth file: %w", err)
	}
	var auth struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", "", fmt.Errorf("cannot parse Codex auth file: %w", err)
	}
	if auth.AuthMode != "chatgpt" {
		return "", "", fmt.Errorf("%w: auth_mode is %q, expected chatgpt", ErrCredentialUnavailable, auth.AuthMode)
	}
	if auth.Tokens.AccessToken == "" {
		return "", "", fmt.Errorf("%w: Codex access token is empty", ErrCredentialUnavailable)
	}
	return auth.Tokens.AccessToken, auth.Tokens.AccountID, nil
}

// FetchCodexUsage fetches Codex usage from the ChatGPT backend API.
// Uses in-memory cache; apiURL overrides the default endpoint when non-empty.
func FetchCodexUsage(cache *UsageCacheEntry, apiURL string) (string, *UsageCacheEntry, error) {
	if cache != nil && time.Since(cache.FetchedAt) < 60*time.Second {
		formatted, err := formatCodexUsage(cache.Data)
		return formatted, cache, err
	}
	token, accountID, err := readCodexToken("")
	if err != nil {
		return "", cache, err
	}
	if apiURL == "" {
		apiURL = defaultCodexAPIURL
	}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", cache, fmt.Errorf("build Codex request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "codex-cli")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", cache, fmt.Errorf("Codex API request failed: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", cache, fmt.Errorf("read Codex response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", cache, fmt.Errorf("Codex token expired, restart Codex CLI to re-login")
	}
	if resp.StatusCode != http.StatusOK {
		return "", cache, fmt.Errorf("Codex API error: %d", resp.StatusCode)
	}
	newCache := &UsageCacheEntry{Data: bodyBytes, FetchedAt: time.Now()}
	formatted, err := formatCodexUsage(bodyBytes)
	return formatted, newCache, err
}

// formatCodexUsage parses the Codex usage API JSON and returns a formatted message.
func formatCodexUsage(data []byte) (string, error) {
	var resp CodexUsageResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse Codex response: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("📊 Codex Usage\n")
	for _, w := range []*CodexWindow{resp.RateLimit.PrimaryWindow, resp.RateLimit.SecondaryWindow} {
		if w == nil {
			continue
		}
		label := codexWindowLabel(w.LimitWindowSeconds)
		resetTime := formatCodexResetTime(w.ResetAt)
		sb.WriteString(fmt.Sprintf("\n%s: %d%% used\n⏰ Resets %s\n", label, int(w.UsedPercent), resetTime))
	}
	result := strings.TrimRight(sb.String(), "\n")
	if result == "📊 Codex Usage" {
		return "", fmt.Errorf("no Codex usage data in response")
	}
	return result, nil
}

// codexWindowLabel converts a limit window duration in seconds to a human-readable label.
func codexWindowLabel(limitSeconds int) string {
	if limitSeconds <= 21600 {
		return "Current session"
	}
	if limitSeconds <= 604800 {
		return "Current week"
	}
	return fmt.Sprintf("%ds window", limitSeconds)
}

// formatCodexResetTime formats a Unix timestamp as a local reset time string.
func formatCodexResetTime(unixSec int64) string {
	t := time.Unix(unixSec, 0).Local()
	tz := GetIANATimezone()
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("3pm") + " (" + tz + ")"
	}
	return t.Format("Jan 2, 3pm") + " (" + tz + ")"
}

// StatuslineRateLimit holds a single rate limit window's usage data from the statusline context file.
type StatuslineRateLimit struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       float64 `json:"resets_at"`
}

// StatuslineRateLimits holds the five-hour and seven-day rate limit windows from the statusline context file.
type StatuslineRateLimits struct {
	FiveHour *StatuslineRateLimit `json:"five_hour"`
	SevenDay *StatuslineRateLimit `json:"seven_day"`
}

// ReadStatuslineRateLimits reads rate limits from the newest tg-cli context JSON file.
func ReadStatuslineRateLimits() (*StatuslineRateLimits, error) {
	dir := filepath.Join(os.TempDir(), "tg-cli", "context")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestTime) {
			newest = e.Name()
			newestTime = info.ModTime()
		}
	}
	if newest == "" {
		return nil, fmt.Errorf("no context files found")
	}
	data, err := os.ReadFile(filepath.Join(dir, newest))
	if err != nil {
		return nil, err
	}
	var ctx map[string]json.RawMessage
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, err
	}
	rlRaw, ok := ctx["rate_limits"]
	if !ok {
		return nil, fmt.Errorf("no rate_limits in context")
	}
	var rl StatuslineRateLimits
	if err := json.Unmarshal(rlRaw, &rl); err != nil {
		return nil, fmt.Errorf("parse rate_limits: %w", err)
	}
	if rl.FiveHour == nil && rl.SevenDay == nil {
		return nil, fmt.Errorf("rate_limits empty")
	}
	return &rl, nil
}

// formatStatuslineUsage formats a StatuslineRateLimits into a human-readable usage message.
func formatStatuslineUsage(rl *StatuslineRateLimits) string {
	var sb strings.Builder
	sb.WriteString("📊 Claude Usage\n")
	if rl.FiveHour != nil {
		pct := int(rl.FiveHour.UsedPercentage)
		resetTime := formatCodexResetTime(int64(rl.FiveHour.ResetsAt))
		sb.WriteString(fmt.Sprintf("\nCurrent session: %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	if rl.SevenDay != nil {
		pct := int(rl.SevenDay.UsedPercentage)
		resetTime := formatCodexResetTime(int64(rl.SevenDay.ResetsAt))
		sb.WriteString(fmt.Sprintf("\nCurrent week (all models): %d%% used\n⏰ Resets %s\n", pct, resetTime))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// MergedUsageOptions holds overridable parameters for FetchMergedUsage.
type MergedUsageOptions struct {
	ClaudeCredsPath string
	ClaudeAPIURL    string
	CodexAPIURL     string
}

// FetchMergedUsage fetches combined Claude and Codex usage, preferring statusline data for Claude.
func FetchMergedUsage(claudeCache *UsageCacheEntry, codexCache *UsageCacheEntry) (string, *UsageCacheEntry, *UsageCacheEntry, error) {
	home, _ := os.UserHomeDir()
	return fetchMergedUsageWith(claudeCache, codexCache, MergedUsageOptions{
		ClaudeCredsPath: filepath.Join(home, ".claude", ".credentials.json"),
	})
}

// FetchMergedUsageWith fetches combined Claude and Codex usage with custom options.
func FetchMergedUsageWith(claudeCache *UsageCacheEntry, codexCache *UsageCacheEntry, opts MergedUsageOptions) (string, *UsageCacheEntry, *UsageCacheEntry, error) {
	return fetchMergedUsageWith(claudeCache, codexCache, opts)
}

// fetchMergedUsageWith is the internal implementation of FetchMergedUsage.
// It prefers statusline context data for Claude usage; falls back to API if unavailable.
func fetchMergedUsageWith(claudeCache *UsageCacheEntry, codexCache *UsageCacheEntry, opts MergedUsageOptions) (string, *UsageCacheEntry, *UsageCacheEntry, error) {
	var claudeSection string
	var claudeErr error

	rl, err := ReadStatuslineRateLimits()
	if err == nil && rl != nil {
		claudeSection = formatStatuslineUsage(rl)
	} else {
		claudeSection, claudeCache, claudeErr = FetchClaudeUsage(claudeCache, opts.ClaudeCredsPath, opts.ClaudeAPIURL)
	}

	codexSection, codexCache, codexErr := FetchCodexUsage(codexCache, opts.CodexAPIURL)

	claudeSkip := claudeErr != nil && errors.Is(claudeErr, ErrCredentialUnavailable)
	codexSkip := codexErr != nil && errors.Is(codexErr, ErrCredentialUnavailable)
	claudeRealErr := claudeErr != nil && !claudeSkip
	codexRealErr := codexErr != nil && !codexSkip

	if claudeSection == "" && codexSection == "" {
		if claudeSkip && codexSkip {
			return "", claudeCache, codexCache, fmt.Errorf("No usage credentials found")
		}
		if claudeRealErr {
			return "", claudeCache, codexCache, claudeErr
		}
		if codexRealErr {
			return "", claudeCache, codexCache, codexErr
		}
		return "", claudeCache, codexCache, fmt.Errorf("No usage credentials found")
	}

	var sb strings.Builder
	if claudeSection != "" {
		sb.WriteString(claudeSection)
	}
	if codexSection != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(codexSection)
	}
	if claudeSection != "" && codexRealErr {
		sb.WriteString(fmt.Sprintf("\n\n⚠️ Codex: %s", codexErr.Error()))
	}
	if codexSection != "" && claudeRealErr {
		sb.WriteString(fmt.Sprintf("\n\n⚠️ Claude: %s", claudeErr.Error()))
	}
	return sb.String(), claudeCache, codexCache, nil
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
	// Always clean up the temp session on return. On paths that successfully capture a pane dump,
	// the capture + logging happen before this deferred kill runs, so debug screenshots are preserved.
	defer injector.GlobalTmuxCmd("kill-session", "-t", sessionName).Run()
	target, _ := injector.ParseTarget(sessionName)
	injector.SendKeys(target, "claude", "Enter")
	if !WaitForPaneContent(target, "❯", 30*time.Second) {
		dump, _ := injector.CapturePane(target)
		logger.Error(fmt.Sprintf("FetchUsageTmux: CC init timeout, session=%s (cleaned up after dump), pane dump:\n%s", sessionName, dump))
		return "", fmt.Errorf("CC failed to initialize (timeout waiting for ❯), session %s cleaned up", sessionName)
	}
	injector.SendKeys(target, "/usage", "Enter")
	if !WaitForPaneContent(target, "used", 10*time.Second) {
		dump, _ := injector.CapturePane(target)
		logger.Error(fmt.Sprintf("FetchUsageTmux: /usage timeout, session=%s (cleaned up after dump), pane dump:\n%s", sessionName, dump))
		return "", fmt.Errorf("failed to get usage data (timeout waiting for 'used'), session %s cleaned up", sessionName)
	}
	time.Sleep(1 * time.Second)
	content, err := injector.CapturePane(target)
	if err != nil {
		logger.Error(fmt.Sprintf("FetchUsageTmux: capture failed, session=%s (cleaned up)", sessionName))
		return "", fmt.Errorf("failed to capture pane: %w, session %s cleaned up", err, sessionName)
	}
	formatted := ParseUsageOutput(content)
	if formatted == "" {
		logger.Error(fmt.Sprintf("FetchUsageTmux: parse empty, session=%s (cleaned up), raw pane:\n%s", sessionName, content))
		return "", fmt.Errorf("failed to parse usage data (empty result), session %s cleaned up", sessionName)
	}
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
	result = append(result, "📊 Claude Usage\n")
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

package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validClaudeUsageJSON returns a minimal valid Claude usage API response JSON.
func validClaudeUsageJSON() []byte {
	return []byte(`{"five_hour":{"utilization":40,"resets_at":"2026-06-14T12:00:00Z"},"seven_day":{"utilization":20,"resets_at":"2026-06-14T12:00:00Z"}}`)
}

// validCodexUsageJSON returns a minimal valid Codex usage API response JSON.
func validCodexUsageJSON() []byte {
	return []byte(`{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_at":1780000000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1781000000}}}`)
}

// writeTempCreds writes a Claude credentials file in tmpDir and returns its path.
func writeTempCreds(t *testing.T, tmpDir string, content map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal creds: %v", err)
	}
	path := filepath.Join(tmpDir, ".credentials.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	return path
}

// writeCodexAuth writes a Codex auth.json in tmpDir.
func writeCodexAuth(t *testing.T, tmpDir string, content map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "auth.json"), data, 0600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

// TC1a: dynamic tier with 0% utilization must appear in FormatClaudeUsage output
func TestFormatClaudeUsage_DynamicTierZero(t *testing.T) {
	data := []byte(`{"five_hour":{"utilization":40,"resets_at":"2026-06-14T12:00:00Z"},"thirty_day":{"utilization":0,"resets_at":"2026-06-14T12:00:00Z"}}`)
	out, err := FormatClaudeUsage(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Thirty Day: 0% used") {
		t.Errorf("expected 'Thirty Day: 0%% used' in output, got:\n%s", out)
	}
}

// TC1b: dynamic tier missing utilization field must not appear in output
func TestFormatClaudeUsage_DynamicTierMissingUtilization(t *testing.T) {
	data := []byte(`{"five_hour":{"utilization":40,"resets_at":"2026-06-14T12:00:00Z"},"mystery_tier":{"resets_at":"2026-06-14T12:00:00Z"}}`)
	out, err := FormatClaudeUsage(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "Mystery Tier") {
		t.Errorf("did not expect 'Mystery Tier' in output, got:\n%s", out)
	}
}

// TC2: readClaudeToken triple-path fallback
func TestReadClaudeToken(t *testing.T) {
	t.Run("flat_accessToken", func(t *testing.T) {
		tmpDir := t.TempDir()
		credsPath := writeTempCreds(t, tmpDir, map[string]interface{}{
			"accessToken": "tok-flat",
		})
		token, expired, err := readClaudeToken(credsPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "tok-flat" {
			t.Errorf("expected tok-flat, got %q", token)
		}
		if expired {
			t.Errorf("expected expired=false for flat accessToken (no expiresAt)")
		}
	})

	t.Run("claudeAiOauth_expired", func(t *testing.T) {
		tmpDir := t.TempDir()
		credsPath := writeTempCreds(t, tmpDir, map[string]interface{}{
			"claudeAiOauth": map[string]interface{}{
				"accessToken": "tok-nested",
				"expiresAt":   "2020-01-01T00:00:00Z",
			},
		})
		token, expired, err := readClaudeToken(credsPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "tok-nested" {
			t.Errorf("expected tok-nested, got %q", token)
		}
		if !expired {
			t.Errorf("expected expired=true for past expiresAt")
		}
	})

	t.Run("claude_ai_oauth_not_expired", func(t *testing.T) {
		tmpDir := t.TempDir()
		credsPath := writeTempCreds(t, tmpDir, map[string]interface{}{
			"claude.ai_oauth": map[string]interface{}{
				"accessToken": "tok-dot",
				"expiresAt":   "2099-01-01T00:00:00Z",
			},
		})
		token, expired, err := readClaudeToken(credsPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "tok-dot" {
			t.Errorf("expected tok-dot, got %q", token)
		}
		if expired {
			t.Errorf("expected expired=false for future expiresAt")
		}
	})

	t.Run("no_token_wraps_ErrCredentialUnavailable", func(t *testing.T) {
		tmpDir := t.TempDir()
		credsPath := writeTempCreds(t, tmpDir, map[string]interface{}{})
		_, _, err := readClaudeToken(credsPath)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, ErrCredentialUnavailable) {
			t.Errorf("expected ErrCredentialUnavailable, got %v", err)
		}
	})
}

// TC9: FetchClaudeUsage with expired token + httptest 200 → normal output
func TestFetchClaudeUsage_ExpiredToken200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(validClaudeUsageJSON())
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	credsPath := writeTempCreds(t, tmpDir, map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken": "tok-expired",
			"expiresAt":   "2020-01-01T00:00:00Z",
		},
	})

	out, _, err := FetchClaudeUsage(nil, credsPath, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Claude Usage") {
		t.Errorf("expected 'Claude Usage' in output, got:\n%s", out)
	}
}

// TC10: FetchClaudeUsage with expired token + httptest 401 → "expired" error
func TestFetchClaudeUsage_ExpiredToken401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	credsPath := writeTempCreds(t, tmpDir, map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken": "tok-expired",
			"expiresAt":   "2020-01-01T00:00:00Z",
		},
	})

	_, _, err := FetchClaudeUsage(nil, credsPath, srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected 'expired' in error message, got: %v", err)
	}
}

// TC3: FetchCodexUsage normal query
func TestFetchCodexUsage_Normal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(validCodexUsageJSON())
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	t.Setenv("CODEX_HOME", tmpDir)
	writeCodexAuth(t, tmpDir, map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens": map[string]interface{}{
			"access_token": "tok-codex",
		},
	})

	out, _, err := FetchCodexUsage(nil, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Codex Usage") {
		t.Errorf("expected 'Codex Usage' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Current session: 30% used") {
		t.Errorf("expected 'Current session: 30%% used' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Current week: 10% used") {
		t.Errorf("expected 'Current week: 10%% used' in output, got:\n%s", out)
	}
}

// TC4: FetchCodexUsage with missing auth.json
func TestFetchCodexUsage_MissingAuth(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CODEX_HOME", tmpDir)

	_, _, err := FetchCodexUsage(nil, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Errorf("expected ErrCredentialUnavailable, got %v", err)
	}
}

// TC11: FetchCodexUsage with CODEX_HOME env
func TestFetchCodexUsage_CustomCODEXHOME(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(validCodexUsageJSON())
	}))
	defer srv.Close()

	customDir := t.TempDir()
	t.Setenv("CODEX_HOME", customDir)
	writeCodexAuth(t, customDir, map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens": map[string]interface{}{
			"access_token": "tok-custom",
		},
	})

	out, _, err := FetchCodexUsage(nil, srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "Codex Usage") {
		t.Errorf("expected 'Codex Usage' in output, got:\n%s", out)
	}
}

// TC12: FetchCodexUsage with auth_mode != "chatgpt"
func TestFetchCodexUsage_WrongAuthMode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CODEX_HOME", tmpDir)
	writeCodexAuth(t, tmpDir, map[string]interface{}{
		"auth_mode": "api_key",
		"tokens": map[string]interface{}{
			"access_token": "tok",
		},
	})

	_, _, err := FetchCodexUsage(nil, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Errorf("expected ErrCredentialUnavailable, got %v", err)
	}
}

// TC13: FetchCodexUsage with empty access_token
func TestFetchCodexUsage_EmptyToken(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CODEX_HOME", tmpDir)
	writeCodexAuth(t, tmpDir, map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens": map[string]interface{}{
			"access_token": "",
		},
	})

	_, _, err := FetchCodexUsage(nil, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Errorf("expected ErrCredentialUnavailable, got %v", err)
	}
}

// TC14: FetchCodexUsage with httptest 401
func TestFetchCodexUsage_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	t.Setenv("CODEX_HOME", tmpDir)
	writeCodexAuth(t, tmpDir, map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens": map[string]interface{}{
			"access_token": "tok-valid",
		},
	})

	_, _, err := FetchCodexUsage(nil, srv.URL)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected 'expired' in error message, got: %v", err)
	}
}

// TC6: ReadStatuslineRateLimits reads rate limits from context file
func TestReadStatuslineRateLimits(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	// Override os.TempDir by creating the expected path
	contextDir := filepath.Join(tmpDir, "tg-cli", "context")
	if err := os.MkdirAll(contextDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	contextJSON := `{"rate_limits":{"five_hour":{"used_percentage":25,"resets_at":1780000000},"seven_day":{"used_percentage":8,"resets_at":1781000000}}}`
	if err := os.WriteFile(filepath.Join(contextDir, "session123.json"), []byte(contextJSON), 0600); err != nil {
		t.Fatalf("write context: %v", err)
	}

	rl, err := ReadStatuslineRateLimits()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rl.FiveHour == nil {
		t.Fatal("expected FiveHour to be non-nil")
	}
	if rl.FiveHour.UsedPercentage != 25 {
		t.Errorf("expected FiveHour.UsedPercentage=25, got %v", rl.FiveHour.UsedPercentage)
	}
	if rl.SevenDay == nil {
		t.Fatal("expected SevenDay to be non-nil")
	}
	if rl.SevenDay.UsedPercentage != 8 {
		t.Errorf("expected SevenDay.UsedPercentage=8, got %v", rl.SevenDay.UsedPercentage)
	}
}

// setupMergedTestEnv creates mock servers and credential files for FetchMergedUsageWith tests.
// Returns opts, tmpDir, and cleanup function.
func setupMergedEnv(t *testing.T, claudeStatus, codexStatus int) (MergedUsageOptions, string) {
	t.Helper()

	claudeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if claudeStatus == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(claudeStatus)
		if claudeStatus == http.StatusOK {
			w.Write(validClaudeUsageJSON())
		}
	}))
	t.Cleanup(claudeSrv.Close)

	codexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if codexStatus == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(codexStatus)
		if codexStatus == http.StatusOK {
			w.Write(validCodexUsageJSON())
		}
	}))
	t.Cleanup(codexSrv.Close)

	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, "claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatalf("mkdir claude: %v", err)
	}
	credsPath := writeTempCreds(t, claudeDir, map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken": "tok-merged",
			"expiresAt":   "2020-01-01T00:00:00Z",
		},
	})

	codexDir := filepath.Join(tmpDir, "codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatalf("mkdir codex: %v", err)
	}
	t.Setenv("CODEX_HOME", codexDir)
	writeCodexAuth(t, codexDir, map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens": map[string]interface{}{
			"access_token": "tok-codex-merged",
		},
	})

	// Use empty TMPDIR so ReadStatuslineRateLimits fails → API fallback for Claude
	emptyTmp := filepath.Join(tmpDir, "empty")
	if err := os.MkdirAll(emptyTmp, 0755); err != nil {
		t.Fatalf("mkdir emptyTmp: %v", err)
	}
	t.Setenv("TMPDIR", emptyTmp)

	opts := MergedUsageOptions{
		ClaudeCredsPath: credsPath,
		ClaudeAPIURL:    claudeSrv.URL,
		CodexAPIURL:     codexSrv.URL,
	}
	return opts, tmpDir
}

// TC7: FetchMergedUsageWith — both succeed
func TestFetchMergedUsageWith_BothSucceed(t *testing.T) {
	opts, _ := setupMergedEnv(t, http.StatusOK, http.StatusOK)
	out, _, _, err := FetchMergedUsageWith(nil, nil, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "📊 Claude Usage") {
		t.Errorf("expected '📊 Claude Usage' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "📊 Codex Usage") {
		t.Errorf("expected '📊 Codex Usage' in output, got:\n%s", out)
	}
}

// TC17: Claude skip (no creds) + Codex valid
func TestFetchMergedUsageWith_ClaudeSkip(t *testing.T) {
	opts, tmpDir := setupMergedEnv(t, http.StatusOK, http.StatusOK)
	// Point Claude creds to a non-existent path so it returns ErrCredentialUnavailable
	opts.ClaudeCredsPath = filepath.Join(tmpDir, "nonexistent.json")

	out, _, _, err := FetchMergedUsageWith(nil, nil, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "📊 Codex Usage") {
		t.Errorf("expected '📊 Codex Usage' in output, got:\n%s", out)
	}
	if strings.Contains(out, "Claude Usage") {
		t.Errorf("did not expect 'Claude Usage' in output when creds missing, got:\n%s", out)
	}
}

// TC18: Both credentials missing
func TestFetchMergedUsageWith_BothMissing(t *testing.T) {
	emptyTmp := t.TempDir()
	t.Setenv("TMPDIR", emptyTmp)
	t.Setenv("CODEX_HOME", t.TempDir()) // no auth.json

	opts := MergedUsageOptions{
		ClaudeCredsPath: filepath.Join(t.TempDir(), "nonexistent.json"),
	}
	_, _, _, err := FetchMergedUsageWith(nil, nil, opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "No usage credentials found") {
		t.Errorf("expected 'No usage credentials found' in error, got: %v", err)
	}
}

// TC19: Claude success + Codex real error (401)
func TestFetchMergedUsageWith_ClaudeOKCodexRealError(t *testing.T) {
	opts, _ := setupMergedEnv(t, http.StatusOK, http.StatusUnauthorized)
	out, _, _, err := FetchMergedUsageWith(nil, nil, opts)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !strings.Contains(out, "📊 Claude Usage") {
		t.Errorf("expected '📊 Claude Usage' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "⚠️ Codex:") {
		t.Errorf("expected '⚠️ Codex:' notice in output, got:\n%s", out)
	}
}

// TC20: Codex success + Claude real error (500)
func TestFetchMergedUsageWith_CodexOKClaudeRealError(t *testing.T) {
	opts, _ := setupMergedEnv(t, http.StatusInternalServerError, http.StatusOK)
	out, _, _, err := FetchMergedUsageWith(nil, nil, opts)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if !strings.Contains(out, "📊 Codex Usage") {
		t.Errorf("expected '📊 Codex Usage' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "⚠️ Claude:") {
		t.Errorf("expected '⚠️ Claude:' notice in output, got:\n%s", out)
	}
}

// Ensure fmt import is used (used in helper only via Sprintf if needed).
var _ = fmt.Sprintf

func TestReadContextUsagePi(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "tg-cli", "context")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir context dir: %v", err)
	}
	write := func(sid, body string) {
		p := filepath.Join(dir, sid+".json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatalf("write %s: %v", sid, err)
		}
		t.Cleanup(func() { os.Remove(p) })
	}

	// Case 1: pi-shape, non-zero tokens -> pct = 48000/(180000-16384)*100 = 29.
	write("tc_pi_ctx_1", `{"backend":"pi","context_tokens":48000,"context_window":180000,"reserve_tokens":16384}`)
	if pct, used, total, ok := ReadContextUsage("tc_pi_ctx_1"); !ok || pct != 29 || used != 48000 || total != 163616 {
		t.Errorf("case1: got pct=%d used=%d total=%d ok=%v; want 29/48000/163616/true", pct, used, total, ok)
	}

	// Case 2: tokens-null observable (extension writes 0) -> 0% per the boss ruling.
	write("tc_pi_ctx_2", `{"backend":"pi","context_tokens":0,"context_window":180000,"reserve_tokens":16384}`)
	if pct, used, total, ok := ReadContextUsage("tc_pi_ctx_2"); !ok || pct != 0 || used != 0 || total != 163616 {
		t.Errorf("case2: got pct=%d used=%d total=%d ok=%v; want 0/0/163616/true", pct, used, total, ok)
	}

	// Case 3: no window (context_window 0) -> effLimit <= 0 -> ok=false (no segment).
	write("tc_pi_ctx_3", `{"backend":"pi","context_tokens":0,"context_window":0,"reserve_tokens":16384}`)
	if _, _, _, ok := ReadContextUsage("tc_pi_ctx_3"); ok {
		t.Errorf("case3: got ok=true; want ok=false for zero window")
	}

	// Case 4: CC-shape file still uses size*80/100 (regression guard; cc/codex numbers must stay byte-identical).
	write("tc_pi_ctx_4", `{"context_window_size":200000,"current_usage":{"input_tokens":80000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	if pct, used, total, ok := ReadContextUsage("tc_pi_ctx_4"); !ok || pct != 50 || used != 80000 || total != 160000 {
		t.Errorf("case4 (CC regression): got pct=%d used=%d total=%d ok=%v; want 50/80000/160000/true", pct, used, total, ok)
	}
}

// TestNormalizePiToolName covers Round-2 Item 2: the six pi tools with a CC analogue map to the canonical CC
// name; `ls` is deliberately left UNCHANGED (no CC analogue — normalising it would silence ls via
// ShouldNotifyTool, note3 CHANGE 2); unknown/codex names pass through untouched.
func TestNormalizePiToolName(t *testing.T) {
	cases := map[string]string{
		"bash": "Bash", "read": "Read", "write": "Write", "edit": "Edit",
		"grep": "Grep", "find": "Glob",
		"ls":     "ls",     // UNCHANGED — no CC analogue
		"Bash":   "Bash",   // already canonical -> untouched
		"unknown": "unknown", // pass-through
	}
	for in, want := range cases {
		if got := NormalizePiToolName(in); got != want {
			t.Errorf("NormalizePiToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

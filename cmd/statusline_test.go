package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/helpers"
)

// feedStdin replaces os.Stdin with a pipe containing data and restores it on cleanup.
func feedStdin(t *testing.T, data string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.WriteString(data)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = origStdin
		r.Close()
	})
}

// captureStdout replaces os.Stdout with a pipe to suppress output and restores it on cleanup.
func captureStdout(t *testing.T) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		w.Close()
		r.Close()
	})
}

// setTmpDir sets TMPDIR to dir and restores the original value on cleanup.
func setTmpDir(t *testing.T, dir string) {
	t.Helper()
	orig := os.Getenv("TMPDIR")
	os.Setenv("TMPDIR", dir)
	t.Cleanup(func() {
		os.Setenv("TMPDIR", orig)
	})
}

// TC5: statusline writes rate_limits and context_window fields to the context JSON file.
func TestRunStatusline_RateLimits(t *testing.T) {
	tmpDir := t.TempDir()
	setTmpDir(t, tmpDir)
	captureStdout(t)

	input := `{"session_id":"test-session","version":"1.0","context_window":{"context_window_size":200000,"current_usage":{"input_tokens":50000}},"rate_limits":{"five_hour":{"used_percentage":25,"resets_at":1780000000},"seven_day":{"used_percentage":8,"resets_at":1781000000}}}`
	feedStdin(t, input)

	if err := runStatusline(nil, nil); err != nil {
		t.Fatalf("runStatusline error: %v", err)
	}

	outPath := filepath.Join(tmpDir, "tg-cli", "context", "test-session.json")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output file not found: %v", err)
	}

	var ctx map[string]interface{}
	if err := json.Unmarshal(data, &ctx); err != nil {
		t.Fatalf("invalid JSON in output: %v", err)
	}

	// Assert context_window_size is present
	if _, ok := ctx["context_window_size"]; !ok {
		t.Error("context_window_size missing from output")
	}

	// Assert current_usage is present
	if _, ok := ctx["current_usage"]; !ok {
		t.Error("current_usage missing from output")
	}

	// Assert rate_limits is present with correct nested values
	rlRaw, ok := ctx["rate_limits"]
	if !ok {
		t.Fatal("rate_limits missing from output")
	}
	rl, ok := rlRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("rate_limits is not an object, got %T", rlRaw)
	}
	fiveHour, ok := rl["five_hour"].(map[string]interface{})
	if !ok {
		t.Fatal("rate_limits.five_hour missing or wrong type")
	}
	if pct, _ := fiveHour["used_percentage"].(float64); pct != 25 {
		t.Errorf("five_hour.used_percentage = %v, want 25", pct)
	}
	sevenDay, ok := rl["seven_day"].(map[string]interface{})
	if !ok {
		t.Fatal("rate_limits.seven_day missing or wrong type")
	}
	if pct, _ := sevenDay["used_percentage"].(float64); pct != 8 {
		t.Errorf("seven_day.used_percentage = %v, want 8", pct)
	}
}

// TC15: data written by runStatusline round-trips correctly through ReadStatuslineRateLimits.
func TestRunStatusline_TMPDIR(t *testing.T) {
	tmpDir := t.TempDir()
	setTmpDir(t, tmpDir)
	captureStdout(t)

	input := `{"session_id":"test-session","version":"1.0","context_window":{"context_window_size":200000,"current_usage":{"input_tokens":50000}},"rate_limits":{"five_hour":{"used_percentage":25,"resets_at":1780000000},"seven_day":{"used_percentage":8,"resets_at":1781000000}}}`
	feedStdin(t, input)

	if err := runStatusline(nil, nil); err != nil {
		t.Fatalf("runStatusline error: %v", err)
	}

	rl, err := helpers.ReadStatuslineRateLimits()
	if err != nil {
		t.Fatalf("ReadStatuslineRateLimits error: %v", err)
	}
	if rl.FiveHour == nil {
		t.Fatal("FiveHour is nil")
	}
	if rl.FiveHour.UsedPercentage != 25 {
		t.Errorf("FiveHour.UsedPercentage = %v, want 25", rl.FiveHour.UsedPercentage)
	}
}

// TC16: statusline writes context fields but omits rate_limits when not present in input.
func TestRunStatusline_NoRateLimits(t *testing.T) {
	tmpDir := t.TempDir()
	setTmpDir(t, tmpDir)
	captureStdout(t)

	input := `{"session_id":"test-session","version":"1.0","context_window":{"context_window_size":200000}}`
	feedStdin(t, input)

	if err := runStatusline(nil, nil); err != nil {
		t.Fatalf("runStatusline error: %v", err)
	}

	outPath := filepath.Join(tmpDir, "tg-cli", "context", "test-session.json")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output file not found: %v", err)
	}

	var ctx map[string]interface{}
	if err := json.Unmarshal(data, &ctx); err != nil {
		t.Fatalf("invalid JSON in output: %v", err)
	}

	// context_window_size must be present
	if _, ok := ctx["context_window_size"]; !ok {
		t.Error("context_window_size missing from output")
	}

	// rate_limits must NOT be present
	if strings.Contains(string(data), "rate_limits") {
		t.Error("rate_limits should not be present when not in input, but found in output")
	}
}

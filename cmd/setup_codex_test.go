package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCodexHooks(t *testing.T) {
	hookCommand := "/home/u/.local/bin/tg-cli hook --port 12500"

	// check seeds a legacy config.toml in codexDir, runs InstallCodexHooks(home, ...)
	// twice, and verifies the migration result + byte-level idempotency. codexDir is
	// where the function is expected to write (CODEX_HOME when set, else home/.codex).
	check := func(t *testing.T, home, codexDir string) {
		if err := os.MkdirAll(codexDir, 0755); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(codexDir, "config.toml")
		// Pre-existing config.toml with the legacy codex_hooks flag (production state).
		if err := os.WriteFile(configPath, []byte("model = \"gpt-5\"\n\n[features]\ncodex_hooks = true\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := InstallCodexHooks(home, hookCommand); err != nil {
			t.Fatalf("InstallCodexHooks: %v", err)
		}
		// config.toml: codex_hooks removed, hooks = true present under [features].
		cfg, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		cfgStr := string(cfg)
		if strings.Contains(cfgStr, "codex_hooks") {
			t.Errorf("config.toml still contains codex_hooks:\n%s", cfgStr)
		}
		if !strings.Contains(cfgStr, "hooks = true") {
			t.Errorf("config.toml missing hooks = true:\n%s", cfgStr)
		}
		if !strings.Contains(cfgStr, "[features]") {
			t.Errorf("config.toml missing [features]:\n%s", cfgStr)
		}
		// hooks.json: all 5 events, each with the tg-cli command + --backend codex.
		hooksPath := filepath.Join(codexDir, "hooks.json")
		data, err := os.ReadFile(hooksPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Hooks map[string]json.RawMessage `json:"hooks"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		for _, ev := range []string{"SessionStart", "Stop", "PreToolUse", "PostToolUse", "UserPromptSubmit"} {
			if _, ok := parsed.Hooks[ev]; !ok {
				t.Errorf("hooks.json missing event %s", ev)
			}
		}
		if !strings.Contains(string(data), hookCommand+" --backend codex") {
			t.Errorf("hooks.json missing command %q --backend codex:\n%s", hookCommand, string(data))
		}
		// Idempotency: a second run leaves config.toml and hooks.json byte-identical.
		if err := InstallCodexHooks(home, hookCommand); err != nil {
			t.Fatalf("InstallCodexHooks (2nd run): %v", err)
		}
		if cfg2, _ := os.ReadFile(configPath); string(cfg2) != cfgStr {
			t.Errorf("config.toml changed on 2nd run:\nfirst:\n%s\nsecond:\n%s", cfgStr, string(cfg2))
		}
		if data2, _ := os.ReadFile(hooksPath); string(data2) != string(data) {
			t.Errorf("hooks.json changed on 2nd run:\nfirst:\n%s\nsecond:\n%s", string(data), string(data2))
		}
	}

	// Explicit CODEX_HOME path: function must write to $CODEX_HOME (home arg unused).
	t.Run("explicit CODEX_HOME", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		check(t, t.TempDir(), codexHome)
	})

	// Default path (production scenario): CODEX_HOME empty → function must write to
	// $home/.codex. This is the branch the production `service upgrade` actually hits.
	t.Run("default home/.codex", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "")
		home := t.TempDir()
		check(t, home, filepath.Join(home, ".codex"))
	})
}

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/internal/config"
)

func TestInstallCCHooks(t *testing.T) {
	// Save and restore config.ConfigDir so other tests are not affected.
	origConfigDir := config.ConfigDir
	defer func() { config.ConfigDir = origConfigDir }()
	// Test uses the default instance (no --config-dir).
	config.ConfigDir = ""

	tmp := t.TempDir()
	settingsPath := filepath.Join(tmp, "settings.json")
	hookCommand := "/home/u/.local/bin/tg-cli hook --port 12500"

	// Seed settings.json with:
	// (a) MessageDisplay: a tg-cli hook WITH "async": true (will be replaced by template which has no async).
	// (b) UserPromptSubmit: a user non-tg-cli hook ("printf hello") that must be preserved.
	initial := map[string]interface{}{
		"hooks": map[string]interface{}{
			"MessageDisplay": []interface{}{
				map[string]interface{}{
					"matcher": "",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": hookCommand + " --backend cc",
							"timeout": 10,
							"async":   true, // stale field — template has no async for MessageDisplay
						},
					},
				},
			},
			"UserPromptSubmit": []interface{}{
				map[string]interface{}{
					"matcher": "",
					"hooks": []interface{}{
						map[string]interface{}{
							"type":    "command",
							"command": "printf hello",
							"timeout": 5,
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	// First run.
	if err := InstallCCHooks(settingsPath, hookCommand); err != nil {
		t.Fatalf("InstallCCHooks: %v", err)
	}

	result, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	// (a) MessageDisplay: the old tg-cli entry (with "async": true) must be replaced.
	// The template's MessageDisplay has no "async" field — verify it's absent in the merged output.
	var settings map[string]interface{}
	if err := json.Unmarshal(result, &settings); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	hooksMap, _ := settings["hooks"].(map[string]interface{})
	mdRaw, _ := json.Marshal(hooksMap["MessageDisplay"])
	mdStr := string(mdRaw)
	if strings.Contains(mdStr, `"async"`) {
		t.Errorf("MessageDisplay still contains async after sync (should be replaced by template):\n%s", mdStr)
	}
	// Template command must be present.
	if !strings.Contains(mdStr, hookCommand+" --backend cc") {
		t.Errorf("MessageDisplay missing expected hook command:\n%s", mdStr)
	}

	// (b) UserPromptSubmit: user's "printf hello" hook must be preserved alongside tg-cli entry.
	upsRaw, _ := json.Marshal(hooksMap["UserPromptSubmit"])
	upsStr := string(upsRaw)
	if !strings.Contains(upsStr, "printf hello") {
		t.Errorf("UserPromptSubmit lost user's printf hello hook:\n%s", upsStr)
	}
	// Template's tg-cli entry must also be present.
	if !strings.Contains(upsStr, hookCommand+" --backend cc") {
		t.Errorf("UserPromptSubmit missing tg-cli hook command:\n%s", upsStr)
	}

	// (c) Idempotency: second run produces byte-identical output.
	if err := InstallCCHooks(settingsPath, hookCommand); err != nil {
		t.Fatalf("InstallCCHooks (2nd run): %v", err)
	}
	result2, _ := os.ReadFile(settingsPath)
	if string(result2) != string(result) {
		t.Errorf("settings.json changed on 2nd run:\nfirst:\n%s\nsecond:\n%s", string(result), string(result2))
	}
}

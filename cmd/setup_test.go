package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallPiExtension verifies the pi-extension install/uninstall lifecycle in an isolated
// pi dir (via PI_CODING_AGENT_DIR, never ~/.pi/agent), including the install-time __HOOK_PORT__
// substitution, byte-idempotency, and that uninstall removes only the file (not the dir).
func TestInstallPiExtension(t *testing.T) {
	// Isolated pi config dir with NO pre-existing extensions/ subdir (boss-machine first-run state).
	piDir := t.TempDir()
	// t.Setenv auto-restores the env after the test AND makes InstallPiExtension write into piDir.
	t.Setenv("PI_CODING_AGENT_DIR", piDir)
	// home is unused because PI_CODING_AGENT_DIR is set, but pass it anyway.
	home := t.TempDir()
	const testPort = 12777

	if err := InstallPiExtension(home, testPort); err != nil {
		t.Fatalf("InstallPiExtension: %v", err)
	}

	// extensions/ dir must have been created (MkdirAll) and the extension file written.
	extDir := filepath.Join(piDir, "extensions")
	if fi, err := os.Stat(extDir); err != nil || !fi.IsDir() {
		t.Fatalf("extensions dir missing or not a dir: err=%v", err)
	}
	target := filepath.Join(extDir, "tg-cli.ts")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("extension file missing: %v", err)
	}

	// The written file must contain the substituted port and NOT the placeholder (Ruling-2).
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "12777") {
		t.Errorf("extension missing substituted port %q:\n%s", "12777", string(content))
	}
	if strings.Contains(string(content), "__HOOK_PORT__") {
		t.Errorf("extension still contains unsubstituted placeholder __HOOK_PORT__:\n%s", string(content))
	}

	// Byte-idempotency: same port -> identical bytes on a second install.
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallPiExtension(home, testPort); err != nil {
		t.Fatalf("InstallPiExtension (2nd run): %v", err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("extension bytes changed on 2nd run:\nfirst:\n%s\nsecond:\n%s", string(before), string(after))
	}

	// No settings.json should be created anywhere under piDir (pi auto-discovers extensions).
	if _, err := os.Stat(filepath.Join(piDir, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("unexpected settings.json under piDir: err=%v", err)
	}

	// Uninstall removes ONLY the file, never the extensions/ dir.
	if err := UninstallPiExtension(home); err != nil {
		t.Fatalf("UninstallPiExtension: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("extension file should be absent after uninstall: err=%v", err)
	}
	if fi, err := os.Stat(extDir); err != nil || !fi.IsDir() {
		t.Errorf("extensions dir should still exist after uninstall: err=%v", err)
	}
}

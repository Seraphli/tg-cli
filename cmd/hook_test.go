package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Seraphli/tg-cli/internal/config"
)

// setTestConfigDir sets config.ConfigDir to dir and returns a cleanup func.
func setTestConfigDir(t *testing.T, dir string) func() {
	t.Helper()
	orig := config.ConfigDir
	config.ConfigDir = dir
	return func() { config.ConfigDir = orig }
}

// writeUpgradeFlag writes a unix timestamp string to the upgrade flag file.
func writeUpgradeFlag(t *testing.T, ts int64) {
	t.Helper()
	if err := os.WriteFile(config.UpgradeFlagPath(), []byte(strconv.FormatInt(ts, 10)), 0644); err != nil {
		t.Fatalf("writeUpgradeFlag: %v", err)
	}
}

// TestUpgradeFlagActive verifies UpgradeFlagActive behavior under flag absent, fresh, stale, and garbage cases.
func TestUpgradeFlagActive(t *testing.T) {
	dir := t.TempDir()
	defer setTestConfigDir(t, dir)()

	// Flag absent → false
	if config.UpgradeFlagActive() {
		t.Fatal("expected UpgradeFlagActive=false when flag file absent")
	}

	// Fresh timestamp (now) → true
	writeUpgradeFlag(t, time.Now().Unix())
	if !config.UpgradeFlagActive() {
		t.Fatal("expected UpgradeFlagActive=true with fresh timestamp")
	}

	// Stale timestamp (100s ago, > 60s threshold) → false
	writeUpgradeFlag(t, time.Now().Unix()-100)
	if config.UpgradeFlagActive() {
		t.Fatal("expected UpgradeFlagActive=false with stale (100s old) timestamp")
	}

	// Garbage contents → false
	if err := os.WriteFile(config.UpgradeFlagPath(), []byte("not-a-number"), 0644); err != nil {
		t.Fatalf("write garbage flag: %v", err)
	}
	if config.UpgradeFlagActive() {
		t.Fatal("expected UpgradeFlagActive=false with garbage flag contents")
	}
}

// freeTCPPort finds a free local TCP port by briefly listening then closing.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPPort listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// TestPostWithUpgradeRetry covers three scenarios:
//  1. No flag + closed port → single fast failure (no long wait).
//  2. Stale flag + closed port → fast failure (UpgradeFlagActive false).
//  3. Fresh flag + server starts 2s late → eventually succeeds within 25s cap.
func TestPostWithUpgradeRetry(t *testing.T) {
	body := []byte(`{"test":true}`)

	// -- Sub-test 1: no flag, closed port → fast failure --
	t.Run("no_flag_fast_fail", func(t *testing.T) {
		dir := t.TempDir()
		defer setTestConfigDir(t, dir)()

		port := freeTCPPort(t)
		url := fmt.Sprintf("http://127.0.0.1:%d/hook/Stop", port)
		client := &http.Client{Timeout: 3 * time.Second}

		start := time.Now()
		_, err := postWithUpgradeRetry(context.Background(), client, url, body)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected error, got nil")
		}
		// Should fail fast (well under 10s) — no retry loop without flag
		if elapsed > 10*time.Second {
			t.Errorf("took too long without flag: %v (expected < 10s)", elapsed)
		}
	})

	// -- Sub-test 2: stale flag + closed port → fast failure --
	t.Run("stale_flag_fast_fail", func(t *testing.T) {
		dir := t.TempDir()
		defer setTestConfigDir(t, dir)()

		// Write flag that is 100s old (stale)
		writeUpgradeFlag(t, time.Now().Unix()-100)

		port := freeTCPPort(t)
		url := fmt.Sprintf("http://127.0.0.1:%d/hook/Stop", port)
		client := &http.Client{Timeout: 3 * time.Second}

		start := time.Now()
		_, err := postWithUpgradeRetry(context.Background(), client, url, body)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected error with stale flag, got nil")
		}
		if elapsed > 10*time.Second {
			t.Errorf("took too long with stale flag: %v (expected < 10s)", elapsed)
		}
	})

	// -- Sub-test 3: fresh flag + server starts ~2s late → succeeds --
	t.Run("fresh_flag_server_starts_late", func(t *testing.T) {
		dir := t.TempDir()
		defer setTestConfigDir(t, dir)()

		// Write a fresh flag
		writeUpgradeFlag(t, time.Now().Unix())

		port := freeTCPPort(t)
		url := fmt.Sprintf("http://127.0.0.1:%d/hook/Stop", port)
		client := &http.Client{Timeout: 5 * time.Second}

		// Start server after ~3s delay
		go func() {
			time.Sleep(3 * time.Second)
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				// Port grabbed by something else — nothing we can do
				return
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/hook/Stop", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			})
			srv := &http.Server{Handler: mux}
			// Accept one request then shut down
			go srv.Serve(l)
			time.AfterFunc(10*time.Second, func() { srv.Close() })
		}()

		start := time.Now()
		resp, err := postWithUpgradeRetry(context.Background(), client, url, body)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("expected success with fresh flag + late server, got err=%v (elapsed=%v)", err, elapsed)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		if elapsed > 25*time.Second {
			t.Errorf("took longer than 25s cap: %v", elapsed)
		}
	})
}

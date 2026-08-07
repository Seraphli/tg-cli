package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestGetWithUpgradeReconnect covers the long-poll reconnect helper shared by mailbox receive (Fix 4)
// and session watch (Fix 5):
//  1. No flag + closed port → single fast failure (abnormal drop, no retry loop).
//  2. Stale flag + closed port → fast failure (UpgradeFlagActive false).
//  3. Fresh flag + server starts ~3s late → reconnects and eventually returns a 200 response.
//  4. Cancelled context → returns ctx.Err() promptly without reporting a lost connection.
func TestGetWithUpgradeReconnect(t *testing.T) {
	// -- Sub-test 1: no flag, closed port → fast failure --
	t.Run("no_flag_fast_fail", func(t *testing.T) {
		dir := t.TempDir()
		defer setTestConfigDir(t, dir)()

		port := freeTCPPort(t)
		url := fmt.Sprintf("http://127.0.0.1:%d/session/watch?name=x", port)

		start := time.Now()
		_, err := getWithUpgradeReconnect(context.Background(), url, "")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if elapsed > 10*time.Second {
			t.Errorf("took too long without flag: %v (expected < 10s)", elapsed)
		}
	})

	// -- Sub-test 2: stale flag, closed port → fast failure --
	t.Run("stale_flag_fast_fail", func(t *testing.T) {
		dir := t.TempDir()
		defer setTestConfigDir(t, dir)()
		writeUpgradeFlag(t, time.Now().Unix()-100)

		port := freeTCPPort(t)
		url := fmt.Sprintf("http://127.0.0.1:%d/session/watch?name=x", port)

		start := time.Now()
		_, err := getWithUpgradeReconnect(context.Background(), url, "")
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected error with stale flag, got nil")
		}
		if elapsed > 10*time.Second {
			t.Errorf("took too long with stale flag: %v (expected < 10s)", elapsed)
		}
	})

	// -- Sub-test 3: fresh flag + server starts ~3s late → reconnects and succeeds --
	t.Run("fresh_flag_server_starts_late", func(t *testing.T) {
		dir := t.TempDir()
		defer setTestConfigDir(t, dir)()
		writeUpgradeFlag(t, time.Now().Unix())

		port := freeTCPPort(t)
		url := fmt.Sprintf("http://127.0.0.1:%d/session/watch?name=x", port)

		go func() {
			time.Sleep(3 * time.Second)
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return // port grabbed by something else — nothing we can do
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/session/watch", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"ok":true}`))
			})
			srv := &http.Server{Handler: mux}
			go srv.Serve(l)
			time.AfterFunc(10*time.Second, func() { srv.Close() })
		}()

		start := time.Now()
		resp, err := getWithUpgradeReconnect(context.Background(), url, "")
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("expected success with fresh flag + late server, got err=%v (elapsed=%v)", err, elapsed)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		if elapsed > 25*time.Second {
			t.Errorf("took longer than expected: %v", elapsed)
		}
	})

	// -- Sub-test 4: cancelled context → returns ctx.Err() promptly --
	t.Run("ctx_cancelled", func(t *testing.T) {
		dir := t.TempDir()
		defer setTestConfigDir(t, dir)()
		writeUpgradeFlag(t, time.Now().Unix()) // fresh flag: prove ctx cancel wins over reconnect

		port := freeTCPPort(t)
		url := fmt.Sprintf("http://127.0.0.1:%d/session/watch?name=x", port)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		_, err := getWithUpgradeReconnect(ctx, url, "")
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if elapsed > 5*time.Second {
			t.Errorf("cancelled context should return promptly, took %v", elapsed)
		}
	})
}

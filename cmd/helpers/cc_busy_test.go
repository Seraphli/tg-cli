package helpers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/cmd/stores"
	"github.com/Seraphli/tg-cli/internal/injector"
	"github.com/Seraphli/tg-cli/internal/logger"
)

// fixtureDir is the cc-busy fixture directory relative to this package (cmd/helpers).
const fixtureDir = "../../tests/fixtures/cc-busy"

// fixture returns the viewport text of one cc-busy fixture.
func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(body)
}

// TestCCBusyFromViewportFixtures runs the pure classifier ccBusyFromViewport over every fixture listed in
// manifest.tsv (each data line is `<filename>\t<busy|idle|waiting|compacting>`) and asserts the verdict matches. 17
// fixtures exercise all 6 spinner glyphs (·✢*✻✽✶); a manifest with fewer than 17 data lines fails the test
// (guards against a silently-empty fixture dir).
func TestCCBusyFromViewportFixtures(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.tsv"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	verdicts := map[string]stores.BusyState{"busy": stores.BusyWorking, "idle": stores.BusyIdle, "waiting": stores.BusyWaiting, "compacting": stores.BusyCompacting}
	dataLines := 0
	for _, ln := range strings.Split(string(manifest), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		cols := strings.Split(ln, "\t")
		if len(cols) != 2 {
			t.Fatalf("malformed manifest line %q, want <filename>\\t<busy|idle|waiting|compacting>", ln)
		}
		name := cols[0]
		want, ok := verdicts[cols[1]]
		if !ok {
			t.Fatalf("fixture %s: unknown verdict %q, want busy|idle|waiting|compacting", name, cols[1])
		}
		if got, _ := ccBusyFromViewport(fixture(t, name), false); got != want {
			t.Errorf("ccBusyFromViewport(%s) = %v, want %v", name, got, want)
		}
		dataLines++
	}
	if dataLines < 17 {
		t.Fatalf("manifest has %d data lines, want >= 17 (silently-empty fixture dir?)", dataLines)
	}
}

// TestCCBusyFromViewportWaitingCountdown proves the stall banner and cc's retry banner (an API error cc retries
// for up to 3 minutes; the hang case shows "Retrying in 0s") read waiting with the countdown copied up to the
// next " · ".
func TestCCBusyFromViewportWaitingCountdown(t *testing.T) {
	for _, c := range []struct{ fixture, want string }{{"busy-apiwait.txt", "4m 40s"}, {"retry-econnreset.txt", "27s"}, {"retry-hang-0s.txt", "0s"}} {
		if got, countdown := ccBusyFromViewport(fixture(t, c.fixture), true); got != stores.BusyWaiting || countdown != c.want {
			t.Errorf("ccBusyFromViewport(%s) = (%v, %q), want (waiting, %q)", c.fixture, got, countdown, c.want)
		}
	}
}

// TestCCBusyFromViewportAgentWaitLine proves the wait line of a turn that left background agents running (one agent,
// two agents) reads idle while the CCActive stamp is fresh: a trailing MessageDisplay re-stamps it after the Stop.
func TestCCBusyFromViewportAgentWaitLine(t *testing.T) {
	for _, name := range []string{"idle-agentwait-1-2287.txt", "idle-agentwait-2-2287.txt"} {
		if got, _ := ccBusyFromViewport(fixture(t, name), true); got != stores.BusyIdle {
			t.Errorf("ccBusyFromViewport(%s, ccActive) = %v, want idle", name, got)
		}
	}
}

// TestCCBusyFromContentCaptureError proves a capture failure gives no viewport: the pane makes no decision,
// so the state is idle.
func TestCCBusyFromContentCaptureError(t *testing.T) {
	errCCBusyStub := errors.New("stub capture error")
	orig := captureViewport
	defer func() { captureViewport = orig }()
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		return "", errCCBusyStub
	}
	if got, _ := ccBusyFromContent(context.Background(), stores.NewHookRunningStateStore(), "%1"); got != stores.BusyIdle {
		t.Errorf("ccBusyFromContent on capture error = %v, want idle", got)
	}
}

// TestCCBusyFromContentRouting proves ccBusyFromContent routes the captured body through the classifier: a
// busy body -> working, an idle body -> idle. Rules are 60 ─ (U+2500).
func TestCCBusyFromContentRouting(t *testing.T) {
	busy := "✻ Kneading…\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	idle := "✻ Cooked for 5s · done 4:41 AM\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	orig := captureViewport
	defer func() { captureViewport = orig }()
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		return busy, nil
	}
	if got, _ := ccBusyFromContent(context.Background(), stores.NewHookRunningStateStore(), "%1"); got != stores.BusyWorking {
		t.Errorf("ccBusyFromContent(busy body) = %v, want working", got)
	}
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		return idle, nil
	}
	if got, _ := ccBusyFromContent(context.Background(), stores.NewHookRunningStateStore(), "%1"); got != stores.BusyIdle {
		t.Errorf("ccBusyFromContent(idle body) = %v, want idle", got)
	}
}

// TestBusyDiagnostic_IdleWhileHookRunning proves the idle diagnostic: a cc session whose stored state was busy
// turns idle while the hook store says a turn runs (a frame without input box decides nothing), and bot.log
// gets ONE line with the pane text; the next tick (the loop has stored idle) writes no second line.
func TestBusyDiagnostic_IdleWhileHookRunning(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "bot.log")
	logger.Init(logPath, false)
	t.Cleanup(func() { logger.Init(os.DevNull, false) })
	orig := captureViewport
	defer func() { captureViewport = orig }()
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) { return "no input box", nil }
	store := stores.NewHookRunningStateStore()
	store.SetRunning("%1")
	store.SetBusyState("%1", stores.BusyWorking, "")
	ccBusyFromContent(context.Background(), store, "%1")
	store.SetBusyState("%1", stores.BusyIdle, "")
	ccBusyFromContent(context.Background(), store, "%1")
	body, _ := os.ReadFile(logPath)
	if n := strings.Count(string(body), "busy diagnostic:"); n != 1 || !strings.Contains(string(body), "no input box") {
		t.Errorf("bot.log holds %d diagnostic lines, want 1 with the pane text (one per busy-to-idle transition):\n%s", n, body)
	}
}

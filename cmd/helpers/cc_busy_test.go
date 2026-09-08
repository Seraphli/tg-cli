package helpers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/internal/injector"
)

// fixtureDir is the cc-busy fixture directory relative to this package (cmd/helpers).
const fixtureDir = "../../tests/fixtures/cc-busy"

// TestCCBusyFromViewportFixtures runs the pure classifier ccBusyFromViewport over every fixture listed in
// manifest.tsv (skip the leading `#` comment; each data line is `<filename>\t<busy|idle>`) and asserts the
// verdict matches. 17 fixtures exercise all 6 spinner glyphs (·✢*✻✽✶); a manifest with fewer than 17 data
// lines fails the test (guards against a silently-empty fixture dir).
func TestCCBusyFromViewportFixtures(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join(fixtureDir, "manifest.tsv"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	dataLines := 0
	for _, ln := range strings.Split(string(manifest), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		cols := strings.Split(ln, "\t")
		if len(cols) != 2 {
			t.Fatalf("malformed manifest line %q, want <filename>\\t<busy|idle>", ln)
		}
		name, verdict := cols[0], cols[1]
		want := verdict == "busy"
		if verdict != "busy" && verdict != "idle" {
			t.Fatalf("fixture %s: unknown verdict %q, want busy|idle", name, verdict)
		}
		body, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		got := ccBusyFromViewport(string(body))
		if got != want {
			t.Errorf("ccBusyFromViewport(%s) = %v, want %v", name, got, want)
		}
		t.Logf("%-26s got=%-5v want=%-5v %s", name, got, want, verdict)
		dataLines++
	}
	if dataLines < 17 {
		t.Fatalf("manifest has %d data lines, want >= 17 (silently-empty fixture dir?)", dataLines)
	}
}

// TestCCBusyFromContentCaptureError proves a capture failure classifies as IDLE: with the capture seam
// stubbed to return an error, ccBusyFromContent must return false.
func TestCCBusyFromContentCaptureError(t *testing.T) {
	errCCBusyStub := errors.New("stub capture error")
	orig := captureViewport
	defer func() { captureViewport = orig }()
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		return "", errCCBusyStub
	}
	if got := ccBusyFromContent(context.Background(), "%1"); got != false {
		t.Errorf("ccBusyFromContent on capture error = %v, want false (capture error -> IDLE)", got)
	}
}

// TestCCBusyFromContentRouting proves ccBusyFromContent routes the captured body through the classifier: a
// busy body -> true, an idle body -> false. Rules are 60 ─ (U+2500).
func TestCCBusyFromContentRouting(t *testing.T) {
	busy := "✻ Kneading…\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	idle := "✻ Cooked for 5s · done\n" + strings.Repeat("─", 60) + "\n❯ \n" + strings.Repeat("─", 60) + "\n"
	orig := captureViewport
	defer func() { captureViewport = orig }()
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		return busy, nil
	}
	if got := ccBusyFromContent(context.Background(), "%1"); got != true {
		t.Errorf("ccBusyFromContent(busy body) = %v, want true", got)
	}
	captureViewport = func(ctx context.Context, target injector.TmuxTarget) (string, error) {
		return idle, nil
	}
	if got := ccBusyFromContent(context.Background(), "%1"); got != false {
		t.Errorf("ccBusyFromContent(idle body) = %v, want false", got)
	}
}

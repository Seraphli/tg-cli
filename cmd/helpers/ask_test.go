package helpers

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Seraphli/tg-cli/internal/injector"
)

func TestSentinels(t *testing.T) {
	// ErrInjectNotConfirmed wraps properly
	err := fmt.Errorf("%w for target=%%5", ErrInjectNotConfirmed)
	if !errors.Is(err, ErrInjectNotConfirmed) {
		t.Fatal("expected errors.Is to match ErrInjectNotConfirmed")
	}

	// ErrSubmitAfterPaste wraps properly
	err2 := fmt.Errorf("%w: tmux error", injector.ErrSubmitAfterPaste)
	if !errors.Is(err2, injector.ErrSubmitAfterPaste) {
		t.Fatal("expected errors.Is to match ErrSubmitAfterPaste")
	}
}

func TestTruncateQueueTexts(t *testing.T) {
	short := []string{"hello", "world"}
	result := truncateQueueTexts(short, 3500)
	if result != "hello\nworld" {
		t.Errorf("expected no truncation, got: %s", result)
	}

	var long []string
	for i := 0; i < 15; i++ {
		long = append(long, strings.Repeat("x", 300))
	}
	result = truncateQueueTexts(long, 3500)
	if len([]rune(result)) > 3600 {
		t.Errorf("truncated result too long: %d runes", len([]rune(result)))
	}
	if !strings.Contains(result, "truncated") {
		t.Error("expected truncation marker")
	}

	header := "⏳ Queued [abc] (15)\n📟 %0@test\n🔒 PermissionRequest pending\n──────\n"
	footer := "\n──────"
	total := header + result + footer
	if len([]rune(total)) > 4096 {
		t.Errorf("full message exceeds TG limit: %d runes", len([]rune(total)))
	}
}
